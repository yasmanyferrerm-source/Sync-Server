package main

import (
	_ "embed"
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

//go:embed assets/rome.jpg
var dashBG []byte

// dashBGHandler serves the dashboard's background painting (embedded, so it works offline).
func (s *server) dashBGHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(dashBG)
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
    --bg:#2a070b; --card:rgba(74,10,16,.76); --card2:rgba(34,4,8,.55);
    --brd:rgba(240,201,135,.50); --ink:#fff7ea; --muted:#f0dcbb; --faint:#d6bd98;
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
  /* Background: Thomas Cole, The Consummation of Empire (1836, public domain). A crimson veil keeps text readable. */
  .scene{position:fixed; inset:0; z-index:-2; overflow:hidden; background:var(--bg)}
  .scene img{position:absolute; inset:0; width:100%; height:100%; object-fit:cover; object-position:50% 40%; filter:saturate(1.12) contrast(1.05)}
  body::before{
    content:""; position:fixed; inset:0; z-index:-1; pointer-events:none;
    background:
      linear-gradient(180deg, rgba(58,6,12,.45) 0%, rgba(70,8,14,.06) 16%, rgba(60,8,12,.04) 40%, rgba(52,6,10,.30) 72%, rgba(34,4,8,.88) 100%),
      radial-gradient(120vw 95vh at 50% 40%, rgba(0,0,0,0) 55%, rgba(40,4,8,.42) 100%);
  }
  .wrap{max-width:1080px;margin:0 auto}
  header{display:flex;align-items:center;gap:16px;flex-wrap:wrap;padding:14px 20px;border-radius:var(--r);
    background:linear-gradient(170deg,rgba(142,24,30,.90),rgba(74,10,16,.90) 60%,rgba(52,6,12,.92));border:1px solid var(--brd);
    box-shadow:0 18px 40px rgba(30,2,6,.45), inset 0 1px 0 rgba(255,226,170,.30);backdrop-filter:blur(9px);-webkit-backdrop-filter:blur(9px)}
  .logo{width:58px;height:58px;flex:none;filter:drop-shadow(0 6px 18px rgba(240,190,110,.35))}
  h1{font-family:var(--serif);font-size:25px;margin:0;font-weight:600;letter-spacing:.14em;text-transform:uppercase;
    background:linear-gradient(180deg,#fff3d6,#f0c987 55%,#c8964a);-webkit-background-clip:text;background-clip:text;color:transparent;text-shadow:none;
    filter:drop-shadow(0 2px 6px rgba(10,5,10,.6))}
  .sub{color:var(--muted);font-size:13px;margin-top:3px;letter-spacing:.02em}
  .live{margin-left:auto;display:flex;align-items:center;gap:9px;font-size:12px;font-weight:700;letter-spacing:.14em;text-transform:uppercase;
    color:var(--ink);background:rgba(60,8,14,.82);border:1px solid var(--brd);border-radius:999px;padding:8px 15px;
    backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
  .dot{width:9px;height:9px;flex:none;border-radius:50%;background:var(--ok);box-shadow:0 0 0 0 rgba(116,230,168,.6);animation:pulse 2s infinite}
  .dot.off{background:var(--warn);animation:none}
  @keyframes pulse{0%{box-shadow:0 0 0 0 rgba(116,230,168,.55)}70%{box-shadow:0 0 0 10px rgba(116,230,168,0)}100%{box-shadow:0 0 0 0 rgba(116,230,168,0)}}
  .rule{display:flex;align-items:center;gap:14px;margin:clamp(70px,17vh,190px) 0 16px;color:var(--gold);text-shadow:0 1px 4px rgba(30,2,6,.9);font-family:var(--serif);font-size:12px;font-weight:700;letter-spacing:.42em;color:#fff3d6;text-shadow:0 1px 3px rgba(20,2,4,1),0 0 14px rgba(40,4,8,.95)}
  .rule::before,.rule::after{content:none;flex:1;height:2px;background:linear-gradient(90deg,transparent,#f0c987);box-shadow:0 1px 4px rgba(20,2,4,.8)}
  .rule::after{background:linear-gradient(270deg,transparent,#f0c987)}
  .grid{display:grid;gap:16px}
  .grid>*{min-width:0}
  .tiles{grid-template-columns:repeat(4,minmax(0,1fr))}
  .cards{grid-template-columns:1.3fr 1fr;margin-top:16px}
  @media(max-width:760px){.tiles{grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.cards{grid-template-columns:1fr}h1{font-size:17px;letter-spacing:.1em}.live{margin-left:0}
    body{padding:16px 12px 40px}header{padding:12px 14px}.logo{width:46px;height:46px}.code{font-size:24px;letter-spacing:4px;padding:10px 14px 10px 18px}}
  @media(max-width:520px){.tile{padding:13px 12px;gap:10px}.tile .ic{width:34px;height:34px;border-radius:10px}.tile .ic svg{width:18px;height:18px}.tile .v{font-size:16.5px}.tile .k{font-size:9.5px;letter-spacing:.1em}header>div:not(.live){min-width:0;flex:1 1 calc(100% - 70px)}}
  .card{position:relative;background:linear-gradient(170deg,rgba(142,24,30,.82),var(--card) 55%,rgba(52,6,12,.86));border:1px solid var(--brd);border-radius:var(--r);padding:20px 20px;
    backdrop-filter:blur(9px) saturate(1.2);-webkit-backdrop-filter:blur(9px) saturate(1.2);
    box-shadow:0 22px 48px rgba(30,2,6,.50), inset 0 1px 0 rgba(255,226,170,.30), inset 0 0 0 1px rgba(60,6,10,.5)}
  .card::before{content:"";position:absolute;left:18px;right:18px;top:-1px;height:1px;background:linear-gradient(90deg,transparent,rgba(255,226,170,.75),transparent)}
  .tile{display:flex;align-items:center;gap:14px;padding:18px 18px}
  .tile .ic{width:44px;height:44px;flex:none;border-radius:13px;display:grid;place-items:center;
    background:linear-gradient(145deg,#f6d79c,#c8964a);color:#5c0f14;border:1px solid rgba(255,236,190,.7);box-shadow:0 4px 12px rgba(30,2,6,.4)}
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
  .row{display:flex;align-items:center;gap:10px;padding:11px 0;border-top:1px solid rgba(240,200,130,.24)}
  .row:first-of-type{border-top:0}
  .name{font-weight:600}
  .seen{margin-left:auto;color:var(--muted);font-size:13px;font-variant-numeric:tabular-nums}
  .pill{font-size:10.5px;font-weight:800;text-transform:uppercase;letter-spacing:.1em;padding:4px 10px;border-radius:999px;text-shadow:none}
  .pill.on{background:rgba(10,40,24,.6);color:var(--ok);border:1px solid rgba(116,230,168,.45)}
  .pill.idle{background:rgba(40,20,4,.55);color:var(--warn);border:1px solid rgba(255,182,72,.45)}
  .pill.role{background:rgba(240,201,135,.18);color:var(--gold);border:1px solid rgba(240,201,135,.5)}
  .empty{color:var(--faint);font-size:14px;padding:8px 0}
  .signin .big{font-size:16px;font-weight:700;margin-bottom:8px}
  .foot{color:var(--muted);font-size:12px;margin-top:20px;line-height:1.7;text-align:center;text-shadow:0 1px 4px rgba(20,2,4,.95)}
  .foot b{color:var(--ink);font-weight:600}
  .off-banner{display:none;background:rgba(60,34,10,.7);border:1px solid rgba(255,182,72,.45);
    color:#ffe0a6;border-radius:12px;padding:10px 14px;font-size:13px;margin-bottom:16px;backdrop-filter:blur(10px)}
  #stopBtn{background:rgba(24,6,8,.82);color:var(--gold);border:1px solid rgba(240,201,135,.6);
    border-radius:999px;padding:9px 16px;font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;cursor:pointer;
    backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);transition:background .15s}
  #stopBtn:hover{background:rgba(142,24,30,.9)}
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
<div class="scene" aria-hidden="true"><img src="/dash-bg.jpg" alt=""></div>
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
  <div class="rule" aria-hidden="true"></div>

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
