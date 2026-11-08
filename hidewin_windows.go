//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow makes a child process run without popping a console window. The
// server is built with -H=windowsgui (no console of its own), so without this
// every PowerShell/adb child would flash its own console window.
func hideWindow(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.HideWindow = true
	c.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
