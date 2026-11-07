package main

import (
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"time"
)

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
	_ = cmd.Start()
}

const dashboardHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Warehouse Sync Server</title>
<style>
  :root{
    --bg:#0b1020; --card:rgba(22,30,53,.66); --card2:rgba(30,40,70,.5);
    --brd:rgba(120,150,220,.16); --ink:#eaf0ff; --muted:#93a0c4; --faint:#6b789f;
    --accent:#5b8cff; --accent2:#7a6bff; --ok:#39d98a; --okdim:rgba(57,217,138,.16);
    --warn:#ffb020; --r:16px;
  }
  *{box-sizing:border-box}
  html,body{margin:0}
  body{
    font-family:"Segoe UI",system-ui,-apple-system,Roboto,Arial,sans-serif;
    color:var(--ink); background:var(--bg); min-height:100vh; padding:28px 20px 48px;
  }
  body::before{
    content:""; position:fixed; inset:0; z-index:-1;
    background:
      radial-gradient(60vw 50vh at 12% -8%, rgba(91,140,255,.22), transparent 60%),
      radial-gradient(55vw 45vh at 108% 4%, rgba(122,107,255,.20), transparent 55%),
      radial-gradient(70vw 60vh at 50% 120%, rgba(57,217,138,.10), transparent 60%),
      var(--bg);
  }
  .wrap{max-width:1040px;margin:0 auto}
  header{display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:22px}
  .logo{width:42px;height:42px;border-radius:12px;background:linear-gradient(135deg,var(--accent),var(--accent2));
    display:grid;place-items:center;font-weight:800;font-size:19px;box-shadow:0 8px 24px rgba(91,140,255,.35)}
  h1{font-size:20px;margin:0;font-weight:700;letter-spacing:.2px}
  .sub{color:var(--muted);font-size:13px;margin-top:2px}
  .live{margin-left:auto;display:flex;align-items:center;gap:8px;font-size:13px;color:var(--muted)}
  .dot{width:9px;height:9px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 0 rgba(57,217,138,.6);animation:pulse 2s infinite}
  .dot.off{background:var(--warn);animation:none}
  @keyframes pulse{0%{box-shadow:0 0 0 0 rgba(57,217,138,.55)}70%{box-shadow:0 0 0 10px rgba(57,217,138,0)}100%{box-shadow:0 0 0 0 rgba(57,217,138,0)}}
  .grid{display:grid;gap:14px}
  .tiles{grid-template-columns:repeat(4,1fr)}
  .cards{grid-template-columns:1.3fr 1fr;margin-top:14px}
  @media(max-width:760px){.tiles{grid-template-columns:repeat(2,1fr)}.cards{grid-template-columns:1fr}}
  .card{background:var(--card);border:1px solid var(--brd);border-radius:var(--r);padding:18px 18px;
    backdrop-filter:blur(14px) saturate(1.3);-webkit-backdrop-filter:blur(14px) saturate(1.3)}
  .tile .k{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.6px}
  .tile .v{font-size:30px;font-weight:750;margin-top:6px;font-variant-numeric:tabular-nums}
  .tile .v small{font-size:13px;color:var(--faint);font-weight:500}
  .card h2{font-size:13px;text-transform:uppercase;letter-spacing:.6px;color:var(--muted);margin:0 0 12px}
  .code{font-family:"Cascadia Code",Consolas,monospace;font-size:26px;font-weight:700;letter-spacing:3px;
    color:#fff;background:var(--card2);border:1px solid var(--brd);border-radius:12px;padding:10px 14px;display:inline-block}
  .addr{font-family:Consolas,monospace;font-size:14px;color:#cfe0ff;background:var(--card2);
    border:1px solid var(--brd);border-radius:9px;padding:7px 10px;margin:5px 0;display:inline-block}
  .row{display:flex;align-items:center;gap:10px;padding:10px 0;border-top:1px solid var(--brd)}
  .row:first-of-type{border-top:0}
  .name{font-weight:600}
  .seen{margin-left:auto;color:var(--muted);font-size:13px;font-variant-numeric:tabular-nums}
  .pill{font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.5px;padding:3px 9px;border-radius:999px}
  .pill.on{background:var(--okdim);color:var(--ok)}
  .pill.idle{background:rgba(255,176,32,.14);color:var(--warn)}
  .pill.role{background:rgba(91,140,255,.16);color:#bcd0ff}
  .empty{color:var(--faint);font-size:14px;padding:8px 0}
  .signin .big{font-size:16px;font-weight:700;margin-bottom:8px}
  .foot{color:var(--faint);font-size:12px;margin-top:18px;line-height:1.7}
  .foot b{color:var(--muted);font-weight:600}
  .off-banner{display:none;background:rgba(255,176,32,.12);border:1px solid rgba(255,176,32,.3);
    color:#ffd98a;border-radius:10px;padding:9px 13px;font-size:13px;margin-bottom:16px}
</style></head>
<body><div class="wrap">
  <header>
    <div class="logo">W</div>
    <div>
      <h1 id="title">Warehouse Sync Server</h1>
      <div class="sub" id="subtitle">Loading…</div>
    </div>
    <div class="live"><span class="dot" id="livedot"></span><span id="livetxt">live</span></div>
  </header>

  <div class="off-banner" id="offBanner">Can't reach the server — retrying…</div>

  <div class="grid tiles">
    <div class="card tile"><div class="k">Connected now</div><div class="v" id="tConn">—</div></div>
    <div class="card tile"><div class="k">Items</div><div class="v" id="tItems">—</div></div>
    <div class="card tile"><div class="k">Movements</div><div class="v" id="tMov">—</div></div>
    <div class="card tile"><div class="k">Revision</div><div class="v" id="tRev">—</div></div>
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
  poll(); setInterval(poll,3000); setInterval(tick,1000);
</script>
</body></html>`
