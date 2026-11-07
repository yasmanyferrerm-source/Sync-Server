//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// showError pops a native Windows message box. Needed because the server is built
// with -H=windowsgui (no console window), so a startup failure would otherwise be
// invisible. 0x10 = MB_ICONERROR, 0x40000 = MB_TOPMOST.
func showError(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	mbox := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(msg)
	mbox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10|0x40000)
}
