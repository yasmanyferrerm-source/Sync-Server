package main

// ---------------------------------------------------------------------------
// Server-owner tools: see every company on this server and give any user
// (usually a locked-out admin who lost their recovery code) a temporary password.
//
// Reachable only on the server PC itself: the request must come from loopback
// AND carry none of the headers a proxy adds, so nothing that arrives through
// the Cloudflare Tunnel (which also connects from loopback) can use them. The
// dashboard also sends the pairing code.
//
// From a terminal (Linux, or with the server stopped):
//   warehouse-sync-server --reset-password someone@example.com
// ---------------------------------------------------------------------------

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// proxied: the request was forwarded (Cloudflare Tunnel, a reverse proxy).
func proxied(r *http.Request) bool {
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Ipcountry", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// onServerPC: someone at the server PC itself, not a device on the network and not the tunnel.
func onServerPC(r *http.Request) bool { return isLoopback(r) && !proxied(r) }

func ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onServerPC(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Owner tools only work on the PC that runs the Warehouse Sync Server."})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

// GET /api/owner/companies — every company, its admins and how many users it has.
func (s *server) ownerCompanies(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	out := []map[string]any{}
	for _, c := range acc.Companies {
		users := []map[string]any{}
		for _, u := range acc.Users {
			if u.CompanyID == c.ID {
				users = append(users, map[string]any{"id": u.ID, "name": u.Name, "username": u.Username, "email": u.Email, "role": u.Role,
					"disabled": u.Disabled, "hasRecovery": u.Recovery != "", "lastLogin": u.LastLogin})
			}
		}
		out = append(out, map[string]any{"id": c.ID, "name": c.Name, "default": c.Default, "created": c.Created, "users": users})
	}
	writeJSON(w, 200, map[string]any{"companies": out, "hosted": s.cfg.Hosted})
}

// caller holds acc.mu. Gives a user a temporary password they must change at next sign-in.
func ownerResetLocked(r *http.Request, who string) (*user, string, error) {
	u := acc.findByID(who)
	if u == nil {
		u = acc.findByName(who)
	}
	if u == nil {
		return nil, "", fmt.Errorf("no user %q on this server", who)
	}
	temp := readableCode(3, 4)
	u.Pass, u.MustChange, u.Disabled, u.Updated = hashPassword(temp), true, false, time.Now().UnixMilli()
	acc.save()
	acc.revokeUser(u.ID, "")
	acc.audit(r, u.CompanyID, "owner", "owner.reset", u.ID, nil)
	return u, temp, nil
}

// POST /api/owner/reset — {user: id, username or email} -> a temporary password, shown once.
func (s *server) ownerReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u, temp, err := ownerResetLocked(r, strings.TrimSpace(req.User))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"user": userPublic(u), "tempPassword": temp})
}

// ownerResetCLI: `--reset-password <email>`. Asks the running server if there is one
// (so its in-memory copy stays right); otherwise edits the account file directly.
func ownerResetCLI(s *server, who string) {
	body, _ := json.Marshal(map[string]string{"user": who})
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/owner/reset", s.cfg.Port), bytes.NewReader(body))
	req.Header.Set("X-Sync-Code", s.cfg.Code)
	req.Header.Set("Content-Type", "application/json")
	var msg string
	if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
		var j map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&j)
		resp.Body.Close()
		if t, ok := j["tempPassword"].(string); ok {
			msg = fmt.Sprintf("Temporary password for %s:\n\n    %s\n\nThey must choose their own password when they sign in. All their devices were signed out.", who, t)
		} else {
			msg = fmt.Sprintf("Could not reset %s: %v", who, j["error"])
		}
	} else {
		acc.init(s.dir)
		acc.mu.Lock()
		u, temp, e := ownerResetLocked(nil, who)
		acc.mu.Unlock()
		if e != nil {
			msg = "Could not reset: " + e.Error()
		} else {
			msg = fmt.Sprintf("Temporary password for %s:\n\n    %s\n\nThey must choose their own password when they sign in.", u.Username, temp)
		}
	}
	fmt.Println(msg)
	_ = os.WriteFile(filepath.Join(s.dir, "reset-password.txt"), []byte(msg+"\n\nDelete this file after you pass the password on.\n"), 0o600)
	showInfo("Warehouse Sync Server", msg)
}
