//go:build darwin

package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/go-macos/objc"
	"github.com/go-macos/statusitem"
)

// nsApplicationActivationPolicyAccessory: a menu-bar-only application, with no
// Dock tile and no application menu.
const accessory = 1

func run() error {
	// The AppKit run loop must own the process main OS thread, and Go will move
	// an unlocked goroutine off it at any preemption point.
	runtime.LockOSThread()

	started := time.Now()
	var item *statusitem.Item

	menu := func() []statusitem.MenuItem {
		return []statusitem.MenuItem{
			// A disabled row: a heading, or a value shown for information.
			{Title: "statusitemdemo"},
			{},
			{Title: "Print the uptime", Key: "u", Do: func() {
				fmt.Printf("up %s\n", time.Since(started).Round(time.Second))
			}},
			{Title: "Rename me", Key: "r", Do: func() {
				// A handler runs on its own goroutine, so it may call back into
				// the item without deadlocking on the main thread.
				if err := item.SetTitle("⌘" + time.Now().Format("05")); err != nil {
					fmt.Println("SetTitle:", err)
				}
			}},
			{},
			{Title: "Quit", Key: "q", Do: func() {
				if err := item.Close(); err != nil {
					fmt.Println("Close:", err)
				}
				// -terminate: is AppKit's, so it goes on the main thread.
				objc.DispatchMain(func() { objc.App().Send(objc.Sel("terminate:"), objc.ID(0)) })
			}},
		}
	}

	var err error
	if item, err = statusitem.New("⌘go", menu()); err != nil {
		return err
	}
	fmt.Println("⌘go is in the menu bar; choose Quit to stop.")
	objc.RunApp(accessory)
	return nil
}
