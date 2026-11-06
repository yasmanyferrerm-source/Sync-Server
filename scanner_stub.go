package main

// ---------------------------------------------------------------------------
// PLACEHOLDER — scanner & USB-phone integration.
//
// The uploaded main.go references localOnly / scanners / scan / usbWatch, but
// their definitions live in the original project's other source file(s), which
// were not included in the single uploaded file. These stubs exist only so the
// server compiles while the accounts feature is developed. They must be replaced
// by the real document-scanner (WIA/TWAIN) + USB-phone (ADB) code before this
// build is deployed, so those features are not lost.
// ---------------------------------------------------------------------------

import (
	"net"
	"net/http"
)

// localOnly restricts a handler to requests coming from this PC / its LAN —
// the scanner endpoints must never be reachable through the public tunnel.
func localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Scanning only works on the PC that runs the Warehouse Sync Server"})
			return
		}
		h(w, r)
	}
}

// scanners — real build enumerates attached document scanners. Placeholder: none.
func (s *server) scanners(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"scanners": []any{}, "note": "scanner support not included in this build"})
}

// scan — real build acquires a page from the scanner. Placeholder: unavailable.
func (s *server) scan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Scanner support is being restored in this build"})
}

// usbWatch — real build watches for USB-cable phones via ADB. Placeholder: no-op.
func (s *server) usbWatch() {}
