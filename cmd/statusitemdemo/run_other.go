//go:build !darwin

package main

import "github.com/go-macos/statusitem"

// run reports the package's own error, so the command builds and runs on every
// platform and says the same thing the library says rather than failing to link.
func run() error {
	_, err := statusitem.New("⌘go", []statusitem.MenuItem{{Title: "Quit", Do: func() {}}})
	return err
}
