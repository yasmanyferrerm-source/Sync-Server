package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GET /api/printers — list printers installed on the PC that runs the server,
// so any paired device (phone or PC) can pick one without a USB cable.
func (s *server) printers(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, 200, map[string]any{"supported": false, "printers": []any{}})
		return
	}
	// Win32_Printer is available on every Windows; ConvertTo-Json keeps names with spaces intact.
	script := `$ErrorActionPreference='Stop'
try {
  $p = @(Get-WmiObject -Class Win32_Printer | Select-Object -ExpandProperty Name)
  if ($p.Count -eq 0) { '[]' } else { ConvertTo-Json @($p) -Compress }
} catch { '[]' }`
	out, err := runPowerShell("-Command", script)
	if err != nil {
		writeJSON(w, 200, map[string]any{"supported": true, "printers": []any{}, "error": "Couldn't read the Windows printer list."})
		return
	}
	var names []string
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(out))), &names)
	if names == nil {
		names = []string{}
	}
	writeJSON(w, 200, map[string]any{"supported": true, "printers": names})
}

// --- small cache so the dashboard (polled every 3s) doesn't run PowerShell constantly
var (
	prnCache   []string
	prnCacheAt time.Time
	prnCacheMu = make(chan struct{}, 1)
)

func cachedPrinters() []string {
	if runtime.GOOS != "windows" {
		return []string{}
	}
	select {
	case prnCacheMu <- struct{}{}:
		defer func() { <-prnCacheMu }()
	default:
		return prnCache // another refresh is in flight; return what we have
	}
	if time.Since(prnCacheAt) < 20*time.Second && prnCache != nil {
		return prnCache
	}
	script := `$ErrorActionPreference='Stop'
try { $p=@(Get-WmiObject -Class Win32_Printer | Select-Object -ExpandProperty Name); if($p.Count -eq 0){'[]'}else{ConvertTo-Json @($p) -Compress} } catch { '[]' }`
	out, err := runPowerShell("-Command", script)
	if err == nil {
		var names []string
		if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &names) == nil {
			prnCache = names
		}
	}
	if prnCache == nil {
		prnCache = []string{}
	}
	prnCacheAt = time.Now()
	return prnCache
}

// POST /api/print-test?printer=NAME — print a small text test label. Loopback only
// (the dashboard runs on the server PC), so it needs no pairing code.
func (s *server) printTest(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Test printing can only be started from the server PC."})
		return
	}
	if runtime.GOOS != "windows" {
		writeJSON(w, 200, map[string]any{"error": "Printing works only on Windows."})
		return
	}
	printer := strings.TrimSpace(r.URL.Query().Get("printer"))
	if printer == "" {
		writeJSON(w, 400, map[string]any{"error": "no printer"})
		return
	}
	dir := filepath.Join(s.dir, "print")
	_ = os.MkdirAll(dir, 0o755)
	ps1 := filepath.Join(dir, "test.ps1")
	_ = os.WriteFile(ps1, []byte(testScript), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", ps1, "-printer", printer).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		writeJSON(w, 200, map[string]any{"error": msg})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func isLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

const testScript = `param([string]$printer)
$ErrorActionPreference='Stop'
Add-Type -AssemblyName System.Drawing
$pd = New-Object System.Drawing.Printing.PrintDocument
$pd.PrinterSettings.PrinterName = $printer
if(-not $pd.PrinterSettings.IsValid){ Write-Error ("Printer not found: " + $printer); exit 2 }
$pd.DefaultPageSettings.Margins = New-Object System.Drawing.Printing.Margins(10,10,10,10)
$pd.add_PrintPage({ param($s,$e)
  $f = New-Object System.Drawing.Font("Arial",11)
  $fb = New-Object System.Drawing.Font("Arial",13,[System.Drawing.FontStyle]::Bold)
  $e.Graphics.DrawString("Warehouse Sync Server", $fb, [System.Drawing.Brushes]::Black, 8, 8)
  $e.Graphics.DrawString("Test print OK", $f, [System.Drawing.Brushes]::Black, 8, 34)
  $e.Graphics.DrawString([string](Get-Date), $f, [System.Drawing.Brushes]::Black, 8, 56)
})
$pd.Print()
`

type printReq struct {
	Printer string   `json:"printer"`
	W       float64  `json:"w"` // label width in inches
	H       float64  `json:"h"` // label height in inches
	Images  []string `json:"images"`
}

var pngPrefix = regexp.MustCompile(`^data:image/png;base64,`)

// POST /api/print — print label PNGs to a named printer on the server PC, using the
// same no-dialog PrintDocument approach the desktop app uses, so output matches.
func (s *server) print(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, 200, map[string]any{"error": "Printing works only when the Sync Server runs on Windows."})
		return
	}
	var req printReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	req.Printer = strings.TrimSpace(req.Printer)
	if req.Printer == "" {
		writeJSON(w, 400, map[string]any{"error": "no printer chosen"})
		return
	}
	if len(req.Images) == 0 {
		writeJSON(w, 400, map[string]any{"error": "nothing to print"})
		return
	}

	dir := filepath.Join(s.dir, "print")
	_ = os.MkdirAll(dir, 0o755)
	// clear stale files from a previous job
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if m, _ := filepath.Match("*.*", e.Name()); m {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	var paths []string
	for i, img := range req.Images {
		b64 := pngPrefix.ReplaceAllString(img, "")
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(raw) == 0 {
			continue
		}
		p := filepath.Join(dir, "lbl-"+strconv.Itoa(i)+".png")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		writeJSON(w, 400, map[string]any{"error": "no valid label images"})
		return
	}

	listFile := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listFile, []byte(strings.Join(paths, "\r\n")), 0o644); err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not stage the print job"})
		return
	}
	ps1 := filepath.Join(dir, "print.ps1")
	if err := os.WriteFile(ps1, []byte(printScript), 0o644); err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not stage the print script"})
		return
	}

	w100 := strconv.Itoa(int(req.W*100 + 0.5))
	h100 := strconv.Itoa(int(req.H*100 + 0.5))
	if req.W <= 0 {
		w100 = "400"
	}
	if req.H <= 0 {
		h100 = "200"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", ps1, "-list", listFile, "-printer", req.Printer, "-w", w100, "-h", h100)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		writeJSON(w, 200, map[string]any{"error": "Printing timed out — check the printer is on, loaded and not jammed."})
		return
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 240 {
			msg = msg[:240]
		}
		writeJSON(w, 200, map[string]any{"error": msg})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "printed": len(paths)})
}

// printScript is the proven PrintDocument routine (identical to the desktop app's),
// so Wi-Fi prints look exactly like local ones.
const printScript = `param([string]$list,[string]$printer,[int]$w,[int]$h)
$ErrorActionPreference='Stop'
Add-Type -AssemblyName System.Drawing
$files = @(Get-Content -LiteralPath $list | Where-Object { $_ -ne "" })
if($files.Count -eq 0){ Write-Error "no label images"; exit 3 }
$pd = New-Object System.Drawing.Printing.PrintDocument
$pd.PrinterSettings.PrinterName = $printer
if(-not $pd.PrinterSettings.IsValid){ Write-Error ("Printer not found: " + $printer); exit 2 }
try { $ps = New-Object System.Drawing.Printing.PaperSize("Label",$w,$h); $pd.DefaultPageSettings.PaperSize = $ps } catch {}
$pd.DefaultPageSettings.Margins = New-Object System.Drawing.Printing.Margins(0,0,0,0)
$pd.OriginAtMargins = $false
$script:i = 0
$script:n = $files.Count
$pd.add_PrintPage({ param($s,$e)
  $img = $null
  try {
    $img = [System.Drawing.Image]::FromFile($files[$script:i])
    $pb = $e.PageBounds
    if (($img.Width -gt $img.Height) -ne ($pb.Width -gt $pb.Height)) { $img.RotateFlip([System.Drawing.RotateFlipType]::Rotate90FlipNone) }
    $e.Graphics.DrawImage($img, $pb)
  } finally { if ($img -ne $null) { $img.Dispose() } }
  $script:i++
  $e.HasMorePages = ($script:i -lt $script:n)
})
$pd.Print()
`
