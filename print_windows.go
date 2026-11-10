//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// platformListPrinters lists installed Windows printers via WMI (available on every Windows).
func platformListPrinters() (bool, []string, error) {
	script := `$ErrorActionPreference='Stop'
try { $p=@(Get-WmiObject -Class Win32_Printer | Select-Object -ExpandProperty Name); if($p.Count -eq 0){'[]'}else{ConvertTo-Json @($p) -Compress} } catch { '[]' }`
	out, err := runPowerShell("-Command", script)
	if err != nil {
		return true, []string{}, errors.New("couldn't read the Windows printer list")
	}
	var names []string
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(out))), &names)
	if names == nil {
		names = []string{}
	}
	return true, names, nil
}

// platformPrintPNGs prints the staged PNGs with the proven PrintDocument routine
// (identical to the desktop app's), so Wi-Fi prints look exactly like local ones.
func platformPrintPNGs(dir, printer string, wIn, hIn float64, paths []string) error {
	listFile := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listFile, []byte(strings.Join(paths, "\r\n")), 0o644); err != nil {
		return errors.New("could not stage the print job")
	}
	ps1 := filepath.Join(dir, "print.ps1")
	if err := os.WriteFile(ps1, []byte(printScript), 0o644); err != nil {
		return errors.New("could not stage the print script")
	}
	w100 := strconv.Itoa(int(wIn*100 + 0.5))
	h100 := strconv.Itoa(int(hIn*100 + 0.5))
	if wIn <= 0 {
		w100 = "400"
	}
	if hIn <= 0 {
		h100 = "200"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", ps1, "-list", listFile, "-printer", printer, "-w", w100, "-h", h100)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("printing timed out — check the printer is on, loaded and not jammed")
	}
	if err != nil {
		return errors.New(psErr(out, err))
	}
	return nil
}

func platformPrintTest(dir, printer string) error {
	ps1 := filepath.Join(dir, "test.ps1")
	if err := os.WriteFile(ps1, []byte(testScript), 0o644); err != nil {
		return errors.New("could not stage the test script")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", ps1, "-printer", printer)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(psErr(out, err))
	}
	return nil
}

func psErr(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return msg
}

const printScript = `param([string]$list,[string]$printer,[int]$w,[int]$h)
$ErrorActionPreference='Stop'
Add-Type -AssemblyName System.Drawing
$files = @(Get-Content -LiteralPath $list | Where-Object { $_ -ne "" })
if($files.Count -eq 0){ Write-Error "no label images"; exit 3 }
$pd = New-Object System.Drawing.Printing.PrintDocument
$pd.PrinterSettings.PrinterName = $printer
if(-not $pd.PrinterSettings.IsValid){ Write-Error ("Printer not found: " + $printer); exit 2 }
# Documents (Letter-size pages) use one of the printer's own paper sizes, so an office printer
# never falls back to a wrong or cropped custom size. Labels keep the exact custom size.
$isDoc = ($w -ge 800 -and $h -ge 800)
$matched = $false
if($isDoc){
  try {
    $sw = [Math]::Min($w,$h); $sh = [Math]::Max($w,$h)
    foreach($s in $pd.PrinterSettings.PaperSizes){
      if([Math]::Abs($s.Width - $sw) -le 8 -and [Math]::Abs($s.Height - $sh) -le 8){
        $pd.DefaultPageSettings.PaperSize = $s
        $pd.DefaultPageSettings.Landscape = ($w -gt $h)
        $matched = $true; break
      }
    }
  } catch { $matched = $false }
}
if(-not $matched){ try { $ps = New-Object System.Drawing.Printing.PaperSize("Label",$w,$h); $pd.DefaultPageSettings.PaperSize = $ps } catch {} }
$pd.DefaultPageSettings.Margins = New-Object System.Drawing.Printing.Margins(0,0,0,0)
# For documents, measure from the true paper edge (not the printer's unprintable margin) so the
# page is not shifted right/down and clipped on the far side. Labels are unchanged.
$pd.OriginAtMargins = [bool]$matched
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
