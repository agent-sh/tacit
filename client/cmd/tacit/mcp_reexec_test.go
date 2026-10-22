package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildTacit builds this package's binary with the version label into path.
func buildTacit(t *testing.T, path, label string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+label, "-o", path, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// lockedBuffer is a bytes.Buffer safe for a process's writes and the test's
// reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMCPServerContinuesInANewBinary starts `tacit mcp`, initializes and
// calls a tool, replaces the binary file the way an upgrade does (a new file
// renamed over the old), and calls again: the same connection (the same
// pipes, the same process id) answers from the new binary, with no new
// initialize, and the two requests sent in one write both come back.
func TestMCPServerContinuesInANewBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec in place on Windows (TestMCPOutdatedNote covers it)")
	}
	if testing.Short() {
		t.Skip("builds the binary twice")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "tacit")
	buildTacit(t, bin, "e2e-old")
	home := filepath.Join(dir, "home")
	os.MkdirAll(home, 0o700)

	cmd := exec.Command(bin, "mcp", "--harness", "codex")
	cmd.Env = append(os.Environ(), "HOME="+home, "TACIT_STORE="+filepath.Join(dir, "store"), "TACIT_STATE_DIR="+filepath.Join(dir, "state"), "TACIT_SETTINGS=", "TACIT_LOCAL="+filepath.Join(dir, "local"), "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	lines := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				lines <- m
			}
		}
		close(lines)
	}()
	send := func(msgs ...string) {
		t.Helper()
		if _, err := io.WriteString(stdin, strings.Join(msgs, "\n")+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	got := map[float64]map[string]any{} // answers read, by id (calls are answered in any order)
	recv := func(id float64) map[string]any {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			if m, ok := got[id]; ok {
				return m
			}
			select {
			case m, ok := <-lines:
				if !ok {
					t.Fatalf("the server closed stdout; stderr:\n%s", stderr.String())
				}
				if n, ok := m["id"].(float64); ok {
					got[n] = m
				}
			case <-deadline:
				t.Fatalf("no answer to %v; stderr:\n%s", id, stderr.String())
			}
		}
	}
	call := func(id int, name string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"tacit_fact","arguments":{"name":%q}}}`, id, name)
	}
	resultText := func(m map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(m["result"])
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		json.Unmarshal(b, &r)
		var parts []string
		for _, c := range r.Content {
			parts = append(parts, c.Text)
		}
		if len(parts) == 0 {
			t.Fatalf("no result: %v", m)
		}
		return strings.Join(parts, "\n")
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`)
	if m := recv(1); m["result"] == nil {
		t.Fatalf("initialize: %v", m)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	send(call(2, "first-call"))
	if got := resultText(recv(2)); !strings.Contains(got, "not found: first-call") {
		t.Fatalf("first call: %q", got)
	}
	if strings.Contains(stderr.String(), "continued in") {
		t.Fatalf("handed over before the binary changed:\n%s", stderr.String())
	}

	// The upgrade: a new binary written beside the old and renamed over it.
	next := filepath.Join(dir, "bin", "tacit.new")
	buildTacit(t, next, "e2e-new")
	past := time.Now().Add(-time.Minute)
	os.Chtimes(next, past, past)
	if err := os.Rename(next, bin); err != nil {
		t.Fatal(err)
	}
	// Two calls in one write: the second sits in the old process's buffer
	// when it hands over.
	send(call(3, "after-upgrade"), call(4, "buffered-behind-it"))
	if got := resultText(recv(3)); !strings.Contains(got, "not found: after-upgrade") || strings.Contains(got, "note:") {
		t.Fatalf("call after the upgrade: %q", got)
	}
	if got := resultText(recv(4)); !strings.Contains(got, "not found: buffered-behind-it") {
		t.Fatalf("buffered call: %q", got)
	}
	if !strings.Contains(stderr.String(), "continued in e2e-new after the binary changed (from e2e-old)") {
		t.Fatalf("not answered by the new binary; stderr:\n%s", stderr.String())
	}
	if cmd.Process.Pid != pid {
		t.Fatal("another process")
	}
	// The session goes on: a list and a call, no new initialize.
	send(`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`)
	if m := recv(5); m["result"] == nil {
		t.Fatalf("tools/list after the handoff: %v", m)
	}
	send(call(6, "later"))
	if got := resultText(recv(6)); !strings.Contains(got, "not found: later") {
		t.Fatalf("later call: %q", got)
	}
	if n := strings.Count(stderr.String(), "continued in"); n != 1 {
		t.Fatalf("%d handoffs, want 1:\n%s", n, stderr.String())
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not end when the client closed stdin")
	}
}

// TestMCPOutdatedNote: where the server cannot move to the new binary
// (Windows, a failed exec) every tool result says it is outdated and how to
// reconnect; with the binary unchanged it says nothing.
func TestMCPOutdatedNote(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tacit")
	os.WriteFile(bin, []byte("old"), 0o755)
	fi, _ := os.Stat(bin)
	conn := &mcpConn{self: &selfBinary{path: bin, started: fi}, inflight: map[string]bool{}}
	handler := outdatedMiddleware(conn)(func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		return text("result"), nil
	})
	got := func() string {
		res, _ := handler(context.Background(), "tools/call", nil)
		var parts []string
		for _, c := range res.(*mcp.CallToolResult).Content {
			parts = append(parts, c.(*mcp.TextContent).Text)
		}
		return strings.Join(parts, "|")
	}
	if g := got(); g != "result" {
		t.Fatalf("unchanged binary: %q", g)
	}
	os.WriteFile(bin+".new", []byte("new binary"), 0o755)
	os.Rename(bin+".new", bin)
	if g := got(); !strings.Contains(g, "note: this tacit MCP server still runs the binary it started with") || !strings.Contains(g, "reconnect") {
		t.Fatalf("changed binary: %q", g)
	}
	// A server that can exec and did not fail hands over instead of noting.
	conn.exec = func(string, []string, []string) error { return nil }
	if g := got(); g != "result" {
		t.Fatalf("exec available: %q", g)
	}
	conn.failed = true
	if g := got(); !strings.Contains(g, "note:") {
		t.Fatalf("exec failed: %q", g)
	}
}

// TestMCPHandoffKeepsTheSession: the state a handoff carries resumes the
// SDK's session (initialize parameters, protocol, initialized, log level).
func TestMCPHandoffKeepsTheSession(t *testing.T) {
	h := mcpHandoff{Initialize: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`), Negotiated: "2025-06-18", Initialized: true, LogLevel: "info", Pending: []byte("{}\n"), From: "old"}
	b, _ := json.Marshal(h)
	t.Setenv(mcpHandoffFileEnv, "")
	f := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(f, b, 0o600)
	t.Setenv(mcpHandoffFileEnv, f)
	got, err := takeHandoff()
	if err != nil || got == nil {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(f); err == nil {
		t.Fatal("the handoff file was left behind")
	}
	if os.Getenv(mcpHandoffFileEnv) != "" {
		t.Fatal("the handoff stays in the environment")
	}
	st, err := got.state()
	if err != nil || st.InitializeParams == nil || st.InitializeParams.ClientInfo.Name != "c" || st.InitializedParams == nil || st.NegotiatedProtocolVersion != "2025-06-18" || st.LogLevel != "info" {
		t.Fatalf("%+v %v", st, err)
	}
	if string(got.Pending) != "{}\n" {
		t.Fatalf("pending %q", got.Pending)
	}
}
