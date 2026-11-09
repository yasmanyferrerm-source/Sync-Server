package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Printing is implemented per-platform (print_windows.go, print_linux.go,
// print_other.go) behind three functions:
//   platformListPrinters() (supported bool, names []string, err error)
//   platformPrintPNGs(dir, printer, wIn, hIn, pngPaths) error
//   platformPrintTest(dir, printer) error
// This file holds the HTTP handlers and the shared staging/caching.

// GET /api/printers — list printers on the PC that runs the server, so any
// paired device (phone or PC) can pick one without a USB cable.
func (s *server) printers(w http.ResponseWriter, r *http.Request) {
	supported, names, err := platformListPrinters()
	if names == nil {
		names = []string{}
	}
	resp := map[string]any{"supported": supported, "printers": names}
	if err != nil && supported {
		resp["error"] = cleanPrintErr(err)
	}
	writeJSON(w, 200, resp)
}

// --- small cache so the dashboard (polled every 3s) doesn't shell out constantly
var (
	prnCache   []string
	prnCacheAt time.Time
	prnCacheMu = make(chan struct{}, 1)
)

func cachedPrinters() []string {
	select {
	case prnCacheMu <- struct{}{}:
		defer func() { <-prnCacheMu }()
	default:
		return prnCache // another refresh is in flight; return what we have
	}
	if time.Since(prnCacheAt) < 20*time.Second && prnCache != nil {
		return prnCache
	}
	if _, names, err := platformListPrinters(); err == nil && names != nil {
		prnCache = names
	}
	if prnCache == nil {
		prnCache = []string{}
	}
	prnCacheAt = time.Now()
	return prnCache
}

// POST /api/print-test?printer=NAME — print a small text test page. Loopback only
// (the dashboard runs on the server PC), so it needs no pairing code.
func (s *server) printTest(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Test printing can only be started from the server PC."})
		return
	}
	printer := strings.TrimSpace(r.URL.Query().Get("printer"))
	if printer == "" {
		writeJSON(w, 400, map[string]any{"error": "no printer"})
		return
	}
	dir := filepath.Join(s.dir, "print")
	_ = os.MkdirAll(dir, 0o755)
	if err := platformPrintTest(dir, printer); err != nil {
		writeJSON(w, 200, map[string]any{"error": cleanPrintErr(err)})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func isLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

type printReq struct {
	Printer string   `json:"printer"`
	W       float64  `json:"w"` // page width in inches
	H       float64  `json:"h"` // page height in inches
	Images  []string `json:"images"`
}

var pngPrefix = regexp.MustCompile(`^data:image/png;base64,`)

// POST /api/print — decode the label/doc PNGs, stage them, and hand them to the
// platform printer.
func (s *server) print(w http.ResponseWriter, r *http.Request) {
	var req printReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	req.Printer = strings.TrimSpace(req.Printer)
	if req.Printer == "" {
		writeJSON(w, 400, map[string]any{"error": "no printer chosen"})
		return
	}
	if len(req.Images) == 0 {
		writeJSON(w, 400, map[string]any{"error": "nothing to print"})
		return
	}

	dir := filepath.Join(s.dir, "print")
	_ = os.MkdirAll(dir, 0o755)
	if ents, err := os.ReadDir(dir); err == nil { // clear a previous job's files
		for _, e := range ents {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}

	var paths []string
	for i, img := range req.Images {
		b64 := pngPrefix.ReplaceAllString(img, "")
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(raw) == 0 {
			continue
		}
		p := filepath.Join(dir, "lbl-"+pad3(i)+".png")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		writeJSON(w, 400, map[string]any{"error": "no valid label images"})
		return
	}

	if err := platformPrintPNGs(dir, req.Printer, req.W, req.H, paths); err != nil {
		writeJSON(w, 200, map[string]any{"error": cleanPrintErr(err)})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "printed": len(paths)})
}

// pad3 keeps page order stable when there are 10+ images (lbl-000, lbl-001, …).
func pad3(i int) string {
	s := itoa(i) // itoa is defined in accounts.go
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func cleanPrintErr(err error) string {
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 240 {
		msg = msg[:240]
	}
	return msg
}
