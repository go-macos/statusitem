// Package statusitem puts an item in the macOS menu bar — the thing everyone
// calls a "tray icon" — from pure Go, with CGO_ENABLED=0. It reaches AppKit
// through github.com/go-macos/objc, which reaches it through purego: no cgo, no
// osascript, no Objective-C source file anywhere in the build.
//
// A [MenuItem] carries a title, an optional key equivalent and a Go func. That
// func is called when the row is chosen.
//
//	item, err := statusitem.New("⌘", []statusitem.MenuItem{
//		{Title: "Preferences…", Key: ",", Do: openPreferences},
//		{}, // a separator
//		{Title: "Quit", Key: "q", Do: quit},
//	})
//	if err != nil {
//		return err
//	}
//	defer item.Close()
//
// # It needs a host application, and it says so
//
// This package does NOT create an application. It expects to be called from a
// process that already has an NSApplication whose run loop is running on the
// process main OS thread — in this fleet that is go-widgets/window, in a plain
// binary it is objc.RunApp. Everything about a status item depends on that loop:
//
//   - +[NSStatusBar systemStatusBar] and -statusItemWithLength: succeed without
//     it, so an item can be BUILT before the loop starts. [New] is therefore
//     safe to call during start-up.
//   - Nothing is ever DRAWN and no row is ever chosen without it. A status item
//     in a process whose main thread never runs a loop is an object with no
//     window, and it is indistinguishable — from Go — from one that works.
//
// If there is no NSApplication at all, [New] creates the shared one
// (+sharedApplication) as a side effect, because -systemStatusBar is meaningless
// before AppKit has an application object. It does not call -finishLaunching and
// it does not touch the activation policy: a host owns those, and changing them
// under a host would move its Dock tile or its main menu.
//
// # The main thread
//
// AppKit is main-thread-only, and violating that does not fail politely: the
// observed failure mode in this fleet's own tray code was an Objective-C
// exception and a SIGABRT, intermittently, depending on which OS thread the Go
// scheduler happened to be running the goroutine on.
//
// So every AppKit call this package makes is marshalled onto the process main
// thread, and every exported function is safe to call from any goroutine.
// -[NSThread isMainThread] decides how: on the main thread the work runs inline
// (so [New] works before the loop is started), and off it the work is queued
// with -performSelectorOnMainThread:withObject:waitUntilDone:NO and waited for
// with a timeout. The timeout is not decoration. waitUntilDone:YES from a
// goroutine in a process whose main thread is not running a loop blocks that
// goroutine FOREVER, with no error and no stack that mentions this package;
// after [MainHopTimeout] the call reports [ErrNoMainLoop] instead.
//
// # Handlers do not run on the main thread
//
// [MenuItem.Do] is called on a fresh goroutine, not on the main thread that
// delivered the click. A handler that blocks — a network fetch, a lock, a
// channel nobody is reading — would otherwise freeze the menu bar of the whole
// session, not merely this application. The cost is that two rapid choices can
// run concurrently and that a handler touching AppKit must marshal itself back;
// that is the cheaper of the two mistakes.
//
// # Portability
//
// Every exported symbol is defined on all platforms, so a consumer
// cross-compiles without a build tag of its own; off darwin the entry points
// report [ErrUnsupported]. What is NOT stubbed out is the menu model — item
// validation, separator and disabled-row classification, tag assignment and tag
// dispatch all live in the portable file and behave identically everywhere,
// which is what lets them be tested to the last branch on a Linux runner with no
// window server in sight.
package statusitem
