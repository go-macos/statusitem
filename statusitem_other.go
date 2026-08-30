//go:build !darwin

package statusitem

// This file is the non-darwin half of the package. Every exported symbol the
// darwin build provides exists here too, so a consumer cross-compiles without a
// build tag of its own and finds out at run time — with a clear error — that
// this platform has no AppKit menu bar to put an item in.
//
// Note what is NOT stubbed out: the menu model. Validation, separator and
// disabled-row classification, tag assignment and tag dispatch are in the
// portable file and behave here exactly as they do on macOS. That is deliberate,
// and it is what lets every branch of them be tested on a Linux runner with no
// window server anywhere.

// Item is a status item in the menu bar. On non-darwin platforms one can never
// be created, so no value of this type is ever handed out by [New]; the type
// exists so that consumer code naming it still compiles.
type Item struct {
	// tbl is the tag -> handler table. It is never consulted here, because
	// nothing on this platform can deliver a click; it is present so the shape
	// of the type — and of the portable code that reaches for it — is the same
	// on every platform.
	tbl table
}

// New reports [ErrUnsupported]: the menu bar this package fills is AppKit's.
//
// It validates its arguments FIRST, so that a developer working on Linux gets
// exactly the complaint macOS would make about a malformed menu — an empty
// title, a separator carrying a handler, a two-character shortcut — instead of a
// blanket ErrUnsupported that hides the defect until the code is built on a Mac.
func New(title string, items []MenuItem) (*Item, error) {
	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if err := validate(items); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// SetTitle reports [ErrUnsupported]. The title is still validated first, for
// the same reason [New] validates.
func (i *Item) SetTitle(s string) error {
	if err := validateTitle(s); err != nil {
		return err
	}
	return ErrUnsupported
}

// SetMenu reports [ErrUnsupported]. The rows are still validated first, for the
// same reason [New] validates.
func (i *Item) SetMenu(items []MenuItem) error {
	if err := validate(items); err != nil {
		return err
	}
	return ErrUnsupported
}

// Close reports [ErrUnsupported]. There is nothing in a menu bar to remove.
func (i *Item) Close() error { return ErrUnsupported }

// OnScreen reports [ErrUnsupported]: there is no menu bar here.
func (i *Item) OnScreen() (bool, error) { return false, ErrUnsupported }
