//go:build !windows

package main

import "time"

func systemUptime() time.Duration { return 0 }
