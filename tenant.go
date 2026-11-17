package main

// ---------------------------------------------------------------------------
// Companies' data stores.
//
// The server's original data (sync-data/sync-db.json) is the default company.
// Every other company gets its own folder, sync-data/companies/<id>/, with its
// own database, change log, backups and attachments. A request is served from
// the store of the company in its SESSION, so one business can never read or
// write another's data, whatever the app sends.
// ---------------------------------------------------------------------------

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var runSaver = true // tests turn the background saver off

var cidRx = regexp.MustCompile(`^c-[0-9a-f]{16}$`)

// storeFor returns the data store of a company (nil if the company does not exist).
func (s *server) storeFor(cid string) *server {
	acc.mu.Lock()
	c := acc.companyByID(cid)
	acc.mu.Unlock()
	if c == nil {
		return nil
	}
	if c.Default {
		return s
	}
	if !cidRx.MatchString(cid) { // ids are made by the server; never let one become a path trick
		return nil
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if st, ok := s.stores[cid]; ok {
		return st
	}
	dir := filepath.Join(s.dir, "companies", cid)
	if err := os.MkdirAll(filepath.Join(dir, "attachments"), 0o755); err != nil {
		return nil
	}
	st := &server{dir: dir, cfg: s.cfg, devices: map[string]time.Time{}, usbPhones: map[string]string{}, stores: map[string]*server{}}
	st.loadDB()
	if runSaver {
		go st.saver()
	}
	s.stores[cid] = st
	return st
}

// tenant requires a signed-in session on data endpoints once accounts are configured
// and runs the handler on that user's company store. Before setup it is transparent,
// so existing (token-less) app versions keep working against the default data.
func (s *server) tenant(pick func(*server) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s
		if acc.configured() {
			me, ok := acc.sessionFor(r)
			if !ok {
				writeJSON(w, 401, map[string]any{"error": "Please sign in", "needLogin": true})
				return
			}
			acc.mu.Lock()
			u := acc.findByID(me.UID)
			must := u != nil && u.MustChange
			acc.mu.Unlock()
			if must { // a temporary password opens nothing but the change-password call
				writeJSON(w, 403, map[string]any{"error": "Set your own password first", "mustChange": true})
				return
			}
			if st = s.storeFor(me.CID); st == nil {
				writeJSON(w, 401, map[string]any{"error": "Please sign in", "needLogin": true})
				return
			}
		}
		pick(st)(w, r)
	}
}

// Paths a hosted server answers without the pairing code (they check their own credentials).
var hostedOpenPaths = map[string]bool{
	"/api/auth/status": true, "/api/auth/login": true, "/api/auth/refresh": true,
	"/api/auth/recover": true, "/api/auth/create-company": true,
}

// Paths any signed-in user of any company may reach without the pairing code on a hosted
// server. Everything else (printers, scanners, ...) belongs to the server's own business
// and still needs the pairing code or a sign-in to the default company.
var tenantPaths = map[string]bool{
	"/api/state": true, "/api/check": true, "/api/changes": true, "/api/push": true, "/api/file": true,
	"/api/company": true, "/api/users": true,
}

// hostedPass lets a request through the pairing-code check on a hosted server.
func (s *server) hostedPass(r *http.Request) bool {
	if !s.cfg.Hosted {
		return false
	}
	p := r.URL.Path
	if hostedOpenPaths[p] {
		return true
	}
	me, ok := acc.sessionFor(r)
	if !ok {
		return false
	}
	if tenantPaths[p] || strings.HasPrefix(p, "/api/auth/") || strings.HasPrefix(p, "/api/users/") {
		return true
	}
	acc.mu.Lock()
	c := acc.companyByID(me.CID)
	acc.mu.Unlock()
	return c != nil && c.Default
}
