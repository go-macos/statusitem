// Command statusitemdemo puts a status item in the macOS menu bar and waits for
// it to be used. It is the smallest thing that proves the package works in a
// real application rather than in a test: a menu bar you can click, in a process
// that owns its own NSApplication.
//
// Run it on a Mac with a GUI session:
//
//	go run github.com/go-macos/statusitem/cmd/statusitemdemo
//
// Look for ⌘go in the menu bar. Choosing a row prints to the terminal; Quit
// stops the application.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "statusitemdemo:", err)
		os.Exit(1)
	}
}
