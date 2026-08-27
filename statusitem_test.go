package statusitem

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The portable suite. It is the whole menu model: what counts as a separator,
// which rows can be chosen, which tag each chooseable row gets, and what
// happens to a tag that does not name a handler. None of it needs a window
// server, so all of it runs on every platform and is gated at 100% in CI.
//
// Every test here carries a negative control where one is possible: an
// assertion that cannot fail is not a test, and the guards below are exactly
// the kind that quietly stop guarding.

// fired is the timeout used when waiting for a handler. [table.fire] runs the
// handler on a fresh goroutine on purpose (a handler must never block the menu
// bar), so a test cannot simply look at a variable afterwards.
const fired = 2 * time.Second

// waitFired waits for a send on ch and fails if none arrives.
func waitFired(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("the wrong handler ran: got %q, want %q", got, want)
		}
	case <-time.After(fired):
		t.Fatalf("handler %q never ran", want)
	}
}

// waitQuiet fails if anything runs at all within a short grace period.
func waitQuiet(t *testing.T, ch <-chan string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("handler %q ran when nothing should have", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestIsSeparatorIsExactlyTheEmptyTitle(t *testing.T) {
	if !(MenuItem{}).IsSeparator() {
		t.Error("the zero MenuItem is not reported as a separator")
	}
	if !(MenuItem{Title: ""}).IsSeparator() {
		t.Error("an explicitly empty title is not reported as a separator")
	}
	// The negative control. Without it the test passes for an IsSeparator that
	// returns true unconditionally — which would turn every menu into a stack
	// of dividers.
	if (MenuItem{Title: "Quit"}).IsSeparator() {
		t.Error("a titled row is reported as a separator")
	}
}

func TestPlanClassifiesEveryKindOfRow(t *testing.T) {
	ran := make(chan string, 4)
	items := []MenuItem{
		{Title: "Status: idle"}, // disabled: no Do
		{},                      // separator
		{Title: "Preferences…", Key: ",", Do: func() { ran <- "prefs" }},
		{Title: "Quit", Key: "q", Do: func() { ran <- "quit" }},
	}
	rows, fns := plan(items)

	want := []row{
		{Title: "Status: idle", Tag: noTag},
		{Separator: true, Tag: noTag},
		{Title: "Preferences…", Key: ",", Tag: 0},
		{Title: "Quit", Key: "q", Tag: 1},
	}
	if len(rows) != len(want) {
		t.Fatalf("plan produced %d rows, want %d", len(rows), len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	// The tags must index the table plan itself produced. A tag that indexes a
	// DIFFERENT table is a row that runs the wrong function, which is worse
	// than one that runs nothing, so the correspondence is checked by calling.
	if len(fns) != 2 {
		t.Fatalf("plan produced %d handlers, want 2 (only chooseable rows have one)", len(fns))
	}
	fns[rows[2].Tag]()
	waitFired(t, ran, "prefs")
	fns[rows[3].Tag]()
	waitFired(t, ran, "quit")
}

func TestPlanGivesNoTagToRowsThatCannotBeChosen(t *testing.T) {
	// Three unchooseable rows before the only chooseable one. If separators or
	// disabled rows consumed a tag, this tag would be 3 and the handler table
	// would be four long with three nil entries in it — a menu whose Quit row
	// calls nothing. That is the negative control, and it is the reason the
	// numbering is counted off len(fns) rather than off the loop index.
	rows, fns := plan([]MenuItem{
		{}, {Title: "heading"}, {},
		{Title: "Quit", Do: func() {}},
	})
	if got := rows[3].Tag; got != 0 {
		t.Errorf("the first chooseable row got tag %d, want 0", got)
	}
	if len(fns) != 1 {
		t.Errorf("handler table is %d long, want 1", len(fns))
	}
	for i, r := range rows[:3] {
		if r.Tag != noTag {
			t.Errorf("unchooseable row %d got tag %d, want noTag", i, r.Tag)
		}
	}
}

func TestPlanOfAnEmptyMenu(t *testing.T) {
	rows, fns := plan(nil)
	if len(rows) != 0 || len(fns) != 0 {
		t.Fatalf("plan(nil) = %d rows, %d handlers; want none of either", len(rows), len(fns))
	}
}

func TestValidateItem(t *testing.T) {
	cases := []struct {
		name string
		item MenuItem
		want error
	}{
		{"a bare separator", MenuItem{}, nil},
		{"a chooseable row", MenuItem{Title: "Quit", Key: "q", Do: func() {}}, nil},
		{"a disabled row", MenuItem{Title: "Status: idle"}, nil},
		{"no key equivalent", MenuItem{Title: "About", Do: func() {}}, nil},
		// A one-RUNE key that is several BYTES: the check must count runes, or
		// every non-ASCII shortcut (⌫, é) is refused for being "too long".
		{"a multibyte one-rune key", MenuItem{Title: "Delete", Key: "⌫", Do: func() {}}, nil},

		{"a separator carrying a handler", MenuItem{Do: func() {}}, ErrSeparatorNotEmpty},
		{"a separator carrying a key", MenuItem{Key: "q"}, ErrSeparatorNotEmpty},
		{"a NUL in the title", MenuItem{Title: "Qu\x00it"}, ErrHasNUL},
		{"a NUL in the key", MenuItem{Title: "Quit", Key: "\x00"}, ErrHasNUL},
		{"a two-character key", MenuItem{Title: "Quit", Key: "qq"}, ErrKeyNotOneRune},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateItem(c.item)
			if !errors.Is(err, c.want) {
				t.Fatalf("validateItem = %v, want %v", err, c.want)
			}
		})
	}
}

func TestValidateNamesTheOffendingRow(t *testing.T) {
	// A fifteen-row menu and an error that says only "a separator cannot carry
	// a Do" sends the caller through all fifteen. The index is the whole value
	// of the message.
	err := validate([]MenuItem{{Title: "a", Do: func() {}}, {}, {Key: "x"}})
	if !errors.Is(err, ErrSeparatorNotEmpty) {
		t.Fatalf("validate = %v, want ErrSeparatorNotEmpty", err)
	}
	if !strings.Contains(err.Error(), "row 2") {
		t.Errorf("the error does not name the row: %q", err)
	}
	// The negative control: a valid menu must produce no error at all,
	// otherwise the check above would pass for a validate that always fails.
	if err := validate([]MenuItem{{Title: "a", Do: func() {}}, {}, {Title: "b"}}); err != nil {
		t.Errorf("a valid menu was rejected: %v", err)
	}
	if err := validate(nil); err != nil {
		t.Errorf("an empty menu was rejected: %v", err)
	}
}

func TestValidateTitle(t *testing.T) {
	if err := validateTitle(""); !errors.Is(err, ErrEmptyTitle) {
		t.Errorf("validateTitle(\"\") = %v, want ErrEmptyTitle", err)
	}
	if err := validateTitle("a\x00b"); !errors.Is(err, ErrHasNUL) {
		t.Errorf("validateTitle with a NUL = %v, want ErrHasNUL", err)
	}
	// An emoji title is the ordinary case for a menu-bar item, so it had
	// better pass; it is also the negative control for the two checks above.
	if err := validateTitle("⌘"); err != nil {
		t.Errorf("validateTitle(\"⌘\") = %v, want nil", err)
	}
}

func TestTableFireRunsTheHandlerForTheTag(t *testing.T) {
	ran := make(chan string, 4)
	var tbl table
	tbl.set([]func(){
		func() { ran <- "zero" },
		func() { ran <- "one" },
	})

	// Tag 0 first, deliberately: 0 is AppKit's DEFAULT tag, so it must be a
	// valid index rather than a sentinel. If it were treated as "no handler",
	// the first row of every menu would be dead.
	tbl.fire(0)
	waitFired(t, ran, "zero")
	tbl.fire(1)
	waitFired(t, ran, "one")
}

func TestTableFireIgnoresATagWithNoHandler(t *testing.T) {
	ran := make(chan string, 4)
	var tbl table
	tbl.set([]func(){func() { ran <- "zero" }})

	// noTag is what a separator and a disabled row carry, and AppKit does
	// deliver an action for a row whose menu was replaced under it. Every one
	// of these must be a no-op: a panic here happens inside an Objective-C
	// frame on the main thread, where Go's recovery story is "the application
	// is gone".
	tbl.fire(noTag)
	tbl.fire(-99)
	tbl.fire(1)
	tbl.fire(1 << 20)
	waitQuiet(t, ran)

	// The negative control. Without it this test passes for a fire that does
	// nothing at all, which is precisely the bug it is supposed to exclude.
	tbl.fire(0)
	waitFired(t, ran, "zero")
}

func TestTableFireOnAnEmptyTable(t *testing.T) {
	// The zero table: an item whose menu has no chooseable row at all.
	var tbl table
	tbl.fire(0)
}

func TestTableSetReplacesTheWholeTable(t *testing.T) {
	ran := make(chan string, 4)
	var tbl table
	tbl.set([]func(){func() { ran <- "old" }})
	tbl.set([]func(){func() { ran <- "new" }})

	tbl.fire(0)
	waitFired(t, ran, "new")
	// The old table must be gone, not shadowed: a tag still valid in it would
	// keep the previous menu's handler alive after the menu was replaced.
	tbl.fire(1)
	waitQuiet(t, ran)
}

func TestRegistryDispatchesByTarget(t *testing.T) {
	ranA := make(chan string, 2)
	ranB := make(chan string, 2)
	var a, b table
	a.set([]func(){func() { ranA <- "a" }})
	b.set([]func(){func() { ranB <- "b" }})

	var r registry
	r.put(0x1000, &a)
	r.put(0x2000, &b)

	if r.get(0x1000) != &a || r.get(0x2000) != &b {
		t.Fatal("the registry handed back the wrong table")
	}
	// Two items in one process must not share a tag space; this is what makes
	// the tags per-item and small.
	r.fire(0x1000, 0)
	waitFired(t, ranA, "a")
	waitQuiet(t, ranB)
	r.fire(0x2000, 0)
	waitFired(t, ranB, "b")
	waitQuiet(t, ranA)
}

func TestRegistryForgetsAClosedTarget(t *testing.T) {
	ran := make(chan string, 2)
	var tbl table
	tbl.set([]func(){func() { ran <- "gone" }})

	var r registry
	r.put(0x3000, &tbl)
	r.drop(0x3000)

	if got := r.get(0x3000); got != nil {
		t.Errorf("get after drop = %p, want nil", got)
	}
	// An action really can arrive after Close: the menu can be OPEN at the
	// moment the item is removed. It must be a no-op.
	r.fire(0x3000, 0)
	// An unknown target — one that was never registered — likewise.
	r.fire(0x4000, 0)
	waitQuiet(t, ran)

	// The negative control: the same registry still dispatches for a target it
	// does know, so the silence above is the drop working rather than the
	// registry being broken.
	r.put(0x5000, &tbl)
	r.fire(0x5000, 0)
	waitFired(t, ran, "gone")
}

func TestNewValidatesBeforeItTouchesThePlatform(t *testing.T) {
	// This runs on macOS too, where New really would build a status item. It
	// must not get that far: validation comes first, so a malformed call is a
	// clear error on every platform rather than a menu-bar item on one and an
	// ErrUnsupported on the others.
	if _, err := New("", nil); !errors.Is(err, ErrEmptyTitle) {
		t.Errorf("New with an empty title = %v, want ErrEmptyTitle", err)
	}
	if _, err := New("a\x00b", nil); !errors.Is(err, ErrHasNUL) {
		t.Errorf("New with a NUL in the title = %v, want ErrHasNUL", err)
	}
	if _, err := New("⌘", []MenuItem{{Do: func() {}}}); !errors.Is(err, ErrSeparatorNotEmpty) {
		t.Errorf("New with a separator carrying a handler = %v, want ErrSeparatorNotEmpty", err)
	}
	if _, err := New("⌘", []MenuItem{{Title: "Quit", Key: "qq", Do: func() {}}}); !errors.Is(err, ErrKeyNotOneRune) {
		t.Errorf("New with a two-character key = %v, want ErrKeyNotOneRune", err)
	}
}
