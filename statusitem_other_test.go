//go:build !darwin

package statusitem

import (
	"errors"
	"testing"
)

// The non-darwin half. What matters here is that a consumer cross-compiles and
// gets a CLEAR error rather than a link failure or a silent no-op — and that a
// malformed menu is still reported as malformed, so a developer working on Linux
// is told about it here instead of on the Mac where it would finally be built.

func TestNewIsUnsupported(t *testing.T) {
	i, err := New("⌘", []MenuItem{
		{Title: "Preferences…", Key: ",", Do: func() {}},
		{},
		{Title: "Quit", Key: "q", Do: func() {}},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("New = %v, want ErrUnsupported", err)
	}
	if i != nil {
		t.Fatal("an Item was handed out on a platform that has no menu bar")
	}
}

func TestNewStillValidates(t *testing.T) {
	// The negative control for TestNewIsUnsupported: New does not answer
	// ErrUnsupported unconditionally, so the menu really is being read here.
	// Without this, a validation regression would be invisible on every lane
	// except the macOS one.
	if _, err := New("", nil); !errors.Is(err, ErrEmptyTitle) {
		t.Errorf("New with an empty title = %v, want ErrEmptyTitle", err)
	}
	if _, err := New("⌘", []MenuItem{{Key: "q"}}); !errors.Is(err, ErrSeparatorNotEmpty) {
		t.Errorf("New with a separator carrying a key = %v, want ErrSeparatorNotEmpty", err)
	}
}

func TestItemMethodsAreUnsupportedButStillValidate(t *testing.T) {
	// Consumer code names these on a value of the type, so they must exist and
	// must not panic on the zero Item.
	var i Item

	if err := i.SetTitle("⏻"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetTitle = %v, want ErrUnsupported", err)
	}
	if err := i.SetTitle(""); !errors.Is(err, ErrEmptyTitle) {
		t.Errorf("SetTitle(\"\") = %v, want ErrEmptyTitle", err)
	}
	if err := i.SetMenu([]MenuItem{{Title: "Quit", Do: func() {}}}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetMenu = %v, want ErrUnsupported", err)
	}
	if err := i.SetMenu([]MenuItem{{Title: "Quit", Key: "qq", Do: func() {}}}); !errors.Is(err, ErrKeyNotOneRune) {
		t.Errorf("SetMenu with a two-character key = %v, want ErrKeyNotOneRune", err)
	}
	if err := i.Close(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Close = %v, want ErrUnsupported", err)
	}
}

func TestOnScreenIsUnsupported(t *testing.T) {
	// The whole point of OnScreen is telling a caller the truth about a menu
	// bar. Here there is no menu bar, and saying "not on screen" would read as
	// "your item is hidden" rather than "there is nowhere to put one".
	var i Item
	on, err := i.OnScreen()
	if on {
		t.Error("an item is on screen on a platform with no menu bar")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("OnScreen = %v, want ErrUnsupported", err)
	}
}

func TestSetSymbolIsUnsupported(t *testing.T) {
	var i Item
	if err := i.SetSymbol("display", "XR desk"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetSymbol = %v, want ErrUnsupported", err)
	}
}
