package main

// ---------------------------------------------------------------------------
// Document scanner (Windows WIA) + USB-phone watch (adb), reconstructed to match
// what the app expects:
//   GET  /api/scanners -> {supported, scanners:[{id,name}], error?}
//   POST /api/scan {device,source,color,dpi} -> {pages:[<base64 jpeg>], source?, error?}
// Both are local-only (never reachable through the public tunnel). The real work
// is done by Windows PowerShell/WIA, so this must be tested on the Windows PC
// with the scanner attached.
// ---------------------------------------------------------------------------

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// localOnly restricts a handler to this PC / its LAN — the scanner must never be
// reachable through the public Cloudflare tunnel.
func localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Scanning only works on the PC that runs the Warehouse Sync Server"})
			return
		}
		h(w, r)
	}
}

func runPowerShell(args ...string) ([]byte, error) {
	base := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}
	cmd := exec.Command("powershell", append(base, args...)...)
	hideWindow(cmd)
	return cmd.Output()
}

// GET /api/scanners — enumerate attached WIA scanners.
func (s *server) scanners(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, 200, map[string]any{"supported": false, "scanners": []any{}})
		return
	}
	script := `$ErrorActionPreference='Stop'
try {
  $dm = New-Object -ComObject WIA.DeviceManager
  $out = @()
  foreach ($d in $dm.DeviceInfos) {
    if ($d.Type -eq 1) {
      $name = ''
      try { $name = [string]$d.Properties.Item('Name').Value } catch {}
      $out += [pscustomobject]@{ id = [string]$d.DeviceID; name = $name }
    }
  }
  if ($out.Count -eq 0) { '[]' } else { ConvertTo-Json @($out) -Compress }
} catch { '[]' }`
	out, err := runPowerShell("-Command", script)
	if err != nil {
		writeJSON(w, 200, map[string]any{"supported": true, "scanners": []any{}, "error": "Couldn't reach Windows scanning (WIA). Check the scanner is on and shows in Windows Settings → Printers & scanners."})
		return
	}
	var list []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(out))), &list)
	if list == nil {
		list = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"supported": true, "scanners": list})
}

// POST /api/scan — acquire page(s) from the chosen scanner and return them as base64 JPEGs.
func (s *server) scan(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Scanning works on the Windows PC that runs the server."})
		return
	}
	var req struct {
		Device, Source, Color string
		Dpi                   int
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Dpi < 72 || req.Dpi > 1200 {
		req.Dpi = 200
	}
	dir, err := os.MkdirTemp("", "wss-scan-")
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "Couldn't make a temporary folder for the scan"})
		return
	}
	defer os.RemoveAll(dir)
	ps := filepath.Join(dir, "scan.ps1")
	if err := os.WriteFile(ps, []byte(scanScript), 0o600); err != nil {
		writeJSON(w, 500, map[string]any{"error": "Couldn't write the scan helper"})
		return
	}
	out, err := runPowerShell("-File", ps, "-Id", req.Device, "-Source", req.Source, "-Color", req.Color, "-Dpi", itoa(req.Dpi), "-OutDir", dir)
	res := strings.TrimSpace(string(out))
	source := "flatbed"
	if strings.Contains(res, "feeder") {
		source = "feeder"
	}
	// collect page_*.jpg in order
	pages := []string{}
	for i := 0; i < 200; i++ {
		b, e := os.ReadFile(filepath.Join(dir, "page_"+itoa(i)+".jpg"))
		if e != nil {
			break
		}
		pages = append(pages, base64.StdEncoding.EncodeToString(b))
	}
	resp := map[string]any{"pages": pages, "source": source}
	if len(pages) == 0 {
		if strings.Contains(res, "NODEV") {
			resp["error"] = "No scanner found. Turn it on and check it shows in Windows Settings → Printers & scanners."
		} else if err != nil || strings.Contains(res, "ERR") {
			resp["error"] = "The scan didn't complete. Make sure paper is loaded and the scanner isn't busy."
		} else {
			resp["error"] = "Nothing was scanned."
		}
	}
	writeJSON(w, 200, resp)
}

// usbWatch keeps s.usbPhones (serial -> model) current from `adb devices` so the
// status page can show USB-cable phones. No adb / no phones -> empty, no error.
func (s *server) usbWatch() {
	for {
		m := map[string]string{}
		adbCmd := exec.Command("adb", "devices", "-l")
		hideWindow(adbCmd)
		if out, err := adbCmd.Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "List of devices") {
					continue
				}
				f := strings.Fields(line)
				if len(f) >= 2 && f[1] == "device" {
					model := ""
					for _, kv := range f[2:] {
						if strings.HasPrefix(kv, "model:") {
							model = strings.ReplaceAll(strings.TrimPrefix(kv, "model:"), "_", " ")
						}
					}
					m[f[0]] = model
				}
			}
		}
		s.mu.Lock()
		s.usbPhones = m
		s.mu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

// WIA acquisition script. Feeder scans every page; flatbed scans one.
const scanScript = `param([string]$Id,[string]$Source,[string]$Color,[int]$Dpi,[string]$OutDir)
$ErrorActionPreference='Stop'
$XRES=6147; $YRES=6148; $XEXT=6151; $YEXT=6152; $DATATYPE=4103
$DHSELECT=3088; $DHCAPS=3087; $FEEDER=1; $FLATBED=2
$JPEG='{B96B3CAE-0728-11D3-9D7B-0000F81EF32E}'
function SetP($props,$id,$val){ try{ foreach($p in $props){ if($p.PropertyID -eq $id){ $p.Value=$val; break } } }catch{} }
try{
  $dm = New-Object -ComObject WIA.DeviceManager
  $info = $null
  foreach($d in $dm.DeviceInfos){ if($d.DeviceID -eq $Id){ $info=$d; break } }
  if(-not $info){ foreach($d in $dm.DeviceInfos){ if($d.Type -eq 1){ $info=$d; break } } }
  if(-not $info){ Write-Output 'NODEV'; exit }
  $device = $info.Connect()
  $useFeeder = $false
  if($Source -ne 'flatbed'){
    try{
      $caps = 0; foreach($p in $device.Properties){ if($p.PropertyID -eq $DHCAPS){ $caps=$p.Value } }
      if(($caps -band $FEEDER) -ne 0){ SetP $device.Properties $DHSELECT $FEEDER; $useFeeder=$true }
    }catch{}
  }
  $item = $device.Items.Item(1)
  SetP $item.Properties $XRES $Dpi
  SetP $item.Properties $YRES $Dpi
  $dt = 3; if($Color -eq 'gray'){ $dt=2 } elseif($Color -eq 'bw'){ $dt=0 }
  SetP $item.Properties $DATATYPE $dt
  SetP $item.Properties $XEXT ([int]($Dpi*8.5))
  SetP $item.Properties $YEXT ([int]($Dpi*11))
  $n=0
  while($true){
    try{ $img = $item.Transfer() }catch{ break }
    try{
      $ip = New-Object -ComObject WIA.ImageProcess
      $ip.Filters.Add($ip.FilterInfos.Item('Convert').FilterID)
      $ip.Filters.Item($ip.Filters.Count).Properties.Item('FormatID').Value = $JPEG
      $ip.Filters.Item($ip.Filters.Count).Properties.Item('Quality').Value = 80
      $img = $ip.Apply($img)
    }catch{}
    $p = Join-Path $OutDir ('page_{0}.jpg' -f $n)
    if(Test-Path $p){ Remove-Item $p -Force }
    $img.SaveFile($p)
    $n++
    if(-not $useFeeder){ break }
    if($n -ge 100){ break }
  }
  Write-Output ('OK ' + $(if($useFeeder){'feeder'}else{'flatbed'}) + ' ' + $n)
}catch{ Write-Output ('ERR ' + $_.Exception.Message) }`
