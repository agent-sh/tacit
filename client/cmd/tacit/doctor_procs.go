package main

// Running tacit processes on an outdated binary (doctor): an agent
// session's `tacit mcp` server, or a job, started before an upgrade keeps
// the old code. On Linux the kernel says so: /proc/<pid>/exe ends in
// " (deleted)" when the file was replaced, or names a file whose inode is no
// longer the one running. On macOS ps gives each process's executable and
// start time, and a binary modified after the process started is newer than
// the one it runs. Windows is not checked.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// staleProc is a running tacit process whose binary on disk changed.
type staleProc struct {
	PID     int
	Command string // its command line, cut
	Why     string
}

// staleTacitProcs are this machine's running tacit processes on an outdated
// binary (not this one), and whether the check could run here.
func staleTacitProcs() ([]staleProc, bool) {
	switch runtime.GOOS {
	case "linux":
		return staleProcsLinux("/proc", os.Getpid()), true
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,lstart=,comm=").Output()
		if err != nil {
			return nil, false
		}
		return staleProcsPS(string(out), os.Getpid(), psArgs), true
	}
	return nil, false
}

// isTacitBinary reports an executable named tacit (tacit.exe), not the server.
func isTacitBinary(p string) bool {
	b := strings.TrimSuffix(filepath.Base(p), ".exe")
	return b == "tacit"
}

// staleProcsLinux reads proc (the /proc file system) for tacit processes
// other than self whose executable was replaced or deleted.
func staleProcsLinux(proc string, self int) []staleProc {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	var out []staleProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		link := filepath.Join(proc, e.Name(), "exe")
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		path, deleted := strings.CutSuffix(target, " (deleted)")
		if !isTacitBinary(path) {
			continue
		}
		why := ""
		switch {
		case deleted:
			why = "its binary " + path + " was replaced or removed after it started"
		default:
			running, err1 := os.Stat(link)
			onDisk, err2 := os.Stat(path)
			if err1 == nil && err2 == nil && !os.SameFile(running, onDisk) {
				why = "its binary " + path + " was replaced after it started"
			}
		}
		if why == "" {
			continue
		}
		out = append(out, staleProc{PID: pid, Command: procCmdline(filepath.Join(proc, e.Name(), "cmdline")), Why: why})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func procCmdline(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return clipOne(strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")), 120)
}

// psArgs is a process's command line by ps.
func psArgs(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return clipOne(strings.TrimSpace(string(out)), 120)
}

// staleProcsPS reads `ps -o pid=,lstart=,comm=` output for tacit processes
// other than self whose executable was modified (or removed) after they
// started; args gives a process's command line.
func staleProcsPS(out string, self int, args func(int) string) []staleProc {
	var res []staleProc
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		// pid, then lstart's five fields (Thu Oct 23 10:00:00 2026), then comm.
		if len(f) < 7 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid == self {
			continue
		}
		comm := strings.Join(f[6:], " ")
		if !isTacitBinary(comm) || !filepath.IsAbs(comm) {
			continue
		}
		started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[1:6], " "), time.Local)
		if err != nil {
			continue
		}
		why := ""
		fi, err := os.Stat(comm)
		switch {
		case err != nil:
			why = "its binary " + comm + " is gone"
		case fi.ModTime().After(started.Add(time.Second)):
			why = fmt.Sprintf("its binary %s changed at %s, after it started at %s", comm, fi.ModTime().Format("2006-01-02 15:04"), started.Format("2006-01-02 15:04"))
		}
		if why == "" {
			continue
		}
		res = append(res, staleProc{PID: pid, Command: args(pid), Why: why})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].PID < res[j].PID })
	return res
}

// staleProcsCheck is doctor's line for them.
func staleProcsCheck() (status, detail string) {
	procs, ok := staleTacitProcs()
	if !ok {
		return "info", "running processes not checked on " + runtime.GOOS
	}
	if len(procs) == 0 {
		return "ok", "no running tacit process is on an outdated binary"
	}
	var parts []string
	for i, p := range procs {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("and %d more", len(procs)-8))
			break
		}
		c := p.Command
		if c == "" {
			c = "tacit"
		}
		parts = append(parts, fmt.Sprintf("pid %d (%s): %s", p.PID, c, p.Why))
	}
	return "warn", fmt.Sprintf("%d running tacit process(es) on an outdated binary: %s. A `tacit mcp` server from v0.9.3 on moves to the new binary at its next request; an older one, or one on Windows, keeps the old code until its MCP connection restarts (Claude Code: /mcp, reconnect tacit; other harnesses: restart the session); stop any other with kill <pid>", len(procs), strings.Join(parts, "; "))
}
