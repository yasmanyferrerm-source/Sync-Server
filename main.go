// Warehouse Sync Server — keeps the warehouse inventory data in one place on the warehouse PC
// so the desktop app and the Android app (on the same Wi-Fi) share it. Nothing leaves the building.
//
//   - listens on the local network (default port 8787)
//   - every device must enter the pairing code shown in this window
//   - data is saved next to this program in the "sync-data" folder (plus daily backups, 30 kept)
//
// Plain Go standard library only — no internet access, no other programs started.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "4.2.0"

//go:embed www
var wwwFS embed.FS

type config struct {
	Port int    `json:"port"`
	Code string `json:"code"`
	Name string `json:"name"`
}

type logEntry struct {
	Rev  int64
	Coll string // collection name, or "" for settings / top-level values
	Key  string
	Del  bool
}

type server struct {
	mu        sync.Mutex
	dir       string
	cfg       config
	db        map[string]any
	rev       int64
	logStart  int64 // oldest rev the change log can answer from
	clog      []logEntry
	idx       map[string]map[string]int // collection -> key -> position
	dirty     bool
	devices   map[string]time.Time
	usbPhones map[string]string // phones linked through the USB cable: serial -> model
}

// collections whose records get document numbers; colliding new numbers are renumbered
var counters = map[string]string{
	"tx": "nextTx", "shipments": "nextShip", "customers": "nextCust", "trips": "nextTrip", "receipts": "nextRcv",
	"vendors": "nextVendor", "counts": "nextCount", "samples": "nextSample", "expenses": "nextExp", "carriers": "nextCarrier",
}
var docRx = regexp.MustCompile(`^([A-Za-z]+-?)(\d+)$`)

func main() {
	exe, _ := os.Executable()
	base := filepath.Dir(exe)
	if len(os.Args) > 1 && os.Args[1] == "--dir" && len(os.Args) > 2 {
		base = os.Args[2]
	}
	s := &server{dir: filepath.Join(base, "sync-data"), devices: map[string]time.Time{}, usbPhones: map[string]string{}}
	if err := os.MkdirAll(filepath.Join(s.dir, "attachments"), 0o755); err != nil {
		fatal("Cannot create the data folder: " + err.Error())
	}
	s.loadConfig()
	s.loadDB()
	acc.init(s.dir)
	go s.saver()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", s.cors(s.ping))
	mux.HandleFunc("/api/state", s.cors(s.auth(s.userGate(s.state))))
	mux.HandleFunc("/api/check", s.cors(s.auth(s.userGate(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "rev": s.rev})
	}))))
	mux.HandleFunc("/api/changes", s.cors(s.auth(s.userGate(s.changes))))
	mux.HandleFunc("/api/push", s.cors(s.auth(s.userGate(s.push))))
	mux.HandleFunc("/api/file", s.cors(s.auth(s.userGate(s.file))))
	mux.HandleFunc("/api/app-version", s.cors(s.appVersion))
	// --- accounts (sign-in + roles). Opt-in: inert until an admin runs setup. ---
	mux.HandleFunc("/api/auth/status", s.cors(s.auth(s.authStatus)))
	mux.HandleFunc("/api/auth/setup", s.cors(s.auth(s.authSetup)))
	mux.HandleFunc("/api/auth/login", s.cors(s.auth(s.authLogin)))
	mux.HandleFunc("/api/auth/logout", s.cors(s.auth(s.authLogout)))
	mux.HandleFunc("/api/auth/me", s.cors(s.auth(s.authMe)))
	mux.HandleFunc("/api/auth/password", s.cors(s.auth(s.authPassword)))
	mux.HandleFunc("/api/users", s.cors(s.auth(adminOnly(s.usersRoute))))
	mux.HandleFunc("/api/users/update", s.cors(s.auth(adminOnly(s.usersUpdate))))
	mux.HandleFunc("/api/company", s.cors(s.auth(adminOnly(s.companyUpdate))))
	mux.HandleFunc("/app/", s.appFiles)
	mux.HandleFunc("/m/index.html", s.cors(func(w http.ResponseWriter, r *http.Request) { // the phone app (for silent updates)
		b, err := wwwFS.ReadFile("www/m/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}))
	mux.HandleFunc("/status", s.status)
	mux.HandleFunc("/status.json", s.statusJSON)
	mux.HandleFunc("/api/shutdown", s.cors(s.shutdown))
	mux.HandleFunc("/phone", s.phonePage)
	mux.HandleFunc("/api/scanners", s.cors(localOnly(s.auth(s.scanners))))
	mux.HandleFunc("/api/scan", s.cors(localOnly(s.auth(s.scan))))
	// Printing is allowed for any paired device, local or remote (via the tunnel),
	// so phones and the managed Work PC can print from another network. The pairing
	// code / sign-in is the security boundary, same as for syncing the data itself.
	mux.HandleFunc("/api/printers", s.cors(s.auth(s.printers)))
	mux.HandleFunc("/api/print", s.cors(s.auth(s.print)))
	mux.HandleFunc("/api/print-test", s.cors(s.printTest)) // loopback-only: triggered from the dashboard on the server PC
	sub, _ := fs.Sub(wwwFS, "www")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Most likely our own server is already running (easy to miss with no console
		// window). If so, just open its dashboard and exit quietly instead of alarming.
		if alreadyOurServer(s.cfg.Port) {
			openBrowser(fmt.Sprintf("http://localhost:%d/status", s.cfg.Port))
			os.Exit(0)
		}
		fatal(fmt.Sprintf("Port %d is busy — is the sync server already running?\n%v", s.cfg.Port, err))
	}
	banner(s)
	go s.usbWatch()
	// Open the live dashboard so the operator sees a real window, not the console.
	go func() { time.Sleep(500 * time.Millisecond); openBrowser(fmt.Sprintf("http://localhost:%d/status", s.cfg.Port)) }()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	log.Fatal(srv.Serve(ln))
}

func fatal(msg string) {
	// No console in the windowsgui build, so surface the error in a dialog and a log file.
	fmt.Println("\n  ERROR: " + msg)
	if wd, err := os.Getwd(); err == nil {
		_ = os.WriteFile(filepath.Join(wd, "sync-server-error.log"), []byte(time.Now().Format(time.RFC3339)+"  "+msg+"\n"), 0o644)
	}
	showError("Warehouse Sync Server", msg)
	os.Exit(1)
}

func banner(s *server) {
	fmt.Println()
	fmt.Println("  ==============================================================")
	fmt.Println("   WAREHOUSE SYNC SERVER  v" + version + "   —  keep this window open")
	fmt.Println("  ==============================================================")
	fmt.Println()
	fmt.Println("   Pairing code:   " + s.cfg.Code)
	fmt.Println()
	fmt.Println("   Address for phones / other PCs on this Wi-Fi:")
	for _, ip := range lanIPs() {
		fmt.Printf("       http://%s:%d\n", ip, s.cfg.Port)
	}
	fmt.Printf("   Address on this PC:   http://localhost:%d\n", s.cfg.Port)
	fmt.Printf("\n   Live dashboard (opens automatically):  http://localhost:%d/status\n", s.cfg.Port)
	if v, _, _ := mobileBuild(); v != "" {
		fmt.Println("   Phone app version " + v + " — phones update to it automatically when they connect")
		fmt.Println("   Install the phone app: open  <address above>/phone  in the phone's browser")
	}
	fmt.Println()
	fmt.Println("   Data folder: " + s.dir)
	fmt.Println("   If Windows asks, click  Allow access  (Private networks).")
	fmt.Println()
	s.mu.Lock()
	fmt.Printf("   %d items, %d movements, revision %d\n\n", arrLen(s.db["items"]), arrLen(s.db["tx"]), s.rev)
	s.mu.Unlock()
}

func lanIPs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ config & storage
func (s *server) loadConfig() {
	p := filepath.Join(s.dir, "sync-config.json")
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &s.cfg)
	}
	if s.cfg.Port == 0 {
		s.cfg.Port = 8787
	}
	if len(s.cfg.Code) < 6 {
		n, _ := rand.Int(rand.Reader, big.NewInt(900000))
		s.cfg.Code = strconv.FormatInt(n.Int64()+100000, 10)
	}
	if s.cfg.Name == "" {
		h, _ := os.Hostname()
		s.cfg.Name = h
	}
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	_ = os.WriteFile(p, b, 0o600)
}

type stored struct {
	Rev int64          `json:"rev"`
	DB  map[string]any `json:"db"`
}

func (s *server) loadDB() {
	for _, name := range []string{"sync-db.json", "sync-db.bak.json"} {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		var st stored
		if json.Unmarshal(b, &st) == nil && st.DB != nil {
			s.db, s.rev = st.DB, st.Rev
			break
		}
	}
	if s.db == nil {
		s.db = map[string]any{}
	}
	s.logStart = s.rev
	s.idx = map[string]map[string]int{}
}

// saves at most every half second; write to a temp file, then swap — a crash never leaves a half-written file
func (s *server) saver() {
	lastDay := ""
	for range time.Tick(500 * time.Millisecond) {
		s.mu.Lock()
		if !s.dirty {
			s.mu.Unlock()
			continue
		}
		b, err := json.Marshal(stored{Rev: s.rev, DB: s.db})
		s.dirty = false
		s.mu.Unlock()
		if err != nil {
			log.Println("save:", err)
			continue
		}
		f := filepath.Join(s.dir, "sync-db.json")
		_ = os.Rename(f, filepath.Join(s.dir, "sync-db.bak.json"))
		if err := os.WriteFile(f+".tmp", b, 0o600); err == nil {
			_ = os.Rename(f+".tmp", f)
		} else {
			log.Println("save:", err)
		}
		if day := time.Now().Format("2006-01-02"); day != lastDay {
			lastDay = day
			bk := filepath.Join(s.dir, "backups")
			_ = os.MkdirAll(bk, 0o755)
			_ = os.WriteFile(filepath.Join(bk, "sync-db-"+day+".json"), b, 0o600)
			if es, err := os.ReadDir(bk); err == nil && len(es) > 30 {
				names := []string{}
				for _, e := range es {
					names = append(names, e.Name())
				}
				sort.Strings(names)
				for _, n := range names[:len(names)-30] {
					_ = os.Remove(filepath.Join(bk, n))
				}
			}
		}
	}
}

// ------------------------------------------------------------------ helpers
func arrLen(v any) int {
	if a, ok := v.([]any); ok {
		return len(a)
	}
	return 0
}

func keyOf(coll string, r map[string]any) string {
	if coll == "items" {
		return strings.ToUpper(strings.TrimSpace(fmt.Sprint(r["sku"])))
	}
	if v, ok := r["id"]; ok && v != nil {
		switch t := v.(type) {
		case float64:
			return strconv.FormatInt(int64(t), 10)
		default:
			return fmt.Sprint(t)
		}
	}
	if v, ok := r["sku"]; ok {
		return strings.ToUpper(fmt.Sprint(v))
	}
	return fmt.Sprintf("%v|%v", r["ts"], r["ref"])
}

// a product barcode's match key: uppercase, no spaces, and (for all-digit codes) no leading zeros — mirrors the app's bkey
func bcNormKey(code string) string {
	s := strings.ToUpper(strings.Join(strings.Fields(code), ""))
	allDigit := s != ""
	for _, r := range s {
		if r < '0' || r > '9' {
			allDigit = false
			break
		}
	}
	if allDigit {
		if t := strings.TrimLeft(s, "0"); t != "" {
			return t
		}
		return "0"
	}
	return s
}
func bcEntryCode(e any) string {
	switch t := e.(type) {
	case string:
		return t
	case map[string]any:
		return fmt.Sprint(t["code"])
	}
	return ""
}

// Merge an incoming item's barcodes with the copy already on the server so that one device's edit of an item never
// drops barcodes another device added. Barcodes are unioned (incoming unit wins); a barcode removed on any device is
// kept out via the item's bcDel tombstone list, so removals still stick. `incoming` is updated in place.
func mergeItemBarcodes(existing, incoming map[string]any) {
	bcDelSet := func(m map[string]any) map[string]bool {
		s := map[string]bool{}
		if raw, ok := m["bcDel"].([]any); ok {
			for _, e := range raw {
				if k := bcNormKey(fmt.Sprint(e)); k != "" {
					s[k] = true
				}
			}
		}
		return s
	}
	// codes the incoming record explicitly lists — it asserts these are present now, which clears any old tombstone
	inPresent := map[string]bool{}
	if raw, ok := incoming["barcodes"].([]any); ok {
		for _, e := range raw {
			if k := bcNormKey(bcEntryCode(e)); k != "" {
				inPresent[k] = true
			}
		}
	}
	// final tombstones: whatever the incoming device removed, plus existing tombstones for codes the incoming
	// record is silent about (so a stale device re-sending a removed code can't resurrect it)
	tombs := bcDelSet(incoming)
	for k := range bcDelSet(existing) {
		if !inPresent[k] {
			tombs[k] = true
		}
	}
	out, used := []any{}, map[string]bool{}
	add := func(m map[string]any) {
		raw, _ := m["barcodes"].([]any)
		for _, e := range raw {
			k := bcNormKey(bcEntryCode(e))
			if k == "" || used[k] || tombs[k] {
				continue
			}
			used[k] = true
			out = append(out, e)
		}
	}
	add(incoming) // incoming first: its unit/spelling wins when both have the code
	add(existing)
	if len(out) > 0 {
		incoming["barcodes"] = out
	} else {
		delete(incoming, "barcodes")
	}
	if len(tombs) > 0 {
		td := make([]any, 0, len(tombs))
		for k := range tombs {
			td = append(td, k)
		}
		sort.Slice(td, func(i, j int) bool { return fmt.Sprint(td[i]) < fmt.Sprint(td[j]) })
		incoming["bcDel"] = td
	} else {
		delete(incoming, "bcDel")
	}
}

func (s *server) index(coll string) map[string]int {
	if m, ok := s.idx[coll]; ok {
		return m
	}
	m := map[string]int{}
	if a, ok := s.db[coll].([]any); ok {
		for i, v := range a {
			if r, ok := v.(map[string]any); ok {
				m[keyOf(coll, r)] = i
			}
		}
	}
	s.idx[coll] = m
	return m
}

func (s *server) num(name string) int64 {
	if f, ok := s.db[name].(float64); ok {
		return int64(f)
	}
	return 0
}

// highest number used in a collection (numeric ids or PREFIX-000123 document numbers)
func (s *server) maxNo(coll string) (int64, string, int) {
	var mx int64
	prefix, width := "", 0
	if a, ok := s.db[coll].([]any); ok {
		for _, v := range a {
			r, ok := v.(map[string]any)
			if !ok {
				continue
			}
			switch id := r["id"].(type) {
			case float64:
				if int64(id) > mx {
					mx = int64(id)
				}
			case string:
				if m := docRx.FindStringSubmatch(id); m != nil {
					n, _ := strconv.ParseInt(m[2], 10, 64)
					if n > mx {
						mx = n
					}
					prefix, width = m[1], len(m[2])
				}
			}
		}
	}
	return mx, prefix, width
}

// ------------------------------------------------------------------ HTTP plumbing
func (s *server) cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Sync-Code, X-Sync-Device, X-Sync-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.Header.Get("X-Sync-Code")
		if code == "" {
			code = r.URL.Query().Get("code")
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(s.cfg.Code)) != 1 {
			time.Sleep(400 * time.Millisecond) // slow down guessing
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Wrong pairing code"})
			return
		}
		if d := r.Header.Get("X-Sync-Device"); d != "" {
			s.mu.Lock()
			s.devices[d] = time.Now()
			s.mu.Unlock()
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	has := arrLen(s.db["items"]) > 0 || arrLen(s.db["tx"]) > 0
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"app": "warehouse-sync", "name": s.cfg.Name, "version": version, "hasData": has})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	role := roleFor(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"rev": s.rev, "db": filterDBForRole(s.db, role)})
}

// changes since a revision: only the records that changed (or the whole database if the client is too far behind)
func (s *server) changes(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	role := roleFor(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if since == s.rev {
		writeJSON(w, 200, map[string]any{"rev": s.rev})
		return
	}
	if since < s.logStart || since > s.rev {
		writeJSON(w, 200, map[string]any{"rev": s.rev, "full": true, "db": filterDBForRole(s.db, role)})
		return
	}
	ups, dels, top := map[string][]any{}, map[string][]string{}, map[string]any{}
	seen := map[string]bool{}
	for i := len(s.clog) - 1; i >= 0; i-- {
		e := s.clog[i]
		if e.Rev <= since {
			break
		}
		k := e.Coll + "\x00" + e.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		if e.Coll == "" {
			top[e.Key] = s.db[e.Key]
			continue
		}
		if e.Del {
			dels[e.Coll] = append(dels[e.Coll], e.Key)
			continue
		}
		if i, ok := s.index(e.Coll)[e.Key]; ok {
			ups[e.Coll] = append(ups[e.Coll], s.db[e.Coll].([]any)[i])
		}
	}
	next := map[string]any{}
	for k, v := range s.db {
		if strings.HasPrefix(k, "next") {
			next[k] = v
		}
	}
	ups, dels = filterUpsForRole(ups, dels, role)
	writeJSON(w, 200, map[string]any{"rev": s.rev, "ups": ups, "dels": dels, "top": top, "next": next})
}

type collChange struct {
	Up  []map[string]any `json:"up"`
	Del []string         `json:"del"`
	New []string         `json:"new"`
}
type pushReq struct {
	Dev      string                `json:"dev"`
	Seed     bool                  `json:"seed"`
	Changes  map[string]collChange `json:"changes"`
	Settings map[string]any        `json:"settings"`
	Top      map[string]any        `json:"top"`
	Next     map[string]float64    `json:"next"`
	DB       map[string]any        `json:"db"`
}

func (s *server) push(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	role := roleFor(r)
	var req pushReq
	dec := json.NewDecoder(io.LimitReader(r.Body, 200<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "Bad data: " + err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if role == "warehouse" {
		s.protectWarehousePush(&req)
	}
	// first device to connect to an empty server: its whole database becomes the shared data
	if req.Seed {
		if arrLen(s.db["items"]) > 0 || arrLen(s.db["tx"]) > 0 {
			writeJSON(w, 409, map[string]any{"error": "The warehouse already has data", "rev": s.rev})
			return
		}
		s.db = req.DB
		s.idx = map[string]map[string]int{}
		s.rev++
		s.logStart = s.rev
		s.clog = nil
		s.dirty = true
		writeJSON(w, 200, map[string]any{"rev": s.rev, "renumber": map[string]string{}})
		return
	}
	s.rev++
	rev := s.rev
	renumber := map[string]string{}

	// 1) document numbers created on two devices at the same time: give the newcomer the next free number
	for coll, ch := range req.Changes {
		cnt, numbered := counters[coll]
		if !numbered || len(ch.New) == 0 {
			continue
		}
		idx := s.index(coll)
		isNew := map[string]bool{}
		for _, k := range ch.New {
			isNew[k] = true
		}
		mx, prefix, width := s.maxNo(coll)
		for _, rec := range ch.Up {
			k := keyOf(coll, rec)
			if !isNew[k] {
				continue
			}
			pos, exists := idx[k]
			if !exists {
				continue
			}
			cur, _ := json.Marshal(s.db[coll].([]any)[pos])
			inc, _ := json.Marshal(rec)
			if string(cur) == string(inc) {
				continue // the same record sent twice (retry) — not a collision
			}
			if coll == "vendors" { // same vendor added on two devices: keep one
				if cr, ok := s.db[coll].([]any)[pos].(map[string]any); ok && strings.EqualFold(fmt.Sprint(cr["name"]), fmt.Sprint(rec["name"])) {
					continue
				}
			}
			for _, rr := range ch.Up { // also above anything else this device is adding now
				if m := docRx.FindStringSubmatch(fmt.Sprint(rr["id"])); m != nil {
					if n, _ := strconv.ParseInt(m[2], 10, 64); n > mx {
						mx = n
					}
				} else if f, ok := rr["id"].(float64); ok && int64(f) > mx {
					mx = int64(f)
				}
			}
			mx++
			var nk string
			if _, isNum := rec["id"].(float64); isNum {
				nk = strconv.FormatInt(mx, 10)
			} else {
				if m := docRx.FindStringSubmatch(k); m != nil {
					prefix, width = m[1], len(m[2])
				}
				nk = prefix + fmt.Sprintf("%0*d", width, mx)
			}
			renumber[k] = nk
			if f := s.num(cnt); f <= mx {
				s.db[cnt] = float64(mx + 1)
			}
		}
	}
	if len(renumber) > 0 { // rewrite every reference to a renumbered document inside this push
		b, _ := json.Marshal(req.Changes)
		str := string(b)
		for o, n := range renumber {
			if _, err := strconv.Atoi(o); err == nil {
				continue // numeric movement ids are only fixed on the record itself
			}
			str = strings.ReplaceAll(str, `"`+o+`"`, `"`+n+`"`)
		}
		var fixed map[string]collChange
		if json.Unmarshal([]byte(str), &fixed) == nil {
			for coll, ch := range fixed {
				for _, rec := range ch.Up {
					if _, isNum := rec["id"].(float64); isNum {
						if n, ok := renumber[keyOf(coll, rec)]; ok {
							nv, _ := strconv.ParseFloat(n, 64)
							rec["id"] = nv
						}
					} else if m := docRx.FindStringSubmatch(fmt.Sprint(rec["id"])); m != nil {
						if _, has := rec["no"]; has {
							n, _ := strconv.ParseFloat(m[2], 64)
							rec["no"] = n
						}
					}
				}
			}
			req.Changes = fixed
		}
	}
	// 2) apply deletes and updates record by record (last change wins per record)
	for coll, ch := range req.Changes {
		arr, _ := s.db[coll].([]any)
		idx := s.index(coll)
		if len(ch.Del) > 0 {
			del := map[string]bool{}
			for _, k := range ch.Del {
				if _, ok := idx[k]; ok {
					del[k] = true
					s.clog = append(s.clog, logEntry{rev, coll, k, true})
				}
			}
			if len(del) > 0 {
				kept := arr[:0]
				for _, v := range arr {
					if r, ok := v.(map[string]any); ok && del[keyOf(coll, r)] {
						continue
					}
					kept = append(kept, v)
				}
				arr = kept
				delete(s.idx, coll)
				idx = nil
			}
		}
		if idx == nil {
			s.db[coll] = arr
			idx = s.index(coll)
		}
		for _, rec := range ch.Up {
			k := keyOf(coll, rec)
			if pos, ok := idx[k]; ok {
				if coll == "items" { // keep barcodes added on other devices (and honour removals)
					if ex, ok2 := arr[pos].(map[string]any); ok2 {
						mergeItemBarcodes(ex, rec)
					}
				}
				arr[pos] = rec
			} else {
				if coll == "items" {
					mergeItemBarcodes(map[string]any{}, rec)
				}
				idx[k] = len(arr)
				arr = append(arr, rec)
			}
			s.clog = append(s.clog, logEntry{rev, coll, k, false})
		}
		s.db[coll] = arr
	}
	// 3) shared settings (per setting) and other top-level values
	if len(req.Settings) > 0 {
		st, _ := s.db["settings"].(map[string]any)
		if st == nil {
			st = map[string]any{}
		}
		for k, v := range req.Settings {
			st[k] = v
		}
		s.db["settings"] = st
		s.clog = append(s.clog, logEntry{rev, "", "settings", false})
	}
	for k, v := range req.Top {
		s.db[k] = v
		s.clog = append(s.clog, logEntry{rev, "", k, false})
	}
	// 4) counters only move forward
	for k, v := range req.Next {
		if strings.HasPrefix(k, "next") && int64(v) > s.num(k) {
			s.db[k] = v
		}
	}
	for coll, cnt := range counters {
		if mx, _, _ := s.maxNo(coll); s.num(cnt) <= mx {
			s.db[cnt] = float64(mx + 1)
		}
	}
	if len(s.clog) > 200000 { // keep memory bounded; older clients simply reload everything
		cut := len(s.clog) - 100000
		s.logStart = s.clog[cut-1].Rev
		s.clog = append([]logEntry(nil), s.clog[cut:]...)
	}
	s.dirty = true
	writeJSON(w, 200, map[string]any{"rev": rev, "renumber": renumber})
}

// ------------------------------------------------------------------ attachments (receipt paperwork, invoices)
func safeRel(p string) (string, error) {
	p = path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))[1:]
	if !strings.HasPrefix(p, "attachments/") || strings.Contains(p, "..") {
		return "", errors.New("bad path")
	}
	return p, nil
}

func (s *server) file(w http.ResponseWriter, r *http.Request) {
	rel, err := safeRel(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", 400)
		return
	}
	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		f, err := os.Create(full + ".tmp")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, err = io.Copy(f, io.LimitReader(r.Body, 60<<20))
		f.Close()
		if err != nil {
			os.Remove(full + ".tmp")
			http.Error(w, err.Error(), 500)
			return
		}
		_ = os.Rename(full+".tmp", full)
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodGet:
		http.ServeFile(w, r, full)
	default:
		http.Error(w, "method", 405)
	}
}

// ------------------------------------------------------------------ Android app updates over Wi-Fi: drop the new .apk + version.json into sync-data\app
// what the phone app can update to:
//
//	web: the phone app built into this server (installed silently by the phone when it connects)
//	version/file: an optional full Android install (.apk) in sync-data\app\ — needed only when the
//	              phone needs new Android features; any WarehouseInventory-X.Y.Z.apk there is found by its name
var mobileInfo = struct {
	once    sync.Once
	version string
	level   int
	size    int
}{}
var verRx = regexp.MustCompile(`APP_VERSION = '([0-9.]+)'`)
var lvlRx = regexp.MustCompile(`NATIVE_LEVEL = ([0-9]+)`)
var apkRx = regexp.MustCompile(`(?i)(\d+)\.(\d+)\.(\d+).*\.apk$`)

func mobileBuild() (string, int, int) {
	mobileInfo.once.Do(func() {
		b, err := wwwFS.ReadFile("www/m/index.html")
		if err != nil {
			return
		}
		if m := verRx.FindSubmatch(b); m != nil {
			mobileInfo.version = string(m[1])
		}
		if m := lvlRx.FindSubmatch(b); m != nil {
			mobileInfo.level, _ = strconv.Atoi(string(m[1]))
		}
		mobileInfo.size = len(b)
	})
	return mobileInfo.version, mobileInfo.level, mobileInfo.size
}
func verNum(a, b, c string) int {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	z, _ := strconv.Atoi(c)
	return x*1000000 + y*1000 + z
}

func (s *server) appVersion(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(s.dir, "app", "version.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	} else {
		best := -1
		for _, dir := range s.apkDirs() {
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if m := apkRx.FindStringSubmatch(e.Name()); m != nil && !e.IsDir() {
					if n := verNum(m[1], m[2], m[3]); n > best {
						best = n
						out["version"] = m[1] + "." + m[2] + "." + m[3]
						out["file"] = e.Name()
					}
				}
			}
		}
	}
	if v, lvl, size := mobileBuild(); v != "" {
		out["web"] = map[string]any{"version": v, "url": "/m/index.html", "level": lvl, "size": size}
	}
	writeJSON(w, 200, out)
}

func (s *server) appFiles(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if !strings.HasSuffix(strings.ToLower(name), ".apk") {
		http.NotFound(w, r)
		return
	}
	for _, dir := range s.apkDirs() {
		f := filepath.Join(dir, name)
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			http.ServeFile(w, r, f)
			return
		}
	}
	http.NotFound(w, r)
}

// where Android installs (.apk) are looked for: sync-data\app\ and the "phone-app" folder next to this program
func (s *server) apkDirs() []string {
	return []string{filepath.Join(s.dir, "app"), filepath.Join(filepath.Dir(s.dir), "phone-app")}
}

// a simple page to download the phone app from the phone's browser: http://<this PC>:8787/phone
func (s *server) phonePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font-family:Segoe UI,Roboto,Arial;padding:24px;font-size:17px"><h2>Warehouse Inventory for Android</h2>`)
	found := false
	for _, dir := range s.apkDirs() {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasSuffix(strings.ToLower(e.Name()), ".apk") {
				found = true
				fmt.Fprintf(w, `<p><a style="display:inline-block;background:#1F5FBF;color:#fff;padding:14px 20px;border-radius:10px;text-decoration:none" href="/app/%s">Download %s</a></p>`, htmlEsc(e.Name()), htmlEsc(e.Name()))
			}
		}
	}
	if !found {
		fmt.Fprint(w, `<p>No phone app file on this PC yet. Put the .apk in the "phone-app" folder next to the sync server.</p>`)
	} else {
		fmt.Fprint(w, `<p style="color:#555">After the download, open the file and tap <b>Install</b>. If Android asks, allow installing apps from this browser.</p>`)
	}
	fmt.Fprint(w, `</body></html>`)
}

// status and statusJSON are defined in dashboard.go.

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
