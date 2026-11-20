package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCode = "123456"

type tsrv struct {
	t   *testing.T
	s   *server
	srv *httptest.Server
}

func newTest(t *testing.T, hosted bool) *tsrv {
	t.Helper()
	pbkdf2Iter = 1000
	runSaver = false
	dir := t.TempDir()
	s := newServer(dir)
	_ = os.MkdirAll(filepath.Join(dir, "attachments"), 0o755)
	s.cfg = config{Port: 8787, Code: testCode, Name: "test", Hosted: hosted}
	s.loadDB()
	acc = newAccountStore()
	acc.init(dir)
	h := httptest.NewServer(s.routes())
	t.Cleanup(h.Close)
	return &tsrv{t: t, s: s, srv: h}
}

type opt struct {
	code  bool
	token string
	hdr   map[string]string
}

func (x *tsrv) call(method, path string, body any, o opt) (int, map[string]any) {
	x.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, x.srv.URL+path, rd)
	if o.code {
		req.Header.Set("X-Sync-Code", testCode)
	}
	if o.token != "" {
		req.Header.Set("X-Sync-Token", o.token)
	}
	for k, v := range o.hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func (x *tsrv) must(code int, want int, m map[string]any, what string) {
	x.t.Helper()
	if code != want {
		x.t.Fatalf("%s: got %d want %d: %v", what, code, want, m)
	}
}

// setup makes the server's own company with an admin; returns the admin's access token.
func (x *tsrv) setup() (string, map[string]any) {
	c, m := x.call("POST", "/api/auth/setup", map[string]any{"company": map[string]any{"name": "Buckeye"}, "username": "boss@a.com", "email": "boss@a.com", "name": "Boss", "password": "password-a1", "v": 2}, opt{code: true})
	x.must(c, 200, m, "setup")
	return m["token"].(string), m
}

func (x *tsrv) login(name, pw string, o opt) (int, map[string]any) {
	return x.call("POST", "/api/auth/login", map[string]any{"username": name, "password": pw, "v": 2, "device": "test"}, o)
}

func items(m map[string]any) []any {
	db, _ := m["db"].(map[string]any)
	a, _ := db["items"].([]any)
	return a
}

func pushItems(x *tsrv, tok string, recs ...map[string]any) {
	x.t.Helper()
	c, m := x.call("POST", "/api/push", map[string]any{"changes": map[string]any{"items": map[string]any{"up": recs}}}, opt{code: true, token: tok})
	x.must(c, 200, m, "push")
}

func TestLegacyModeWithoutAccounts(t *testing.T) {
	x := newTest(t, false)
	c, m := x.call("GET", "/api/state", nil, opt{code: true})
	x.must(c, 200, m, "legacy state with pairing code")
	c, m = x.call("GET", "/api/state", nil, opt{})
	x.must(c, 401, m, "no pairing code")
}

func TestSetupGivesRecoveryCodeAndRequiresSignIn(t *testing.T) {
	x := newTest(t, false)
	tok, m := x.setup()
	if rc, _ := m["recoveryCode"].(string); len(normCode(rc)) != 25 {
		t.Fatalf("recovery code missing: %v", m["recoveryCode"])
	}
	if m["refresh"] == nil || m["offlinePass"] == nil || m["perms"] == nil {
		t.Fatalf("setup response incomplete: %v", m)
	}
	c, m := x.call("GET", "/api/state", nil, opt{code: true})
	x.must(c, 401, m, "data without a session")
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: tok})
	x.must(c, 200, m, "data with a session")
	if _, ok := m["db"].(map[string]any); !ok {
		t.Fatalf("no data: %v", m)
	}
}

func TestCompanyCannotReadAnotherCompany(t *testing.T) {
	x := newTest(t, true)
	tokA, _ := x.setup()
	pushItems(x, tokA, map[string]any{"sku": "A-1", "name": "Buckeye item", "cost": 10.0})

	// a second business signs up on the hosted server, without the pairing code
	c, m := x.call("POST", "/api/auth/create-company", map[string]any{"company": map[string]any{"name": "Pet Shop"}, "email": "owner@b.com", "name": "B", "password": "password-b1", "v": 2}, opt{})
	x.must(c, 200, m, "create company B")
	tokB := m["token"].(string)
	cidB := m["user"].(map[string]any)["cid"].(string)
	if cidB == defaultCID {
		t.Fatal("company B got the default company")
	}

	c, m = x.call("GET", "/api/state", nil, opt{token: tokB})
	x.must(c, 200, m, "B state")
	if len(items(m)) != 0 {
		t.Fatalf("company B can see company A's items: %v", items(m))
	}
	c, m = x.call("GET", "/api/changes?since=0", nil, opt{token: tokB})
	x.must(c, 200, m, "B changes")
	if strings.Contains(mustJSON(m), "A-1") {
		t.Fatalf("company A data leaked through /api/changes: %v", m)
	}

	pushItems(x, tokB, map[string]any{"sku": "B-1", "name": "Bird seed"})
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: tokA})
	x.must(c, 200, m, "A state")
	if len(items(m)) != 1 || items(m)[0].(map[string]any)["sku"] != "A-1" {
		t.Fatalf("company B's push reached company A: %v", items(m))
	}

	// users and admin actions stay inside the caller's company
	c, m = x.call("GET", "/api/users", nil, opt{token: tokB})
	x.must(c, 200, m, "B users")
	if n := len(m["users"].([]any)); n != 1 {
		t.Fatalf("B sees %d users, want 1", n)
	}
	acc.mu.Lock()
	aID := acc.findByName("boss@a.com").ID
	acc.mu.Unlock()
	c, m = x.call("POST", "/api/users/update", map[string]any{"id": aID, "disabled": true}, opt{token: tokB})
	x.must(c, 404, m, "B admin touching A's admin")

	// a company-B session never reaches the server's own printers without the pairing code
	c, m = x.call("GET", "/api/printers", nil, opt{token: tokB})
	x.must(c, 401, m, "B on printers")
	// emails are unique across the server
	c, m = x.call("POST", "/api/auth/create-company", map[string]any{"company": map[string]any{"name": "Dup"}, "email": "boss@a.com", "password": "password-c1"}, opt{})
	x.must(c, 409, m, "duplicate email")
}

func TestCreateCompanyOffUnlessHosted(t *testing.T) {
	x := newTest(t, false)
	x.setup()
	c, m := x.call("POST", "/api/auth/create-company", map[string]any{"company": map[string]any{"name": "X"}, "email": "x@x.com", "password": "password-x1"}, opt{code: true})
	x.must(c, 403, m, "create company on a non-hosted server")
}

func TestWarehouseNeverReceivesCosts(t *testing.T) {
	x := newTest(t, false)
	tokA, _ := x.setup()
	c, m := x.call("POST", "/api/push", map[string]any{"changes": map[string]any{
		"items":           map[string]any{"up": []any{map[string]any{"sku": "S1", "name": "Mop", "cost": 7.5, "avgCost": 7.1, "qty": 3}}},
		"expenses":        map[string]any{"up": []any{map[string]any{"id": "EXP-1", "amount": 99}}},
		"vendorInvoices":  map[string]any{"up": []any{map[string]any{"id": "VI-1", "total": 50}}},
		"carrierInvoices": map[string]any{"up": []any{map[string]any{"id": "CI-1", "total": 20}}},
	}}, opt{code: true, token: tokA})
	x.must(c, 200, m, "seed data")
	c, m = x.call("POST", "/api/users", map[string]any{"username": "floor@a.com", "email": "floor@a.com", "role": "warehouse", "password": "password-w1"}, opt{code: true, token: tokA})
	x.must(c, 200, m, "add warehouse user")
	c, m = x.login("floor@a.com", "password-w1", opt{code: true})
	x.must(c, 200, m, "warehouse sign-in")
	tokW := m["token"].(string)

	for _, p := range []string{"/api/state", "/api/changes?since=0", "/api/changes?since=1"} {
		c, m = x.call("GET", p, nil, opt{code: true, token: tokW})
		x.must(c, 200, m, p)
		js := mustJSON(m)
		for _, bad := range []string{`"cost"`, `"avgCost"`, "EXP-1", "VI-1", "CI-1"} {
			if strings.Contains(js, bad) {
				t.Fatalf("%s: warehouse received %s: %s", p, bad, js)
			}
		}
	}
	// a warehouse push can neither wipe the real cost nor touch finance collections
	c, m = x.call("POST", "/api/push", map[string]any{"changes": map[string]any{
		"items":    map[string]any{"up": []any{map[string]any{"sku": "S1", "name": "Mop", "qty": 2}}},
		"expenses": map[string]any{"del": []any{"EXP-1"}},
	}}, opt{code: true, token: tokW})
	x.must(c, 200, m, "warehouse push")
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: tokA})
	js := mustJSON(m)
	if !strings.Contains(js, `"cost":7.5`) || !strings.Contains(js, "EXP-1") || !strings.Contains(js, `"qty":2`) {
		t.Fatalf("warehouse push damaged data: %s", js)
	}
}

func TestRefreshRotatesAndCatchesReuse(t *testing.T) {
	x := newTest(t, false)
	_, m := x.setup()
	r1 := m["refresh"].(string)
	c, m := x.call("POST", "/api/auth/refresh", map[string]any{"refresh": r1}, opt{code: true})
	x.must(c, 200, m, "refresh")
	r2, tok2 := m["refresh"].(string), m["token"].(string)
	if r2 == r1 {
		t.Fatal("refresh token did not rotate")
	}
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: tok2})
	x.must(c, 200, m, "new access token works")
	// the old refresh token long after rotation = stolen copy: the whole session ends
	acc.mu.Lock()
	for _, s := range acc.sessions {
		s.Rotated = time.Now().Add(-time.Hour).UnixMilli()
	}
	acc.mu.Unlock()
	c, m = x.call("POST", "/api/auth/refresh", map[string]any{"refresh": r1}, opt{code: true})
	x.must(c, 401, m, "reused refresh")
	c, m = x.call("POST", "/api/auth/refresh", map[string]any{"refresh": r2}, opt{code: true})
	x.must(c, 401, m, "session revoked after reuse")
}

func TestSessionsSurviveRestart(t *testing.T) {
	x := newTest(t, false)
	_, m := x.setup()
	r1 := m["refresh"].(string)
	c, m := x.call("POST", "/api/auth/login", map[string]any{"username": "boss@a.com", "password": "password-a1"}, opt{code: true}) // old app: no v
	x.must(c, 200, m, "legacy login")
	legacy := m["token"].(string)
	if m["refresh"] != nil {
		t.Fatal("old app got a refresh token")
	}
	dir := acc.dir
	acc = newAccountStore() // "restart"
	acc.init(dir)
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: legacy})
	x.must(c, 200, m, "legacy token after restart")
	c, m = x.call("POST", "/api/auth/refresh", map[string]any{"refresh": r1}, opt{code: true})
	x.must(c, 200, m, "refresh after restart")
}

func TestLockoutAfterTenWrongPasswords(t *testing.T) {
	x := newTest(t, false)
	x.setup()
	for i := 0; i < acctFailLimit; i++ {
		c, m := x.login("boss@a.com", "nope-nope", opt{code: true})
		x.must(c, 401, m, "wrong password")
	}
	c, m := x.login("boss@a.com", "password-a1", opt{code: true})
	x.must(c, 429, m, "locked even with the right password")
}

func TestRecoveryCodeResetsAdminOnce(t *testing.T) {
	x := newTest(t, false)
	_, m := x.setup()
	code := m["recoveryCode"].(string)
	c, m := x.call("POST", "/api/auth/recover", map[string]any{"email": "boss@a.com", "code": strings.ToLower(code), "password": "brand-new-pw", "v": 2}, opt{code: true})
	x.must(c, 200, m, "recover")
	if m["recoveryCode"] == nil || m["recoveryCode"] == code {
		t.Fatal("no new recovery code after use")
	}
	c, m = x.login("boss@a.com", "brand-new-pw", opt{code: true})
	x.must(c, 200, m, "new password works")
	c, m = x.call("POST", "/api/auth/recover", map[string]any{"email": "boss@a.com", "code": code, "password": "another-pw-1"}, opt{code: true})
	x.must(c, 400, m, "old recovery code reused")
}

func TestOneAdminPerCompanyAndTransfer(t *testing.T) {
	x := newTest(t, false)
	tok, _ := x.setup()
	c, m := x.call("POST", "/api/users", map[string]any{"email": "two@a.com", "role": "admin", "password": "password-2a"}, opt{code: true, token: tok})
	x.must(c, 400, m, "second admin")
	c, m = x.call("POST", "/api/users", map[string]any{"email": "two@a.com", "role": "office", "password": "password-2a"}, opt{code: true, token: tok})
	x.must(c, 200, m, "office user")
	id := m["user"].(map[string]any)["id"].(string)
	c, m = x.call("POST", "/api/users/update", map[string]any{"id": id, "role": "admin"}, opt{code: true, token: tok})
	x.must(c, 400, m, "promote to admin")
	acc.mu.Lock()
	meID := acc.findByName("boss@a.com").ID
	acc.mu.Unlock()
	c, m = x.call("POST", "/api/users/update", map[string]any{"id": meID, "disabled": true}, opt{code: true, token: tok})
	x.must(c, 400, m, "admin disabling themselves")
	c, m = x.call("POST", "/api/users/transfer-admin", map[string]any{"id": id, "password": "password-a1"}, opt{code: true, token: tok})
	x.must(c, 200, m, "transfer admin")
	if m["user"].(map[string]any)["role"] != "office" || m["admin"].(map[string]any)["role"] != "admin" {
		t.Fatalf("roles after transfer: %v", m)
	}
}

func TestMustChangeBlocksData(t *testing.T) {
	x := newTest(t, false)
	tok, _ := x.setup()
	x.call("POST", "/api/users", map[string]any{"email": "new@a.com", "role": "warehouse", "password": "Temp-1234-xyz", "mustChange": true}, opt{code: true, token: tok})
	c, m := x.login("new@a.com", "Temp-1234-xyz", opt{code: true})
	x.must(c, 200, m, "temp sign-in")
	t2 := m["token"].(string)
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: t2})
	x.must(c, 403, m, "data before choosing a password")
	c, m = x.call("POST", "/api/auth/password", map[string]any{"old": "Temp-1234-xyz", "new": "my-own-pass"}, opt{code: true, token: t2})
	x.must(c, 200, m, "choose password")
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: t2})
	x.must(c, 200, m, "data after")
}

func TestResetSignsUserOut(t *testing.T) {
	x := newTest(t, false)
	tok, _ := x.setup()
	_, m := x.call("POST", "/api/users", map[string]any{"email": "o@a.com", "role": "office", "password": "password-o1"}, opt{code: true, token: tok})
	id := m["user"].(map[string]any)["id"].(string)
	_, m = x.login("o@a.com", "password-o1", opt{code: true})
	tokO := m["token"].(string)
	c, m := x.call("POST", "/api/users/update", map[string]any{"id": id, "password": "Temp-9999-abc", "mustChange": true}, opt{code: true, token: tok})
	x.must(c, 200, m, "admin reset")
	c, m = x.call("GET", "/api/state", nil, opt{code: true, token: tokO})
	x.must(c, 401, m, "old session after reset")
}

func TestOwnerToolsOnlyOnServerPC(t *testing.T) {
	x := newTest(t, false)
	x.setup()
	c, m := x.call("GET", "/api/owner/companies", nil, opt{code: true, hdr: map[string]string{"Cf-Connecting-Ip": "203.0.113.9"}})
	x.must(c, 403, m, "owner tools through the tunnel")
	c, m = x.call("GET", "/api/owner/companies", nil, opt{code: true})
	x.must(c, 200, m, "owner tools on the server PC")
	c, m = x.call("POST", "/api/owner/reset", map[string]any{"user": "boss@a.com"}, opt{code: true})
	x.must(c, 200, m, "owner reset")
	c, m = x.login("boss@a.com", m["tempPassword"].(string), opt{code: true})
	x.must(c, 200, m, "temp password works")
	if m["user"].(map[string]any)["mustChange"] != true {
		t.Fatal("owner reset did not force a password change")
	}
	// the dashboard data never shows the pairing code through the tunnel
	c, m = x.call("GET", "/status.json", nil, opt{hdr: map[string]string{"Cf-Connecting-Ip": "203.0.113.9"}})
	if c != 200 || m["code"] != nil {
		t.Fatalf("status.json through the tunnel exposed the code: %v", m)
	}
}

func TestUpgradeFromSingleCompanyFile(t *testing.T) {
	dir := t.TempDir()
	pbkdf2Iter = 1000
	old := `{"company":{"name":"Buckeye"},"users":[{"id":"u-1","username":"admin","name":"A","role":"admin","pass":"` + hashPassword("password-a1") + `","created":1,"updated":1}]}`
	_ = os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(old), 0o600)
	acc = newAccountStore()
	acc.init(dir)
	if len(acc.Companies) != 1 || !acc.Companies[0].Default || acc.Companies[0].Name != "Buckeye" || acc.Users[0].CompanyID != defaultCID {
		t.Fatalf("upgrade went wrong: %+v %+v", acc.Companies, acc.Users[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts.v1.json")); err != nil {
		t.Fatal("no backup of the old file")
	}
}

func TestOfflinePassIsSigned(t *testing.T) {
	x := newTest(t, false)
	_, m := x.setup()
	p := m["offlinePass"].(string)
	parts := strings.Split(p, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("pass format: %s", p)
	}
	raw, _ := b64u.DecodeString(parts[1])
	var pl map[string]any
	_ = json.Unmarshal(raw, &pl)
	exp := int64(pl["exp"].(float64))
	if d := time.Until(time.UnixMilli(exp)); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("offline pass expiry %v, want 7 days", d)
	}
	if m["pubkey"].(map[string]any)["crv"] != "P-256" {
		t.Fatal("no public key")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
