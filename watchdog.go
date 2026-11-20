package main

// ---------------------------------------------------------------------------
// Staying alive after a reboot.
//
// The server is started by a shortcut in the Windows Startup folder, so it comes up
// while Windows is still busy signing in. If it ever stops answering (stuck at start-up,
// or later), nobody notices until a phone can't sync. This file makes it recover on its own:
//   * a small log (sync-server.log next to the exe) records every start-up step,
//   * a self-check pings the server every 20 s and restarts it if it stops answering,
//   * a new copy that finds a frozen copy holding the port ends that copy and takes over,
//   * child programs (adb, PowerShell) get time limits so they can't hang a background task.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	logMu   sync.Mutex
	logPath string
	started = time.Now()
)

// slog appends one timestamped line to sync-server.log (kept under ~512 KB).
func slog(format string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + "  " + fmt.Sprintf(format, a...) + "\n"
	fmt.Print("  " + line)
	logMu.Lock()
	defer logMu.Unlock()
	if logPath == "" {
		return
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 512*1024 {
		_ = os.Rename(logPath, logPath+".old")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = io.WriteString(f, line)
	f.Close()
}

// listenWithRecovery binds the port. If another copy holds it: a healthy copy means we just
// open its dashboard and leave; a copy that does not answer is frozen, so we end it and take over.
func listenWithRecovery(port int) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", port)
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ { // up to ~60 s — covers a copy that is still shutting down
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if attempt > 0 {
				slog("port %d is free now (after %d tries)", port, attempt+1)
			}
			return ln, nil
		}
		lastErr = err
		if attempt == 0 {
			slog("port %d is busy: %v", port, err)
		}
		if healthy(port, 4*time.Second) {
			slog("another copy of the server is already running and answering — opening its dashboard and exiting")
			openBrowser(fmt.Sprintf("http://localhost:%d/status", port))
			os.Exit(0)
		}
		if attempt == 1 { // asked twice, no answer: the copy holding the port is frozen
			if pid := portOwner(port); pid > 0 && pid != os.Getpid() {
				if name := processName(pid); isOurServer(name) {
					slog("a frozen copy (%s, PID %d) holds the port and is not answering — ending it", name, pid)
					killProcess(pid)
				} else if name != "" {
					slog("port %d belongs to another program: %s (PID %d)", port, name, pid)
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, lastErr
}

// healthy reports whether a server answers its live-status call on this PC in time.
func healthy(port int, timeout time.Duration) bool {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/status.json", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == 200
}

// debugAfterStart lets a test build simulate a hang; nil in real builds.
var debugAfterStart func(*server)

// watchdog checks every 20 s that this server still answers. Three misses in a row (~1 minute)
// and it restarts itself, the same as ending it in Task Manager and starting it again.
func (s *server) watchdog(srv *http.Server) {
	time.Sleep(30 * time.Second) // let start-up finish first
	misses := 0
	for range time.Tick(20 * time.Second) {
		if healthy(s.cfg.Port, 10*time.Second) {
			if misses > 0 {
				slog("self-check: answering again after %d missed check(s)", misses)
			}
			misses = 0
			continue
		}
		misses++
		slog("self-check: the server did not answer (%d of 3)", misses)
		if misses >= 3 {
			slog("self-check: the server stopped answering — restarting it")
			restartSelf(srv)
		}
	}
}

// restartSelf starts a fresh copy of this exe with the same arguments and exits. The new copy
// waits for the port to be released (listenWithRecovery) before it starts serving.
func restartSelf(srv *http.Server) {
	exe, err := os.Executable()
	if err != nil {
		slog("restart failed: %v", err)
		return
	}
	go func() { _ = srv.Close() }()
	time.Sleep(1500 * time.Millisecond) // give the saver its last 500 ms tick to flush
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		slog("restart failed: %v", err)
		return
	}
	slog("started a new copy (PID %d); this copy (PID %d) is exiting", cmd.Process.Pid, os.Getpid())
	os.Exit(0)
}

func isOurServer(image string) bool {
	n := strings.ToLower(image)
	return strings.Contains(n, "sync server") || strings.Contains(n, "sync-server") || strings.Contains(n, "warehouse-sync")
}

// portOwner returns the PID listening on the port (Windows: netstat; elsewhere: unknown).
func portOwner(port int) int {
	if runtime.GOOS == "linux" {
		return linuxPortOwner(port)
	}
	if runtime.GOOS != "windows" {
		return 0
	}
	out, err := runQuiet(8*time.Second, "netstat", "-ano", "-p", "TCP")
	if err != nil {
		return 0
	}
	want := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.EqualFold(f[3], "LISTENING") && strings.HasSuffix(f[1], want) {
			if pid, err := strconv.Atoi(f[4]); err == nil {
				return pid
			}
		}
	}
	return 0
}

// processName returns the image name of a PID (Windows: tasklist), e.g. "Warehouse Sync Server.exe".
func processName(pid int) string {
	if runtime.GOOS == "linux" {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		return filepath.Base(strings.SplitN(string(b), "\x00", 2)[0])
	}
	if runtime.GOOS != "windows" {
		return ""
	}
	out, err := runQuiet(8*time.Second, "tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "\"") {
		return ""
	}
	if i := strings.Index(line[1:], "\""); i > 0 {
		return line[1 : i+1]
	}
	return ""
}

func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		if err := p.Kill(); err != nil {
			slog("could not end PID %d: %v", pid, err)
			if runtime.GOOS == "windows" {
				_, _ = runQuiet(8*time.Second, "taskkill", "/F", "/PID", strconv.Itoa(pid))
			}
		}
	}
}

// uptimeNote says how long Windows has been running, so the log shows when a start was right after a reboot.
func uptimeNote() string {
	if d := systemUptime(); d > 0 {
		return "Windows has been running for " + d.Round(time.Second).String()
	}
	return ""
}

// linuxPortOwner finds the PID listening on a TCP port by matching the socket inode in
// /proc/net/tcp{,6} against each process's open files (no extra tools needed).
func linuxPortOwner(port int) int {
	hexPort := fmt.Sprintf(":%04X", port)
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			c := strings.Fields(line)
			if len(c) > 9 && strings.HasSuffix(c[1], hexPort) && c[3] == "0A" { // 0A = LISTEN
				inodes["socket:["+c[9]+"]"] = true
			}
		}
	}
	if len(inodes) == 0 {
		return 0
	}
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fds, _ := os.ReadDir("/proc/" + p.Name() + "/fd")
		for _, fd := range fds {
			if t, err := os.Readlink("/proc/" + p.Name() + "/fd/" + fd.Name()); err == nil && inodes[t] {
				return pid
			}
		}
	}
	return 0
}
