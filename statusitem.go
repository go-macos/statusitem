package statusitem

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by every entry point on non-darwin platforms
	// (a menu bar of this shape is AppKit's, and AppKit is macOS-only).
	ErrUnsupported = errors.New("statusitem: unsupported on this platform (darwin only)")

	// ErrClosed reports use of an [Item] that has already been removed from
	// the menu bar.
	ErrClosed = errors.New("statusitem: status item already removed")

	// ErrEmptyTitle reports an empty status-item title. AppKit accepts one and
	// draws a zero-width item: present in the menu bar, impossible to see and
	// impossible to click. There is no image parameter here to make an empty
	// title mean something, so it is refused rather than shipped as a mystery.
	ErrEmptyTitle = errors.New("statusitem: an empty title would be an invisible status item")

	// ErrHasNUL reports a title or key equivalent containing a NUL byte. The
	// Objective-C string bridge is +stringWithUTF8String:, which terminates at
	// the first NUL, so such a string would be silently TRUNCATED rather than
	// rejected — the caller would see a shorter menu row and no error at all.
	ErrHasNUL = errors.New("statusitem: title or key equivalent contains a NUL byte")

	// ErrSeparatorNotEmpty reports a row with an empty Title that also carries
	// a Do or a Key. An empty title is how this package spells "separator", and
	// a separator cannot be chosen, so the Do could never run. Accepting it
	// would mean silently dropping a handler the caller wrote on purpose.
	ErrSeparatorNotEmpty = errors.New("statusitem: a row with an empty title is a separator and can carry neither Do nor Key")

	// ErrKeyNotOneRune reports a key equivalent that is not exactly one
	// character. -[NSMenuItem setKeyEquivalent:] accepts any string, shows it
	// in the row, and then never matches it against a keystroke: the shortcut
	// is drawn and dead.
	ErrKeyNotOneRune = errors.New("statusitem: a key equivalent must be exactly one character")

	// ErrNoMainLoop reports that the process main thread did not service an
	// AppKit request within [MainHopTimeout]. It means no run loop is running
	// there — the caller is a goroutine in a process that has not started one,
	// or has stopped it. Without this the goroutine would block forever.
	ErrNoMainLoop = errors.New("statusitem: the main thread did not service the request (no run loop is running there)")

	// ErrNoTargetClass reports that the runtime class carrying the menu action
	// could not be created. Every menu row needs it as its target, so there is
	// nothing to hand back: an item whose rows have a nil target draws perfectly
	// and answers no click.
	ErrNoTargetClass = errors.New("statusitem: the Objective-C action target class could not be created")

	// ErrNoApplication reports that +[NSApplication sharedApplication] yielded
	// nil. AppKit has no application object, so there is no menu bar to join.
	ErrNoApplication = errors.New("statusitem: +[NSApplication sharedApplication] returned nil")

	// ErrNoButton reports that the status item has no -button. The item exists
	// but has nothing to draw a title in, which is the shape AppKit takes when
	// the process may not draw in the menu bar at all.
	ErrNoButton = errors.New("statusitem: the status item has no button to put a title in")

	// ErrNoStatusBar reports that +[NSStatusBar systemStatusBar] returned nil.
	// There is no menu bar to put an item in: no window server, or a session
	// that has none.
	ErrNoStatusBar = errors.New("statusitem: +[NSStatusBar systemStatusBar] returned nil (no menu bar in this session)")
)

// MenuItem is one row of a status item's menu.
//
// The zero value is a separator. A row with a Title and no Do is a disabled
// row — a heading, or a value shown for information. A row with a Title and a
// Do is chooseable, and the Do is called on a fresh goroutine (see the package
// documentation for why not on the main thread).
type MenuItem struct {
	// Title is the text of the row. An empty Title makes the row a separator,
	// in which case Key and Do must both be zero.
	Title string
	// Key is an optional key equivalent: exactly one character, taken with
	// Command. "," gives ⌘, — the conventional Preferences shortcut.
	Key string
	// Do is called when the row is chosen. A nil Do makes the row disabled.
	Do func()
}

// IsSeparator reports whether the row is a separator, which is exactly the
// case where Title is empty. It is the rule the rest of the package applies,
// exported so that a caller building rows programmatically can apply the same
// one instead of guessing at it.
func (m MenuItem) IsSeparator() bool { return m.Title == "" }

// MainHopTimeout is how long an exported call made from a goroutine other than
// the main one waits for the process main thread to service its AppKit work
// before reporting [ErrNoMainLoop].
//
// It is generous because it is not a performance budget: a main thread that is
// running a loop at all services the request in microseconds, and one that is
// not will never service it. The only case in between is a main thread busy
// inside a handler of its own, and five seconds of that is already a bug
// elsewhere.
const MainHopTimeout = 5 * time.Second

// noTag is the tag given to a row that cannot be chosen — a separator or a
// disabled row. AppKit's default tag is 0, which is a VALID handler index, so
// an untagged row must be given something that is not one; -1 is out of range
// for any slice, and [table.fire] treats it as the no-op it is.
const noTag = -1

// row is one NSMenuItem to build, reduced to the handful of facts the AppKit
// side needs. Separating the model from the AppKit calls is what makes menu
// construction — the part with the actual decisions in it — testable with no
// window server.
type row struct {
	// Separator marks a +[NSMenuItem separatorItem] row.
	Separator bool
	// Title is the row's text (empty exactly when Separator).
	Title string
	// Key is the row's key equivalent, possibly empty.
	Key string
	// Tag is the index into the handler table, or [noTag] for a row with no
	// handler.
	Tag int
}

// plan reduces a validated list of menu items to the rows AppKit should build
// and the handler table their tags index. The two are produced together on
// purpose: a tag that does not index the table it was numbered against is a
// menu row that runs the wrong function, which is worse than one that runs
// nothing.
//
// Only chooseable rows consume a tag, so the table is exactly as long as the
// number of handlers and every tag in the rows is in range by construction.
func plan(items []MenuItem) ([]row, []func()) {
	rows := make([]row, 0, len(items))
	var fns []func()
	for _, it := range items {
		switch {
		case it.IsSeparator():
			rows = append(rows, row{Separator: true, Tag: noTag})
		case it.Do == nil:
			rows = append(rows, row{Title: it.Title, Key: it.Key, Tag: noTag})
		default:
			rows = append(rows, row{Title: it.Title, Key: it.Key, Tag: len(fns)})
			fns = append(fns, it.Do)
		}
	}
	return rows, fns
}

// validate checks a whole menu, naming the offending row. An error that says
// only "a separator cannot carry a Do" for a fifteen-row menu sends the caller
// looking through all fifteen.
func validate(items []MenuItem) error {
	for i, it := range items {
		if err := validateItem(it); err != nil {
			return fmt.Errorf("statusitem: menu row %d: %w", i, err)
		}
	}
	return nil
}

// validateItem checks one row.
func validateItem(m MenuItem) error {
	if m.IsSeparator() {
		if m.Do != nil || m.Key != "" {
			return ErrSeparatorNotEmpty
		}
		return nil
	}
	if hasNUL(m.Title) || hasNUL(m.Key) {
		return ErrHasNUL
	}
	if m.Key != "" && utf8.RuneCountInString(m.Key) != 1 {
		return ErrKeyNotOneRune
	}
	return nil
}

// validateTitle checks a status-item title.
func validateTitle(s string) error {
	switch {
	case s == "":
		return ErrEmptyTitle
	case hasNUL(s):
		return ErrHasNUL
	}
	return nil
}

// hasNUL reports whether s contains a NUL byte.
func hasNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

// table is the tag -> handler mapping for one status item's current menu. It is
// replaced wholesale when the menu is replaced, and it is guarded by a mutex
// because AppKit calls the action selector from the main thread while the owner
// may be replacing the menu from any goroutine.
type table struct {
	mu  sync.Mutex
	fns []func()
}

// set replaces the handler table.
func (t *table) set(fns []func()) {
	t.mu.Lock()
	t.fns = fns
	t.mu.Unlock()
}

// fire runs the handler for tag, if there is one, on a fresh goroutine.
//
// An out-of-range tag is a NO-OP, never a panic. It is reachable through
// several honest paths — a row AppKit tagged 0 by default, a menu replaced
// between the click and the delivery of the action, a submenu somebody adds
// later — and a panic here happens inside an Objective-C frame on the main
// thread, where Go's recovery story is "the application is gone".
func (t *table) fire(tag int) {
	t.mu.Lock()
	var fn func()
	if tag >= 0 && tag < len(t.fns) {
		fn = t.fns[tag]
	}
	t.mu.Unlock()
	if fn == nil {
		return
	}
	go fn()
}

// registry maps an Objective-C target instance to the dispatch table of the
// status item that owns it.
//
// It exists because a runtime class can only be registered ONCE per process
// (objc_allocateClassPair refuses a duplicate name), so the action-method
// closure is created before any [Item] exists and cannot capture one. It
// receives the target instance as its receiver instead, and looks the table up
// here. That is also what keeps tags per-item and small: a process-wide tag
// space would grow without bound every time any menu was replaced.
type registry struct {
	mu sync.Mutex
	m  map[uintptr]*table
}

// put records the table belonging to a target instance.
func (r *registry) put(target uintptr, t *table) {
	r.mu.Lock()
	if r.m == nil {
		r.m = map[uintptr]*table{}
	}
	r.m[target] = t
	r.mu.Unlock()
}

// get returns the table for a target instance, or nil if there is none.
func (r *registry) get(target uintptr) *table {
	r.mu.Lock()
	t := r.m[target]
	r.mu.Unlock()
	return t
}

// drop forgets a target instance.
func (r *registry) drop(target uintptr) {
	r.mu.Lock()
	delete(r.m, target)
	r.mu.Unlock()
}

// fire dispatches a chosen row to its handler. An unknown target — an action
// arriving after [Item.Close] has dropped it, which really happens because a
// menu can be open at the moment the item is removed — is a no-op.
func (r *registry) fire(target uintptr, tag int) {
	if t := r.get(target); t != nil {
		t.fire(tag)
	}
}
