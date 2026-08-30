//go:build darwin

package statusitem

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-macos/objc"
)

// NSVariableStatusItemLength: the item is as wide as its content. It is a
// CGFloat, so it must reach objc_msgSend as a float — an untyped -1.0 constant
// does, an int -1 would be marshalled through the integer registers and give a
// zero-width item.
const nsVariableStatusItemLength = -1.0

// The selectors of the runtime class registered by ensureClass. They are
// namespaced because an Objective-C selector is process-wide: a bare -fire:
// would be answered by, or would collide with, whatever else is linked into the
// host application.
var (
	selFire = objc.Sel("goStatusItemFire:")
	selHop  = objc.Sel("goStatusItemHop:")
)

var (
	classOnce sync.Once
	classErr  error
	targetCls objc.Class

	// reg maps each item's Objective-C target instance to that item's handler
	// table. See [registry] for why the lookup cannot be a captured closure.
	reg registry

	// hopQ carries closures waiting to run on the main thread. One closure is
	// queued per -goStatusItemHop: performed, so the counts match; which
	// invocation picks up which closure does not matter, because each closure
	// signals its own waiter.
	hopQ = make(chan func(), 64)
)

// ensureClass loads AppKit and registers the one runtime class this package
// needs, once per process.
//
// It must be once: objc_allocateClassPair refuses a duplicate class name and
// returns nil, so a second registration would hand back a nil class whose
// -alloc quietly yields nil, and every menu row would then have a nil target —
// a menu that draws perfectly and does nothing when clicked.
func ensureClass() error {
	classOnce.Do(func() {
		if err := objc.Load(objc.AppKit, objc.Foundation); err != nil {
			classErr = fmt.Errorf("statusitem: loading AppKit: %w", err)
			return
		}
		targetCls, classErr = objc.RegisterClass(
			"GoMacOSStatusItemTarget",
			objc.GetClass("NSObject"),
			// MethodDef.Fn is the raw Go func; RegisterClass wraps it into an
			// IMP itself, so wrapping it here would make it wrap an IMP twice
			// and panic.
			[]objc.MethodDef{
				{
					Cmd: selFire,
					Fn: func(self objc.ID, _ objc.SEL, sender objc.ID) {
						reg.fire(uintptr(self), int(sender.Send(objc.Sel("tag"))))
					},
				},
				{
					Cmd: selHop,
					Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
						select {
						case fn := <-hopQ:
							fn()
						default:
						}
					},
				},
			},
		)
		if classErr != nil {
			classErr = fmt.Errorf("statusitem: registering the action target class: %w", classErr)
			return
		}
		if targetCls == 0 {
			classErr = ErrNoTargetClass
		}
	})
	return classErr
}

// Item is a live status item in the macOS menu bar. Its methods are safe to
// call from any goroutine; each marshals its AppKit work onto the process main
// thread.
type Item struct {
	// mu guards closed, and nothing else: every other field is touched only on
	// the main thread, inside an onMain closure.
	mu     sync.Mutex
	closed bool

	// tbl is the tag -> handler table for the menu currently installed. It is
	// swapped on the main thread, in the same closure that installs the menu,
	// so a click can never be dispatched against a table belonging to a
	// different menu.
	tbl table

	// target answers the action selector and is the object main-thread hops are
	// performed on. It is created before the first hop and never reassigned.
	target objc.ID

	statusBar objc.ID
	item      objc.ID
	button    objc.ID
	menu      objc.ID
}

// New puts a status item in the menu bar with the given title and menu. The
// title is text or an emoji ("⌘", "⏻", "42 ms"); there is no image parameter,
// which is why an empty title is refused rather than drawn.
//
// It may be called before the application's run loop is started, and from any
// goroutine — but see the package documentation: from a goroutine other than the
// main one, in a process whose main thread is not running a loop, it reports
// [ErrNoMainLoop] after [MainHopTimeout] rather than blocking for ever.
func New(title string, items []MenuItem) (*Item, error) {
	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if err := validate(items); err != nil {
		return nil, err
	}
	if err := ensureClass(); err != nil {
		return nil, err
	}

	i := &Item{}
	i.target = objc.ID(targetCls).Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	if i.target == 0 {
		return nil, ErrNoTargetClass
	}
	// The target outlives every reference AppKit holds to it, so it is retained
	// here and NEVER released — see [Item.Close] for why releasing it would be
	// the wrong kind of tidy.
	i.target.Send(objc.Sel("retain"))
	reg.put(uintptr(i.target), &i.tbl)

	rows, fns := plan(items)
	var mainErr error
	if err := i.onMain(func() {
		objc.AutoreleasePool(func() { mainErr = i.create(title, rows, fns) })
	}); err != nil {
		reg.drop(uintptr(i.target))
		return nil, err
	}
	if mainErr != nil {
		reg.drop(uintptr(i.target))
		return nil, mainErr
	}
	return i, nil
}

// create builds the status item. It runs on the main thread, inside an
// autorelease pool.
func (i *Item) create(title string, rows []row, fns []func()) error {
	// +[NSStatusBar systemStatusBar] is meaningless before AppKit has an
	// application object, and +sharedApplication is what creates one. A host
	// application has already done this; a bare binary has not, and would
	// otherwise get a nil status bar with no explanation. Neither
	// -finishLaunching nor the activation policy is touched: those belong to
	// whoever owns the process, and changing them would move a host's Dock tile
	// or replace its main menu.
	if objc.App() == 0 {
		return ErrNoApplication
	}
	i.statusBar = objc.ClassID("NSStatusBar").Send(objc.Sel("systemStatusBar"))
	if i.statusBar == 0 {
		return ErrNoStatusBar
	}
	i.item = i.statusBar.Send(objc.Sel("statusItemWithLength:"), nsVariableStatusItemLength)
	if i.item == 0 {
		return ErrNoStatusBar
	}
	// statusItemWithLength: hands back an AUTORELEASED reference, and this runs
	// inside a pool that is about to drain. Without the retain the item is freed
	// on the way out of New and the menu bar shows nothing.
	i.item.Send(objc.Sel("retain"))
	i.button = i.item.Send(objc.Sel("button"))
	if i.button == 0 {
		return ErrNoButton
	}
	i.button.Send(objc.Sel("setTitle:"), objc.NSString(title))
	i.applyMenu(rows, fns)
	return nil
}

// SetTitle replaces the text shown in the menu bar.
func (i *Item) SetTitle(s string) error {
	if err := validateTitle(s); err != nil {
		return err
	}
	if err := i.alive(); err != nil {
		return err
	}
	return i.onMain(func() {
		objc.AutoreleasePool(func() {
			i.button.Send(objc.Sel("setTitle:"), objc.NSString(s))
		})
	})
}

// SetMenu replaces the whole menu.
//
// It returns an error, unlike the setter a caller might expect, because the
// rows are validated: a row that could never be chosen, or a shortcut that
// AppKit would draw and never match, is a defect the caller wants told about at
// the call site rather than discovered by a user who clicked and got nothing.
func (i *Item) SetMenu(items []MenuItem) error {
	if err := validate(items); err != nil {
		return err
	}
	if err := i.alive(); err != nil {
		return err
	}
	rows, fns := plan(items)
	return i.onMain(func() {
		objc.AutoreleasePool(func() { i.applyMenu(rows, fns) })
	})
}

// applyMenu installs a freshly built NSMenu and swaps in its handler table. It
// runs on the main thread, inside an autorelease pool.
//
// The table is swapped HERE rather than in the caller, and that is the whole
// point of the method existing: AppKit delivers the action on this same thread,
// so no click can land between the new table and the new menu. Swapping the
// table from the calling goroutine instead leaves a window in which a click on
// the old menu looks up an old tag in the new table and runs the wrong function
// — a menu that mostly works.
func (i *Item) applyMenu(rows []row, fns []func()) {
	i.tbl.set(fns)
	menu := i.buildMenu(rows)
	i.item.Send(objc.Sel("setMenu:"), menu)
	// NSStatusItem retains its menu, so the reference from -alloc is ours to
	// give up; the previous menu's is too, now that nothing points at it.
	if i.menu != 0 {
		i.menu.Send(objc.Sel("release"))
	}
	i.menu = menu
}

// buildMenu turns rows into an NSMenu.
func (i *Item) buildMenu(rows []row) objc.ID {
	menu := objc.ClassID("NSMenu").Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	// Autoenabling asks each row's target for -validateMenuItem:. This
	// package's target class does not implement it, and AppKit's fallback for a
	// target that does not respond is to enable the row — which would light up
	// the rows deliberately left disabled. Enabling is decided here instead.
	menu.Send(objc.Sel("setAutoenablesItems:"), false)
	for _, r := range rows {
		if r.Separator {
			sep := objc.ClassID("NSMenuItem").Send(objc.Sel("separatorItem"))
			// A separator arrives with tag 0 — measured, on macOS 26.6.2, by
			// reading the tag back out of a built menu. Zero is a VALID handler
			// index, so leaving it there puts the first chooseable row's
			// function one stray action-dispatch away from a divider. It costs
			// one message to make the tag say what the row is.
			sep.Send(objc.Sel("setTag:"), r.Tag)
			menu.Send(objc.Sel("addItem:"), sep)
			continue
		}
		action := selFire
		if r.Tag == noTag {
			action = 0 // a disabled row has nothing to send
		}
		mi := objc.ClassID("NSMenuItem").Send(objc.Sel("alloc")).Send(
			objc.Sel("initWithTitle:action:keyEquivalent:"),
			objc.NSString(r.Title), action, objc.NSString(r.Key))
		mi.Send(objc.Sel("setTag:"), r.Tag)
		if r.Tag != noTag {
			mi.Send(objc.Sel("setTarget:"), i.target)
		}
		mi.Send(objc.Sel("setEnabled:"), r.Tag != noTag)
		menu.Send(objc.Sel("addItem:"), mi)
		// -addItem: retains, so the reference from -alloc is spent.
		mi.Send(objc.Sel("release"))
	}
	return menu
}

// Close removes the item from the menu bar. A second Close reports [ErrClosed].
func (i *Item) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return ErrClosed
	}
	i.closed = true
	i.mu.Unlock()

	err := i.onMain(func() {
		objc.AutoreleasePool(func() { i.remove() })
	})
	// Dropped after the removal, and only from the Go side: an action can be in
	// flight on the main thread at this moment, because a menu can be OPEN when
	// the item is removed. The registry then answers nil and the click is a
	// no-op. The target itself is deliberately never released — one small
	// NSObject per status item is leaked on purpose, because a released one
	// would be a dangling receiver for exactly that in-flight action, and that
	// is a crash inside AppKit rather than a click that did nothing.
	reg.drop(uintptr(i.target))
	return err
}

// remove takes the item out of the menu bar and gives up the references create
// took. It runs on the main thread, inside an autorelease pool.
func (i *Item) remove() {
	if i.statusBar != 0 && i.item != 0 {
		i.statusBar.Send(objc.Sel("removeStatusItem:"), i.item)
	}
	if i.menu != 0 {
		i.menu.Send(objc.Sel("release"))
		i.menu = 0
	}
	if i.item != 0 {
		i.item.Send(objc.Sel("release"))
		i.item = 0
	}
	i.button = 0
	i.statusBar = 0
}

// alive reports [ErrClosed] once the item has been removed.
func (i *Item) alive() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	return nil
}

// onMain runs fn on the process main thread and waits for it.
//
// On the main thread it runs INLINE. That matters more than it looks:
// -performSelectorOnMainThread:withObject:waitUntilDone:NO would instead queue
// the work for the next turn of a run loop, so a call made during start-up —
// before [NSApp run] — would never happen at all.
//
// Off the main thread the work is queued and waited for with a timeout, because
// waitUntilDone:YES in a process whose main thread is not running a loop blocks
// the calling goroutine FOREVER: no error, no deadlock detection (the runtime
// sees a live thread), and no frame of this package anywhere in the stack. The
// timeout turns that into [ErrNoMainLoop].
//
// A closure abandoned by the timeout is skipped if it has not started, and if it
// has, it finishes on the main thread as normal — so the worst case is a status
// item that appears after its New already returned an error, never a data race:
// every field it touches belongs to that thread.
func (i *Item) onMain(fn func()) error { return runOnMain(i.target, fn) }

// runOnMain is [Item.onMain] with the target passed in explicitly, so a caller
// with no item yet — the live suite, which hops onto the main thread to read
// AppKit properties — can use the same mechanism the package uses.
func runOnMain(target objc.ID, fn func()) error {
	if onMainThread() {
		fn()
		return nil
	}
	var abandoned atomic.Bool
	done := make(chan struct{})
	queued := func() {
		if abandoned.Load() {
			return
		}
		fn()
		close(done)
	}
	// The SEND is under the timeout too, and that is not belt-and-braces. A main
	// thread that services nothing leaves its abandoned closures in the queue,
	// so the 65th caller in a process with no run loop would block on a full
	// channel — forever, before ever reaching the timeout below. The whole point
	// of this function is that it cannot do that.
	select {
	case hopQ <- queued:
	case <-time.After(MainHopTimeout):
		return ErrNoMainLoop
	}
	target.Send(objc.Sel("performSelectorOnMainThread:withObject:waitUntilDone:"),
		selHop, objc.ID(0), false)
	select {
	case <-done:
		return nil
	case <-time.After(MainHopTimeout):
		abandoned.Store(true)
		return ErrNoMainLoop
	}
}

// onMainThread reports whether the caller is running on the process main
// thread, as AppKit understands it (+[NSThread isMainThread]).
func onMainThread() bool {
	return objc.Send[bool](objc.ClassID("NSThread"), objc.Sel("isMainThread"))
}

// OnScreen reports whether this item is actually IN the menu bar.
//
// It exists because of the one failure this package documents and could not
// otherwise answer: "a status item in a process whose main thread never runs a
// loop is an object with no window, and it is indistinguishable -- from Go --
// from one that works". A caller that has just built an item, logged that it
// did, and shown a person nothing, has no way to tell which of those happened.
// This is that way.
//
// Two questions, because they fail differently. -[NSStatusItem isVisible] is
// false for an item the system has hidden -- a full menu bar, or a person
// dragging it off with Command -- and the BUTTON's window is nil for an item
// nothing has ever drawn, which is what a missing run loop produces.
func (i *Item) OnScreen() (bool, error) {
	if err := i.alive(); err != nil {
		return false, err
	}
	var on bool
	err := i.onMain(func() {
		if i.item == 0 || i.button == 0 {
			return
		}
		if i.item.Send(objc.Sel("isVisible")) == 0 {
			return
		}
		on = i.button.Send(objc.Sel("window")) != 0
	})
	return on, err
}
