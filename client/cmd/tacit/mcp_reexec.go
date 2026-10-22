package main

// A long-lived `tacit mcp` server inside an agent session keeps running the
// binary it started with: after an upgrade it still reads the store with the
// old code (on the rig, servers three days old ran a binary that misread the
// store's new format). So the server checks, at each request the client
// sends, whether the binary on disk changed (a stat of the path it was
// started from: file identity, size and modification time), and when it did
// and the new binary answers `version`, it hands the connection to it in
// place:
//
//   - between requests only: the request that found the change is held until
//     every earlier call is answered, then the process re-executes itself
//     (same pid, same stdin and stdout, so the client's connection is the
//     same pipe);
//   - the new process gets the session's state from the old (the initialize
//     request's parameters, the negotiated protocol version, the initialized
//     notification, the log level) and every byte the old one had read but
//     not handled (the held request first), and serves on without a new
//     initialize: the client never sees the switch.
//
// Windows has no exec in place: there the server goes on with the old binary
// and says so in every tool result, with how to reconnect. So does a server
// whose re-exec failed.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/platform"
)

// mcpHandoffEnv carries the session's state to the re-executed server (JSON,
// base64); mcpHandoffFileEnv names a file that holds it instead, when it is
// too large for the environment. The new process reads it, deletes the file
// and clears both before it runs anything else.
const (
	mcpHandoffEnv     = "TACIT_MCP_HANDOFF"
	mcpHandoffFileEnv = "TACIT_MCP_HANDOFF_FILE"
	// mcpHandoffMaxEnv bounds the state carried in the environment.
	mcpHandoffMaxEnv = 64 << 10
	// mcpDrainWait bounds the wait for earlier calls before a handoff; past
	// it the request is served by this binary and the next one tries again.
	mcpDrainWait = 30 * time.Second
	// mcpSettle is how old the new binary's modification time must be: a
	// file still being written is left for a later request.
	mcpSettle = 2 * time.Second
)

// mcpHandoff is what one server process hands the next.
type mcpHandoff struct {
	Initialize  json.RawMessage `json:"initialize,omitempty"`
	Negotiated  string          `json:"negotiated,omitempty"`
	Initialized bool            `json:"initialized,omitempty"`
	LogLevel    string          `json:"log_level,omitempty"`
	// Pending is the bytes read off stdin and not yet handled, the held
	// request first.
	Pending []byte `json:"pending,omitempty"`
	// From is the version of the binary that handed over.
	From string `json:"from,omitempty"`
	// Hops counts the handoffs of this connection.
	Hops int `json:"hops,omitempty"`
}

// state is the session state the SDK resumes from.
func (h *mcpHandoff) state() (*mcp.ServerSessionState, error) {
	st := &mcp.ServerSessionState{NegotiatedProtocolVersion: h.Negotiated, LogLevel: mcp.LoggingLevel(h.LogLevel)}
	if len(h.Initialize) > 0 {
		var p mcp.InitializeParams
		if err := json.Unmarshal(h.Initialize, &p); err != nil {
			return nil, fmt.Errorf("handoff: initialize parameters: %w", err)
		}
		st.InitializeParams = &p
	}
	if h.Initialized {
		st.InitializedParams = &mcp.InitializedParams{}
	}
	return st, nil
}

// takeHandoff reads and clears the handoff a previous server process left
// in the environment; nil when this process starts a connection.
func takeHandoff() (*mcpHandoff, error) {
	raw, file := os.Getenv(mcpHandoffEnv), os.Getenv(mcpHandoffFileEnv)
	os.Unsetenv(mcpHandoffEnv)
	os.Unsetenv(mcpHandoffFileEnv)
	var b []byte
	switch {
	case raw != "":
		var err error
		if b, err = base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("handoff: %w", err)
		}
	case file != "":
		var err error
		b, err = os.ReadFile(file)
		os.Remove(file)
		if err != nil {
			return nil, fmt.Errorf("handoff: %w", err)
		}
	default:
		return nil, nil
	}
	var h mcpHandoff
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("handoff: %w", err)
	}
	return &h, nil
}

// selfBinary is the executable this server was started from, as the client
// would start it again (the path it was invoked by, symlinks followed at each
// look), and what it was then.
type selfBinary struct {
	path    string
	started os.FileInfo
	mu      sync.Mutex
	// checked is the last changed file that was tried (its size and time),
	// so a binary that fails its version check is not run again per request.
	checked string
	ok      bool
}

// newSelfBinary finds the executable: the path the process was invoked by
// (made absolute; a bare name is looked up on PATH), else os.Executable.
func newSelfBinary() *selfBinary {
	var p string
	if a := os.Args[0]; a != "" {
		switch {
		case filepath.IsAbs(a):
			p = a
		case strings.ContainsRune(a, filepath.Separator) || strings.ContainsRune(a, '/'):
			p, _ = filepath.Abs(a)
		default:
			p, _ = exec.LookPath(a)
		}
	}
	if p == "" {
		p, _ = os.Executable()
	}
	if p == "" {
		return nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil
	}
	return &selfBinary{path: p, started: fi}
}

// changed reports a binary on disk that is not the one this process runs,
// settled (not being written) and answering `version`; with the version it
// printed.
func (b *selfBinary) changed() (bool, string) {
	if b == nil {
		return false, ""
	}
	fi, err := os.Stat(b.path)
	if err != nil || !fi.Mode().IsRegular() {
		return false, "" // gone or moved: nothing to run
	}
	if os.SameFile(fi, b.started) && fi.Size() == b.started.Size() && fi.ModTime().Equal(b.started.ModTime()) {
		return false, ""
	}
	if time.Since(fi.ModTime()) < mcpSettle {
		return false, ""
	}
	key := fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
	b.mu.Lock()
	defer b.mu.Unlock()
	if key == b.checked {
		return b.ok, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.path, "version")
	platform.Prepare(cmd)
	out, err := cmd.Output()
	b.checked, b.ok = key, err == nil && strings.HasPrefix(string(out), "tacit ")
	return b.ok, strings.TrimSpace(string(out))
}

// stale reports, without running anything, a binary on disk that differs
// from the one this process runs (for the note in tool results).
func (b *selfBinary) stale() bool {
	if b == nil {
		return false
	}
	fi, err := os.Stat(b.path)
	if err != nil {
		return false
	}
	return !os.SameFile(fi, b.started) || fi.Size() != b.started.Size() || !fi.ModTime().Equal(b.started.ModTime())
}

// mcpConn is the stdio connection of `tacit mcp`: newline-delimited JSON-RPC
// on stdin and stdout, read with no read-ahead it cannot hand over, and
// watching for a new binary between requests.
type mcpConn struct {
	pre  *bytes.Reader // bytes a previous process read and did not handle
	in   *bufio.Reader
	out  io.Writer
	wmu  sync.Mutex
	self *selfBinary
	// exec replaces the process with the binary at path, the handoff in env;
	// nil where that is impossible (Windows). It returns only on failure.
	exec func(path string, args, env []string) error

	mu       sync.Mutex
	inflight map[string]bool // calls read and not answered, by id
	state    mcpHandoff      // the session as the messages built it
	initID   string
	failed   bool // a re-exec failed: serve on, and say so
	closed   bool
}

func newMCPConn(h *mcpHandoff, self *selfBinary, execFn func(string, []string, []string) error) *mcpConn {
	c := &mcpConn{out: os.Stdout, self: self, exec: execFn, inflight: map[string]bool{}}
	var pending []byte
	if h != nil {
		pending = h.Pending
		c.state = *h
		c.state.Pending = nil
	}
	c.pre = bytes.NewReader(pending)
	c.in = bufio.NewReaderSize(io.MultiReader(c.pre, os.Stdin), 64<<10)
	return c
}

// Connect implements mcp.Transport: the connection is c itself.
func (c *mcpConn) Connect(context.Context) (mcp.Connection, error) { return c, nil }

func idKey(id jsonrpc.ID) string { return fmt.Sprintf("%T:%v", id.Raw(), id.Raw()) }

// Read is the next message the client sent. A call (other than initialize)
// that finds a new binary on disk is handed to it with the connection.
func (c *mcpConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		line, err := c.in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		msg, derr := jsonrpc.DecodeMessage(bytes.TrimSpace(line))
		if derr != nil {
			return nil, derr
		}
		if req, ok := msg.(*jsonrpc.Request); ok {
			c.note(req)
			if req.IsCall() && req.Method != "initialize" {
				c.maybeHandoff(line)
				c.mu.Lock()
				c.inflight[idKey(req.ID)] = true
				c.mu.Unlock()
			}
		}
		return msg, nil
	}
}

// note keeps what a message says of the session's state.
func (c *mcpConn) note(req *jsonrpc.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch req.Method {
	case "initialize":
		c.state.Initialize = append(json.RawMessage(nil), req.Params...)
		c.state.Initialized = false
		c.initID = idKey(req.ID)
	case "notifications/initialized":
		c.state.Initialized = true
	case "notifications/cancelled":
		// A cancelled call may get no answer: it is no longer waited for.
		var p struct {
			RequestID any `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.RequestID != nil {
			if id, err := jsonrpc.MakeID(p.RequestID); err == nil {
				delete(c.inflight, idKey(id))
			}
		}
	case "logging/setLevel":
		var p struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(req.Params, &p) == nil {
			c.state.LogLevel = p.Level
		}
	}
}

// Write sends a message to the client.
func (c *mcpConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	b, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	_, err = c.out.Write(append(b, '\n'))
	c.wmu.Unlock()
	if resp, ok := msg.(*jsonrpc.Response); ok {
		k := idKey(resp.ID)
		c.mu.Lock()
		if k == c.initID && resp.Error == nil {
			var r struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(resp.Result, &r) == nil {
				c.state.Negotiated = r.ProtocolVersion
			}
		}
		delete(c.inflight, k)
		c.mu.Unlock()
	}
	return err
}

// Close ends the connection. A Read blocked on stdin ends with the process.
func (c *mcpConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// SessionID implements mcp.Connection: stdio has none.
func (c *mcpConn) SessionID() string { return "" }

// outdated reports a server that runs an older binary than the one on disk
// and could not hand over to it (Windows, or a failed re-exec).
func (c *mcpConn) outdated() bool {
	c.mu.Lock()
	failed := c.failed
	c.mu.Unlock()
	return (c.exec == nil || failed) && c.self.stale()
}

// maybeHandoff re-executes the new binary with the connection when there is
// one, after the calls read before line are answered; it returns when there
// is none, or when the handoff could not be made (the request is then served
// here).
func (c *mcpConn) maybeHandoff(line []byte) {
	c.mu.Lock()
	skip := c.exec == nil || c.failed || c.closed
	c.mu.Unlock()
	if skip {
		return
	}
	ok, newVersion := c.self.changed()
	if !ok {
		return
	}
	deadline := time.Now().Add(mcpDrainWait)
	for {
		c.mu.Lock()
		n := len(c.inflight)
		c.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Everything read and not handled: the held request, what the reader
	// buffered after it, and what a previous handoff left unread.
	pending := append([]byte(nil), line...)
	if n := c.in.Buffered(); n > 0 {
		b, _ := c.in.Peek(n)
		pending = append(pending, b...)
	}
	rest, _ := io.ReadAll(c.pre)
	pending = append(pending, rest...)
	c.mu.Lock()
	h := c.state
	c.mu.Unlock()
	h.Pending, h.From, h.Hops = pending, version, h.Hops+1
	b, err := json.Marshal(h)
	if err == nil {
		env := os.Environ()
		file := ""
		if enc := base64.StdEncoding.EncodeToString(b); len(enc) <= mcpHandoffMaxEnv {
			env = append(env, mcpHandoffEnv+"="+enc)
		} else {
			file, err = writeHandoffFile(b)
			env = append(env, mcpHandoffFileEnv+"="+file)
		}
		if err == nil {
			fmt.Fprintf(os.Stderr, "tacit mcp: the binary on disk changed (%s); continuing this connection in it\n", newVersion)
			err = c.exec(c.self.path, os.Args, env)
		}
		if file != "" {
			os.Remove(file)
		}
	}
	// The exec failed: this process still holds the connection, its reader
	// as it was (nothing was consumed), and serves on with the old binary.
	fmt.Fprintf(os.Stderr, "tacit mcp: could not continue in the new binary: %v; serving on with %s\n", err, version)
	c.mu.Lock()
	c.failed = true
	c.mu.Unlock()
	c.pre = bytes.NewReader(rest) // what was drained from it, put back
	c.in = bufio.NewReaderSize(io.MultiReader(bytes.NewReader(bufferedCopy(c.in)), c.pre, os.Stdin), 64<<10)
}

// bufferedCopy is a copy of what r holds buffered, consumed from r.
func bufferedCopy(r *bufio.Reader) []byte {
	n := r.Buffered()
	b := make([]byte, n)
	io.ReadFull(r, b)
	return b
}

// writeHandoffFile keeps a large handoff in a private file of the state
// directory, for the new process to read and delete.
func writeHandoffFile(b []byte) (string, error) {
	home, _ := os.UserHomeDir()
	dir := platform.StateDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "mcp-handoff-*.json")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(f.Name())
		return "", werr
	}
	return f.Name(), nil
}

// outdatedNote is what a tool result says when the server could not move to
// the new binary.
func outdatedNote() string {
	return fmt.Sprintf("note: this tacit MCP server still runs the binary it started with (%s); tacit was updated on disk since. Restart or reconnect this MCP server to use the new one (Claude Code: /mcp, then reconnect tacit; other harnesses: restart the session).", version)
}

// outdatedMiddleware adds outdatedNote to every tool result while conn says
// the server is outdated.
func outdatedMiddleware(conn *mcpConn) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil || !conn.outdated() {
				return res, err
			}
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
				r.Content = append(r.Content, &mcp.TextContent{Text: outdatedNote()})
			}
			return res, err
		}
	}
}

// serveMCP serves server on stdio until the client goes: resumed from a
// handoff when a previous process left one, and handing over to a new
// binary when one appears.
func serveMCP(server *mcp.Server) error {
	h, err := takeHandoff()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tacit mcp:", err)
		h = nil
	}
	conn := newMCPConn(h, newSelfBinary(), execSelf)
	server.AddReceivingMiddleware(outdatedMiddleware(conn))
	var opts *mcp.ServerSessionOptions
	if h != nil {
		st, err := h.state()
		if err != nil {
			return err
		}
		opts = &mcp.ServerSessionOptions{State: st}
		fmt.Fprintf(os.Stderr, "tacit mcp: continued in %s after the binary changed (from %s)\n", version, h.From)
	}
	ss, err := server.Connect(context.Background(), conn, opts)
	if err != nil {
		return err
	}
	if err := ss.Wait(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
