//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// platformListPrinters lists CUPS print destinations via `lpstat -e`.
func platformListPrinters() (bool, []string, error) {
	out, err := exec.Command("lpstat", "-e").Output()
	if err != nil {
		if isNotFound(err) {
			return true, []string{}, errors.New("CUPS not found — install it with: sudo apt install cups cups-client")
		}
		// lpstat present but errored (e.g. cups service down): return empty, no hard error
		return true, []string{}, nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	if names == nil {
		names = []string{}
	}
	return true, names, nil
}

// platformPrintPNGs prints the staged PNGs via CUPS. CUPS rasterises images itself,
// so we hand it the files, the media size, and ask it to fit the page.
func platformPrintPNGs(dir, printer string, wIn, hIn float64, paths []string) error {
	media := "Custom." + inch(wIn) + "x" + inch(hIn) + "in"
	args := []string{"-d", printer, "-o", "media=" + media, "-o", "fit-to-page"}
	args = append(args, paths...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lp", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("printing timed out — check the printer is on, loaded and not jammed")
	}
	if err != nil {
		return errors.New(lpErr(out, err))
	}
	return nil
}

func platformPrintTest(dir, printer string) error {
	f := filepath.Join(dir, "test.txt")
	body := "Warehouse Sync Server\nTest print OK\n" + time.Now().Format(time.RFC1123) + "\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		return errors.New("could not stage the test page")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lp", "-d", printer, f).CombinedOutput()
	if err != nil {
		return errors.New(lpErr(out, err))
	}
	return nil
}

func inch(v float64) string {
	if v <= 0 {
		return "4"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found")
}

func lpErr(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		if isNotFound(err) {
			return "CUPS not found — install it with: sudo apt install cups cups-client"
		}
		msg = err.Error()
	}
	return msg
}
