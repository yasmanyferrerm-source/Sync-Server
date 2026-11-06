package main

// ---------------------------------------------------------------------------
// Accounts — per-server user sign-in with roles (admin / office / warehouse).
//
// Opt-in: a server with no users behaves exactly as before (pairing code only,
// full data, everyone an implicit admin). Once an admin runs /api/auth/setup,
// the data endpoints require a valid session token and the server enforces
// roles. Credentials live in sync-data/accounts.json and are NEVER put into the
// synced database, so password hashes never reach any device.
//
// Passwords are hashed with PBKDF2-HMAC-SHA256 (standard library only).
// ---------------------------------------------------------------------------

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const pbkdf2Iter = 120000

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"` // admin | office | warehouse
	Pass     string `json:"pass"` // pbkdf2$sha256$<iter>$<saltB64>$<hashB64>
	Disabled bool   `json:"disabled"`
	Created  int64  `json:"created"`
	Updated  int64  `json:"updated"`
}

type session struct {
	UID      string
	Role     string
	Name     string
	Username string
	Exp      time.Time
}

type accountStore struct {
	mu       sync.Mutex
	dir      string
	Company  map[string]any `json:"company"`
	Users    []*user        `json:"users"`
	sessions map[string]*session
}

var acc = &accountStore{sessions: map[string]*session{}}

var validRoles = map[string]bool{"admin": true, "office": true, "warehouse": true}

// ---------------------------------------------------------------- persistence
func (a *accountStore) init(dir string) {
	a.dir = dir
	a.load()
}

func (a *accountStore) path() string { return filepath.Join(a.dir, "accounts.json") }

func (a *accountStore) load() {
	b, err := os.ReadFile(a.path())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, a)
	if a.Company == nil {
		a.Company = map[string]any{}
	}
}

// caller holds a.mu
func (a *accountStore) save() {
	b, _ := json.MarshalIndent(struct {
		Company map[string]any `json:"company"`
		Users   []*user        `json:"users"`
	}{a.Company, a.Users}, "", "  ")
	tmp := a.path() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.path())
	}
}

func (a *accountStore) configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.Users) > 0
}

// caller holds a.mu
func (a *accountStore) findByName(username string) *user {
	u := strings.ToLower(strings.TrimSpace(username))
	for _, usr := range a.Users {
		if strings.ToLower(usr.Username) == u {
			return usr
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) findByID(id string) *user {
	for _, usr := range a.Users {
		if usr.ID == id {
			return usr
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) adminCount(excludeID string) int {
	n := 0
	for _, usr := range a.Users {
		if usr.Role == "admin" && !usr.Disabled && usr.ID != excludeID {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- crypto
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	h := sha256.New
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	var out []byte
	buf := make([]byte, 4)
	for b := 1; b <= blocks; b++ {
		mac := hmac.New(h, password)
		mac.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(b))
		mac.Write(buf)
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac = hmac.New(h, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iter, 32)
	return "pbkdf2$sha256$" + itoa(pbkdf2Iter) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)
}

func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter := atoi(parts[2])
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[3])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[4])
	if err1 != nil || err2 != nil || iter < 1 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ---------------------------------------------------------------- sessions
const sessionTTL = 30 * 24 * time.Hour

// caller holds a.mu
func (a *accountStore) newSession(u *user) string {
	tok := newToken()
	a.sessions[tok] = &session{UID: u.ID, Role: u.Role, Name: u.Name, Username: u.Username, Exp: time.Now().Add(sessionTTL)}
	return tok
}

// session for a request token, refreshed against the current user record.
func (a *accountStore) sessionFor(r *http.Request) (*session, bool) {
	tok := r.Header.Get("X-Sync-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tok]
	if !ok || time.Now().After(s.Exp) {
		if ok {
			delete(a.sessions, tok)
		}
		return nil, false
	}
	// re-check the user still exists and is enabled; pick up role changes live
	u := a.findByID(s.UID)
	if u == nil || u.Disabled {
		delete(a.sessions, tok)
		return nil, false
	}
	s.Role, s.Name, s.Username = u.Role, u.Name, u.Username
	return s, true
}

func (a *accountStore) dropToken(r *http.Request) {
	tok := r.Header.Get("X-Sync-Token")
	if tok == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

// role for a request: the signed-in user's role, or "" when not signed in.
// When accounts are not configured, everyone is an implicit admin (legacy mode).
func roleFor(r *http.Request) string {
	if !acc.configured() {
		return "admin"
	}
	if s, ok := acc.sessionFor(r); ok {
		return s.Role
	}
	return ""
}

func userPublic(u *user) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "name": u.Name, "role": u.Role, "disabled": u.Disabled, "created": u.Created, "updated": u.Updated}
}

// ---------------------------------------------------------------- handlers
// GET /api/auth/status — does this server have accounts yet, and the company name.
func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	writeJSON(w, 200, map[string]any{"configured": len(acc.Users) > 0, "company": acc.Company})
}

// POST /api/auth/setup — first-run: create the company + first admin. Only when no users exist.
func (s *server) authSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Company  map[string]any `json:"company"`
		Username string         `json:"username"`
		Name     string         `json:"name"`
		Password string         `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if len(acc.Users) > 0 {
		writeJSON(w, 409, map[string]any{"error": "This server is already set up. Sign in instead."})
		return
	}
	if len(strings.TrimSpace(req.Username)) < 2 || len(req.Password) < 6 {
		writeJSON(w, 400, map[string]any{"error": "Enter a username and a password of at least 6 characters."})
		return
	}
	now := time.Now().UnixMilli()
	u := &user{ID: newID("u-"), Username: strings.TrimSpace(req.Username), Name: strings.TrimSpace(req.Name), Role: "admin", Pass: hashPassword(req.Password), Created: now, Updated: now}
	acc.Users = append(acc.Users, u)
	if req.Company != nil {
		acc.Company = req.Company
	} else if acc.Company == nil {
		acc.Company = map[string]any{}
	}
	acc.save()
	tok := acc.newSession(u)
	writeJSON(w, 200, map[string]any{"token": tok, "user": userPublic(u), "company": acc.Company})
}

// POST /api/auth/login — {username,password} -> token + user.
func (s *server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u := acc.findByName(req.Username)
	if u == nil || u.Disabled || !verifyPassword(req.Password, u.Pass) {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeJSON(w, 401, map[string]any{"error": "Wrong username or password"})
		return
	}
	tok := acc.newSession(u)
	writeJSON(w, 200, map[string]any{"token": tok, "user": userPublic(u), "company": acc.Company})
}

// POST /api/auth/logout
func (s *server) authLogout(w http.ResponseWriter, r *http.Request) {
	acc.dropToken(r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/auth/me — validate token -> current user.
func (s *server) authMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := acc.sessionFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
		return
	}
	acc.mu.Lock()
	company := acc.Company
	acc.mu.Unlock()
	writeJSON(w, 200, map[string]any{"user": map[string]any{"id": sess.UID, "username": sess.Username, "name": sess.Name, "role": sess.Role}, "company": company})
}

// POST /api/auth/password — change own password {old,new}.
func (s *server) authPassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := acc.sessionFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
		return
	}
	var req struct{ Old, New string }
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u := acc.findByID(sess.UID)
	if u == nil || !verifyPassword(req.Old, u.Pass) {
		writeJSON(w, 400, map[string]any{"error": "Current password is wrong"})
		return
	}
	if len(req.New) < 6 {
		writeJSON(w, 400, map[string]any{"error": "New password must be at least 6 characters"})
		return
	}
	u.Pass = hashPassword(req.New)
	u.Updated = time.Now().UnixMilli()
	acc.save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminOnly wraps a handler so only a signed-in admin can reach it.
func adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := acc.sessionFor(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
			return
		}
		if sess.Role != "admin" {
			writeJSON(w, 403, map[string]any{"error": "Only an admin can manage users"})
			return
		}
		h(w, r)
	}
}

// GET /api/users — list (admin).
func (s *server) usersList(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	out := make([]map[string]any, 0, len(acc.Users))
	for _, u := range acc.Users {
		out = append(out, userPublic(u))
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

// POST /api/users — create (admin).
func (s *server) usersCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Name, Role, Password string }
	if !decodeBody(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 2 || len(req.Password) < 6 {
		writeJSON(w, 400, map[string]any{"error": "Enter a username and a password of at least 6 characters."})
		return
	}
	if !validRoles[req.Role] {
		writeJSON(w, 400, map[string]any{"error": "Role must be admin, office or warehouse."})
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.findByName(req.Username) != nil {
		writeJSON(w, 409, map[string]any{"error": "That username is already taken."})
		return
	}
	now := time.Now().UnixMilli()
	u := &user{ID: newID("u-"), Username: req.Username, Name: strings.TrimSpace(req.Name), Role: req.Role, Pass: hashPassword(req.Password), Created: now, Updated: now}
	acc.Users = append(acc.Users, u)
	acc.save()
	writeJSON(w, 200, map[string]any{"user": userPublic(u)})
}

// POST /api/users/update — change name/role/disabled/password, or delete (admin).
func (s *server) usersUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string  `json:"id"`
		Name     *string `json:"name"`
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
		Delete   bool    `json:"delete"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u := acc.findByID(req.ID)
	if u == nil {
		writeJSON(w, 404, map[string]any{"error": "User not found"})
		return
	}
	// guard: never remove or demote the last active admin
	losesAdmin := req.Delete ||
		(req.Role != nil && *req.Role != "admin" && u.Role == "admin") ||
		(req.Disabled != nil && *req.Disabled && u.Role == "admin")
	if losesAdmin && u.Role == "admin" && acc.adminCount(u.ID) == 0 {
		writeJSON(w, 400, map[string]any{"error": "This is the only admin — make another user an admin first."})
		return
	}
	if req.Delete {
		kept := acc.Users[:0]
		for _, x := range acc.Users {
			if x.ID != u.ID {
				kept = append(kept, x)
			}
		}
		acc.Users = kept
		acc.save()
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": u.ID})
		return
	}
	if req.Name != nil {
		u.Name = strings.TrimSpace(*req.Name)
	}
	if req.Role != nil {
		if !validRoles[*req.Role] {
			writeJSON(w, 400, map[string]any{"error": "Invalid role"})
			return
		}
		u.Role = *req.Role
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	if req.Password != nil {
		if len(*req.Password) < 6 {
			writeJSON(w, 400, map[string]any{"error": "Password must be at least 6 characters"})
			return
		}
		u.Pass = hashPassword(*req.Password)
	}
	u.Updated = time.Now().UnixMilli()
	acc.save()
	writeJSON(w, 200, map[string]any{"user": userPublic(u)})
}

// POST /api/company — update company profile (admin).
func (s *server) companyUpdate(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.Company = req
	acc.save()
	writeJSON(w, 200, map[string]any{"company": acc.Company})
}

// ---------------------------------------------------------------- small utils
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]any{"error": "Bad data: " + err.Error()})
		return false
	}
	return true
}

func itoa(n int) string { return intToStr(int64(n)) }
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// userGate requires a valid session on data endpoints once accounts are configured.
// Before setup it is transparent, so existing (token-less) app versions keep working.
func (s *server) userGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if acc.configured() {
			if _, ok := acc.sessionFor(r); !ok {
				writeJSON(w, 401, map[string]any{"error": "Please sign in", "needLogin": true})
				return
			}
		}
		h(w, r)
	}
}

// /api/users — GET lists users, POST creates one (both admin-only).
func (s *server) usersRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.usersList(w, r)
		return
	}
	s.usersCreate(w, r)
}
