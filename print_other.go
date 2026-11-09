//go:build !windows && !linux

package main

import "errors"

func platformListPrinters() (bool, []string, error) { return false, []string{}, nil }

func platformPrintPNGs(dir, printer string, wIn, hIn float64, paths []string) error {
	return errors.New("printing is supported on Windows and Linux only")
}

func platformPrintTest(dir, printer string) error {
	return errors.New("printing is supported on Windows and Linux only")
}
