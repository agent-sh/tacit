package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// Which session a tacit MCP server answers (SPEC.md, Hold-outs). The
// server runs as long as its harness process does, through clears and
// resumes, and its environment names no session (or the one it started
// in), so it cannot know the session from itself. The harness's hooks can:
// each start and each prompt hook records the session it runs for under
// the harness process's id (the hook's ancestors), and the server, a child
// of that same process, reads the record of its parent on every call. A
// server registers itself under its parent at its start, so a hook knows a
// tacit MCP server of its harness can resolve its session.

// harnessPIDDir holds the records: <pid> is the session a harness process
// runs now, mcp-<pid> the tacit MCP server it started.
func harnessPIDDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(platform.StateDir(home), "harness-pids")
}

// hookAncestors is the hook's ancestor processes, nearest first (at most
// eight, never init); tests replace it.
var hookAncestors = func() []int {
	var out []int
	for pid := os.Getppid(); pid > 1 && len(out) < 8; pid = parentPID(pid) {
		out = append(out, pid)
	}
	return out
}

// serverParent is the MCP server's parent: the harness process; tests
// replace it.
var serverParent = os.Getppid

// parentPID is a process's parent: /proc on Linux, ps on macOS, 0 where
// neither is there (no record is written, so no hold-out is drawn).
func parentPID(pid int) int {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return 0
		}
		// pid (comm) state ppid ...: comm may hold spaces and parentheses.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			return 0
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return 0
		}
		n, _ := strconv.Atoi(f[1])
		return n
	case "darwin":
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return n
	}
	return 0
}

func writeSmall(path, text string) {
	os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(text), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// recordHarnessSession records session as the one each of the hook's
// ancestors runs now, and removes the records of processes that are gone.
func recordHarnessSession(session string) {
	if !agentreview.ValidSession(session) {
		return
	}
	dir := harnessPIDDir()
	for _, pid := range hookAncestors() {
		writeSmall(filepath.Join(dir, strconv.Itoa(pid)), session)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 64 {
		for _, e := range entries {
			n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(e.Name(), ".tmp"), "mcp-"))
			if err == nil && !platform.Alive(n) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// registerMCPServer records that this tacit MCP server answers its parent
// harness process.
func registerMCPServer() {
	if p := serverParent(); p > 1 {
		writeSmall(filepath.Join(harnessPIDDir(), "mcp-"+strconv.Itoa(p)), strconv.Itoa(os.Getpid()))
	}
}

// mcpSession is the session the MCP server's harness runs now, or "" when
// no record says (the call then reads the store as no session's view).
func mcpSession() string {
	p := serverParent()
	if p <= 1 {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(harnessPIDDir(), strconv.Itoa(p)))
	if err != nil {
		return ""
	}
	if s := strings.TrimSpace(string(b)); agentreview.ValidSession(s) {
		return s
	}
	return ""
}

// holdoutResolvable reports whether a hold-out may be drawn in session:
// some ancestor of the hook is recorded as running it, and, when the
// harness has tacit's MCP server (mcp), a live tacit MCP server is
// registered under that same process, so its tools will read the session's
// view. It answers the harness process's id, or the reason it cannot.
func holdoutResolvable(session string, mcp bool) (int, string) {
	dir := harnessPIDDir()
	why := "no session record"
	for _, pid := range hookAncestors() {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)))
		if err != nil || strings.TrimSpace(string(b)) != session {
			continue
		}
		if !mcp {
			return pid, ""
		}
		sb, err := os.ReadFile(filepath.Join(dir, "mcp-"+strconv.Itoa(pid)))
		if n, _ := strconv.Atoi(strings.TrimSpace(string(sb))); err == nil && n > 0 && platform.Alive(n) {
			return pid, ""
		}
		why = "no MCP server"
	}
	return 0, why
}
