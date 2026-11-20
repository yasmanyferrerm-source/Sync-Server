package main

// ---------------------------------------------------------------------------
// Accounts — companies, users, roles and sign-in sessions.
//
// Opt-in: a server with no users behaves exactly as before (pairing code only,
// full data, everyone an implicit admin). Once the first admin runs setup, data
// endpoints require a signed-in session and the server enforces roles.
//
// A server can hold several companies. Each company's data lives in its own
// folder (the server's original data is the "default" company, so nothing is
// moved). The company a request reads or writes is ALWAYS taken from the
// signed-in session, never from anything the app sends.
//
// Credentials live in sync-data/accounts.json and sessions in sessions.json;
// neither is ever part of the synced data, so hashes never reach a device.
//
// Passwords: PBKDF2-HMAC-SHA256 (standard library), 600k iterations; older
// hashes are upgraded on the next successful sign-in.
// Sessions: a 15-minute access token plus a 30-day refresh token that rotates on
// every use. Only SHA-256 hashes of tokens are stored. Older app versions that
// do not know about refresh tokens get one 30-day token, as before.
// ---------------------------------------------------------------------------

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var pbkdf2Iter = 600000 // var so tests can lower it

const (
	minPassword   = 8
	accessTTL     = 15 * time.Minute
	refreshTTL    = 30 * 24 * time.Hour
	legacyTTL     = 30 * 24 * time.Hour
	rotateGrace   = 2 * time.Minute // a refresh token just replaced is still accepted this long (two requests racing)
	offlineDays   = 7
	defaultCID    = "c-default"
	acctFailLimit = 10
	ipFailLimit   = 30
	failWindow    = 15 * time.Minute
	lockFor       = 15 * time.Minute
)

type company struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Profile map[string]any `json:"profile"`
	Default bool           `json:"default,omitempty"` // data lives directly in sync-data (the server's original company)
	Status  string         `json:"status"`
	Created int64          `json:"created"`
}

type user struct {
	ID        string `json:"id"`
	CompanyID string `json:"cid"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Role      string `json:"role"` // admin | office | warehouse
	Pass      string `json:"pass"` // pbkdf2$sha256$<iter>$<saltB64>$<hashB64>
	Disabled  bool   `json:"disabled"`
	Email     string `json:"email,omitempty"`
	// MustChange is set when an admin issues a temporary password: the user can sign in with it,
	// but must choose their own password before any data is served.
	MustChange bool `json:"mustChange,omitempty"`
	// Recovery is the SHA-256 of the admin's one-time recovery code ("" = none issued).
	Recovery  string `json:"recovery,omitempty"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
	LastLogin int64  `json:"lastLogin,omitempty"`
}

// session is one signed-in device. Persisted (hashes only) so a restart signs nobody out.
type session struct {
	ID       string `json:"id"`
	UID      string `json:"uid"`
	CID      string `json:"cid"`
	Device   string `json:"device,omitempty"`
	Refresh  string `json:"refresh"`        // hash of the current refresh token (legacy: of the one long-lived token)
	Prev     string `json:"prev,omitempty"` // hash of the refresh token it replaced
	Rotated  int64  `json:"rotated,omitempty"`
	Legacy   bool   `json:"legacy,omitempty"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed"`
	Exp      int64  `json:"exp"`
}

type accessTok struct {
	SID string
	Exp time.Time
}

// authCtx is what a handler knows about the caller.
type authCtx struct {
	SID, UID, CID, Role, Name, Username, Email string
}

type failRec struct {
	n     int
	first time.Time
	until time.Time
}

type accountStore struct {
	mu        sync.Mutex
	dir       string
	Companies []*company
	Users     []*user
	sessions  map[string]*session   // by session id
	access    map[string]*accessTok // by token hash
	fails     map[string]*failRec
	amu       sync.Mutex // audit file
}

var acc = newAccountStore()

func newAccountStore() *accountStore {
	return &accountStore{sessions: map[string]*session{}, access: map[string]*accessTok{}, fails: map[string]*failRec{}}
}

var validRoles = map[string]bool{"admin": true, "office": true, "warehouse": true}

// ---------------------------------------------------------------- persistence
func (a *accountStore) init(dir string) {
	a.dir = dir
	a.load()
	a.loadSessions()
}

func (a *accountStore) path() string         { return filepath.Join(a.dir, "accounts.json") }
func (a *accountStore) sessionsPath() string { return filepath.Join(a.dir, "sessions.json") }

func (a *accountStore) load() {
	b, err := os.ReadFile(a.path())
	if err != nil {
		return
	}
	var f struct {
		Version   int            `json:"version"`
		Company   map[string]any `json:"company"` // version 1 (one company per server)
		Companies []*company     `json:"companies"`
		Users     []*user        `json:"users"`
	}
	if json.Unmarshal(b, &f) != nil {
		return
	}
	a.Companies, a.Users = f.Companies, f.Users
	if f.Version < 2 && len(f.Users) > 0 {
		// Upgrade from 4.4.x: the one company becomes the default company, its data stays where it is.
		_ = os.WriteFile(filepath.Join(a.dir, "accounts.v1.json"), b, 0o600)
		name := ""
		if f.Company != nil {
			if n, ok := f.Company["name"].(string); ok {
				name = n
			}
		} else {
			f.Company = map[string]any{}
		}
		a.Companies = []*company{{ID: defaultCID, Name: name, Profile: f.Company, Default: true, Status: "active", Created: time.Now().UnixMilli()}}
		for _, u := range a.Users {
			u.CompanyID = defaultCID
		}
		a.save()
	}
}

// caller holds a.mu
func (a *accountStore) save() {
	b, _ := json.MarshalIndent(struct {
		Version   int        `json:"version"`
		Companies []*company `json:"companies"`
		Users     []*user    `json:"users"`
	}{2, a.Companies, a.Users}, "", "  ")
	writeFileAtomic(a.path(), b)
}

func (a *accountStore) loadSessions() {
	b, err := os.ReadFile(a.sessionsPath())
	if err != nil {
		return
	}
	var list []*session
	if json.Unmarshal(b, &list) != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, s := range list {
		if s.Exp > now {
			a.sessions[s.ID] = s
		}
	}
}

// caller holds a.mu
func (a *accountStore) saveSessions() {
	list := make([]*session, 0, len(a.sessions))
	now := time.Now().UnixMilli()
	for id, s := range a.sessions {
		if s.Exp <= now {
			delete(a.sessions, id)
			continue
		}
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created < list[j].Created })
	b, _ := json.Marshal(list)
	writeFileAtomic(a.sessionsPath(), b)
}

func writeFileAtomic(p string, b []byte) {
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func (a *accountStore) configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.Users) > 0
}

// caller holds a.mu. Sign-in names (username and email) are unique across the whole server.
func (a *accountStore) findByName(name string) *user {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return nil
	}
	for _, u := range a.Users {
		if strings.ToLower(u.Username) == n {
			return u
		}
	}
	if strings.Contains(n, "@") {
		for _, u := range a.Users {
			if u.Email != "" && strings.ToLower(u.Email) == n {
				return u
			}
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) findByID(id string) *user {
	for _, u := range a.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) companyByID(id string) *company {
	for _, c := range a.Companies {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) defaultCompany() *company {
	for _, c := range a.Companies {
		if c.Default {
			return c
		}
	}
	return nil
}

// caller holds a.mu
func (a *accountStore) adminCount(cid, excludeID string) int {
	n := 0
	for _, u := range a.Users {
		if u.CompanyID == cid && u.Role == "admin" && !u.Disabled && u.ID != excludeID {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- crypto
func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
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
	if err1 != nil || err2 != nil || iter < 1 || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func hashIter(encoded string) int {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 {
		return 0
	}
	return atoi(parts[2])
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

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Recovery codes and server-made temporary passwords use letters and digits that are
// hard to misread (no 0/O, 1/I/L).
const readable = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func readableCode(groups, size int) string {
	b := make([]byte, groups*size)
	_, _ = rand.Read(b)
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%size == 0 {
			out.WriteByte('-')
		}
		out.WriteByte(readable[int(c)%len(readable)])
	}
	return out.String()
}

func normCode(c string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(c) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// caller holds a.mu. Gives the admin a new recovery code; returns it (shown once).
func (a *accountStore) newRecoveryCode(u *user) string {
	code := readableCode(5, 5)
	u.Recovery = sha(normCode(code))
	return code
}

// ---------------------------------------------------------------- sessions
func tokenFrom(r *http.Request) string {
	if t := r.Header.Get("X-Sync-Token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.URL.Query().Get("token")
}

type issued struct {
	Access, Refresh string
	Sess            *session
}

// caller holds a.mu
func (a *accountStore) issue(u *user, device string, legacy bool) issued {
	now := time.Now()
	s := &session{ID: newID("s-"), UID: u.ID, CID: u.CompanyID, Device: trimTo(device, 80), Created: now.UnixMilli(), LastUsed: now.UnixMilli()}
	out := issued{Sess: s}
	if legacy {
		tok := newToken()
		s.Legacy, s.Refresh, s.Exp = true, sha(tok), now.Add(legacyTTL).UnixMilli()
		out.Access = tok
	} else {
		rt := newToken()
		s.Refresh, s.Exp = sha(rt), now.Add(refreshTTL).UnixMilli()
		out.Refresh = rt
		out.Access = a.newAccess(s.ID)
	}
	a.sessions[s.ID] = s
	a.saveSessions()
	return out
}

// caller holds a.mu
func (a *accountStore) newAccess(sid string) string {
	t := newToken()
	now := time.Now()
	a.access[sha(t)] = &accessTok{SID: sid, Exp: now.Add(accessTTL)}
	if len(a.access) > 5000 { // drop expired ones now and then
		for k, v := range a.access {
			if now.After(v.Exp) {
				delete(a.access, k)
			}
		}
	}
	return t
}

// caller holds a.mu
func (a *accountStore) revokeSession(sid string) {
	delete(a.sessions, sid)
	for k, v := range a.access {
		if v.SID == sid {
			delete(a.access, k)
		}
	}
}

// caller holds a.mu. Signs a user out everywhere except keepSID ("" = everywhere).
func (a *accountStore) revokeUser(uid, keepSID string) int {
	n := 0
	for id, s := range a.sessions {
		if s.UID == uid && id != keepSID {
			a.revokeSession(id)
			n++
		}
	}
	a.saveSessions()
	return n
}

// caller holds a.mu. The session behind a token, checked against the current user record.
func (a *accountStore) resolve(tok string) (*session, *user, bool) {
	if tok == "" {
		return nil, nil, false
	}
	h := sha(tok)
	now := time.Now()
	var s *session
	if at, ok := a.access[h]; ok {
		if now.After(at.Exp) {
			delete(a.access, h)
			return nil, nil, false
		}
		s = a.sessions[at.SID]
	} else {
		for _, x := range a.sessions { // old app versions: one long-lived token
			if x.Legacy && subtle.ConstantTimeCompare([]byte(x.Refresh), []byte(h)) == 1 {
				s = x
				break
			}
		}
	}
	if s == nil || now.UnixMilli() > s.Exp {
		return nil, nil, false
	}
	u := a.findByID(s.UID)
	if u == nil || u.Disabled || u.CompanyID != s.CID {
		a.revokeSession(s.ID)
		return nil, nil, false
	}
	s.LastUsed = now.UnixMilli()
	return s, u, true
}

// sessionFor reads the caller from the request (role and name are always the user's current ones).
func (a *accountStore) sessionFor(r *http.Request) (*authCtx, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, u, ok := a.resolve(tokenFrom(r))
	if !ok {
		return nil, false
	}
	return &authCtx{SID: s.ID, UID: u.ID, CID: u.CompanyID, Role: u.Role, Name: u.Name, Username: u.Username, Email: u.Email}, true
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

// ---------------------------------------------------------------- sign-in limits
// clientIP is the real caller: Cloudflare's header when the request came in through the tunnel.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); cf != "" {
			return cf
		}
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	return host
}

// caller holds a.mu. Returns how long a key is still locked (0 = not locked).
func (a *accountStore) lockedFor(key string) time.Duration {
	f := a.fails[key]
	if f == nil {
		return 0
	}
	if d := time.Until(f.until); d > 0 {
		return d
	}
	return 0
}

// caller holds a.mu
func (a *accountStore) addFail(key string, limit int) {
	now := time.Now()
	f := a.fails[key]
	if f == nil || now.Sub(f.first) > failWindow {
		f = &failRec{first: now}
		a.fails[key] = f
	}
	f.n++
	if f.n >= limit {
		f.until = now.Add(lockFor)
		f.n, f.first = 0, now
	}
	if len(a.fails) > 10000 {
		for k, v := range a.fails {
			if now.Sub(v.first) > failWindow && now.After(v.until) {
				delete(a.fails, k)
			}
		}
	}
}

func lockMsg(d time.Duration) string {
	m := int(d.Minutes()) + 1
	return "Too many sign-in attempts. Please wait " + itoa(m) + " minute" + map[bool]string{true: "", false: "s"}[m == 1] + " and try again."
}

// ---------------------------------------------------------------- audit log
// One JSON line per account event in sync-data/audit.log (sign-ins, user and role changes, resets).
func (a *accountStore) audit(r *http.Request, cid, actor, action, target string, extra map[string]any) {
	e := map[string]any{"t": time.Now().UTC().Format(time.RFC3339), "cid": cid, "actor": actor, "action": action}
	if target != "" {
		e["target"] = target
	}
	if r != nil {
		e["ip"] = clientIP(r)
	}
	for k, v := range extra {
		e[k] = v
	}
	b, _ := json.Marshal(e)
	a.amu.Lock()
	defer a.amu.Unlock()
	f, err := os.OpenFile(filepath.Join(a.dir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

// ---------------------------------------------------------------- views
func userPublic(u *user) map[string]any {
	return map[string]any{"id": u.ID, "cid": u.CompanyID, "username": u.Username, "name": u.Name, "role": u.Role, "disabled": u.Disabled, "email": u.Email,
		"mustChange": u.MustChange, "hasRecovery": u.Recovery != "", "created": u.Created, "updated": u.Updated, "lastLogin": u.LastLogin}
}

func companyPublic(c *company) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range c.Profile {
		out[k] = v
	}
	out["id"] = c.ID
	if c.Name != "" {
		out["name"] = c.Name
	}
	return out
}

// caller holds a.mu. Everything the app needs after a sign-in or refresh.
func (a *accountStore) signedIn(u *user, t issued) map[string]any {
	c := a.companyByID(u.CompanyID)
	out := map[string]any{
		"token":       t.Access,
		"user":        userPublic(u),
		"company":     companyPublic(c),
		"perms":       permsFor(u.Role),
		"offlinePass": offlinePass(u, c),
		"offlineDays": offlineDays,
		"pubkey":      offlinePubJWK(),
		"session":     t.Sess.ID,
	}
	if t.Refresh != "" {
		out["refresh"] = t.Refresh
		out["expiresIn"] = int(accessTTL.Seconds())
	}
	return out
}

// ---------------------------------------------------------------- handlers: sign-in
// GET /api/auth/status — does this server have accounts yet, and which features it has.
func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	out := map[string]any{"configured": len(acc.Users) > 0, "v": 2, "hosted": s.cfg.Hosted, "signup": s.cfg.Hosted, "minPassword": minPassword, "offlineDays": offlineDays}
	if !s.cfg.Hosted { // a hosted server holds many companies: never show one of them on everyone's sign-in screen
		out["company"] = companyPublic(acc.defaultCompany())
	}
	writeJSON(w, 200, out)
}

type signUpReq struct {
	Company  map[string]any `json:"company"`
	Username string         `json:"username"`
	Email    string         `json:"email"`
	Name     string         `json:"name"`
	Password string         `json:"password"`
	Device   string         `json:"device"`
	V        int            `json:"v"`
}

// POST /api/auth/setup — first run: the server's own company and its admin. Only when no users exist.
func (s *server) authSetup(w http.ResponseWriter, r *http.Request) {
	var req signUpReq
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if len(acc.Users) > 0 {
		writeJSON(w, 409, map[string]any{"error": "This server is already set up. Sign in instead."})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		req.Username = req.Email
	}
	if req.Email != "" && !validEmail(req.Email) {
		writeJSON(w, 400, map[string]any{"error": "That email address does not look right."})
		return
	}
	if len(req.Username) < 2 || len(req.Password) < minPassword {
		writeJSON(w, 400, map[string]any{"error": "Enter a username and a password of at least 8 characters."})
		return
	}
	now := time.Now().UnixMilli()
	prof := req.Company
	if prof == nil {
		prof = map[string]any{}
	}
	name, _ := prof["name"].(string)
	c := acc.defaultCompany()
	if c == nil {
		c = &company{ID: defaultCID, Default: true, Status: "active", Created: now}
		acc.Companies = append(acc.Companies, c)
	}
	c.Name, c.Profile = strings.TrimSpace(name), prof
	u := &user{ID: newID("u-"), CompanyID: c.ID, Username: req.Username, Email: req.Email, Name: strings.TrimSpace(req.Name), Role: "admin", Pass: hashPassword(req.Password), Created: now, Updated: now, LastLogin: now}
	code := acc.newRecoveryCode(u)
	acc.Users = append(acc.Users, u)
	acc.save()
	t := acc.issue(u, req.Device, req.V < 2)
	acc.audit(r, c.ID, u.ID, "company.setup", c.ID, nil)
	out := acc.signedIn(u, t)
	out["recoveryCode"] = code
	writeJSON(w, 200, out)
}

// POST /api/auth/create-company — a new business signs up (hosted servers only).
func (s *server) authCreateCompany(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Hosted {
		writeJSON(w, 403, map[string]any{"error": "This server does not take new companies. Ask its owner, or sign in."})
		return
	}
	var req signUpReq
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	ipKey := "signup:" + clientIP(r)
	if d := acc.lockedFor(ipKey); d > 0 {
		writeJSON(w, 429, map[string]any{"error": "Too many new companies from this network. Please try again later."})
		return
	}
	name, _ := req.Company["name"].(string)
	name = strings.TrimSpace(name)
	req.Email = strings.TrimSpace(req.Email)
	switch {
	case len(name) < 2:
		writeJSON(w, 400, map[string]any{"error": "Enter the company name."})
		return
	case !validEmail(req.Email):
		writeJSON(w, 400, map[string]any{"error": "Enter a valid email address — it is how you sign in."})
		return
	case len(req.Password) < minPassword:
		writeJSON(w, 400, map[string]any{"error": "Choose a password of at least 8 characters."})
		return
	case acc.findByName(req.Email) != nil:
		writeJSON(w, 409, map[string]any{"error": "An account with that email already exists. Sign in instead."})
		return
	}
	acc.addFail(ipKey, 5) // at most 5 new companies per network every 15 minutes
	now := time.Now().UnixMilli()
	prof := map[string]any{}
	for k, v := range req.Company {
		prof[k] = v
	}
	prof["name"] = name
	c := &company{ID: newID("c-"), Name: name, Profile: prof, Status: "active", Created: now}
	u := &user{ID: newID("u-"), CompanyID: c.ID, Username: req.Email, Email: req.Email, Name: strings.TrimSpace(req.Name), Role: "admin", Pass: hashPassword(req.Password), Created: now, Updated: now, LastLogin: now}
	code := acc.newRecoveryCode(u)
	acc.Companies = append(acc.Companies, c)
	acc.Users = append(acc.Users, u)
	acc.save()
	t := acc.issue(u, req.Device, req.V < 2)
	acc.audit(r, c.ID, u.ID, "company.create", c.ID, map[string]any{"name": name})
	out := acc.signedIn(u, t)
	out["recoveryCode"] = code
	writeJSON(w, 200, out)
}

// POST /api/auth/login — {username (or email), password, device, v} -> tokens + user.
func (s *server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Device   string `json:"device"`
		V        int    `json:"v"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ident := strings.TrimSpace(req.Username)
	if ident == "" {
		ident = strings.TrimSpace(req.Email)
	}
	uKey, ipKey := "u:"+strings.ToLower(ident), "ip:"+clientIP(r)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if d := max(acc.lockedFor(uKey), acc.lockedFor(ipKey)); d > 0 {
		writeJSON(w, 429, map[string]any{"error": lockMsg(d), "locked": true})
		return
	}
	u := acc.findByName(ident)
	if u == nil || u.Disabled || !verifyPassword(req.Password, u.Pass) {
		acc.addFail(uKey, acctFailLimit)
		acc.addFail(ipKey, ipFailLimit)
		cid := ""
		if u != nil {
			cid = u.CompanyID
		}
		acc.audit(r, cid, "", "signin.fail", ident, nil)
		acc.mu.Unlock()
		time.Sleep(400 * time.Millisecond) // slow down guessing (without holding the lock)
		acc.mu.Lock()
		writeJSON(w, 401, map[string]any{"error": "Wrong username or password"})
		return
	}
	delete(acc.fails, uKey)
	now := time.Now().UnixMilli()
	if hashIter(u.Pass) < pbkdf2Iter { // upgrade older, weaker hashes
		u.Pass = hashPassword(req.Password)
	}
	u.LastLogin = now
	acc.save()
	t := acc.issue(u, req.Device, req.V < 2)
	acc.audit(r, u.CompanyID, u.ID, "signin", u.ID, map[string]any{"device": trimTo(req.Device, 80)})
	writeJSON(w, 200, acc.signedIn(u, t))
}

// POST /api/auth/refresh — {refresh} -> a new access token and a new refresh token.
func (s *server) authRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Refresh string `json:"refresh"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	h := sha(req.Refresh)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	now := time.Now()
	var sess *session
	reused := false
	for _, x := range acc.sessions {
		if x.Legacy {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(x.Refresh), []byte(h)) == 1 {
			sess = x
			break
		}
		if x.Prev != "" && subtle.ConstantTimeCompare([]byte(x.Prev), []byte(h)) == 1 {
			sess, reused = x, true
			break
		}
	}
	if sess == nil || now.UnixMilli() > sess.Exp {
		writeJSON(w, 401, map[string]any{"error": "Please sign in again", "needLogin": true})
		return
	}
	if reused && now.Sub(time.UnixMilli(sess.Rotated)) > rotateGrace {
		// an old refresh token came back long after it was replaced: someone copied it. End the session.
		acc.audit(r, sess.CID, sess.UID, "session.reuse", sess.ID, nil)
		acc.revokeSession(sess.ID)
		acc.saveSessions()
		writeJSON(w, 401, map[string]any{"error": "Please sign in again", "needLogin": true})
		return
	}
	u := acc.findByID(sess.UID)
	if u == nil || u.Disabled || u.CompanyID != sess.CID {
		acc.revokeSession(sess.ID)
		acc.saveSessions()
		writeJSON(w, 401, map[string]any{"error": "Please sign in again", "needLogin": true})
		return
	}
	rt := newToken()
	if !reused {
		sess.Prev = sess.Refresh
	}
	sess.Refresh, sess.Rotated, sess.LastUsed, sess.Exp = sha(rt), now.UnixMilli(), now.UnixMilli(), now.Add(refreshTTL).UnixMilli()
	acc.saveSessions()
	writeJSON(w, 200, acc.signedIn(u, issued{Access: acc.newAccess(sess.ID), Refresh: rt, Sess: sess}))
}

// POST /api/auth/logout — this device.
func (s *server) authLogout(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	if sess, _, ok := acc.resolve(tokenFrom(r)); ok {
		acc.revokeSession(sess.ID)
		acc.saveSessions()
	}
	acc.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/logout-all — every device this user is signed in on.
func (s *server) authLogoutAll(w http.ResponseWriter, r *http.Request) {
	me, ok := acc.sessionFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
		return
	}
	acc.mu.Lock()
	n := acc.revokeUser(me.UID, "")
	acc.mu.Unlock()
	acc.audit(r, me.CID, me.UID, "signout.all", me.UID, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "devices": n})
}

// GET /api/auth/me — validate token -> current user, company, permissions and a fresh offline pass.
func (s *server) authMe(w http.ResponseWriter, r *http.Request) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	sess, u, ok := acc.resolve(tokenFrom(r))
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
		return
	}
	out := acc.signedIn(u, issued{Sess: sess})
	delete(out, "token")
	writeJSON(w, 200, out)
}

// POST /api/auth/password — change own password {old,new}. Other devices are signed out.
func (s *server) authPassword(w http.ResponseWriter, r *http.Request) {
	me, ok := acc.sessionFor(r)
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
	u := acc.findByID(me.UID)
	if u == nil || !verifyPassword(req.Old, u.Pass) {
		writeJSON(w, 400, map[string]any{"error": "Current password is wrong"})
		return
	}
	if len(req.New) < minPassword {
		writeJSON(w, 400, map[string]any{"error": "New password must be at least 8 characters"})
		return
	}
	if req.New == req.Old {
		writeJSON(w, 400, map[string]any{"error": "Choose a password that is different from the current one"})
		return
	}
	u.Pass = hashPassword(req.New)
	u.MustChange = false
	u.Updated = time.Now().UnixMilli()
	acc.save()
	acc.revokeUser(u.ID, me.SID)
	acc.audit(r, u.CompanyID, u.ID, "password.change", u.ID, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "user": userPublic(u)})
}

// POST /api/auth/recover — an admin who lost their password: {username|email, code, password}.
// Sets the new password, signs out every device, signs this one in and returns a NEW recovery code.
func (s *server) authRecover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Code     string `json:"code"`
		Password string `json:"password"`
		Device   string `json:"device"`
		V        int    `json:"v"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ident := strings.TrimSpace(req.Username)
	if ident == "" {
		ident = strings.TrimSpace(req.Email)
	}
	rKey, ipKey := "r:"+strings.ToLower(ident), "ip:"+clientIP(r)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if d := max(acc.lockedFor(rKey), acc.lockedFor(ipKey)); d > 0 {
		writeJSON(w, 429, map[string]any{"error": lockMsg(d), "locked": true})
		return
	}
	if len(req.Password) < minPassword {
		writeJSON(w, 400, map[string]any{"error": "Choose a new password of at least 8 characters."})
		return
	}
	u := acc.findByName(ident)
	good := u != nil && !u.Disabled && u.Role == "admin" && u.Recovery != "" &&
		subtle.ConstantTimeCompare([]byte(u.Recovery), []byte(sha(normCode(req.Code)))) == 1
	if !good {
		acc.addFail(rKey, 5)
		acc.addFail(ipKey, ipFailLimit)
		acc.audit(r, "", "", "recover.fail", ident, nil)
		writeJSON(w, 400, map[string]any{"error": "That email and recovery code don't match. Only an administrator can use a recovery code — everyone else, ask your administrator to reset your password."})
		return
	}
	delete(acc.fails, rKey)
	now := time.Now().UnixMilli()
	u.Pass, u.MustChange, u.Updated, u.LastLogin = hashPassword(req.Password), false, now, now
	code := acc.newRecoveryCode(u)
	acc.save()
	acc.revokeUser(u.ID, "")
	t := acc.issue(u, req.Device, req.V < 2)
	acc.audit(r, u.CompanyID, u.ID, "recover.ok", u.ID, nil)
	out := acc.signedIn(u, t)
	out["recoveryCode"] = code
	writeJSON(w, 200, out)
}

// POST /api/auth/recovery-code — a signed-in admin makes a new recovery code {password}. The old one stops working.
func (s *server) authRecoveryCode(w http.ResponseWriter, r *http.Request) {
	me, ok := acc.sessionFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u := acc.findByID(me.UID)
	if u == nil || u.Role != "admin" {
		writeJSON(w, 403, map[string]any{"error": "Only the administrator has a recovery code."})
		return
	}
	if !verifyPassword(req.Password, u.Pass) {
		writeJSON(w, 400, map[string]any{"error": "Your password is wrong."})
		return
	}
	code := acc.newRecoveryCode(u)
	u.Updated = time.Now().UnixMilli()
	acc.save()
	acc.audit(r, u.CompanyID, u.ID, "recovery.new", u.ID, nil)
	writeJSON(w, 200, map[string]any{"recoveryCode": code, "user": userPublic(u)})
}

// ---------------------------------------------------------------- handlers: admin
// adminOnly wraps a handler so only a signed-in admin (with their own password set) can reach it.
func adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok := acc.sessionFor(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": "Not signed in", "needLogin": true})
			return
		}
		if me.Role != "admin" {
			writeJSON(w, 403, map[string]any{"error": "Only an admin can manage users"})
			return
		}
		acc.mu.Lock()
		u := acc.findByID(me.UID)
		must := u != nil && u.MustChange
		acc.mu.Unlock()
		if must {
			writeJSON(w, 403, map[string]any{"error": "Set your own password first", "mustChange": true})
			return
		}
		h(w, r)
	}
}

// GET /api/users — the admin's own company only.
func (s *server) usersList(w http.ResponseWriter, r *http.Request) {
	me, _ := acc.sessionFor(r)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	out := make([]map[string]any, 0, len(acc.Users))
	for _, u := range acc.Users {
		if u.CompanyID == me.CID {
			out = append(out, userPublic(u))
		}
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

// POST /api/users — create a user in the admin's company.
func (s *server) usersCreate(w http.ResponseWriter, r *http.Request) {
	me, _ := acc.sessionFor(r)
	var req struct {
		Username, Name, Role, Password, Email string
		MustChange                            bool
	}
	if !decodeBody(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Email != "" && !validEmail(req.Email) {
		writeJSON(w, 400, map[string]any{"error": "That email address does not look right."})
		return
	}
	if req.Username == "" {
		req.Username = req.Email // added by email: the address is the sign-in name
	}
	if len(req.Username) < 2 || len(req.Password) < minPassword {
		writeJSON(w, 400, map[string]any{"error": "Enter a username and a password of at least 8 characters."})
		return
	}
	if !validRoles[req.Role] {
		writeJSON(w, 400, map[string]any{"error": "Role must be admin, office or warehouse."})
		return
	}
	if req.Role == "admin" {
		writeJSON(w, 400, map[string]any{"error": "A company has one administrator. Add them as Office, then use Make administrator on their account to hand the role over."})
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.findByName(req.Username) != nil {
		writeJSON(w, 409, map[string]any{"error": "That username is already taken."})
		return
	}
	if req.Email != "" && acc.findByName(req.Email) != nil {
		writeJSON(w, 409, map[string]any{"error": "A user with that email already exists."})
		return
	}
	now := time.Now().UnixMilli()
	u := &user{ID: newID("u-"), CompanyID: me.CID, Username: req.Username, Name: strings.TrimSpace(req.Name), Role: req.Role, Pass: hashPassword(req.Password), Email: req.Email, MustChange: req.MustChange, Created: now, Updated: now}
	acc.Users = append(acc.Users, u)
	acc.save()
	acc.audit(r, me.CID, me.UID, "user.create", u.ID, map[string]any{"role": u.Role})
	writeJSON(w, 200, map[string]any{"user": userPublic(u)})
}

// POST /api/users/update — change name/role/disabled/password/email, sign out, or delete (admin, own company).
func (s *server) usersUpdate(w http.ResponseWriter, r *http.Request) {
	me, _ := acc.sessionFor(r)
	var req struct {
		ID         string  `json:"id"`
		Name       *string `json:"name"`
		Role       *string `json:"role"`
		Disabled   *bool   `json:"disabled"`
		Password   *string `json:"password"`
		Email      *string `json:"email"`
		MustChange *bool   `json:"mustChange"`
		SignOut    bool    `json:"signOut"`
		Delete     bool    `json:"delete"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	u := acc.findByID(req.ID)
	if u == nil || u.CompanyID != me.CID {
		writeJSON(w, 404, map[string]any{"error": "User not found"})
		return
	}
	self := u.ID == me.UID
	if self && (req.Delete || (req.Disabled != nil && *req.Disabled) || (req.Role != nil && *req.Role != u.Role)) {
		writeJSON(w, 400, map[string]any{"error": "You can't delete, turn off or change the role of your own account. To step down, make someone else the administrator first."})
		return
	}
	// guard: never remove or demote the last active admin
	losesAdmin := req.Delete || (req.Role != nil && *req.Role != "admin") || (req.Disabled != nil && *req.Disabled)
	if losesAdmin && u.Role == "admin" && acc.adminCount(u.CompanyID, u.ID) == 0 {
		writeJSON(w, 400, map[string]any{"error": "This is the only admin — make another user an admin first."})
		return
	}
	if req.Role != nil && *req.Role == "admin" && u.Role != "admin" {
		writeJSON(w, 400, map[string]any{"error": "A company has one administrator. Use Make administrator to hand the role over."})
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
		acc.revokeUser(u.ID, "")
		acc.audit(r, me.CID, me.UID, "user.delete", u.ID, nil)
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": u.ID})
		return
	}
	signOut := req.SignOut
	if req.Name != nil {
		u.Name = strings.TrimSpace(*req.Name)
	}
	if req.Role != nil && *req.Role != u.Role {
		if !validRoles[*req.Role] {
			writeJSON(w, 400, map[string]any{"error": "Invalid role"})
			return
		}
		acc.audit(r, me.CID, me.UID, "user.role", u.ID, map[string]any{"from": u.Role, "to": *req.Role})
		u.Role = *req.Role
	}
	if req.Disabled != nil && *req.Disabled != u.Disabled {
		u.Disabled = *req.Disabled
		signOut = signOut || u.Disabled
		acc.audit(r, me.CID, me.UID, map[bool]string{true: "user.disable", false: "user.enable"}[u.Disabled], u.ID, nil)
	}
	if req.Password != nil {
		if len(*req.Password) < minPassword {
			writeJSON(w, 400, map[string]any{"error": "Password must be at least 8 characters"})
			return
		}
		u.Pass = hashPassword(*req.Password)
		signOut = true
		acc.audit(r, me.CID, me.UID, "user.reset", u.ID, nil)
	}
	if req.Email != nil {
		e := strings.TrimSpace(*req.Email)
		if e != "" && !validEmail(e) {
			writeJSON(w, 400, map[string]any{"error": "That email address does not look right."})
			return
		}
		if e != "" {
			if o := acc.findByName(e); o != nil && o.ID != u.ID {
				writeJSON(w, 409, map[string]any{"error": "A user with that email already exists."})
				return
			}
		}
		u.Email = e
	}
	if req.MustChange != nil {
		u.MustChange = *req.MustChange
	}
	u.Updated = time.Now().UnixMilli()
	acc.save()
	if signOut {
		keep := ""
		if self {
			keep = me.SID
		}
		acc.revokeUser(u.ID, keep)
		if req.SignOut {
			acc.audit(r, me.CID, me.UID, "user.signout", u.ID, nil)
		}
	}
	writeJSON(w, 200, map[string]any{"user": userPublic(u)})
}

// POST /api/users/transfer-admin — {id, password}: hand the administrator role to another user.
// The old admin becomes an Office user and their recovery code stops working.
func (s *server) usersTransferAdmin(w http.ResponseWriter, r *http.Request) {
	me, _ := acc.sessionFor(r)
	var req struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	cur := acc.findByID(me.UID)
	t := acc.findByID(req.ID)
	if cur == nil || !verifyPassword(req.Password, cur.Pass) {
		writeJSON(w, 400, map[string]any{"error": "Your password is wrong."})
		return
	}
	if t == nil || t.CompanyID != me.CID || t.ID == cur.ID {
		writeJSON(w, 404, map[string]any{"error": "User not found"})
		return
	}
	if t.Disabled || t.MustChange {
		writeJSON(w, 400, map[string]any{"error": "That user has to sign in and set their own password before they can become the administrator."})
		return
	}
	now := time.Now().UnixMilli()
	t.Role, t.Updated = "admin", now
	cur.Role, cur.Recovery, cur.Updated = "office", "", now
	acc.save()
	acc.audit(r, me.CID, me.UID, "admin.transfer", t.ID, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "user": userPublic(cur), "admin": userPublic(t)})
}

// POST /api/company — update the admin's own company profile.
func (s *server) companyUpdate(w http.ResponseWriter, r *http.Request) {
	me, _ := acc.sessionFor(r)
	var req map[string]any
	if !decodeBody(w, r, &req) {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	c := acc.companyByID(me.CID)
	if c == nil {
		writeJSON(w, 404, map[string]any{"error": "Company not found"})
		return
	}
	delete(req, "id")
	c.Profile = req
	if n, ok := req["name"].(string); ok && strings.TrimSpace(n) != "" {
		c.Name = strings.TrimSpace(n)
	}
	acc.save()
	acc.audit(r, me.CID, me.UID, "company.update", c.ID, nil)
	writeJSON(w, 200, map[string]any{"company": companyPublic(c)})
}

// ---------------------------------------------------------------- small utils
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, 400, map[string]any{"error": "Bad data: " + err.Error()})
		return false
	}
	return true
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
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

// validEmail is a light shape check — one @, something on both sides, a dot in the domain, no spaces.
func validEmail(e string) bool {
	at := strings.LastIndex(e, "@")
	if at < 1 || at == len(e)-1 || strings.ContainsAny(e, " \t\r\n<>,;") || strings.Count(e, "@") != 1 {
		return false
	}
	d := e[at+1:]
	return strings.Contains(d, ".") && !strings.HasPrefix(d, ".") && !strings.HasSuffix(d, ".") && len(e) <= 254
}

// /api/users — GET lists users, POST creates one (both admin-only).
func (s *server) usersRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.usersList(w, r)
		return
	}
	s.usersCreate(w, r)
}
