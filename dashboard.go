package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"time"
)

// alreadyOurServer reports whether a Warehouse Sync Server is already listening on
// the port (so a second launch can just open the dashboard instead of erroring).
func alreadyOurServer(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/status.json", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return false
	}
	_, ok := m["version"]
	return ok
}

// shutdown lets the operator stop the server from the dashboard (loopback only,
// so only someone physically on the server PC can trigger it — never a LAN device).
// Needed because the windowsgui build has no console window to close.
func (s *server) shutdown(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "The server can only be stopped from the PC it runs on."})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	// Give the background saver (runs every 500ms) time to flush any pending write.
	go func() { time.Sleep(800 * time.Millisecond); os.Exit(0) }()
}

// statusJSON is the live data the dashboard polls every few seconds.
func (s *server) statusJSON(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	s.mu.Lock()
	type dev struct {
		Name   string `json:"name"`
		Last   int64  `json:"last"`
		Online bool   `json:"online"`
	}
	devs := make([]dev, 0, len(s.devices))
	for name, t := range s.devices {
		devs = append(devs, dev{name, t.UnixMilli(), now.Sub(t) < 90*time.Second})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Last > devs[j].Last })

	phones := make([]map[string]string, 0, len(s.usbPhones))
	for serial, model := range s.usbPhones {
		phones = append(phones, map[string]string{"serial": serial, "model": model})
	}
	items, mov, rev := arrLen(s.db["items"]), arrLen(s.db["tx"]), s.rev
	s.mu.Unlock()

	pv, _, _ := mobileBuild()

	acc.mu.Lock()
	type usr struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		Disabled bool   `json:"disabled"`
	}
	users := make([]usr, 0, len(acc.Users))
	admins := 0
	for _, u := range acc.Users {
		users = append(users, usr{u.Name, u.Role, u.Disabled})
		if u.Role == "admin" && !u.Disabled {
			admins++
		}
	}
	configured := len(acc.Users) > 0
	company := acc.Company
	acc.mu.Unlock()

	writeJSON(w, 200, map[string]any{
		"name":         s.cfg.Name,
		"version":      version,
		"port":         s.cfg.Port,
		"code":         s.cfg.Code,
		"phoneVersion": pv,
		"dataDir":      s.dir,
		"ips":          lanIPs(),
		"nowMs":        now.UnixMilli(),
		"counts":       map[string]any{"items": items, "movements": mov, "revision": rev},
		"signin":       map[string]any{"configured": configured, "company": company, "admins": admins, "users": users},
		"devices":      devs,
		"usb":          phones,
		"printers":     cachedPrinters(),
	})
}

// status serves the dashboard shell (a single self-contained page, no external
// resources so it works on an offline LAN). It fills itself from /status.json.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

// openBrowser best-effort opens the local dashboard so the operator sees a real
// window instead of the console. Any failure is ignored; the server keeps running
// and the console still prints the URL.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 avoids spawning a cmd shell; it just hands the URL to the default browser.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	hideWindow(cmd)
	_ = cmd.Start()
}

const dashboardHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Warehouse Sync Server</title>
<style>
  :root{
    --bg:#0d0912; --card:rgba(22,12,20,.34); --card2:rgba(12,7,12,.40);
    --brd:rgba(240,200,130,.20); --ink:#fff7ea; --muted:#e2ccab; --faint:#bfa888;
    --gold:#f0c987; --gold2:#b8873c; --accent:#f0c987; --ok:#74e6a8; --okdim:rgba(116,230,168,.16);
    --warn:#ffb648; --r:18px;
    --serif:"Cinzel","Trajan Pro 3","Trajan Pro","Palatino Linotype","Book Antiqua",Palatino,Georgia,serif;
  }
  *{box-sizing:border-box}
  html,body{margin:0}
  body{
    font-family:"Segoe UI Variable Text","Segoe UI",system-ui,-apple-system,Roboto,Arial,sans-serif;
    color:var(--ink); background:var(--bg); min-height:100vh; padding:34px 22px 56px;
    text-shadow:0 1px 3px rgba(10,5,10,.7);
  }
  /* Rome at dusk sits furthest back; a soft veil on top keeps every card readable */
  .scene{position:fixed; inset:0; z-index:-2; overflow:hidden; background:var(--bg)}
  .scene svg{position:absolute; inset:0; width:100%; height:100%}
  body::before{
    content:""; position:fixed; inset:0; z-index:-1; pointer-events:none;
    background:
      radial-gradient(90vw 70vh at 50% 42%, rgba(12,7,14,.16), rgba(12,7,14,.04) 70%),
      linear-gradient(180deg, rgba(10,6,16,.70) 0%, rgba(10,6,16,.12) 26%, rgba(10,6,16,.10) 62%, rgba(8,5,9,.78) 100%);
  }
  .wrap{max-width:1080px;margin:0 auto}
  header{display:flex;align-items:center;gap:16px;flex-wrap:wrap;margin-bottom:10px}
  .logo{width:58px;height:58px;flex:none;filter:drop-shadow(0 6px 18px rgba(240,190,110,.35))}
  h1{font-family:var(--serif);font-size:25px;margin:0;font-weight:600;letter-spacing:.14em;text-transform:uppercase;
    background:linear-gradient(180deg,#fff3d6,#f0c987 55%,#c8964a);-webkit-background-clip:text;background-clip:text;color:transparent;text-shadow:none;
    filter:drop-shadow(0 2px 6px rgba(10,5,10,.6))}
  .sub{color:var(--muted);font-size:13px;margin-top:3px;letter-spacing:.02em}
  .live{margin-left:auto;display:flex;align-items:center;gap:9px;font-size:12px;font-weight:700;letter-spacing:.14em;text-transform:uppercase;
    color:var(--ink);background:var(--card);border:1px solid var(--brd);border-radius:999px;padding:8px 15px;
    backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
  .dot{width:9px;height:9px;flex:none;border-radius:50%;background:var(--ok);box-shadow:0 0 0 0 rgba(116,230,168,.6);animation:pulse 2s infinite}
  .dot.off{background:var(--warn);animation:none}
  @keyframes pulse{0%{box-shadow:0 0 0 0 rgba(116,230,168,.55)}70%{box-shadow:0 0 0 10px rgba(116,230,168,0)}100%{box-shadow:0 0 0 0 rgba(116,230,168,0)}}
  .rule{display:flex;align-items:center;gap:14px;margin:14px 0 22px;color:var(--gold);font-family:var(--serif);font-size:11px;letter-spacing:.42em;opacity:.9}
  .rule::before,.rule::after{content:"";flex:1;height:1px;background:linear-gradient(90deg,transparent,rgba(240,201,135,.65))}
  .rule::after{background:linear-gradient(270deg,transparent,rgba(240,201,135,.65))}
  .grid{display:grid;gap:16px}
  .grid>*{min-width:0}
  .tiles{grid-template-columns:repeat(4,minmax(0,1fr))}
  .cards{grid-template-columns:1.3fr 1fr;margin-top:16px}
  @media(max-width:760px){.tiles{grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.cards{grid-template-columns:1fr}h1{font-size:17px;letter-spacing:.1em}.live{margin-left:0}
    body{padding:22px 14px 40px}.logo{width:46px;height:46px}.code{font-size:24px;letter-spacing:4px;padding:10px 14px 10px 18px}}
  @media(max-width:520px){.tile{padding:13px 12px;gap:10px}.tile .ic{width:34px;height:34px;border-radius:10px}.tile .ic svg{width:18px;height:18px}.tile .v{font-size:18px}.tile .k{font-size:9.5px;letter-spacing:.1em}header>div:not(.live){min-width:0;flex:1 1 calc(100% - 70px)}}
  .card{position:relative;background:linear-gradient(180deg,rgba(40,22,30,.30),var(--card));border:1px solid var(--brd);border-radius:var(--r);padding:20px 20px;
    backdrop-filter:blur(5px) saturate(1.15) brightness(.82);-webkit-backdrop-filter:blur(5px) saturate(1.15) brightness(.82);
    box-shadow:0 22px 50px rgba(6,3,8,.38), inset 0 1px 0 rgba(255,236,200,.14)}
  .card::before{content:"";position:absolute;left:18px;right:18px;top:-1px;height:1px;background:linear-gradient(90deg,transparent,rgba(255,226,170,.75),transparent)}
  .tile{display:flex;align-items:center;gap:14px;padding:18px 18px}
  .tile .ic{width:44px;height:44px;flex:none;border-radius:13px;display:grid;place-items:center;color:var(--gold);
    background:linear-gradient(145deg,rgba(240,201,135,.24),rgba(240,201,135,.06));border:1px solid rgba(240,201,135,.28)}
  .tile .ic svg{width:22px;height:22px;fill:none;stroke:currentColor;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}
  .tile .k{color:var(--muted);font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.14em}
  .tile .v{font-size:30px;font-weight:700;margin-top:3px;font-variant-numeric:tabular-nums;line-height:1.1}
  .tile .v small{font-size:13px;color:var(--faint);font-weight:500}
  .card h2{font-family:var(--serif);font-size:13px;font-weight:600;text-transform:uppercase;letter-spacing:.2em;color:var(--gold);margin:0 0 14px}
  .code{font-family:"Cascadia Code",Consolas,monospace;font-size:30px;font-weight:700;letter-spacing:6px;
    color:#fff3d6;background:var(--card2);border:1px solid rgba(240,201,135,.38);border-radius:14px;padding:12px 18px 12px 24px;display:inline-block;
    box-shadow:inset 0 2px 10px rgba(0,0,0,.35)}
  .addr{font-family:Consolas,monospace;font-size:13.5px;color:#ffe9c4;background:var(--card2);
    border:1px solid var(--brd);border-radius:9px;padding:7px 11px;margin:5px 6px 0 0;display:inline-block}
  .row{display:flex;align-items:center;gap:10px;padding:11px 0;border-top:1px solid rgba(240,200,130,.14)}
  .row:first-of-type{border-top:0}
  .name{font-weight:600}
  .seen{margin-left:auto;color:var(--muted);font-size:13px;font-variant-numeric:tabular-nums}
  .pill{font-size:10.5px;font-weight:800;text-transform:uppercase;letter-spacing:.1em;padding:4px 10px;border-radius:999px;text-shadow:none}
  .pill.on{background:var(--okdim);color:var(--ok);border:1px solid rgba(116,230,168,.3)}
  .pill.idle{background:rgba(255,182,72,.14);color:var(--warn);border:1px solid rgba(255,182,72,.28)}
  .pill.role{background:rgba(240,201,135,.16);color:var(--gold);border:1px solid rgba(240,201,135,.3)}
  .empty{color:var(--faint);font-size:14px;padding:8px 0}
  .signin .big{font-size:16px;font-weight:700;margin-bottom:8px}
  .foot{color:var(--muted);font-size:12px;margin-top:20px;line-height:1.7;text-align:center}
  .foot b{color:var(--ink);font-weight:600}
  .off-banner{display:none;background:rgba(60,34,10,.7);border:1px solid rgba(255,182,72,.45);
    color:#ffe0a6;border-radius:12px;padding:10px 14px;font-size:13px;margin-bottom:16px;backdrop-filter:blur(10px)}
  #stopBtn{background:rgba(150,36,34,.42);color:#ffd2cc;border:1px solid rgba(255,140,128,.45);
    border-radius:999px;padding:9px 16px;font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;cursor:pointer;
    backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);transition:background .15s}
  #stopBtn:hover{background:rgba(176,44,40,.62)}
  .tbtn{margin-left:auto;background:rgba(240,201,135,.12);color:var(--gold);border:1px solid rgba(240,201,135,.35);border-radius:999px;
    padding:6px 14px;font-size:12px;font-weight:700;cursor:pointer;transition:background .15s}
  .tbtn:hover{background:rgba(240,201,135,.26)}
  .tbtn:disabled{opacity:.6;cursor:default}
  button:focus-visible{outline:2px solid var(--gold);outline-offset:2px}
  .stopped{position:fixed;inset:0;background:rgba(10,6,12,.88);display:grid;place-items:center;z-index:99;
    color:var(--muted);font-size:17px;text-align:center;padding:24px}
  @media (prefers-reduced-motion:reduce){.dot{animation:none}}
</style></head>
<body>
<div class="scene" aria-hidden="true">
<svg viewBox="0 0 1600 900" preserveAspectRatio="xMidYMid slice" xmlns="http://www.w3.org/2000/svg">
<defs>
<linearGradient id="sky" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#0d1230"/><stop offset=".22" stop-color="#27214f"/><stop offset=".42" stop-color="#6a3760"/>
 <stop offset=".56" stop-color="#c25f4c"/><stop offset=".66" stop-color="#f09a52"/><stop offset=".73" stop-color="#ffd08a"/><stop offset="1" stop-color="#ffd08a"/></linearGradient>
<radialGradient id="sun" cx="420" cy="628" r="520" gradientUnits="userSpaceOnUse">
 <stop offset="0" stop-color="#fff6d8"/><stop offset=".05" stop-color="#ffe9a8" stop-opacity=".95"/><stop offset=".18" stop-color="#ffb65c" stop-opacity=".55"/>
 <stop offset=".5" stop-color="#ff8a4a" stop-opacity=".16"/><stop offset="1" stop-color="#ff8a4a" stop-opacity="0"/></radialGradient>
<linearGradient id="stone" x1="0" y1="0" x2="1" y2="0">
 <stop offset="0" stop-color="#f3c184"/><stop offset=".22" stop-color="#d89c62"/><stop offset=".55" stop-color="#9a6447"/><stop offset=".85" stop-color="#5a3a36"/><stop offset="1" stop-color="#3d282c"/></linearGradient>
<linearGradient id="stoneIn" x1="0" y1="0" x2="1" y2="0">
 <stop offset="0" stop-color="#8a5a45"/><stop offset="1" stop-color="#2e1e25"/></linearGradient>
<linearGradient id="vshade" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#fff" stop-opacity=".10"/><stop offset=".6" stop-color="#000" stop-opacity="0"/><stop offset="1" stop-color="#1a0c12" stop-opacity=".55"/></linearGradient>
<linearGradient id="arch" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#120a10"/><stop offset=".55" stop-color="#2a1414"/><stop offset="1" stop-color="#a8521f"/></linearGradient>
<linearGradient id="archUp" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#150c13"/><stop offset="1" stop-color="#3a1f1e"/></linearGradient>
<linearGradient id="archSky" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#b9584c"/><stop offset="1" stop-color="#f3a25a"/></linearGradient>
<linearGradient id="colL" x1="0" y1="0" x2="1" y2="0">
 <stop offset="0" stop-color="#ffcf8f"/><stop offset=".07" stop-color="#c98a5a"/><stop offset=".3" stop-color="#6b4338"/><stop offset=".7" stop-color="#2c1b22"/><stop offset="1" stop-color="#170e15"/></linearGradient>
<linearGradient id="col" x1="1" y1="0" x2="0" y2="0">
 <stop offset="0" stop-color="#ffcf8f"/><stop offset=".07" stop-color="#c98a5a"/><stop offset=".3" stop-color="#6b4338"/><stop offset=".7" stop-color="#2c1b22"/><stop offset="1" stop-color="#170e15"/></linearGradient>
<linearGradient id="ground" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#6b4136"/><stop offset=".25" stop-color="#3a2328"/><stop offset="1" stop-color="#0c070b"/></linearGradient>
<linearGradient id="mist" x1="0" y1="0" x2="0" y2="1">
 <stop offset="0" stop-color="#ffd9a0" stop-opacity="0"/><stop offset=".5" stop-color="#ffc98c" stop-opacity=".42"/><stop offset="1" stop-color="#ffc98c" stop-opacity="0"/></linearGradient>
<linearGradient id="hill1" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#c0705c"/><stop offset="1" stop-color="#e09a68"/></linearGradient>
<linearGradient id="hill2" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#8a4a50"/><stop offset="1" stop-color="#b56a55"/></linearGradient>
<filter id="blur8" x="-20%" y="-50%" width="140%" height="200%"><feGaussianBlur stdDeviation="8"/></filter>
<filter id="blur2"><feGaussianBlur stdDeviation="1.2"/></filter>
<filter id="cloud" x="-20%" y="-60%" width="140%" height="220%">
 <feTurbulence type="fractalNoise" baseFrequency=".008 .022" numOctaves="4" seed="4"/><feDisplacementMap in="SourceGraphic" scale="70"/><feGaussianBlur stdDeviation="7"/></filter>
<filter id="rock" x="0" y="0" width="100%" height="100%">
 <feTurbulence type="fractalNoise" baseFrequency=".035 .11" numOctaves="4" seed="11" result="n"/>
 <feColorMatrix in="n" type="matrix" values="0 0 0 0 0  0 0 0 0 0  0 0 0 0 0  .9 .6 0 0 -.35" result="a"/>
 <feFlood flood-color="#2a1418"/><feComposite in2="a" operator="in" result="dark"/>
 <feMerge><feMergeNode in="SourceGraphic"/><feMergeNode in="dark"/></feMerge><feComposite in2="SourceGraphic" operator="in"/></filter>
<filter id="grain" x="0" y="0" width="100%" height="100%">
 <feTurbulence type="fractalNoise" baseFrequency=".9" numOctaves="2" seed="3"/><feColorMatrix type="matrix" values="0 0 0 0 1  0 0 0 0 .95  0 0 0 0 .85  0 0 0 .09 0"/></filter>
</defs>
<rect width="1600" height="900" fill="url(#sky)"/>
<circle cx="518" cy="39" r="1.0" fill="#fff" opacity="0.17"/>
<circle cx="857" cy="95" r="0.5" fill="#fff" opacity="0.29"/>
<circle cx="60" cy="113" r="0.5" fill="#fff" opacity="0.12"/>
<circle cx="679" cy="215" r="0.5" fill="#fff" opacity="0.08"/>
<circle cx="1004" cy="246" r="0.9" fill="#fff" opacity="0.07"/>
<circle cx="1562" cy="12" r="1.2" fill="#fff" opacity="0.30"/>
<circle cx="231" cy="31" r="0.7" fill="#fff" opacity="0.54"/>
<circle cx="289" cy="151" r="1.0" fill="#fff" opacity="0.18"/>
<circle cx="876" cy="16" r="0.5" fill="#fff" opacity="0.25"/>
<circle cx="1089" cy="111" r="0.7" fill="#fff" opacity="0.30"/>
<circle cx="725" cy="78" r="1.1" fill="#fff" opacity="0.40"/>
<circle cx="391" cy="149" r="0.9" fill="#fff" opacity="0.32"/>
<circle cx="1167" cy="75" r="1.3" fill="#fff" opacity="0.16"/>
<circle cx="669" cy="197" r="0.5" fill="#fff" opacity="0.14"/>
<circle cx="63" cy="174" r="1.1" fill="#fff" opacity="0.20"/>
<circle cx="1401" cy="82" r="1.0" fill="#fff" opacity="0.35"/>
<circle cx="928" cy="119" r="1.2" fill="#fff" opacity="0.40"/>
<circle cx="759" cy="173" r="0.5" fill="#fff" opacity="0.23"/>
<circle cx="1035" cy="258" r="1.1" fill="#fff" opacity="0.04"/>
<circle cx="617" cy="174" r="0.4" fill="#fff" opacity="0.17"/>
<circle cx="269" cy="30" r="0.5" fill="#fff" opacity="0.51"/>
<circle cx="207" cy="64" r="0.8" fill="#fff" opacity="0.49"/>
<circle cx="129" cy="117" r="0.9" fill="#fff" opacity="0.39"/>
<circle cx="1311" cy="225" r="0.7" fill="#fff" opacity="0.10"/>
<circle cx="574" cy="230" r="1.3" fill="#fff" opacity="0.05"/>
<circle cx="282" cy="60" r="0.6" fill="#fff" opacity="0.33"/>
<circle cx="943" cy="68" r="0.4" fill="#fff" opacity="0.29"/>
<circle cx="591" cy="147" r="1.3" fill="#fff" opacity="0.27"/>
<circle cx="825" cy="161" r="1.0" fill="#fff" opacity="0.08"/>
<circle cx="1439" cy="203" r="1.2" fill="#fff" opacity="0.19"/>
<circle cx="628" cy="104" r="0.5" fill="#fff" opacity="0.33"/>
<circle cx="100" cy="18" r="0.6" fill="#fff" opacity="0.23"/>
<circle cx="544" cy="14" r="0.4" fill="#fff" opacity="0.22"/>
<circle cx="162" cy="95" r="0.4" fill="#fff" opacity="0.43"/>
<circle cx="983" cy="39" r="0.6" fill="#fff" opacity="0.30"/>
<circle cx="583" cy="32" r="1.2" fill="#fff" opacity="0.62"/>
<circle cx="746" cy="126" r="0.5" fill="#fff" opacity="0.12"/>
<circle cx="548" cy="69" r="1.1" fill="#fff" opacity="0.18"/>
<circle cx="37" cy="247" r="0.9" fill="#fff" opacity="0.04"/>
<circle cx="869" cy="7" r="0.9" fill="#fff" opacity="0.67"/>
<circle cx="1381" cy="181" r="0.6" fill="#fff" opacity="0.14"/>
<circle cx="267" cy="201" r="0.9" fill="#fff" opacity="0.19"/>
<circle cx="527" cy="58" r="1.1" fill="#fff" opacity="0.56"/>
<circle cx="1364" cy="210" r="1.1" fill="#fff" opacity="0.17"/>
<circle cx="363" cy="135" r="0.7" fill="#fff" opacity="0.09"/>
<circle cx="45" cy="73" r="0.6" fill="#fff" opacity="0.40"/>
<circle cx="1530" cy="116" r="1.2" fill="#fff" opacity="0.42"/>
<circle cx="1528" cy="95" r="0.6" fill="#fff" opacity="0.19"/>
<circle cx="315" cy="53" r="1.0" fill="#fff" opacity="0.53"/>
<circle cx="1345" cy="125" r="1.0" fill="#fff" opacity="0.34"/>
<circle cx="136" cy="172" r="1.2" fill="#fff" opacity="0.25"/>
<circle cx="1200" cy="124" r="0.6" fill="#fff" opacity="0.34"/>
<circle cx="532" cy="208" r="1.3" fill="#fff" opacity="0.11"/>
<circle cx="642" cy="246" r="1.1" fill="#fff" opacity="0.04"/>
<circle cx="203" cy="39" r="1.2" fill="#fff" opacity="0.52"/>
<circle cx="234" cy="215" r="1.3" fill="#fff" opacity="0.15"/>
<circle cx="561" cy="143" r="0.5" fill="#fff" opacity="0.08"/>
<circle cx="1553" cy="169" r="0.9" fill="#fff" opacity="0.29"/>
<circle cx="694" cy="227" r="1.1" fill="#fff" opacity="0.07"/>
<circle cx="403" cy="76" r="0.6" fill="#fff" opacity="0.35"/>
<circle cx="415" cy="109" r="0.5" fill="#fff" opacity="0.41"/>
<circle cx="566" cy="119" r="0.9" fill="#fff" opacity="0.39"/>
<circle cx="673" cy="239" r="0.9" fill="#fff" opacity="0.09"/>
<circle cx="838" cy="5" r="0.8" fill="#fff" opacity="0.25"/>
<circle cx="6" cy="208" r="0.6" fill="#fff" opacity="0.13"/>
<circle cx="1160" cy="145" r="0.7" fill="#fff" opacity="0.23"/>
<circle cx="889" cy="204" r="0.5" fill="#fff" opacity="0.15"/>
<circle cx="398" cy="72" r="1.1" fill="#fff" opacity="0.33"/>
<circle cx="899" cy="198" r="1.2" fill="#fff" opacity="0.13"/>
<circle cx="980" cy="131" r="0.9" fill="#fff" opacity="0.30"/>
<circle cx="724" cy="139" r="0.8" fill="#fff" opacity="0.36"/>
<circle cx="1119" cy="228" r="1.2" fill="#fff" opacity="0.07"/>
<circle cx="895" cy="245" r="1.2" fill="#fff" opacity="0.04"/>
<circle cx="195" cy="115" r="0.5" fill="#fff" opacity="0.17"/>
<circle cx="117" cy="174" r="1.1" fill="#fff" opacity="0.27"/>
<circle cx="247" cy="186" r="1.0" fill="#fff" opacity="0.09"/>
<circle cx="1413" cy="252" r="0.6" fill="#fff" opacity="0.11"/>
<circle cx="637" cy="127" r="1.3" fill="#fff" opacity="0.35"/>
<circle cx="258" cy="112" r="0.9" fill="#fff" opacity="0.21"/>
<circle cx="313" cy="83" r="1.0" fill="#fff" opacity="0.12"/>
<circle cx="886" cy="115" r="0.4" fill="#fff" opacity="0.21"/>
<circle cx="998" cy="133" r="0.5" fill="#fff" opacity="0.38"/>
<circle cx="1261" cy="253" r="0.5" fill="#fff" opacity="0.05"/>
<circle cx="63" cy="203" r="0.6" fill="#fff" opacity="0.07"/>
<circle cx="676" cy="237" r="1.1" fill="#fff" opacity="0.06"/>
<circle cx="239" cy="239" r="0.9" fill="#fff" opacity="0.11"/>
<circle cx="143" cy="15" r="1.0" fill="#fff" opacity="0.36"/>
<circle cx="116" cy="244" r="1.0" fill="#fff" opacity="0.11"/>
<circle cx="134" cy="223" r="0.5" fill="#fff" opacity="0.16"/>
<circle cx="726" cy="88" r="0.9" fill="#fff" opacity="0.47"/>
<rect width="1600" height="900" fill="url(#sun)"/>
<circle cx="420" cy="628" r="34" fill="#fff3cf"/>
<g filter="url(#cloud)">
<ellipse cx="300" cy="300" rx="330" ry="26" fill="#e8806a" opacity="0.55"/>
<ellipse cx="900" cy="240" rx="420" ry="22" fill="#b7577a" opacity="0.5"/>
<ellipse cx="1300" cy="330" rx="300" ry="20" fill="#f09a6a" opacity="0.5"/>
<ellipse cx="640" cy="400" rx="360" ry="18" fill="#ffb37a" opacity="0.55"/>
<ellipse cx="1150" cy="455" rx="380" ry="14" fill="#ffc48a" opacity="0.5"/>
<ellipse cx="150" cy="470" rx="260" ry="14" fill="#ffd09a" opacity="0.55"/>
<ellipse cx="1450" cy="200" rx="260" ry="18" fill="#7d4a86" opacity="0.45"/>
</g>
<path d="M691,330 q4.5,-6.0 9.0,0 q4.5,-6.0 9.0,0" fill="none" stroke="#2a1a2a" stroke-width="1.6" opacity=".7"/>
<path d="M719,318 q3.6,-4.8 7.2,0 q3.6,-4.8 7.2,0" fill="none" stroke="#2a1a2a" stroke-width="1.3" opacity=".7"/>
<path d="M746,338 q3.1,-4.2 6.3,0 q3.1,-4.2 6.3,0" fill="none" stroke="#2a1a2a" stroke-width="1.1" opacity=".7"/>
<path d="M1172,270 q4.0,-5.4 8.1,0 q4.0,-5.4 8.1,0" fill="none" stroke="#2a1a2a" stroke-width="1.4" opacity=".7"/>
<path d="M1200,282 q2.7,-3.6 5.4,0 q2.7,-3.6 5.4,0" fill="none" stroke="#2a1a2a" stroke-width="1.0" opacity=".7"/>
<path d="M0,640 C150,600 260,612 380,622 C520,596 640,580 800,604 C960,570 1100,590 1260,575 C1400,560 1500,590 1600,580 L1600,720 L0,720Z" fill="url(#hill1)" opacity=".85"/>
<g fill="#9a5350" opacity="0.55"><rect x="838.5741365445524" y="607.402845031443" width="21.618294397069207" height="20.538300530543435"/><path d="M835.5741365445524,607.402845031443 L849.383283743087,602.6468202640878 L863.1924309416216,607.402845031443Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="727.9070552615158" y="611.4460515711045" width="19.410632081866915" height="14.035364975370001"/><path d="M724.9070552615158,611.4460515711045 L737.6123713024492,607.1757125130938 L750.3176873433827,611.4460515711045Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="1349.8781851985036" y="590.4559744872581" width="26.118903372282027" height="20.001771997236787"/><path d="M1346.8781851985036,590.4559744872581 L1362.9376368846447,584.7098157453561 L1378.9970885707855,590.4559744872581Z"/></g>
<rect x="579" y="612" width="25" height="10" fill="#9a5350" opacity=".5"/>
<rect x="757" y="596" width="31" height="29" fill="#9a5350" opacity=".5"/>
<rect x="1009" y="601" width="32" height="27" fill="#9a5350" opacity=".5"/>
<rect x="1275" y="603" width="46" height="17" fill="#9a5350" opacity=".5"/>
<g fill="#9a5350" opacity="0.55"><rect x="1221.4160268404153" y="598.1447501606233" width="29.331535843791556" height="16.95104360310464"/><path d="M1218.4160268404153,598.1447501606233 L1236.0817947623111,591.6918122749892 L1253.7475626842067,598.1447501606233Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="633.5517282073664" y="604.4375498542271" width="38.74489754912197" height="15.111877535393939"/><path d="M630.5517282073664,604.4375498542271 L652.9241769819274,595.9136723934203 L675.2966257564884,604.4375498542271Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="1434.9197411247867" y="585.860354985036" width="42.37505899493695" height="23.41086595817357"/><path d="M1431.9197411247867,585.860354985036 L1456.107270622255,576.5378420061498 L1480.2948001197237,585.860354985036Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="864.7808322835489" y="617.1784047692532" width="30.86468241505218" height="13.150658796584114"/><path d="M861.7808322835489,617.1784047692532 L880.2131734910749,610.3881746379418 L898.645514698601,617.1784047692532Z"/></g>
<rect x="1560" y="588" width="45" height="21" fill="#9a5350" opacity=".5"/>
<rect x="882" y="620" width="28" height="10" fill="#9a5350" opacity=".5"/>
<g fill="#9a5350" opacity="0.55"><rect x="1082.8745666314558" y="601.4671573206331" width="23.627441517628903" height="20.094712790286252"/><path d="M1079.8745666314558,601.4671573206331 L1094.6882873902703,596.2691201867547 L1109.5020081490848,601.4671573206331Z"/></g>
<g fill="#9a5350" opacity="0.55"><rect x="653.3435337962192" y="608.3186810855666" width="29.186312768089923" height="10.833339153823054"/><path d="M650.3435337962192,608.3186810855666 L667.9366901802641,601.8976922765868 L685.5298465643091,608.3186810855666Z"/></g>
<rect x="802" y="612" width="34" height="21" fill="#9a5350" opacity=".5"/>
<rect x="1305" y="596" width="43" height="18" fill="#9a5350" opacity=".5"/>
<rect x="715" y="599" width="38" height="23" fill="#9a5350" opacity=".5"/>
<g fill="#9a5350" opacity="0.55"><rect x="1487.6200501136516" y="589.4481239310446" width="35.56529948129394" height="24.677042469539238"/><path d="M1484.6200501136516,589.4481239310446 L1505.4026998542986,581.62375804516 L1526.1853495949456,589.4481239310446Z"/></g>
<rect x="1105" y="602" width="32" height="27" fill="#9a5350" opacity=".5"/>
<g fill="#9a5350" opacity="0.55"><rect x="1167.4239774784883" y="600.6593826776506" width="42.99923261935422" height="23.657907390010013"/><path d="M1164.4239774784883,600.6593826776506 L1188.9235937881654,591.1995515013926 L1213.4232100978425,600.6593826776506Z"/></g>
<rect x="592" y="600" width="22" height="17" fill="#9a5350" opacity=".5"/>
<rect x="1141" y="603" width="36" height="23" fill="#9a5350" opacity=".5"/>
<rect x="563" y="594" width="40" height="25" fill="#9a5350" opacity=".5"/>
<g fill="#9a5350" opacity="0.55"><rect x="1245.671468876524" y="591.0901428231572" width="19.849409974220254" height="24.73576657084501"/><path d="M1242.671468876524,591.0901428231572 L1255.596173863634,586.7232726288287 L1268.520878850744,591.0901428231572Z"/></g>
<rect x="836" y="619" width="38" height="14" fill="#9a5350" opacity=".5"/>
<path d="M1330,588 a34,34 0 0 1 68,0Z" fill="#9a5350" opacity=".6"/><rect x="1322" y="588" width="84" height="22" fill="#9a5350" opacity=".6"/>
<g opacity=".8">
<path d="M-10,580 L640,592 L640,600 L-10,588Z" fill="#a55f55"/>
<rect x="-10" y="588" width="9" height="82" fill="#a55f55"/>
<path d="M-1,604 a12.5,12.5 0 0 1 25,0 L24,588 L-1,588Z" fill="#a55f55"/>
<rect x="24" y="589" width="9" height="81" fill="#a55f55"/>
<path d="M33,605 a12.5,12.5 0 0 1 25,0 L58,589 L33,589Z" fill="#a55f55"/>
<rect x="58" y="589" width="9" height="81" fill="#a55f55"/>
<path d="M67,605 a12.5,12.5 0 0 1 25,0 L92,589 L67,589Z" fill="#a55f55"/>
<rect x="92" y="590" width="9" height="80" fill="#a55f55"/>
<path d="M101,606 a12.5,12.5 0 0 1 25,0 L126,590 L101,590Z" fill="#a55f55"/>
<rect x="126" y="591" width="9" height="79" fill="#a55f55"/>
<path d="M135,607 a12.5,12.5 0 0 1 25,0 L160,591 L135,591Z" fill="#a55f55"/>
<rect x="160" y="591" width="9" height="79" fill="#a55f55"/>
<path d="M169,607 a12.5,12.5 0 0 1 25,0 L194,591 L169,591Z" fill="#a55f55"/>
<rect x="194" y="592" width="9" height="78" fill="#a55f55"/>
<path d="M203,608 a12.5,12.5 0 0 1 25,0 L228,592 L203,592Z" fill="#a55f55"/>
<rect x="228" y="592" width="9" height="78" fill="#a55f55"/>
<path d="M237,608 a12.5,12.5 0 0 1 25,0 L262,592 L237,592Z" fill="#a55f55"/>
<rect x="262" y="593" width="9" height="77" fill="#a55f55"/>
<path d="M271,609 a12.5,12.5 0 0 1 25,0 L296,593 L271,593Z" fill="#a55f55"/>
<rect x="296" y="594" width="9" height="76" fill="#a55f55"/>
<path d="M305,610 a12.5,12.5 0 0 1 25,0 L330,594 L305,594Z" fill="#a55f55"/>
<rect x="330" y="594" width="9" height="76" fill="#a55f55"/>
<path d="M339,610 a12.5,12.5 0 0 1 25,0 L364,594 L339,594Z" fill="#a55f55"/>
<rect x="364" y="595" width="9" height="75" fill="#a55f55"/>
<path d="M373,611 a12.5,12.5 0 0 1 25,0 L398,595 L373,595Z" fill="#a55f55"/>
<rect x="398" y="596" width="9" height="74" fill="#a55f55"/>
<path d="M407,612 a12.5,12.5 0 0 1 25,0 L432,596 L407,596Z" fill="#a55f55"/>
<rect x="432" y="596" width="9" height="74" fill="#a55f55"/>
<path d="M441,612 a12.5,12.5 0 0 1 25,0 L466,596 L441,596Z" fill="#a55f55"/>
<rect x="466" y="597" width="9" height="73" fill="#a55f55"/>
<path d="M475,613 a12.5,12.5 0 0 1 25,0 L500,597 L475,597Z" fill="#a55f55"/>
<rect x="500" y="597" width="9" height="73" fill="#a55f55"/>
<path d="M509,613 a12.5,12.5 0 0 1 25,0 L534,597 L509,597Z" fill="#a55f55"/>
<rect x="534" y="598" width="9" height="72" fill="#a55f55"/>
<path d="M543,614 a12.5,12.5 0 0 1 25,0 L568,598 L543,598Z" fill="#a55f55"/>
<rect x="568" y="599" width="9" height="71" fill="#a55f55"/>
<path d="M577,615 a12.5,12.5 0 0 1 25,0 L602,599 L577,599Z" fill="#a55f55"/>
<rect x="602" y="599" width="9" height="71" fill="#a55f55"/>
<path d="M611,615 a12.5,12.5 0 0 1 25,0 L636,599 L611,599Z" fill="#a55f55"/>
<rect x="636" y="600" width="9" height="70" fill="#a55f55"/>
<path d="M645,616 a12.5,12.5 0 0 1 25,0 L670,600 L645,600Z" fill="#a55f55"/>
</g>
<path d="M0,668 C200,640 420,650 640,662 C900,640 1200,650 1600,632 L1600,740 L0,740Z" fill="url(#hill2)" opacity=".9"/>
<path d="M560,690 C552,672 556,642 560,630 C564,642 568,672 560,690Z" fill="#4a2a38" opacity="0.9"/>
<path d="M585,690 C575,667 580,628 585,612 C590,628 595,667 585,690Z" fill="#4a2a38" opacity="0.9"/>
<path d="M612,690 C605,674 608,647 612,636 C616,647 619,674 612,690Z" fill="#4a2a38" opacity="0.9"/>
<path d="M1425,690 C1416,669 1420,634 1425,620 C1430,634 1434,669 1425,690Z" fill="#4a2a38" opacity="0.9"/>
<path d="M1452,690 C1440,662 1446,616 1452,598 C1458,616 1464,662 1452,690Z" fill="#4a2a38" opacity="0.9"/>
<path d="M1478,690 C1470,671 1474,639 1478,626 C1482,639 1486,671 1478,690Z" fill="#4a2a38" opacity="0.9"/>
<g fill="#3d2433" opacity="0.92"><path d="M666,694 L667,618 L671,618 L672,694Z"/><path d="M668,636 L689,610 L691,612 L671,642Z"/><path d="M668,631 L649,608 L651,606 L670,627Z"/><ellipse cx="668" cy="604" rx="38" ry="13"/><ellipse cx="647" cy="608" rx="21" ry="9"/><ellipse cx="691" cy="607" rx="21" ry="8"/><ellipse cx="672" cy="596" rx="23" ry="7"/></g>
<path d="M1091.4,712 L1091.4,530.9 1101.5,531.0 1111.6,531.2 1121.6,531.3 1131.5,531.4 1141.2,531.6 1150.9,531.8 1160.4,532.0 1169.8,532.2 1179.0,532.5 1188.0,532.7 1196.9,533.0 1205.5,533.3 1214.0,533.6 1222.2,533.9 1230.3,534.3 1238.1,534.6 1245.6,535.0 1252.9,535.4 1260.0,535.8 1266.7,536.2 1273.3,536.6 1279.5,537.0 1285.4,537.4 1291.0,537.9 1296.3,538.4 1301.4,538.8 1306.0,539.3 1310.4,539.8 1314.4,540.3 1318.1,540.8 1321.5,541.3 1324.5,541.8 1327.2,542.3 1329.5,542.8 1331.5,543.3 1333.1,543.9 1334.4,544.4 L1334.4,712Z" fill="url(#stoneIn)" filter="url(#rock)"/>
<path d="M1129.5,598.1 L1129.5,561.3 a14.0,14.0 0 0 1 28.1,0 L1157.6,598.1Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1172.5,598.8 L1172.5,561.2 a13.0,13.0 0 0 1 26.1,0 L1198.6,598.8Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1212.3,599.7 L1212.3,561.0 a11.7,11.7 0 0 1 23.4,0 L1235.7,599.7Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1247.8,600.9 L1247.8,560.9 a10.1,10.1 0 0 1 20.2,0 L1268.1,600.9Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1278.2,602.4 L1278.2,560.8 a8.3,8.3 0 0 1 16.5,0 L1294.8,602.4Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1302.8,604.0 L1302.8,560.8 a6.2,6.2 0 0 1 12.4,0 L1315.2,604.0Z" fill="url(#archSky)" opacity=".8"/>
<path d="M1320.9,605.8 L1320.9,560.8 a4.0,4.0 0 0 1 8.0,0 L1328.9,605.8Z" fill="url(#archSky)" opacity=".8"/>
<g filter="url(#rock)"><path d="M711.8,712 L711.8,474.1 713.2,473.5 715.0,472.9 717.2,472.2 719.8,471.6 722.8,471.0 726.2,470.4 729.9,469.8 734.0,469.2 738.5,468.6 743.4,468.1 748.6,467.5 754.2,467.0 760.1,466.4 766.4,465.9 773.0,465.4 780.0,464.9 787.2,464.4 794.8,463.9 802.6,463.4 810.8,463.0 819.2,462.5 827.9,462.1 836.8,461.7 846.0,461.4 855.5,461.0 865.1,460.7 875.0,460.3 885.1,460.0 895.3,459.7 905.8,459.5 916.4,459.2 927.1,459.0 938.0,458.8 949.0,458.6 960.2,458.4 971.4,458.3 982.7,458.2 994.1,458.1 1005.5,458.0 1017.0,457.9 1028.5,457.9 1040.0,457.9 1051.5,457.9 1063.0,457.9 1074.5,458.0 1085.9,458.1 1097.3,458.2 1108.6,458.3 1119.8,458.4 1131.0,473.3 1142.0,488.1 1152.9,503.0 1163.6,517.8 1174.2,533.2 1184.7,548.6 1194.9,563.9 1205.0,579.3 1214.9,579.5 1224.5,579.7 1234.0,580.0 1243.2,580.2 1252.1,580.5 1260.8,580.8 1269.2,581.1 1277.4,581.4 1285.2,581.7 1292.8,582.0 1300.0,582.4 1307.0,582.7 1313.6,583.1 1319.9,583.4 1325.8,583.8 1331.4,584.2 1336.6,584.6 1341.5,585.0 1346.0,585.4 1350.1,585.8 1353.8,586.2 1357.2,586.6 1360.2,587.0 1362.8,587.4 1365.0,587.8 1366.8,588.3 1368.2,588.7 L1368.2,712Z" fill="url(#stone)"/></g>
<path d="M711.8,712 L711.8,474.1 713.2,473.5 715.0,472.9 717.2,472.2 719.8,471.6 722.8,471.0 726.2,470.4 729.9,469.8 734.0,469.2 738.5,468.6 743.4,468.1 748.6,467.5 754.2,467.0 760.1,466.4 766.4,465.9 773.0,465.4 780.0,464.9 787.2,464.4 794.8,463.9 802.6,463.4 810.8,463.0 819.2,462.5 827.9,462.1 836.8,461.7 846.0,461.4 855.5,461.0 865.1,460.7 875.0,460.3 885.1,460.0 895.3,459.7 905.8,459.5 916.4,459.2 927.1,459.0 938.0,458.8 949.0,458.6 960.2,458.4 971.4,458.3 982.7,458.2 994.1,458.1 1005.5,458.0 1017.0,457.9 1028.5,457.9 1040.0,457.9 1051.5,457.9 1063.0,457.9 1074.5,458.0 1085.9,458.1 1097.3,458.2 1108.6,458.3 1119.8,458.4 1131.0,473.3 1142.0,488.1 1152.9,503.0 1163.6,517.8 1174.2,533.2 1184.7,548.6 1194.9,563.9 1205.0,579.3 1214.9,579.5 1224.5,579.7 1234.0,580.0 1243.2,580.2 1252.1,580.5 1260.8,580.8 1269.2,581.1 1277.4,581.4 1285.2,581.7 1292.8,582.0 1300.0,582.4 1307.0,582.7 1313.6,583.1 1319.9,583.4 1325.8,583.8 1331.4,584.2 1336.6,584.6 1341.5,585.0 1346.0,585.4 1350.1,585.8 1353.8,586.2 1357.2,586.6 1360.2,587.0 1362.8,587.4 1365.0,587.8 1366.8,588.3 1368.2,588.7 L1368.2,712Z" fill="url(#vshade)"/>
<polyline points="711.8,651.0 713.2,650.7 715.0,650.4 717.2,650.1 719.8,649.8 722.8,649.4 726.2,649.1 729.9,648.8 734.0,648.5 738.5,648.2 743.4,647.9 748.6,647.6 754.2,647.4 760.1,647.1 766.4,646.8 773.0,646.5 780.0,646.3 787.2,646.0 794.8,645.8 802.6,645.5 810.8,645.3 819.2,645.1 827.9,644.9 836.8,644.7 846.0,644.5 855.5,644.3 865.1,644.1 875.0,643.9 885.1,643.8 895.3,643.6 905.8,643.5 916.4,643.4 927.1,643.3 938.0,643.2 949.0,643.1 960.2,643.0 971.4,642.9 982.7,642.8 994.1,642.8 1005.5,642.8 1017.0,642.7 1028.5,642.7 1040.0,642.7 1051.5,642.7 1063.0,642.7 1074.5,642.8 1085.9,642.8 1097.3,642.8 1108.6,642.9 1119.8,643.0 1131.0,643.1 1142.0,643.2 1152.9,643.3 1163.6,643.4 1174.2,643.5 1184.7,643.6 1194.9,643.8 1205.0,643.9 1214.9,644.1 1224.5,644.3 1234.0,644.5 1243.2,644.7 1252.1,644.9 1260.8,645.1 1269.2,645.3 1277.4,645.5 1285.2,645.8 1292.8,646.0 1300.0,646.3 1307.0,646.5 1313.6,646.8 1319.9,647.1 1325.8,647.4 1331.4,647.6 1336.6,647.9 1341.5,648.2 1346.0,648.5 1350.1,648.8 1353.8,649.1 1357.2,649.4 1360.2,649.8 1362.8,650.1 1365.0,650.4 1366.8,650.7 1368.2,651.0" fill="none" stroke="#2b1a1e" stroke-width="5" opacity=".55"/>
<polyline points="711.8,647.5 713.2,647.2 715.0,646.9 717.2,646.6 719.8,646.3 722.8,645.9 726.2,645.6 729.9,645.3 734.0,645.0 738.5,644.7 743.4,644.4 748.6,644.1 754.2,643.9 760.1,643.6 766.4,643.3 773.0,643.0 780.0,642.8 787.2,642.5 794.8,642.3 802.6,642.0 810.8,641.8 819.2,641.6 827.9,641.4 836.8,641.2 846.0,641.0 855.5,640.8 865.1,640.6 875.0,640.4 885.1,640.3 895.3,640.1 905.8,640.0 916.4,639.9 927.1,639.8 938.0,639.7 949.0,639.6 960.2,639.5 971.4,639.4 982.7,639.3 994.1,639.3 1005.5,639.3 1017.0,639.2 1028.5,639.2 1040.0,639.2 1051.5,639.2 1063.0,639.2 1074.5,639.3 1085.9,639.3 1097.3,639.3 1108.6,639.4 1119.8,639.5 1131.0,639.6 1142.0,639.7 1152.9,639.8 1163.6,639.9 1174.2,640.0 1184.7,640.1 1194.9,640.3 1205.0,640.4 1214.9,640.6 1224.5,640.8 1234.0,641.0 1243.2,641.2 1252.1,641.4 1260.8,641.6 1269.2,641.8 1277.4,642.0 1285.2,642.3 1292.8,642.5 1300.0,642.8 1307.0,643.0 1313.6,643.3 1319.9,643.6 1325.8,643.9 1331.4,644.1 1336.6,644.4 1341.5,644.7 1346.0,645.0 1350.1,645.3 1353.8,645.6 1357.2,645.9 1360.2,646.3 1362.8,646.6 1365.0,646.9 1366.8,647.2 1368.2,647.5" fill="none" stroke="#ffe0ae" stroke-width="1.6" opacity=".45"/>
<polyline points="711.8,588.7 713.2,588.3 715.0,587.8 717.2,587.4 719.8,587.0 722.8,586.6 726.2,586.2 729.9,585.8 734.0,585.4 738.5,585.0 743.4,584.6 748.6,584.2 754.2,583.8 760.1,583.4 766.4,583.1 773.0,582.7 780.0,582.4 787.2,582.0 794.8,581.7 802.6,581.4 810.8,581.1 819.2,580.8 827.9,580.5 836.8,580.2 846.0,580.0 855.5,579.7 865.1,579.5 875.0,579.3 885.1,579.1 895.3,578.9 905.8,578.7 916.4,578.5 927.1,578.3 938.0,578.2 949.0,578.1 960.2,578.0 971.4,577.9 982.7,577.8 994.1,577.7 1005.5,577.7 1017.0,577.6 1028.5,577.6 1040.0,577.6 1051.5,577.6 1063.0,577.6 1074.5,577.7 1085.9,577.7 1097.3,577.8 1108.6,577.9 1119.8,578.0 1131.0,578.1 1142.0,578.2 1152.9,578.3 1163.6,578.5 1174.2,578.7 1184.7,578.9 1194.9,579.1 1205.0,579.3 1214.9,579.5 1224.5,579.7 1234.0,580.0 1243.2,580.2 1252.1,580.5 1260.8,580.8 1269.2,581.1 1277.4,581.4 1285.2,581.7 1292.8,582.0 1300.0,582.4 1307.0,582.7 1313.6,583.1 1319.9,583.4 1325.8,583.8 1331.4,584.2 1336.6,584.6 1341.5,585.0 1346.0,585.4 1350.1,585.8 1353.8,586.2 1357.2,586.6 1360.2,587.0 1362.8,587.4 1365.0,587.8 1366.8,588.3 1368.2,588.7" fill="none" stroke="#2b1a1e" stroke-width="5" opacity=".55"/>
<polyline points="711.8,585.2 713.2,584.8 715.0,584.3 717.2,583.9 719.8,583.5 722.8,583.1 726.2,582.7 729.9,582.3 734.0,581.9 738.5,581.5 743.4,581.1 748.6,580.7 754.2,580.3 760.1,579.9 766.4,579.6 773.0,579.2 780.0,578.9 787.2,578.5 794.8,578.2 802.6,577.9 810.8,577.6 819.2,577.3 827.9,577.0 836.8,576.7 846.0,576.5 855.5,576.2 865.1,576.0 875.0,575.8 885.1,575.6 895.3,575.4 905.8,575.2 916.4,575.0 927.1,574.8 938.0,574.7 949.0,574.6 960.2,574.5 971.4,574.4 982.7,574.3 994.1,574.2 1005.5,574.2 1017.0,574.1 1028.5,574.1 1040.0,574.1 1051.5,574.1 1063.0,574.1 1074.5,574.2 1085.9,574.2 1097.3,574.3 1108.6,574.4 1119.8,574.5 1131.0,574.6 1142.0,574.7 1152.9,574.8 1163.6,575.0 1174.2,575.2 1184.7,575.4 1194.9,575.6 1205.0,575.8 1214.9,576.0 1224.5,576.2 1234.0,576.5 1243.2,576.7 1252.1,577.0 1260.8,577.3 1269.2,577.6 1277.4,577.9 1285.2,578.2 1292.8,578.5 1300.0,578.9 1307.0,579.2 1313.6,579.6 1319.9,579.9 1325.8,580.3 1331.4,580.7 1336.6,581.1 1341.5,581.5 1346.0,581.9 1350.1,582.3 1353.8,582.7 1357.2,583.1 1360.2,583.5 1362.8,583.9 1365.0,584.3 1366.8,584.8 1368.2,585.2" fill="none" stroke="#ffe0ae" stroke-width="1.6" opacity=".45"/>
<polyline points="711.8,530.4 713.2,529.9 715.0,529.3 717.2,528.8 719.8,528.3 722.8,527.8 726.2,527.3 729.9,526.8 734.0,526.3 738.5,525.8 743.4,525.3 748.6,524.8 754.2,524.4 760.1,523.9 766.4,523.4 773.0,523.0 780.0,522.6 787.2,522.2 794.8,521.8 802.6,521.4 810.8,521.0 819.2,520.6 827.9,520.3 836.8,519.9 846.0,519.6 855.5,519.3 865.1,519.0 875.0,518.7 885.1,518.5 895.3,518.2 905.8,518.0 916.4,517.8 927.1,517.6 938.0,517.4 949.0,517.3 960.2,517.2 971.4,517.0 982.7,516.9 994.1,516.8 1005.5,516.8 1017.0,516.7 1028.5,516.7 1040.0,516.7 1051.5,516.7 1063.0,516.7 1074.5,516.8 1085.9,516.8 1097.3,516.9 1108.6,517.0 1119.8,517.2 1131.0,517.3 1142.0,517.4 1152.9,517.6 1163.6,517.8" fill="none" stroke="#2b1a1e" stroke-width="5" opacity=".55"/>
<polyline points="711.8,526.9 713.2,526.4 715.0,525.8 717.2,525.3 719.8,524.8 722.8,524.3 726.2,523.8 729.9,523.3 734.0,522.8 738.5,522.3 743.4,521.8 748.6,521.3 754.2,520.9 760.1,520.4 766.4,519.9 773.0,519.5 780.0,519.1 787.2,518.7 794.8,518.3 802.6,517.9 810.8,517.5 819.2,517.1 827.9,516.8 836.8,516.4 846.0,516.1 855.5,515.8 865.1,515.5 875.0,515.2 885.1,515.0 895.3,514.7 905.8,514.5 916.4,514.3 927.1,514.1 938.0,513.9 949.0,513.8 960.2,513.7 971.4,513.5 982.7,513.4 994.1,513.3 1005.5,513.3 1017.0,513.2 1028.5,513.2 1040.0,513.2 1051.5,513.2 1063.0,513.2 1074.5,513.3 1085.9,513.3 1097.3,513.4 1108.6,513.5 1119.8,513.7 1131.0,513.8 1142.0,513.9 1152.9,514.1 1163.6,514.3" fill="none" stroke="#ffe0ae" stroke-width="1.6" opacity=".45"/>
<polyline points="711.8,474.1 713.2,473.5 715.0,472.9 717.2,472.2 719.8,471.6 722.8,471.0 726.2,470.4 729.9,469.8 734.0,469.2 738.5,468.6 743.4,468.1 748.6,467.5 754.2,467.0 760.1,466.4 766.4,465.9 773.0,465.4 780.0,464.9 787.2,464.4 794.8,463.9 802.6,463.4 810.8,463.0 819.2,462.5 827.9,462.1 836.8,461.7 846.0,461.4 855.5,461.0 865.1,460.7 875.0,460.3 885.1,460.0 895.3,459.7 905.8,459.5 916.4,459.2 927.1,459.0 938.0,458.8 949.0,458.6 960.2,458.4 971.4,458.3 982.7,458.2 994.1,458.1 1005.5,458.0 1017.0,457.9 1028.5,457.9 1040.0,457.9 1051.5,457.9 1063.0,457.9 1074.5,458.0 1085.9,458.1 1097.3,458.2 1108.6,458.3 1119.8,458.4" fill="none" stroke="#2b1a1e" stroke-width="6" opacity=".55"/>
<polyline points="711.8,470.6 713.2,470.0 715.0,469.4 717.2,468.7 719.8,468.1 722.8,467.5 726.2,466.9 729.9,466.3 734.0,465.7 738.5,465.1 743.4,464.6 748.6,464.0 754.2,463.5 760.1,462.9 766.4,462.4 773.0,461.9 780.0,461.4 787.2,460.9 794.8,460.4 802.6,459.9 810.8,459.5 819.2,459.0 827.9,458.6 836.8,458.2 846.0,457.9 855.5,457.5 865.1,457.2 875.0,456.8 885.1,456.5 895.3,456.2 905.8,456.0 916.4,455.7 927.1,455.5 938.0,455.3 949.0,455.1 960.2,454.9 971.4,454.8 982.7,454.7 994.1,454.6 1005.5,454.5 1017.0,454.4 1028.5,454.4 1040.0,454.4 1051.5,454.4 1063.0,454.4 1074.5,454.5 1085.9,454.6 1097.3,454.7 1108.6,454.8 1119.8,454.9" fill="none" stroke="#ffe0ae" stroke-width="1.6" opacity=".45"/>
<path d="M716.6,716.5 L716.6,666.2 a3.5,3.5 0 0 1 7.0,0 L723.6,716.5Z" fill="url(#arch)"/>
<path d="M716.6,716.5 L716.6,666.2 a3.5,3.5 0 0 1 3.5,-3.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.54"/>
<ellipse cx="720.1" cy="712.5" rx="2.8" ry="3.8" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M716.6,646.8 L716.6,603.5 a3.5,3.5 0 0 1 7.0,0 L723.6,646.8Z" fill="url(#archUp)"/>
<path d="M716.6,646.8 L716.6,603.5 a3.5,3.5 0 0 1 3.5,-3.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.54"/>
<path d="M716.6,584.0 L716.6,544.8 a3.5,3.5 0 0 1 7.0,0 L723.6,584.0Z" fill="url(#archUp)"/>
<path d="M716.6,584.0 L716.6,544.8 a3.5,3.5 0 0 1 3.5,-3.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.54"/>
<rect x="714.2" y="655.4" width="1.6" height="58.6" fill="#ffe2b4" opacity="0.38"/>
<rect x="715.8" y="655.4" width="1.0" height="58.6" fill="#1c0f14" opacity=".28"/>
<rect x="714.2" y="592.8" width="1.6" height="54.5" fill="#ffe2b4" opacity="0.38"/>
<rect x="715.8" y="592.8" width="1.0" height="54.5" fill="#1c0f14" opacity=".28"/>
<rect x="714.2" y="534.3" width="1.6" height="50.5" fill="#ffe2b4" opacity="0.38"/>
<rect x="715.8" y="534.3" width="1.0" height="50.5" fill="#1c0f14" opacity=".28"/>
<rect x="714.2" y="477.9" width="1.6" height="48.5" fill="#ffe2b4" opacity="0.38"/>
<rect x="715.8" y="477.9" width="1.0" height="48.5" fill="#1c0f14" opacity=".28"/>
<rect x="719.0" y="497.3" width="2.2" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M728.9,715.8 L728.9,666.9 a5.4,5.4 0 0 1 10.8,0 L739.7,715.8Z" fill="url(#arch)"/>
<path d="M728.9,715.8 L728.9,666.9 a5.4,5.4 0 0 1 5.4,-5.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.51"/>
<ellipse cx="734.3" cy="711.8" rx="4.3" ry="5.9" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M728.9,645.5 L728.9,603.7 a5.4,5.4 0 0 1 10.8,0 L739.7,645.5Z" fill="url(#archUp)"/>
<path d="M728.9,645.5 L728.9,603.7 a5.4,5.4 0 0 1 5.4,-5.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.51"/>
<path d="M728.9,582.4 L728.9,544.7 a5.4,5.4 0 0 1 10.8,0 L739.7,582.4Z" fill="url(#archUp)"/>
<path d="M728.9,582.4 L728.9,544.7 a5.4,5.4 0 0 1 5.4,-5.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.51"/>
<rect x="725.3" y="654.1" width="1.7" height="59.0" fill="#ffe2b4" opacity="0.36"/>
<rect x="727.0" y="654.1" width="1.0" height="59.0" fill="#1c0f14" opacity=".28"/>
<rect x="725.3" y="591.2" width="1.7" height="55.0" fill="#ffe2b4" opacity="0.36"/>
<rect x="727.0" y="591.2" width="1.0" height="55.0" fill="#1c0f14" opacity=".28"/>
<rect x="725.3" y="532.3" width="1.7" height="50.9" fill="#ffe2b4" opacity="0.36"/>
<rect x="727.0" y="532.3" width="1.0" height="50.9" fill="#1c0f14" opacity=".28"/>
<rect x="725.3" y="475.4" width="1.7" height="48.9" fill="#ffe2b4" opacity="0.36"/>
<rect x="727.0" y="475.4" width="1.0" height="48.9" fill="#1c0f14" opacity=".28"/>
<path d="M747.3,715.0 L747.3,667.5 a7.2,7.2 0 0 1 14.4,0 L761.7,715.0Z" fill="url(#arch)"/>
<path d="M747.3,715.0 L747.3,667.5 a7.2,7.2 0 0 1 7.2,-7.2" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.48"/>
<ellipse cx="754.5" cy="711.0" rx="5.8" ry="7.9" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M747.3,644.4 L747.3,604.0 a7.2,7.2 0 0 1 14.4,0 L761.7,644.4Z" fill="url(#archUp)"/>
<path d="M747.3,644.4 L747.3,604.0 a7.2,7.2 0 0 1 7.2,-7.2" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.48"/>
<path d="M747.3,580.8 L747.3,544.5 a7.2,7.2 0 0 1 14.4,0 L761.7,580.8Z" fill="url(#archUp)"/>
<path d="M747.3,580.8 L747.3,544.5 a7.2,7.2 0 0 1 7.2,-7.2" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.48"/>
<rect x="742.2" y="652.9" width="2.4" height="59.4" fill="#ffe2b4" opacity="0.35"/>
<rect x="744.6" y="652.9" width="1.4" height="59.4" fill="#1c0f14" opacity=".28"/>
<rect x="742.2" y="589.6" width="2.4" height="55.4" fill="#ffe2b4" opacity="0.35"/>
<rect x="744.6" y="589.6" width="1.4" height="55.4" fill="#1c0f14" opacity=".28"/>
<rect x="742.2" y="530.3" width="2.4" height="51.3" fill="#ffe2b4" opacity="0.35"/>
<rect x="744.6" y="530.3" width="1.4" height="51.3" fill="#1c0f14" opacity=".28"/>
<rect x="742.2" y="473.1" width="2.4" height="49.2" fill="#ffe2b4" opacity="0.35"/>
<rect x="744.6" y="473.1" width="1.4" height="49.2" fill="#1c0f14" opacity=".28"/>
<rect x="752.2" y="493.4" width="4.6" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M771.3,714.3 L771.3,668.1 a8.9,8.9 0 0 1 17.7,0 L789.1,714.3Z" fill="url(#arch)"/>
<path d="M771.3,714.3 L771.3,668.1 a8.9,8.9 0 0 1 8.9,-8.9" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.45"/>
<path d="M771.3,643.3 L771.3,604.2 a8.9,8.9 0 0 1 17.7,0 L789.1,643.3Z" fill="url(#archUp)"/>
<path d="M771.3,643.3 L771.3,604.2 a8.9,8.9 0 0 1 8.9,-8.9" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.45"/>
<path d="M771.3,579.4 L771.3,544.4 a8.9,8.9 0 0 1 17.7,0 L789.1,579.4Z" fill="url(#archUp)"/>
<path d="M771.3,579.4 L771.3,544.4 a8.9,8.9 0 0 1 8.9,-8.9" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.45"/>
<rect x="764.9" y="651.8" width="3.1" height="59.8" fill="#ffe2b4" opacity="0.33"/>
<rect x="768.0" y="651.8" width="1.8" height="59.8" fill="#1c0f14" opacity=".28"/>
<rect x="764.9" y="588.1" width="3.1" height="55.7" fill="#ffe2b4" opacity="0.33"/>
<rect x="768.0" y="588.1" width="1.8" height="55.7" fill="#1c0f14" opacity=".28"/>
<rect x="764.9" y="528.4" width="3.1" height="51.6" fill="#ffe2b4" opacity="0.33"/>
<rect x="768.0" y="528.4" width="1.8" height="51.6" fill="#1c0f14" opacity=".28"/>
<rect x="764.9" y="470.9" width="3.1" height="49.6" fill="#ffe2b4" opacity="0.33"/>
<rect x="768.0" y="470.9" width="1.8" height="49.6" fill="#1c0f14" opacity=".28"/>
<path d="M800.6,713.7 L800.6,668.7 a10.4,10.4 0 0 1 20.7,0 L821.3,713.7Z" fill="url(#arch)"/>
<path d="M800.6,713.7 L800.6,668.7 a10.4,10.4 0 0 1 10.4,-10.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.43"/>
<path d="M800.6,642.3 L800.6,604.4 a10.4,10.4 0 0 1 20.7,0 L821.3,642.3Z" fill="url(#archUp)"/>
<path d="M800.6,642.3 L800.6,604.4 a10.4,10.4 0 0 1 10.4,-10.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.43"/>
<path d="M800.6,578.1 L800.6,544.3 a10.4,10.4 0 0 1 20.7,0 L821.3,578.1Z" fill="url(#archUp)"/>
<path d="M800.6,578.1 L800.6,544.3 a10.4,10.4 0 0 1 10.4,-10.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.43"/>
<rect x="792.9" y="650.8" width="3.7" height="60.2" fill="#ffe2b4" opacity="0.31"/>
<rect x="796.6" y="650.8" width="2.2" height="60.2" fill="#1c0f14" opacity=".28"/>
<rect x="792.9" y="586.7" width="3.7" height="56.1" fill="#ffe2b4" opacity="0.31"/>
<rect x="796.6" y="586.7" width="2.2" height="56.1" fill="#1c0f14" opacity=".28"/>
<rect x="792.9" y="526.8" width="3.7" height="51.9" fill="#ffe2b4" opacity="0.31"/>
<rect x="796.6" y="526.8" width="2.2" height="51.9" fill="#1c0f14" opacity=".28"/>
<rect x="792.9" y="468.9" width="3.7" height="49.9" fill="#ffe2b4" opacity="0.31"/>
<rect x="796.6" y="468.9" width="2.2" height="49.9" fill="#1c0f14" opacity=".28"/>
<rect x="807.7" y="490.0" width="6.6" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M834.6,713.1 L834.6,669.1 a11.6,11.6 0 0 1 23.3,0 L857.9,713.1Z" fill="url(#arch)"/>
<path d="M834.6,713.1 L834.6,669.1 a11.6,11.6 0 0 1 11.6,-11.6" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.40"/>
<path d="M834.6,641.5 L834.6,604.6 a11.6,11.6 0 0 1 23.3,0 L857.9,641.5Z" fill="url(#archUp)"/>
<path d="M834.6,641.5 L834.6,604.6 a11.6,11.6 0 0 1 11.6,-11.6" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.40"/>
<path d="M834.6,577.0 L834.6,544.3 a11.6,11.6 0 0 1 23.3,0 L857.9,577.0Z" fill="url(#archUp)"/>
<path d="M834.6,577.0 L834.6,544.3 a11.6,11.6 0 0 1 11.6,-11.6" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.40"/>
<rect x="825.8" y="649.9" width="4.2" height="60.5" fill="#ffe2b4" opacity="0.30"/>
<rect x="830.0" y="649.9" width="2.5" height="60.5" fill="#1c0f14" opacity=".28"/>
<rect x="825.8" y="585.5" width="4.2" height="56.4" fill="#ffe2b4" opacity="0.30"/>
<rect x="830.0" y="585.5" width="2.5" height="56.4" fill="#1c0f14" opacity=".28"/>
<rect x="825.8" y="525.3" width="4.2" height="52.2" fill="#ffe2b4" opacity="0.30"/>
<rect x="830.0" y="525.3" width="2.5" height="52.2" fill="#1c0f14" opacity=".28"/>
<rect x="825.8" y="467.1" width="4.2" height="50.1" fill="#ffe2b4" opacity="0.30"/>
<rect x="830.0" y="467.1" width="2.5" height="50.1" fill="#1c0f14" opacity=".28"/>
<path d="M872.5,712.7 L872.5,669.5 a12.7,12.7 0 0 1 25.4,0 L897.9,712.7Z" fill="url(#arch)"/>
<path d="M872.5,712.7 L872.5,669.5 a12.7,12.7 0 0 1 12.7,-12.7" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.37"/>
<path d="M872.5,640.8 L872.5,604.8 a12.7,12.7 0 0 1 25.4,0 L897.9,640.8Z" fill="url(#archUp)"/>
<path d="M872.5,640.8 L872.5,604.8 a12.7,12.7 0 0 1 12.7,-12.7" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.37"/>
<path d="M872.5,576.1 L872.5,544.2 a12.7,12.7 0 0 1 25.4,0 L897.9,576.1Z" fill="url(#archUp)"/>
<path d="M872.5,576.1 L872.5,544.2 a12.7,12.7 0 0 1 12.7,-12.7" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.37"/>
<rect x="862.8" y="649.1" width="4.7" height="60.8" fill="#ffe2b4" opacity="0.28"/>
<rect x="867.5" y="649.1" width="2.8" height="60.8" fill="#1c0f14" opacity=".28"/>
<rect x="862.8" y="584.5" width="4.7" height="56.6" fill="#ffe2b4" opacity="0.28"/>
<rect x="867.5" y="584.5" width="2.8" height="56.6" fill="#1c0f14" opacity=".28"/>
<rect x="862.8" y="524.0" width="4.7" height="52.5" fill="#ffe2b4" opacity="0.28"/>
<rect x="867.5" y="524.0" width="2.8" height="52.5" fill="#1c0f14" opacity=".28"/>
<rect x="862.8" y="465.7" width="4.7" height="50.4" fill="#ffe2b4" opacity="0.28"/>
<rect x="867.5" y="465.7" width="2.8" height="50.4" fill="#1c0f14" opacity=".28"/>
<rect x="881.2" y="487.5" width="8.1" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M913.7,712.4 L913.7,669.8 a13.5,13.5 0 0 1 27.1,0 L940.8,712.4Z" fill="url(#arch)"/>
<path d="M913.7,712.4 L913.7,669.8 a13.5,13.5 0 0 1 13.5,-13.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.34"/>
<ellipse cx="927.2" cy="708.4" rx="10.8" ry="14.9" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M913.7,640.3 L913.7,604.9 a13.5,13.5 0 0 1 27.1,0 L940.8,640.3Z" fill="url(#archUp)"/>
<path d="M913.7,640.3 L913.7,604.9 a13.5,13.5 0 0 1 13.5,-13.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.34"/>
<path d="M913.7,575.3 L913.7,544.1 a13.5,13.5 0 0 1 27.1,0 L940.8,575.3Z" fill="url(#archUp)"/>
<path d="M913.7,575.3 L913.7,544.1 a13.5,13.5 0 0 1 13.5,-13.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.34"/>
<rect x="903.3" y="648.5" width="5.0" height="61.0" fill="#ffe2b4" opacity="0.26"/>
<rect x="908.3" y="648.5" width="3.0" height="61.0" fill="#1c0f14" opacity=".28"/>
<rect x="903.3" y="583.7" width="5.0" height="56.8" fill="#ffe2b4" opacity="0.26"/>
<rect x="908.3" y="583.7" width="3.0" height="56.8" fill="#1c0f14" opacity=".28"/>
<rect x="903.3" y="523.0" width="5.0" height="52.6" fill="#ffe2b4" opacity="0.26"/>
<rect x="908.3" y="523.0" width="3.0" height="52.6" fill="#1c0f14" opacity=".28"/>
<rect x="903.3" y="464.5" width="5.0" height="50.6" fill="#ffe2b4" opacity="0.26"/>
<rect x="908.3" y="464.5" width="3.0" height="50.6" fill="#1c0f14" opacity=".28"/>
<path d="M957.4,712.1 L957.4,670.0 a14.1,14.1 0 0 1 28.2,0 L985.5,712.1Z" fill="url(#arch)"/>
<path d="M957.4,712.1 L957.4,670.0 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.32"/>
<ellipse cx="971.5" cy="708.1" rx="11.3" ry="15.5" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M957.4,639.9 L957.4,605.0 a14.1,14.1 0 0 1 28.2,0 L985.5,639.9Z" fill="url(#archUp)"/>
<path d="M957.4,639.9 L957.4,605.0 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.32"/>
<path d="M957.4,574.9 L957.4,544.1 a14.1,14.1 0 0 1 28.2,0 L985.5,574.9Z" fill="url(#archUp)"/>
<path d="M957.4,574.9 L957.4,544.1 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.32"/>
<rect x="946.4" y="648.1" width="5.3" height="61.2" fill="#ffe2b4" opacity="0.25"/>
<rect x="951.7" y="648.1" width="3.2" height="61.2" fill="#1c0f14" opacity=".28"/>
<rect x="946.4" y="583.1" width="5.3" height="57.0" fill="#ffe2b4" opacity="0.25"/>
<rect x="951.7" y="583.1" width="3.2" height="57.0" fill="#1c0f14" opacity=".28"/>
<rect x="946.4" y="522.3" width="5.3" height="52.8" fill="#ffe2b4" opacity="0.25"/>
<rect x="951.7" y="522.3" width="3.2" height="52.8" fill="#1c0f14" opacity=".28"/>
<rect x="946.4" y="463.6" width="5.3" height="50.7" fill="#ffe2b4" opacity="0.25"/>
<rect x="951.7" y="463.6" width="3.2" height="50.7" fill="#1c0f14" opacity=".28"/>
<rect x="966.9" y="486.0" width="9.0" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M1002.6,712.0 L1002.6,670.1 a14.4,14.4 0 0 1 28.7,0 L1031.4,712.0Z" fill="url(#arch)"/>
<path d="M1002.6,712.0 L1002.6,670.1 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.29"/>
<ellipse cx="1017.0" cy="708.0" rx="11.5" ry="15.8" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1002.6,639.7 L1002.6,605.0 a14.4,14.4 0 0 1 28.7,0 L1031.4,639.7Z" fill="url(#archUp)"/>
<path d="M1002.6,639.7 L1002.6,605.0 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.29"/>
<path d="M1002.6,574.6 L1002.6,544.1 a14.4,14.4 0 0 1 28.7,0 L1031.4,574.6Z" fill="url(#archUp)"/>
<path d="M1002.6,574.6 L1002.6,544.1 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.29"/>
<rect x="991.3" y="647.8" width="5.4" height="61.3" fill="#ffe2b4" opacity="0.23"/>
<rect x="996.8" y="647.8" width="3.3" height="61.3" fill="#1c0f14" opacity=".28"/>
<rect x="991.3" y="582.7" width="5.4" height="57.1" fill="#ffe2b4" opacity="0.23"/>
<rect x="996.8" y="582.7" width="3.3" height="57.1" fill="#1c0f14" opacity=".28"/>
<rect x="991.3" y="521.8" width="5.4" height="52.9" fill="#ffe2b4" opacity="0.23"/>
<rect x="996.8" y="521.8" width="3.3" height="52.9" fill="#1c0f14" opacity=".28"/>
<rect x="991.3" y="463.1" width="5.4" height="50.8" fill="#ffe2b4" opacity="0.23"/>
<rect x="996.8" y="463.1" width="3.3" height="50.8" fill="#1c0f14" opacity=".28"/>
<path d="M1048.6,712.0 L1048.6,670.1 a14.4,14.4 0 0 1 28.7,0 L1077.4,712.0Z" fill="url(#arch)"/>
<path d="M1048.6,712.0 L1048.6,670.1 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.26"/>
<path d="M1048.6,639.7 L1048.6,605.0 a14.4,14.4 0 0 1 28.7,0 L1077.4,639.7Z" fill="url(#archUp)"/>
<path d="M1048.6,639.7 L1048.6,605.0 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.26"/>
<path d="M1048.6,574.6 L1048.6,544.1 a14.4,14.4 0 0 1 28.7,0 L1077.4,574.6Z" fill="url(#archUp)"/>
<path d="M1048.6,574.6 L1048.6,544.1 a14.4,14.4 0 0 1 14.4,-14.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.26"/>
<rect x="1037.2" y="647.7" width="5.5" height="61.3" fill="#ffe2b4" opacity="0.21"/>
<rect x="1042.8" y="647.7" width="3.3" height="61.3" fill="#1c0f14" opacity=".28"/>
<rect x="1037.2" y="582.6" width="5.5" height="57.1" fill="#ffe2b4" opacity="0.21"/>
<rect x="1042.8" y="582.6" width="3.3" height="57.1" fill="#1c0f14" opacity=".28"/>
<rect x="1037.2" y="521.7" width="5.5" height="52.9" fill="#ffe2b4" opacity="0.21"/>
<rect x="1042.8" y="521.7" width="3.3" height="52.9" fill="#1c0f14" opacity=".28"/>
<rect x="1037.2" y="462.9" width="5.5" height="50.8" fill="#ffe2b4" opacity="0.21"/>
<rect x="1042.8" y="462.9" width="3.3" height="50.8" fill="#1c0f14" opacity=".28"/>
<rect x="1058.4" y="485.7" width="9.2" height="15" fill="#1a0f14" opacity=".8"/>
<path d="M1094.5,712.1 L1094.5,670.0 a14.1,14.1 0 0 1 28.2,0 L1122.6,712.1Z" fill="url(#arch)"/>
<path d="M1094.5,712.1 L1094.5,670.0 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.23"/>
<ellipse cx="1108.5" cy="708.1" rx="11.3" ry="15.5" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1094.5,639.9 L1094.5,605.0 a14.1,14.1 0 0 1 28.2,0 L1122.6,639.9Z" fill="url(#archUp)"/>
<path d="M1094.5,639.9 L1094.5,605.0 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.23"/>
<path d="M1094.5,574.9 L1094.5,544.1 a14.1,14.1 0 0 1 28.2,0 L1122.6,574.9Z" fill="url(#archUp)"/>
<path d="M1094.5,574.9 L1094.5,544.1 a14.1,14.1 0 0 1 14.1,-14.1" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.23"/>
<rect x="1083.2" y="647.8" width="5.4" height="61.3" fill="#ffe2b4" opacity="0.19"/>
<rect x="1088.7" y="647.8" width="3.3" height="61.3" fill="#1c0f14" opacity=".28"/>
<rect x="1083.2" y="582.7" width="5.4" height="57.1" fill="#ffe2b4" opacity="0.19"/>
<rect x="1088.7" y="582.7" width="3.3" height="57.1" fill="#1c0f14" opacity=".28"/>
<rect x="1083.2" y="521.8" width="5.4" height="52.9" fill="#ffe2b4" opacity="0.19"/>
<rect x="1088.7" y="521.8" width="3.3" height="52.9" fill="#1c0f14" opacity=".28"/>
<rect x="1083.2" y="463.1" width="5.4" height="50.8" fill="#ffe2b4" opacity="0.19"/>
<rect x="1088.7" y="463.1" width="3.3" height="50.8" fill="#1c0f14" opacity=".28"/>
<path d="M1139.2,712.4 L1139.2,669.8 a13.5,13.5 0 0 1 27.1,0 L1166.3,712.4Z" fill="url(#arch)"/>
<path d="M1139.2,712.4 L1139.2,669.8 a13.5,13.5 0 0 1 13.5,-13.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.21"/>
<path d="M1139.2,640.3 L1139.2,604.9 a13.5,13.5 0 0 1 27.1,0 L1166.3,640.3Z" fill="url(#archUp)"/>
<path d="M1139.2,640.3 L1139.2,604.9 a13.5,13.5 0 0 1 13.5,-13.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.21"/>
<rect x="1128.3" y="648.1" width="5.3" height="61.2" fill="#ffe2b4" opacity="0.18"/>
<rect x="1133.6" y="648.1" width="3.2" height="61.2" fill="#1c0f14" opacity=".28"/>
<rect x="1128.3" y="583.1" width="5.3" height="57.0" fill="#ffe2b4" opacity="0.18"/>
<rect x="1133.6" y="583.1" width="3.2" height="57.0" fill="#1c0f14" opacity=".28"/>
<rect x="1128.3" y="522.3" width="5.3" height="52.8" fill="#ffe2b4" opacity="0.18"/>
<rect x="1133.6" y="522.3" width="3.2" height="52.8" fill="#1c0f14" opacity=".28"/>
<path d="M1182.1,712.7 L1182.1,669.5 a12.7,12.7 0 0 1 25.4,0 L1207.5,712.7Z" fill="url(#arch)"/>
<path d="M1182.1,712.7 L1182.1,669.5 a12.7,12.7 0 0 1 12.7,-12.7" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.18"/>
<ellipse cx="1194.8" cy="708.7" rx="10.2" ry="14.0" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1182.1,640.8 L1182.1,604.8 a12.7,12.7 0 0 1 25.4,0 L1207.5,640.8Z" fill="url(#archUp)"/>
<path d="M1182.1,640.8 L1182.1,604.8 a12.7,12.7 0 0 1 12.7,-12.7" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.18"/>
<rect x="1171.7" y="648.5" width="5.0" height="61.0" fill="#ffe2b4" opacity="0.16"/>
<rect x="1176.7" y="648.5" width="3.0" height="61.0" fill="#1c0f14" opacity=".28"/>
<rect x="1171.7" y="583.7" width="5.0" height="56.8" fill="#ffe2b4" opacity="0.16"/>
<rect x="1176.7" y="583.7" width="3.0" height="56.8" fill="#1c0f14" opacity=".28"/>
<path d="M1222.1,713.1 L1222.1,669.1 a11.6,11.6 0 0 1 23.3,0 L1245.4,713.1Z" fill="url(#arch)"/>
<path d="M1222.1,713.1 L1222.1,669.1 a11.6,11.6 0 0 1 11.6,-11.6" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.15"/>
<ellipse cx="1233.8" cy="709.1" rx="9.3" ry="12.8" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1222.1,641.5 L1222.1,604.6 a11.6,11.6 0 0 1 23.3,0 L1245.4,641.5Z" fill="url(#archUp)"/>
<path d="M1222.1,641.5 L1222.1,604.6 a11.6,11.6 0 0 1 11.6,-11.6" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.15"/>
<rect x="1212.5" y="649.1" width="4.7" height="60.8" fill="#ffe2b4" opacity="0.14"/>
<rect x="1217.2" y="649.1" width="2.8" height="60.8" fill="#1c0f14" opacity=".28"/>
<rect x="1212.5" y="584.5" width="4.7" height="56.6" fill="#ffe2b4" opacity="0.14"/>
<rect x="1217.2" y="584.5" width="2.8" height="56.6" fill="#1c0f14" opacity=".28"/>
<path d="M1258.7,713.7 L1258.7,668.7 a10.4,10.4 0 0 1 20.7,0 L1279.4,713.7Z" fill="url(#arch)"/>
<path d="M1258.7,713.7 L1258.7,668.7 a10.4,10.4 0 0 1 10.4,-10.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.12"/>
<ellipse cx="1269.0" cy="709.7" rx="8.3" ry="11.4" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1258.7,642.3 L1258.7,604.4 a10.4,10.4 0 0 1 20.7,0 L1279.4,642.3Z" fill="url(#archUp)"/>
<path d="M1258.7,642.3 L1258.7,604.4 a10.4,10.4 0 0 1 10.4,-10.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.12"/>
<rect x="1250.0" y="649.9" width="4.2" height="60.5" fill="#ffe2b4" opacity="0.13"/>
<rect x="1254.2" y="649.9" width="2.5" height="60.5" fill="#1c0f14" opacity=".28"/>
<rect x="1250.0" y="585.5" width="4.2" height="56.4" fill="#ffe2b4" opacity="0.13"/>
<rect x="1254.2" y="585.5" width="2.5" height="56.4" fill="#1c0f14" opacity=".28"/>
<path d="M1290.9,714.3 L1290.9,668.1 a8.9,8.9 0 0 1 17.7,0 L1308.7,714.3Z" fill="url(#arch)"/>
<path d="M1290.9,714.3 L1290.9,668.1 a8.9,8.9 0 0 1 8.9,-8.9" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.10"/>
<path d="M1290.9,643.3 L1290.9,604.2 a8.9,8.9 0 0 1 17.7,0 L1308.7,643.3Z" fill="url(#archUp)"/>
<path d="M1290.9,643.3 L1290.9,604.2 a8.9,8.9 0 0 1 8.9,-8.9" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.10"/>
<rect x="1283.4" y="650.8" width="3.7" height="60.2" fill="#ffe2b4" opacity="0.11"/>
<rect x="1287.1" y="650.8" width="2.2" height="60.2" fill="#1c0f14" opacity=".28"/>
<rect x="1283.4" y="586.7" width="3.7" height="56.1" fill="#ffe2b4" opacity="0.11"/>
<rect x="1287.1" y="586.7" width="2.2" height="56.1" fill="#1c0f14" opacity=".28"/>
<path d="M1318.3,715.0 L1318.3,667.5 a7.2,7.2 0 0 1 14.4,0 L1332.7,715.0Z" fill="url(#arch)"/>
<path d="M1318.3,715.0 L1318.3,667.5 a7.2,7.2 0 0 1 7.2,-7.2" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.07"/>
<path d="M1318.3,644.4 L1318.3,604.0 a7.2,7.2 0 0 1 14.4,0 L1332.7,644.4Z" fill="url(#archUp)"/>
<path d="M1318.3,644.4 L1318.3,604.0 a7.2,7.2 0 0 1 7.2,-7.2" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.07"/>
<rect x="1312.0" y="651.8" width="3.1" height="59.8" fill="#ffe2b4" opacity="0.09"/>
<rect x="1315.1" y="651.8" width="1.8" height="59.8" fill="#1c0f14" opacity=".28"/>
<rect x="1312.0" y="588.1" width="3.1" height="55.7" fill="#ffe2b4" opacity="0.09"/>
<rect x="1315.1" y="588.1" width="1.8" height="55.7" fill="#1c0f14" opacity=".28"/>
<path d="M1340.3,715.8 L1340.3,666.9 a5.4,5.4 0 0 1 10.8,0 L1351.1,715.8Z" fill="url(#arch)"/>
<path d="M1340.3,715.8 L1340.3,666.9 a5.4,5.4 0 0 1 5.4,-5.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.04"/>
<path d="M1340.3,645.5 L1340.3,603.7 a5.4,5.4 0 0 1 10.8,0 L1351.1,645.5Z" fill="url(#archUp)"/>
<path d="M1340.3,645.5 L1340.3,603.7 a5.4,5.4 0 0 1 5.4,-5.4" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.04"/>
<rect x="1335.4" y="652.9" width="2.4" height="59.4" fill="#ffe2b4" opacity="0.08"/>
<rect x="1337.8" y="652.9" width="1.4" height="59.4" fill="#1c0f14" opacity=".28"/>
<rect x="1335.4" y="589.6" width="2.4" height="55.4" fill="#ffe2b4" opacity="0.08"/>
<rect x="1337.8" y="589.6" width="1.4" height="55.4" fill="#1c0f14" opacity=".28"/>
<path d="M1356.4,716.5 L1356.4,666.2 a3.5,3.5 0 0 1 7.0,0 L1363.4,716.5Z" fill="url(#arch)"/>
<path d="M1356.4,716.5 L1356.4,666.2 a3.5,3.5 0 0 1 3.5,-3.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.01"/>
<ellipse cx="1359.9" cy="712.5" rx="2.8" ry="3.8" fill="#ffb45a" opacity=".5" filter="url(#blur2)"/>
<path d="M1356.4,646.8 L1356.4,603.5 a3.5,3.5 0 0 1 7.0,0 L1363.4,646.8Z" fill="url(#archUp)"/>
<path d="M1356.4,646.8 L1356.4,603.5 a3.5,3.5 0 0 1 3.5,-3.5" fill="none" stroke="#ffd9a0" stroke-width="1.3" opacity="0.01"/>
<rect x="1353.0" y="654.1" width="1.7" height="59.0" fill="#ffe2b4" opacity="0.06"/>
<rect x="1354.7" y="654.1" width="1.0" height="59.0" fill="#1c0f14" opacity=".28"/>
<rect x="1353.0" y="591.2" width="1.7" height="55.0" fill="#ffe2b4" opacity="0.06"/>
<rect x="1354.7" y="591.2" width="1.0" height="55.0" fill="#1c0f14" opacity=".28"/>
<polyline points="1119.8,458.4 1131.0,473.3 1142.0,488.1 1152.9,503.0 1163.6,517.8 1174.2,533.2 1184.7,548.6 1194.9,563.9 1205.0,579.3" fill="none" stroke="#ffd9a0" stroke-width="2" opacity=".5"/>
<ellipse cx="1070" cy="716" rx="370" ry="14" fill="#1a0d12" opacity=".55" filter="url(#blur8)"/>
<path d="M0,704 C300,694 700,712 1000,708 C1250,704 1450,712 1600,706 L1600,900 L0,900Z" fill="url(#ground)"/>
<path d="M620,730 Q800,735 980,730" fill="none" stroke="#ffcf96" stroke-width="0.60" opacity="0.020"/>
<path d="M574,731 Q800,737 1026,731" fill="none" stroke="#ffcf96" stroke-width="0.72" opacity="0.026"/>
<path d="M528,734 Q800,740 1072,734" fill="none" stroke="#ffcf96" stroke-width="0.84" opacity="0.032"/>
<path d="M482,739 Q800,746 1118,739" fill="none" stroke="#ffcf96" stroke-width="0.96" opacity="0.038"/>
<path d="M436,745 Q800,753 1164,745" fill="none" stroke="#ffcf96" stroke-width="1.08" opacity="0.044"/>
<path d="M390,754 Q800,763 1210,754" fill="none" stroke="#ffcf96" stroke-width="1.20" opacity="0.050"/>
<path d="M344,764 Q800,774 1256,764" fill="none" stroke="#ffcf96" stroke-width="1.32" opacity="0.056"/>
<path d="M298,777 Q800,787 1302,777" fill="none" stroke="#ffcf96" stroke-width="1.44" opacity="0.062"/>
<path d="M252,791 Q800,802 1348,791" fill="none" stroke="#ffcf96" stroke-width="1.56" opacity="0.068"/>
<path d="M206,807 Q800,819 1394,807" fill="none" stroke="#ffcf96" stroke-width="1.68" opacity="0.074"/>
<path d="M160,825 Q800,838 1440,825" fill="none" stroke="#ffcf96" stroke-width="1.80" opacity="0.080"/>
<path d="M114,845 Q800,859 1486,845" fill="none" stroke="#ffcf96" stroke-width="1.92" opacity="0.086"/>
<path d="M68,867 Q800,881 1532,867" fill="none" stroke="#ffcf96" stroke-width="2.04" opacity="0.092"/>
<path d="M22,891 Q800,906 1578,891" fill="none" stroke="#ffcf96" stroke-width="2.16" opacity="0.098"/>
<rect x="0" y="640" width="1600" height="130" fill="url(#mist)" filter="url(#blur8)"/>
<ellipse cx="430" cy="716" rx="330" ry="9" fill="#ffd9a0" opacity=".35" filter="url(#blur8)"/>
<path d="M-20,862 L420,862 L440,900 L-20,900Z" fill="#120a10"/>
<g filter="url(#rock)"><rect x="71.0" y="318" width="50" height="528" fill="url(#col)"/></g>
<rect x="78.1" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.23"/>
<rect x="85.3" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.28"/>
<rect x="92.4" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.33"/>
<rect x="99.6" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.38"/>
<rect x="106.7" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.43"/>
<rect x="113.9" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.48"/>
<path d="M71.0,318 C65.0,304 57.0,296 55.0,278 L137.0,278 C135.0,296 127.0,304 121.0,318Z" fill="url(#col)"/>
<path d="M70.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M83.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M96.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M109.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M122.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<rect x="51.0" y="270" width="90.0" height="9" fill="url(#col)"/>
<path d="M71.0,824 L60.0,836 L60.0,846 L132.0,846 L132.0,836 L121.0,824Z" fill="url(#col)"/><rect x="53.0" y="846" width="86.0" height="16" fill="#1a1016"/>
<g filter="url(#rock)"><rect x="181.0" y="318" width="50" height="528" fill="url(#col)"/></g>
<rect x="188.1" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.23"/>
<rect x="195.3" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.28"/>
<rect x="202.4" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.33"/>
<rect x="209.6" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.38"/>
<rect x="216.7" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.43"/>
<rect x="223.9" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.48"/>
<path d="M181.0,318 C175.0,304 167.0,296 165.0,278 L247.0,278 C245.0,296 237.0,304 231.0,318Z" fill="url(#col)"/>
<path d="M180.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M193.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M206.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M219.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M232.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<rect x="161.0" y="270" width="90.0" height="9" fill="url(#col)"/>
<path d="M181.0,824 L170.0,836 L170.0,846 L242.0,846 L242.0,836 L231.0,824Z" fill="url(#col)"/><rect x="163.0" y="846" width="86.0" height="16" fill="#1a1016"/>
<g filter="url(#rock)"><rect x="291.0" y="318" width="50" height="528" fill="url(#col)"/></g>
<rect x="298.1" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.23"/>
<rect x="305.3" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.28"/>
<rect x="312.4" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.33"/>
<rect x="319.6" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.38"/>
<rect x="326.7" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.43"/>
<rect x="333.9" y="324" width="1.4" height="516" fill="#0c060a" opacity="0.48"/>
<path d="M291.0,318 C285.0,304 277.0,296 275.0,278 L357.0,278 C355.0,296 347.0,304 341.0,318Z" fill="url(#col)"/>
<path d="M290.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M303.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M316.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M329.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<path d="M342.0,314 q-5.0,-16 1.0,-30" fill="none" stroke="#0c060a" stroke-width="1.6" opacity=".4"/>
<rect x="271.0" y="270" width="90.0" height="9" fill="url(#col)"/>
<path d="M291.0,824 L280.0,836 L280.0,846 L352.0,846 L352.0,836 L341.0,824Z" fill="url(#col)"/><rect x="273.0" y="846" width="86.0" height="16" fill="#1a1016"/>
<g filter="url(#rock)"><rect x="42" y="214" width="330" height="56" fill="url(#col)"/></g>
<rect x="42" y="232" width="330" height="3" fill="#0c060a" opacity=".45"/><rect x="42" y="250" width="330" height="3" fill="#0c060a" opacity=".45"/>
<path d="M30,214 L384,214 L396,196 L18,196Z" fill="url(#col)"/><rect x="14" y="186" width="388" height="11" fill="url(#col)"/>
<rect x="34" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="50" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="66" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="82" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="98" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="114" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="130" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="146" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="162" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="178" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="194" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="210" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="226" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="242" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="258" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="274" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="290" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="306" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="322" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="338" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="354" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<rect x="370" y="202" width="8" height="8" fill="#0c060a" opacity=".45"/>
<path d="M14,186 L402,186 L402,188 L14,188Z" fill="#ffd9a0" opacity=".6"/>
<path d="M372,214 l12,6 l-6,12 l10,8 l-8,14 l6,10 l-14,6 Z" fill="#170e15"/>
<ellipse cx="530" cy="874" rx="80" ry="8" fill="#000" opacity=".4" filter="url(#blur8)"/>
<g filter="url(#rock)"><ellipse cx="520" cy="852" rx="58" ry="20" fill="#3a2428"/><rect x="462" y="822" width="116" height="30" fill="url(#col)"/><ellipse cx="520" cy="822" rx="58" ry="18" fill="#7a4d3c"/></g>
<ellipse cx="520" cy="822" rx="40" ry="11" fill="none" stroke="#1a0f14" stroke-width="1.5" opacity=".5"/>
<g fill="#0f0a10" opacity="1"><path d="M1496,880 L1500,570 L1518,570 L1520,880Z"/><path d="M1505,644 L1591,536 L1600,545 L1518,665Z"/><path d="M1505,622 L1428,527 L1436,519 L1514,605Z"/><ellipse cx="1505" cy="510" rx="155" ry="52"/><ellipse cx="1419" cy="527" rx="86" ry="37"/><ellipse cx="1600" cy="523" rx="86" ry="34"/><ellipse cx="1522" cy="480" rx="95" ry="30"/></g>
<g fill="#150d15" opacity="1"><path d="M1386,872 L1388,656 L1401,656 L1402,872Z"/><path d="M1392,707 L1452,632 L1458,638 L1401,722Z"/><path d="M1392,692 L1338,626 L1344,620 L1398,680Z"/><ellipse cx="1392" cy="614" rx="108" ry="36"/><ellipse cx="1332" cy="626" rx="60" ry="26"/><ellipse cx="1458" cy="623" rx="60" ry="24"/><ellipse cx="1404" cy="593" rx="66" ry="21"/></g>
<path d="M1568,880 C1529,790 1547,640 1568,580 C1589,640 1607,790 1568,880Z" fill="#0d080e" opacity="1"/>
<path d="M1300,880 C1278,829 1288,744 1300,710 C1312,744 1322,829 1300,880Z" fill="#0d080e" opacity="1"/>
<path d="M1336,880 C1309,817 1321,712 1336,670 C1351,712 1363,817 1336,880Z" fill="#0d080e" opacity="1"/>
<g filter="url(#rock)"><rect x="1218" y="772" width="46" height="96" fill="url(#colL)"/></g><path d="M1218,772 l10,-12 l9,8 l10,-14 l8,10 l9,-4 l0,12Z" fill="#6b4338"/><rect x="1204" y="868" width="74" height="14" fill="#1a1016"/>
<path d="M1150,884 C1300,868 1480,872 1600,862 L1600,900 L1150,900Z" fill="#0b070b"/>
<path d="M828,872 q-2.3,-6.4 3.9,-10.7" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M583,897 q-2.9,-9.2 -0.4,-15.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1061,896 q-1.4,-6.3 -2.9,-10.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1158,860 q-2.1,-7.1 0.2,-11.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1164,856 q0.1,-8.5 3.9,-14.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M972,861 q-0.1,-9.0 -4.8,-15.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M433,874 q-1.2,-6.3 -3.6,-10.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M695,865 q-3.0,-8.6 2.5,-14.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1076,856 q1.3,-9.2 4.0,-15.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M653,868 q3.0,-6.0 0.9,-9.9" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M708,871 q-2.7,-5.3 -4.0,-8.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1073,864 q-1.5,-9.2 -2.3,-15.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M823,859 q2.7,-5.8 3.8,-9.7" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1055,880 q2.6,-9.1 0.5,-15.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M984,852 q-0.3,-8.0 2.5,-13.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M926,864 q2.6,-3.9 -3.7,-6.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M794,866 q1.4,-5.4 4.8,-9.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M630,881 q0.3,-5.4 -1.1,-9.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M559,858 q2.4,-4.8 -0.0,-8.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M599,894 q-0.3,-9.6 -3.6,-16.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M578,854 q-2.5,-5.7 -2.6,-9.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M629,877 q1.5,-8.9 -0.9,-14.9" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M749,875 q-1.0,-5.9 -4.4,-9.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M644,896 q0.0,-4.4 1.3,-7.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1094,860 q-1.5,-5.2 -1.0,-8.7" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M773,896 q2.2,-8.7 -4.8,-14.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M455,884 q-0.2,-9.0 0.9,-15.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M430,869 q2.0,-9.2 3.6,-15.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1179,862 q-2.1,-4.3 0.2,-7.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M955,895 q0.9,-7.9 2.6,-13.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M782,876 q1.7,-3.8 -2.7,-6.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1138,881 q-2.2,-5.4 -2.5,-9.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M920,884 q-2.6,-4.3 0.2,-7.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M879,869 q0.6,-4.9 -4.9,-8.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M662,872 q0.9,-9.4 3.8,-15.6" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M796,861 q2.8,-5.1 2.0,-8.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M667,851 q1.0,-6.6 -0.8,-11.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M628,882 q-1.6,-9.2 -4.7,-15.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M690,870 q-1.8,-7.7 3.0,-12.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M999,874 q2.8,-4.8 -1.9,-8.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1061,861 q1.6,-4.9 -2.1,-8.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1163,874 q-1.7,-4.7 -0.8,-7.9" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M942,896 q-0.6,-4.5 -2.9,-7.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1180,857 q-2.6,-3.9 -1.1,-6.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1122,892 q3.0,-8.0 4.3,-13.3" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M684,859 q1.5,-9.2 -4.7,-15.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M942,868 q-1.0,-5.8 -3.3,-9.7" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M432,863 q2.7,-5.7 -3.8,-9.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1172,860 q1.9,-5.7 3.2,-9.6" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M763,852 q-0.8,-6.4 4.2,-10.7" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M579,867 q-2.8,-9.0 -0.9,-15.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1055,887 q-2.8,-3.8 -4.4,-6.4" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1138,862 q2.4,-8.1 -1.6,-13.5" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M640,896 q-1.4,-7.3 2.2,-12.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M674,863 q1.5,-3.6 4.2,-6.0" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M918,895 q-1.6,-3.7 -0.2,-6.2" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1167,896 q-1.5,-5.9 -0.7,-9.9" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M810,895 q1.8,-4.7 2.4,-7.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M1064,887 q-1.0,-7.2 -1.8,-12.1" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<path d="M709,888 q-1.8,-4.1 2.5,-6.8" stroke="#0e080c" stroke-width="1.3" fill="none" opacity=".8"/>
<rect width="1600" height="900" filter="url(#grain)" opacity=".55"/>
</svg>
</div>
<div class="wrap">
  <header>
    <svg class="logo" viewBox="0 0 64 64" aria-hidden="true">
      <defs><linearGradient id="lg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fff0c8"/><stop offset=".55" stop-color="#f0c987"/><stop offset="1" stop-color="#b8873c"/></linearGradient></defs>
      <g fill="url(#lg)">
        <g id="lf"><ellipse cx="13" cy="44" rx="3.4" ry="7.5" transform="rotate(-38 13 44)"/><ellipse cx="8.5" cy="35" rx="3.4" ry="7.5" transform="rotate(-18 8.5 35)"/><ellipse cx="8" cy="25" rx="3.4" ry="7.5" transform="rotate(4 8 25)"/><ellipse cx="11.5" cy="15.5" rx="3.4" ry="7.5" transform="rotate(26 11.5 15.5)"/><ellipse cx="18.5" cy="8.5" rx="3.2" ry="7" transform="rotate(50 18.5 8.5)"/><ellipse cx="21" cy="51" rx="3.2" ry="7" transform="rotate(-62 21 51)"/></g>
        <use href="#lf" transform="translate(64 0) scale(-1 1)"/>
        <path d="M22 22h5.2l3 14.5L33.4 22h4.2l3.2 14.5 3-14.5H49l-5.4 22h-5.2L35.5 31 32.6 44h-5.2z" transform="translate(-3.5 0)"/>
        <rect x="27" y="55" width="10" height="2.6" rx="1.3"/>
      </g>
    </svg>
    <div>
      <h1 id="title">Warehouse Sync Server</h1>
      <div class="sub" id="subtitle">Loading…</div>
    </div>
    <div class="live"><span class="dot" id="livedot"></span><span id="livetxt">live</span></div>
    <button id="stopBtn" style="display:none">Stop server</button>
  </header>
  <div class="rule">SYNC &nbsp;·&nbsp; PRINT &nbsp;·&nbsp; PROTECT</div>

  <div class="off-banner" id="offBanner">Can't reach the server — retrying…</div>

  <div class="grid tiles">
    <div class="card tile"><div class="ic"><svg viewBox="0 0 24 24"><rect x="3" y="4" width="13" height="10" rx="1.5"/><path d="M1.5 18h16"/><rect x="18" y="8" width="4.5" height="10" rx="1.2"/></svg></div><div><div class="k">Connected now</div><div class="v" id="tConn">—</div></div></div>
    <div class="card tile"><div class="ic"><svg viewBox="0 0 24 24"><path d="M12 2.5 3.5 7v10L12 21.5 20.5 17V7z"/><path d="M3.5 7 12 11.5 20.5 7M12 11.5v10"/></svg></div><div><div class="k">Items</div><div class="v" id="tItems">—</div></div></div>
    <div class="card tile"><div class="ic"><svg viewBox="0 0 24 24"><path d="M4 8h14l-3.5-3.5M20 16H6l3.5 3.5"/></svg></div><div><div class="k">Movements</div><div class="v" id="tMov">—</div></div></div>
    <div class="card tile"><div class="ic"><svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 1 1-2.6-5.9"/><path d="M20 4v5h-5"/><path d="M12 8v4.5l3 1.8"/></svg></div><div><div class="k">Revision</div><div class="v" id="tRev">—</div></div></div>
  </div>

  <div class="grid cards">
    <div class="card">
      <h2>Connected devices</h2>
      <div id="devices"><div class="empty">No devices have synced yet.</div></div>
    </div>
    <div>
      <div class="card" style="margin-bottom:14px">
        <h2>Pairing code</h2>
        <div class="code" id="code">— — —</div>
        <div id="addrs" style="margin-top:12px"></div>
      </div>
      <div class="card signin">
        <h2>Sign-in</h2>
        <div id="signin"></div>
      </div>
    </div>
  </div>

  <div class="card" id="prnCard" style="margin-top:14px;display:none">
    <h2>Shared printers</h2>
    <div class="sub" style="margin-bottom:6px">Installed on this PC and available to every paired device over Wi-Fi.</div>
    <div id="printers"></div>
  </div>

  <div class="card" id="usbCard" style="margin-top:14px;display:none">
    <h2>Phones linked by USB cable</h2>
    <div id="usb"></div>
  </div>

  <div class="foot" id="foot"></div>
</div>

<script>
  var skew = 0; // server clock - browser clock
  function esc(s){var d=document.createElement('div');d.textContent=s==null?'':String(s);return d.innerHTML;}
  function ago(ms){
    var d=Math.max(0,(Date.now()+skew)-ms)/1000;
    if(d<5)return'just now';
    if(d<60)return Math.floor(d)+'s ago';
    if(d<3600)return Math.floor(d/60)+'m ago';
    if(d<86400)return Math.floor(d/3600)+'h ago';
    return Math.floor(d/86400)+'d ago';
  }
  function roleLabel(r){return r==='admin'?'Admin':r==='office'?'Office':r==='warehouse'?'Warehouse':esc(r);}

  function render(s){
    skew = s.nowMs - Date.now();
    document.getElementById('offBanner').style.display='none';
    var co = s.signin && s.signin.company;
    var company = (co && (co.name || co.company)) ? (co.name || co.company) : '';
    document.getElementById('title').textContent = company || s.name || 'Warehouse Sync Server';
    document.getElementById('subtitle').textContent = 'v'+s.version+(s.phoneVersion?('  ·  phone app '+s.phoneVersion):'');

    var online = (s.devices||[]).filter(function(d){return d.online}).length;
    document.getElementById('tConn').innerHTML = online+' <small>/ '+(s.devices||[]).length+' total</small>';
    document.getElementById('tItems').textContent = (s.counts.items||0).toLocaleString();
    document.getElementById('tMov').textContent = (s.counts.movements||0).toLocaleString();
    document.getElementById('tRev').textContent = s.counts.revision||0;

    var dev = document.getElementById('devices');
    if(!s.devices || !s.devices.length){ dev.innerHTML='<div class="empty">No devices have synced yet.</div>'; }
    else dev.innerHTML = s.devices.map(function(d){
      return '<div class="row"><span class="dot '+(d.online?'':'off')+'" style="animation:'+(d.online?'':'none')+'"></span>'+
        '<span class="name">'+esc(d.name)+'</span>'+
        '<span class="pill '+(d.online?'on':'idle')+'">'+(d.online?'online':'idle')+'</span>'+
        '<span class="seen" data-last="'+d.last+'">'+ago(d.last)+'</span></div>';
    }).join('');

    document.getElementById('code').textContent = s.code||'—';
    var addrs = (s.ips||[]).map(function(ip){return '<div class="addr">http://'+esc(ip)+':'+s.port+'</div>';});
    addrs.push('<div class="addr">http://localhost:'+s.port+'</div>');
    document.getElementById('addrs').innerHTML = addrs.join('');

    var si = document.getElementById('signin');
    if(s.signin && s.signin.configured){
      var roles = (s.signin.users||[]).map(function(u){
        return '<div class="row"><span class="name">'+esc(u.name)+'</span>'+
          '<span class="pill role">'+roleLabel(u.role)+'</span>'+
          (u.disabled?'<span class="pill idle" style="margin-left:6px">disabled</span>':'')+'</div>';
      }).join('');
      si.innerHTML='<div class="big" style="color:var(--ok)">Sign-in ON</div>'+
        '<div class="sub" style="margin-bottom:6px">'+(s.signin.users.length)+' user(s), '+s.signin.admins+' admin</div>'+roles;
    } else {
      si.innerHTML='<div class="big">Legacy mode</div><div class="sub">Sign-in is off. Devices pair with the code only. '+
        'Turn it on in the app: Settings → Set up sign-in.</div>';
    }

    var usbCard=document.getElementById('usbCard');
    if(s.usb && s.usb.length){
      usbCard.style.display='';
      document.getElementById('usb').innerHTML=s.usb.map(function(u){
        return '<div class="row"><span class="name">'+esc(u.model||u.serial)+'</span><span class="seen">'+esc(u.serial)+'</span></div>';
      }).join('');
    } else usbCard.style.display='none';

    var prnCard=document.getElementById('prnCard');
    if(s.printers && s.printers.length){
      prnCard.style.display='';
      document.getElementById('printers').innerHTML=s.printers.map(function(n){
        return '<div class="row"><span class="name">'+esc(n)+'</span>'+
          '<button class="tbtn" data-prn="'+encodeURIComponent(n)+'">Test print</button></div>';
      }).join('');
      Array.prototype.forEach.call(document.querySelectorAll('.tbtn'),function(btn){
        btn.onclick=function(){
          btn.disabled=true; var old=btn.textContent; btn.textContent='Printing…';
          fetch('/api/print-test?printer='+btn.dataset.prn,{method:'POST'}).then(function(r){return r.json();}).then(function(j){
            btn.textContent=(j&&j.ok)?'Sent ✓':'Failed'; if(j&&j.error)alert('Test print failed:\n'+j.error);
            setTimeout(function(){btn.textContent=old;btn.disabled=false;},2500);
          }).catch(function(){btn.textContent='Failed';setTimeout(function(){btn.textContent=old;btn.disabled=false;},2500);});
        };
      });
    } else prnCard.style.display='none';

    document.getElementById('foot').innerHTML='<b>Data folder:</b> '+esc(s.dataDir)+
      '<br>Install the phone app: open <b>&lt;address above&gt;/phone</b> in the phone\'s browser. Keep this window — and the server — running.';
  }

  function tick(){ // keep "x ago" fresh between fetches
    document.querySelectorAll('.seen[data-last]').forEach(function(el){ el.textContent=ago(+el.dataset.last); });
  }
  function fail(){
    document.getElementById('offBanner').style.display='';
    document.getElementById('livedot').className='dot off';
    document.getElementById('livetxt').textContent='reconnecting';
  }
  function poll(){
    fetch('/status.json',{cache:'no-store'}).then(function(r){return r.json();}).then(function(s){
      document.getElementById('livedot').className='dot';
      document.getElementById('livetxt').textContent='live';
      render(s);
    }).catch(fail);
  }
  // The server can only be stopped from the PC it runs on, so only show the control there.
  var onServer = ['localhost','127.0.0.1','::1','[::1]'].indexOf(location.hostname)>=0;
  var stopBtn = document.getElementById('stopBtn');
  if(onServer){
    stopBtn.style.display='';
    stopBtn.onclick=function(){
      if(!confirm('Stop the Warehouse Sync Server?\n\nPhones and PCs will not be able to sync until it is started again.'))return;
      stopBtn.disabled=true; stopBtn.textContent='Stopping…';
      fetch('/api/shutdown',{method:'POST'}).catch(function(){}).then(function(){
        var o=document.createElement('div'); o.className='stopped';
        o.innerHTML='<div>Server stopped.<br><span style="font-size:14px">You can close this tab. Start it again by running the Warehouse Sync Server.</span></div>';
        document.body.appendChild(o);
      });
    };
  }
  poll(); setInterval(poll,3000); setInterval(tick,1000);
</script>
</body></html>`
