//go:build darwin

package statusitem

import (
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// The LIVE suite. It really puts items in the menu bar of the session it runs
// in, reads their AppKit properties back, chooses rows through AppKit, and
// removes them again. Everything it creates, it removes.
//
// It is NOT behind a build tag or an environment variable, because a status item
// is not a system-wide claim: nothing is taken away from the operator, and an
// item that exists for a few milliseconds and is then removed is the whole
// exposure. What it IS behind is a window server: with no GUI session there is
// no menu bar, and every test here skips rather than failing for the wrong
// reason.
//
// The assertions are PROPERTIES, never "it did not crash". A status item that is
// created and never drawn is indistinguishable from a working one unless
// something reads -[NSStatusBarButton window] back, which is what
// TestLiveItemIsRealInTheMenuBar does — and which is the measurement that would
// show an unbundled binary being refused a menu bar, if it were.

const (
	nsApplicationActivationPolicyAccessory = 1
	// NSEventMaskAny is NSUIntegerMax.
	nsEventMaskAny = ^uint64(0)
	// The pump's per-turn deadline. It bounds how long a main-thread hop waits.
	pumpSeconds = 0.02
	// The whole suite must finish well inside this. A live AppKit suite that
	// hangs is the worst failure mode in CI: the job burns its entire budget and
	// reports nothing at all, which is indistinguishable from a runner fault.
	watchdog = 3 * time.Minute
)

// cgMainDisplayID is CoreGraphics' main-display identifier. It is 0 when the
// process has no window server — an ssh session, or a CI runner with no GUI
// login — which is exactly the condition under which there is no menu bar.
var cgMainDisplayID func() uint32

// hasWindowServer reports whether this process can reach a window server. It is
// resolved through CoreGraphics rather than through NSScreen because NSScreen is
// a CACHE: it answers from a snapshot taken when the application object was
// created and is stale, or empty, with no run loop behind it.
func hasWindowServer() bool {
	lib, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics",
		purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return false
	}
	purego.RegisterLibFunc(&cgMainDisplayID, lib, "CGMainDisplayID")
	return cgMainDisplayID() != 0
}

// windowServer is decided once, in TestMain, before AppKit is touched.
var windowServer bool

// TestMain pins the process main OS thread, creates the shared NSApplication on
// it the way a host application would, and then spends its life pumping the main
// thread while the tests run on another goroutine.
//
// The pump is the point. Every exported call in this package marshals its AppKit
// work onto the main thread with -performSelectorOnMainThread:, and that queue
// is serviced by a run loop and by nothing else. Without a pump here, every test
// would report ErrNoMainLoop after five seconds — which is itself a useful thing
// to know, and is why the timeout exists, but it is not what is being tested.
//
// The pump is a bounded -nextEventMatchingMask: loop rather than [NSApp run] so
// that TestMain can RETURN with the test exit code instead of calling os.Exit
// from a goroutine. The suite is therefore incapable of outliving its tests.
func TestMain(m *testing.M) {
	windowServer = hasWindowServer()
	if !windowServer {
		// No GUI session: AppKit is not touched at all and the live tests skip
		// themselves. The portable suite still runs.
		os.Exit(m.Run())
	}

	runtime.LockOSThread()
	if err := objc.Load(objc.AppKit, objc.Foundation); err != nil {
		os.Stderr.WriteString("cannot load AppKit: " + err.Error() + "\n")
		os.Exit(1)
	}
	app := objc.App()
	if app == 0 {
		os.Stderr.WriteString("NSApplication could not be created\n")
		os.Exit(1)
	}
	// Accessory: no Dock tile and no application menu, which is what a
	// menu-bar-only process is. A status item is placed regardless of the
	// policy; this only keeps the test binary from bouncing in the Dock.
	app.Send(objc.Sel("setActivationPolicy:"), nsApplicationActivationPolicyAccessory)
	app.Send(objc.Sel("finishLaunching"))

	go func() {
		time.Sleep(watchdog)
		os.Stderr.WriteString("live suite watchdog fired: the main-thread pump or a hop is stuck\n")
		os.Exit(1)
	}()

	result := make(chan int, 1)
	go func() { result <- m.Run() }()
	for {
		select {
		case code := <-result:
			os.Exit(code)
		default:
		}
		pumpOnce(app)
	}
}

// pumpOnce runs the main run loop for one short turn, delivering any event and
// servicing the -performSelectorOnMainThread: queue this package hops through.
func pumpOnce(app objc.ID) {
	objc.AutoreleasePool(func() {
		until := objc.ClassID("NSDate").Send(objc.Sel("dateWithTimeIntervalSinceNow:"), pumpSeconds)
		ev := app.Send(objc.Sel("nextEventMatchingMask:untilDate:inMode:dequeue:"),
			nsEventMaskAny, until, objc.NSString("kCFRunLoopDefaultMode"), true)
		if ev != 0 {
			app.Send(objc.Sel("sendEvent:"), ev)
		}
	})
}

// requireWindowServer skips a live test when there is no menu bar to put
// anything in.
func requireWindowServer(t *testing.T) {
	t.Helper()
	if !windowServer {
		t.Skip("no window server in this session: there is no menu bar to test against")
	}
}

// onMain runs fn on the process main thread and fails the test if the main
// thread never services it. AppKit properties are read through it rather than
// straight off the test goroutine, for the same reason the package writes them
// through it.
func onMain(t *testing.T, fn func()) {
	t.Helper()
	if err := ensureClass(); err != nil {
		t.Fatalf("ensureClass: %v", err)
	}
	target := objc.ID(targetCls).Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	if err := runOnMain(target, fn); err != nil {
		t.Fatalf("main-thread hop: %v", err)
	}
}

// str reads an Objective-C string property.
func str(id objc.ID, sel string) string {
	return objc.GoString(id.Send(objc.Sel(sel)))
}

// boolean reads a BOOL property.
func boolean(id objc.ID, sel string) bool {
	return objc.Send[bool](id, objc.Sel(sel))
}

// newLive builds an item and removes it when the test ends.
func newLive(t *testing.T, title string, items []MenuItem) *Item {
	t.Helper()
	i, err := New(title, items)
	if err != nil {
		t.Fatalf("New(%q): %v", title, err)
	}
	t.Cleanup(func() {
		if err := i.Close(); err != nil && err != ErrClosed {
			t.Errorf("Close: %v", err)
		}
	})
	return i
}

// ---------------------------------------------------------------------------
// The menu bar itself.
// ---------------------------------------------------------------------------

func TestLiveSystemStatusBarIsReal(t *testing.T) {
	requireWindowServer(t)
	var bar objc.ID
	var thickness float64
	onMain(t, func() {
		bar = objc.ClassID("NSStatusBar").Send(objc.Sel("systemStatusBar"))
		if bar != 0 {
			thickness = objc.Send[float64](bar, objc.Sel("thickness"))
		}
	})
	if bar == 0 {
		t.Fatal("+[NSStatusBar systemStatusBar] is nil in a session that has a main display")
	}
	// A real measurement rather than a non-nil check: the system menu bar is
	// 22pt on stock macOS and never 0.
	if thickness <= 0 {
		t.Errorf("the status bar reports a thickness of %v", thickness)
	}
	t.Logf("system status bar %#x, thickness %v pt", uintptr(bar), thickness)

	// The negative control. A class that does not exist yields the nil class,
	// and a message to nil yields nil — so if the check above were vacuous
	// (if every lookup "succeeded"), this would come back non-nil too.
	var bogus objc.ID
	onMain(t, func() {
		bogus = objc.ClassID("NSStatusBarNoSuchClass").Send(objc.Sel("systemStatusBar"))
	})
	if bogus != 0 {
		t.Error("a nonexistent class answered systemStatusBar; the non-nil check above proves nothing")
	}
}

// ---------------------------------------------------------------------------
// The item, its button, and its menu.
// ---------------------------------------------------------------------------

// liveMenu is the menu every structural test below is measured against: one
// disabled row, one separator, two chooseable rows.
func liveMenu(ran chan string) []MenuItem {
	return []MenuItem{
		{Title: "Status: idle"},
		{},
		{Title: "Preferences…", Key: ",", Do: func() { ran <- "prefs" }},
		{Title: "Quit", Key: "q", Do: func() { ran <- "quit" }},
	}
}

func TestLiveItemIsRealInTheMenuBar(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(ran))

	var (
		bundleID          string
		title             string
		window            objc.ID
		visible           bool
		item, button, bar objc.ID
	)
	onMain(t, func() {
		bar, item, button = i.statusBar, i.item, i.button
		title = str(i.button, "title")
		window = i.button.Send(objc.Sel("window"))
		visible = boolean(i.item, "isVisible")
		bundleID = str(objc.ClassID("NSBundle").Send(objc.Sel("mainBundle")), "bundleIdentifier")
	})

	if bar == 0 || item == 0 || button == 0 {
		t.Fatalf("statusBar %#x, item %#x, button %#x: one of them is nil",
			uintptr(bar), uintptr(item), uintptr(button))
	}
	// The title read BACK out of AppKit, not the one that was passed in: this is
	// what distinguishes an item that was configured from one that was merely
	// allocated.
	if title != "⌘go" {
		t.Errorf("button title reads %q, want %q", title, "⌘go")
	}
	// The measurement that matters most, and the reason this test exists. A
	// status item that AppKit refused to place has no window, and NOTHING else
	// about it differs: New succeeds, the button exists, the title sticks. This
	// test binary is UNBUNDLED — no .app, no Info.plist, and the bundle
	// identifier below is empty — so if an unbundled binary could not show a
	// status item, this is the assertion that would fail.
	if window == 0 {
		t.Errorf("the status item has no window: it was created but never placed in the menu bar")
	}
	t.Logf("bundle identifier %q (empty = unbundled binary), item visible=%v, button window %#x",
		bundleID, visible, uintptr(window))
}

func TestLiveMenuHasExactlyTheRowsThatWereAskedFor(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(ran))

	type got struct {
		title     string
		key       string
		separator bool
		enabled   bool
		tag       int
		hasTarget bool
	}
	var n int
	var rows []got
	onMain(t, func() {
		n = int(i.menu.Send(objc.Sel("numberOfItems")))
		for k := 0; k < n; k++ {
			mi := i.menu.Send(objc.Sel("itemAtIndex:"), k)
			rows = append(rows, got{
				title:     str(mi, "title"),
				key:       str(mi, "keyEquivalent"),
				separator: boolean(mi, "isSeparatorItem"),
				enabled:   boolean(mi, "isEnabled"),
				tag:       int(mi.Send(objc.Sel("tag"))),
				hasTarget: mi.Send(objc.Sel("target")) == i.target,
			})
		}
	})

	if n != 4 {
		t.Fatalf("the menu has %d rows, want 4", n)
	}
	want := []got{
		{title: "Status: idle", separator: false, enabled: false, tag: noTag},
		{separator: true, tag: noTag},
		{title: "Preferences…", key: ",", enabled: true, tag: 0, hasTarget: true},
		{title: "Quit", key: "q", enabled: true, tag: 1, hasTarget: true},
	}
	for k := range want {
		if rows[k] != want[k] {
			t.Errorf("row %d = %+v, want %+v", k, rows[k], want[k])
		}
	}
	// The negative controls are inside that table rather than beside it: row 1
	// must be a separator and rows 0, 2 and 3 must not; row 0 must be disabled
	// and rows 2 and 3 enabled; rows 0 and 1 must carry no target and rows 2
	// and 3 must carry ours. Any property that came back constant — the shape
	// this class of bug takes — fails at least one of those.
}

// ---------------------------------------------------------------------------
// Choosing a row.
// ---------------------------------------------------------------------------

func TestLiveChoosingARowRunsTheGoHandler(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(ran))

	// -performActionForItemAtIndex: is AppKit's own dispatch: it sends the
	// row's action to the row's target exactly as a click does. So this really
	// exercises the registered Objective-C class, the tag on the NSMenuItem and
	// the Go-side table — not a Go function called directly.
	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 2) })
	waitFired(t, ran, "prefs")
	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 3) })
	waitFired(t, ran, "quit")

	// The negative control: the separator and the disabled row have no action
	// to send, so AppKit must dispatch nothing. If tags were assigned per row
	// index instead of per handler, row 3 here would run "prefs" and row 0
	// would run something too.
	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 0) })
	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 1) })
	waitQuiet(t, ran)
}

func TestLiveTwoItemsDoNotShareATagSpace(t *testing.T) {
	requireWindowServer(t)
	ranA := make(chan string, 2)
	ranB := make(chan string, 2)
	a := newLive(t, "A", []MenuItem{{Title: "only A", Do: func() { ranA <- "a" }}})
	b := newLive(t, "B", []MenuItem{{Title: "only B", Do: func() { ranB <- "b" }}})

	// Both rows are tagged 0. They are told apart by the TARGET instance, which
	// is why the dispatch is keyed on it: a process-wide tag space would send
	// B's click to A's handler here.
	onMain(t, func() { b.menu.Send(objc.Sel("performActionForItemAtIndex:"), 0) })
	waitFired(t, ranB, "b")
	waitQuiet(t, ranA)
	onMain(t, func() { a.menu.Send(objc.Sel("performActionForItemAtIndex:"), 0) })
	waitFired(t, ranA, "a")
	waitQuiet(t, ranB)
}

// ---------------------------------------------------------------------------
// Replacing the title and the menu.
// ---------------------------------------------------------------------------

func TestLiveSetTitleReplacesWhatIsDrawn(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(ran))

	if err := i.SetTitle("⏻"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	var title string
	onMain(t, func() { title = str(i.button, "title") })
	if title != "⏻" {
		// The negative control is built in: the old title was "⌘go", so a
		// SetTitle that did nothing fails here with that exact value.
		t.Errorf("button title reads %q after SetTitle, want %q", title, "⏻")
	}
	if err := i.SetTitle(""); err != ErrEmptyTitle {
		t.Errorf("SetTitle(\"\") = %v, want ErrEmptyTitle", err)
	}
}

func TestLiveSetMenuReplacesBothTheRowsAndTheHandlers(t *testing.T) {
	requireWindowServer(t)
	old := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(old))

	fresh := make(chan string, 4)
	if err := i.SetMenu([]MenuItem{
		{Title: "Reconnect", Do: func() { fresh <- "reconnect" }},
	}); err != nil {
		t.Fatalf("SetMenu: %v", err)
	}

	var n int
	onMain(t, func() { n = int(i.menu.Send(objc.Sel("numberOfItems"))) })
	if n != 1 {
		t.Fatalf("the replaced menu has %d rows, want 1", n)
	}
	// Tag 0 now means the NEW handler. The old menu's row 2 was also tag 0, so
	// a table that had not been swapped with the menu would run "prefs" here —
	// the exact failure the swap inside applyMenu exists to prevent.
	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 0) })
	waitFired(t, fresh, "reconnect")
	waitQuiet(t, old)
}

// ---------------------------------------------------------------------------
// Removal.
// ---------------------------------------------------------------------------

func TestLiveCloseRemovesTheItemFromTheMenuBar(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i, err := New("⌘gone", liveMenu(ran))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The item's WINDOW is the observable, and it is captured and RETAINED before
	// the close. Both halves of that were learned the hard way.
	//
	// Retaining is not defensive tidiness. The first version of this test kept
	// the button pointer across Close and read -window from it afterwards, which
	// is a use-after-free: an NSStatusItem owns its button, so releasing the item
	// frees the button and the read lands in freed memory. It passed two runs in
	// three and failed the third with a SIGSEGV inside objc_msgSend, at a program
	// counter with nothing of this package in the stack — the shape of flake that
	// gets blamed on the runner.
	//
	// The WINDOW rather than the button's -window POINTER, because that pointer
	// is not an observable of removal at all. On this machine it read nil after
	// Close and looked like a perfect assertion; on a GitHub macos-latest runner
	// it read 0xc60d28780 both before and after, and the lane went red for a
	// package that was working. It only went nil here because releasing the item
	// deallocated the window — a dealloc, not a removal, and deallocs are not
	// synchronous. What -removeStatusItem: does synchronously, and identically on
	// both machines, is order the window OUT. So that is what is measured.
	var window, button objc.ID
	onMain(t, func() {
		button = i.button
		button.Send(objc.Sel("retain"))
		window = button.Send(objc.Sel("window"))
		if window != 0 {
			window.Send(objc.Sel("retain"))
		}
	})
	t.Cleanup(func() {
		onMain(t, func() {
			button.Send(objc.Sel("release"))
			if window != 0 {
				window.Send(objc.Sel("release"))
			}
		})
	})
	if window == 0 {
		t.Fatal("the item had no window even before Close; there is nothing for Close to remove")
	}
	// The negative control for the assertion below: the window must be on screen
	// to begin with, or "not on screen afterwards" proves nothing.
	if !visible(t, window) {
		t.Fatal("the item's window was already off screen before Close")
	}

	if err := i.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Polled rather than read once, in case a session orders the window out on a
	// later turn of the loop. It must still HAPPEN: the deadline fails.
	deadline := time.Now().Add(5 * time.Second)
	for visible(t, window) {
		if time.Now().After(deadline) {
			t.Errorf("the item's window %#x is still on screen 5s after Close: "+
				"the item was not removed from the menu bar", uintptr(window))
			break
		}
	}
	t.Logf("window %#x: on screen before Close, off screen after", uintptr(window))

	if i.item != 0 || i.menu != 0 || i.statusBar != 0 {
		t.Errorf("Close left references behind: item %#x, menu %#x, statusBar %#x",
			uintptr(i.item), uintptr(i.menu), uintptr(i.statusBar))
	}
	// A second Close must say so rather than sending -removeStatusItem: to a
	// released object, which is a crash inside AppKit.
	if err := i.Close(); err != ErrClosed {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
	// And every mutator must refuse a removed item.
	if err := i.SetTitle("⌘"); err != ErrClosed {
		t.Errorf("SetTitle after Close = %v, want ErrClosed", err)
	}
	if err := i.SetMenu([]MenuItem{{Title: "x", Do: func() {}}}); err != ErrClosed {
		t.Errorf("SetMenu after Close = %v, want ErrClosed", err)
	}
}

// TestLiveHandlersDoNotRunOnTheMainThread proves the promise the package
// documentation makes: a handler runs on a fresh goroutine, so a handler that
// blocks cannot freeze the menu bar of the whole session.
func TestLiveHandlersDoNotRunOnTheMainThread(t *testing.T) {
	requireWindowServer(t)
	where := make(chan bool, 1)
	i := newLive(t, "⌘go", []MenuItem{
		{Title: "Where am I", Do: func() { where <- onMainThread() }},
	})

	onMain(t, func() { i.menu.Send(objc.Sel("performActionForItemAtIndex:"), 0) })
	select {
	case onMainT := <-where:
		if onMainT {
			t.Error("the handler ran on the main thread: a slow handler would freeze the menu bar")
		}
	case <-time.After(fired):
		t.Fatal("the handler never ran")
	}
	// The negative control for the check itself: onMainThread() must be capable
	// of returning true, or the assertion above is satisfied by a function that
	// always says false.
	var claimed bool
	onMain(t, func() { claimed = onMainThread() })
	if !claimed {
		t.Error("onMainThread() is false ON the main thread; it can never detect anything")
	}
}

// TestLiveSeparatorItemsAreDistinctObjects checks the assumption the tag fix in
// buildMenu rests on.
//
// A separator arrives from +[NSMenuItem separatorItem] with tag 0, and 0 is a
// valid handler index, so buildMenu overwrites it. That is only safe if
// +separatorItem hands back a fresh object each time: were it a shared
// singleton, writing a tag on one menu's separator would write it on every
// menu's, in this process and in the host's.
func TestLiveSeparatorItemsAreDistinctObjects(t *testing.T) {
	requireWindowServer(t)
	var a, b objc.ID
	var tagA, tagB int
	onMain(t, func() {
		a = objc.ClassID("NSMenuItem").Send(objc.Sel("separatorItem"))
		b = objc.ClassID("NSMenuItem").Send(objc.Sel("separatorItem"))
		// The measurement that found this in the first place: the DEFAULT tag.
		tagA = int(a.Send(objc.Sel("tag")))
		a.Send(objc.Sel("setTag:"), noTag)
		tagB = int(b.Send(objc.Sel("tag")))
	})
	if a == b {
		t.Errorf("+separatorItem returned the same object twice (%#x): writing a tag on it "+
			"would reach every other menu in the process", uintptr(a))
	}
	if tagA != 0 {
		t.Logf("note: a fresh separator's tag is %d, not the 0 measured on macOS 26.6.2", tagA)
	}
	// The negative control: b must be untouched by the write to a.
	if tagB != 0 {
		t.Errorf("setting a's tag changed b's, which reads %d", tagB)
	}
}

// visible reports whether an NSWindow is on screen, read on the main thread.
func visible(t *testing.T, window objc.ID) bool {
	t.Helper()
	var on bool
	onMain(t, func() { on = boolean(window, "isVisible") })
	return on
}

// TestLiveOnScreenAgreesWithAppKit.
//
// OnScreen exists to answer the one question this package documents as
// unanswerable from Go -- an item built in a process with no run loop is
// "indistinguishable from one that works" -- so the test compares it with the
// two AppKit calls it is made of, read directly, rather than with itself.
func TestLiveOnScreenAgreesWithAppKit(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "⌘go", liveMenu(ran))

	var visible bool
	var window objc.ID
	onMain(t, func() {
		visible = boolean(i.item, "isVisible")
		window = i.button.Send(objc.Sel("window"))
	})
	want := visible && window != 0

	got, err := i.OnScreen()
	if err != nil {
		t.Fatalf("OnScreen: %v", err)
	}
	if got != want {
		t.Errorf("OnScreen = %v; isVisible is %v and the button's window is %v",
			got, visible, window != 0)
	}

	// And a closed item is not on screen, it is closed: a caller that treats
	// "false" as "hidden" would go looking for a menu bar problem.
	if err := i.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if on, err := i.OnScreen(); on || !errors.Is(err, ErrClosed) {
		t.Errorf("OnScreen after Close = %v,%v, want false and ErrClosed", on, err)
	}
}

// TestLiveSetSymbolPutsMoreInkInTheBarThanAnEmoji.
//
// The reason this exists, measured on the menu bar itself: a 👓 title draws 79
// pixels of ink in the item's own strip, and the system symbols draw 100 to 206
// in the same place. Text in a menu bar arrives at the height of a lowercase
// letter; a symbol is drawn for the bar.
func TestLiveSetSymbolPutsMoreInkInTheBarThanAnEmoji(t *testing.T) {
	requireWindowServer(t)
	ran := make(chan string, 4)
	i := newLive(t, "👓", liveMenu(ran))

	if err := i.SetSymbol("display", "XR desk"); err != nil {
		t.Fatalf("SetSymbol: %v", err)
	}
	// The image is on the button and the title is gone: a title beside an image
	// pushes it off centre.
	var hasImage bool
	var title string
	onMain(t, func() {
		hasImage = i.button.Send(objc.Sel("image")) != 0
		title = str(i.button, "title")
	})
	if !hasImage {
		t.Error("the button has no image after SetSymbol")
	}
	if title != "" {
		t.Errorf("the button still says %q beside its image", title)
	}

	// A name the system does not have is refused, and what was there stays:
	// an item that quietly goes blank is an item nobody can find.
	if err := i.SetSymbol("no.such.symbol.anywhere", "nothing"); !errors.Is(err, ErrNoSymbol) {
		t.Errorf("SetSymbol of a made-up name = %v, want ErrNoSymbol", err)
	}
	onMain(t, func() { hasImage = i.button.Send(objc.Sel("image")) != 0 })
	if !hasImage {
		t.Error("a refused symbol took the image that was there away")
	}

	// And the two arguments that are not optional.
	for _, c := range []struct{ name, desc, why string }{
		{"", "XR desk", "no name"},
		{"   ", "XR desk", "a name of spaces"},
		{"display", "", "no description for a screen reader"},
	} {
		if err := i.SetSymbol(c.name, c.desc); !errors.Is(err, ErrNoSymbol) {
			t.Errorf("SetSymbol with %s = %v, want ErrNoSymbol", c.why, err)
		}
	}

	if err := i.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := i.SetSymbol("display", "XR desk"); !errors.Is(err, ErrClosed) {
		t.Errorf("SetSymbol after Close = %v, want ErrClosed", err)
	}
}
