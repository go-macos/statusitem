# go-macos/statusitem

[![ci](https://github.com/go-macos/statusitem/actions/workflows/ci.yml/badge.svg)](https://github.com/go-macos/statusitem/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-macos/statusitem.svg)](https://pkg.go.dev/github.com/go-macos/statusitem)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

**A macOS menu-bar status item — the thing everyone calls a "tray icon" — with a
menu, from pure Go, `CGO_ENABLED=0`.** No cgo, no `osascript`, no Objective-C
source file anywhere in the build: it reaches AppKit through
[`go-macos/objc`](https://github.com/go-macos/objc), which reaches it through
[purego](https://github.com/ebitengine/purego).

```go
item, err := statusitem.New("⌘", []statusitem.MenuItem{
        {Title: "Status: idle"},                                  // no Do → a disabled row
        {},                                                       // no Title → a separator
        {Title: "Preferences…", Key: ",", Do: openPreferences},   // ⌘,
        {Title: "Quit", Key: "q", Do: quit},                      // ⌘Q
})
if err != nil {
        return err
}
defer item.Close()
```

That is the whole API:

| | |
|---|---|
| `New(title string, items []MenuItem) (*Item, error)` | put an item in the menu bar. `title` is text or an emoji. |
| `(*Item) SetTitle(string) error` | replace what is drawn. |
| `(*Item) SetMenu([]MenuItem) error` | replace the whole menu. |
| `(*Item) Close() error` | remove it. A second `Close` reports `ErrClosed`. |
| `MenuItem{Title, Key string; Do func()}` | one row. `IsSeparator()` is the rule the package applies. |

## The setters return errors, and that is on purpose

A `SetTitle`/`SetMenu` that returned nothing would have to be **lenient**, and
being lenient here means silently discarding something the caller wrote:

- **An empty title.** AppKit accepts one and draws a zero-width item — present
  in the menu bar, impossible to see and impossible to click. There is no image
  parameter here to make an empty title mean something, so it is `ErrEmptyTitle`.
- **A row with an empty `Title` that carries a `Do`.** An empty title is how
  this package spells *separator*, and a separator cannot be chosen, so the `Do`
  could never run: `ErrSeparatorNotEmpty`.
- **A key equivalent that is not exactly one character.**
  `-[NSMenuItem setKeyEquivalent:]` takes any string, **draws it in the row**,
  and then never matches it against a keystroke. A shortcut that is drawn and
  dead is worse than one that is refused: `ErrKeyNotOneRune`.
- **A NUL byte.** The Objective-C bridge is `+stringWithUTF8String:`, which
  terminates at the first NUL, so the string would be *truncated* with no error
  at all: `ErrHasNUL`.

The row number is in the message, because "a separator cannot carry a Do" for a
fifteen-row menu sends you through all fifteen.

## It needs a host application, and it says which parts need what

This package does **not** create an application. It expects a process that
already has an `NSApplication` whose run loop is on the process main OS thread —
in this fleet that is `go-widgets/window`; in a bare binary it is
`objc.RunApp`. The distinction that matters:

| | without a running run loop |
|---|---|
| `+[NSStatusBar systemStatusBar]`, `-statusItemWithLength:`, `-setTitle:`, `-setMenu:` | **work.** An item can be built during start-up, and `New` is safe to call there. |
| drawing, and choosing a row | **never happen.** |

So a status item in a process whose main thread never runs a loop is an object
with no window — and from Go it is *indistinguishable* from one that works.
That is the failure this package's live test exists to rule out, and it does so
by reading `-[NSStatusBarButton window]` back out of AppKit.

If there is no `NSApplication` at all, `New` creates the shared one as a side
effect, because `-systemStatusBar` is meaningless before AppKit has an
application object. It does **not** call `-finishLaunching` and does **not**
touch the activation policy: those belong to whoever owns the process, and
changing them under a host would move its Dock tile or replace its main menu.

## The main thread, and the hang that is not a hang

AppKit is main-thread-only, and violating that does not fail politely: in this
fleet's own tray code it was an Objective-C exception and a `SIGABRT`,
*intermittently*, depending on which OS thread the Go scheduler happened to have
the goroutine on.

So every AppKit call here is marshalled onto the process main thread, and every
exported function is safe to call from any goroutine. `-[NSThread isMainThread]`
decides how:

- **On the main thread**, the work runs **inline**. It has to:
  `performSelectorOnMainThread:…waitUntilDone:NO` would queue it for the next
  turn of a loop, so a call made during start-up would never happen at all.
- **Off the main thread**, the work is queued and waited for **with a timeout**.
  `waitUntilDone:YES` from a goroutine in a process whose main thread is not
  running a loop blocks that goroutine **forever** — no error, no deadlock
  detection (the runtime sees a live thread), and no frame of this package
  anywhere in the stack. After `MainHopTimeout` (5s) the call reports
  `ErrNoMainLoop` instead.

**Handlers do not run on the main thread.** `MenuItem.Do` is called on a fresh
goroutine. A handler that blocks — a network fetch, a lock, a channel nobody is
reading — would otherwise freeze the menu bar of the whole **session**, not
merely this application. The cost is that two rapid choices can run concurrently
and that a handler touching AppKit must marshal itself back; that is the cheaper
of the two mistakes, and the live suite asserts it (`onMainThread()` is false
inside a handler, and true on the main thread, so the assertion is not vacuous).

## What was measured, not assumed

On **macOS 26.6.2 (25G83), arm64**, by the live suite in this repository:

**An unbundled binary DOES get a real menu-bar item.** This is the question
worth settling, because the failure would be silent. The test binary has no
`.app` and no `Info.plist` — `[[NSBundle mainBundle] bundleIdentifier]` reads
`""` — and its status item still comes back with a live
`-[NSStatusBarButton window]` (`0x1517096f0` on one run), `-[NSStatusItem
isVisible]` true, in a status bar reporting a thickness of **22 pt**. No bundle,
no `Info.plist`, no `LSUIElement` key was needed for any of it. `objc.RunApp(1)`
in `cmd/statusitemdemo` shows the same item in the menu bar of a real session.

**A separator arrives with tag `0`, and `0` is a valid handler index.** Found by
reading the tag back out of a built menu rather than by trusting
`+[NSMenuItem separatorItem]`. Left alone it would put the first chooseable
row's function one stray action-dispatch away from a divider, so `buildMenu`
overwrites it — after checking, in a test, that `+separatorItem` really does hand
back a **distinct object** each time, which is what makes writing to it safe.

**An `NSStatusItem` owns its button, so the button dies with it.** The first
version of the removal test kept the button pointer across `Close` and sent it
`-window` afterwards. That is a use-after-free: it passed two runs in three and
failed the third with a `SIGSEGV` inside `objc_msgSend`, at a program counter
with nothing of this package in the stack — exactly the flake that gets blamed
on the runner. The test retains what it reads now.

**The button's `-window` POINTER is not an observable of removal, and believing
it was cost a red CI lane.** After `Close` it read `0x0` on this machine and
looked like a perfect assertion. On a GitHub `macos-latest` runner it read
`0xc60d28780` both before and after, and the darwin lane went red for a package
that was working correctly. It only went nil here because releasing the item
*deallocated* the window — a dealloc, not a removal, and deallocs are not
synchronous. What `-removeStatusItem:` does synchronously, and identically on
both machines, is order the window **out**: `-[NSWindow isVisible]` goes from
true to false. That is what the test measures now.

## Running the tests

```bash
# Portable + live. The live tests really place items in your menu bar and remove
# them again; with no window server they skip.
CGO_ENABLED=0 go test -v ./...

# The stubs and the whole menu model, off macOS.
CGO_ENABLED=0 GOOS=linux go test ./...

# A real menu-bar application, in the menu bar.
go run ./cmd/statusitemdemo
```

The **portable menu model** — validation, separator and disabled-row
classification, tag assignment, tag dispatch, the per-item registry — is at
**100% statement coverage**, gated in CI by *shape* (anything that is not
`_<goos>.go` and not under `cmd/`) on both the darwin and the linux lane, and
*run*, not merely compiled, on all six of Go's 64-bit architectures.

The live suite asserts **properties**, never "it did not crash": the button's
title read back out of AppKit, `numberOfItems`, each row's
`isSeparatorItem`/`isEnabled`/`tag`/`target`, the item window being ordered out by
`Close`, and the Go handler firing through AppKit's own
`-performActionForItemAtIndex:`. Every test carries a negative control — a
nonexistent class must yield nil, the disabled row and the separator must
dispatch *nothing*, two items must not share a tag space — because an assertion
that cannot fail is not a test.

## Platforms

macOS only. Every other platform **compiles** and reports `ErrUnsupported`, so
consumers cross-compile without a build tag of their own — verified on
linux/{amd64,arm64,riscv64,loong64,ppc64le,s390x}, windows/{amd64,arm64},
darwin/amd64, android/arm64 and js/wasm. What is **not** stubbed out is the menu
model: it behaves identically everywhere, which is what makes it testable off a
Mac.

Off darwin `New` still **validates before** it reports `ErrUnsupported`, so a
developer working on Linux is told about a malformed menu there instead of
discovering it on the Mac where it is finally built.

## Licence

BSD-3-Clause.
