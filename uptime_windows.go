//go:build windows

package main

import (
	"syscall"
	"time"
)

func systemUptime() time.Duration {
	r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetTickCount64").Call()
	return time.Duration(r) * time.Millisecond
}
