//go:build !windows

package main

import "fmt"

// showError prints on non-Windows platforms (where there is a console).
func showError(title, msg string) {
	fmt.Println("\n  " + title + ": " + msg)
}

// showInfo is a no-op on non-Windows platforms: the message was already printed.
func showInfo(title, msg string) {}
