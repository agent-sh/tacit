package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/store"
)

// App is what the page of tacit app calls. In the desktop window its exported
// methods are Wails bindings (window.go.main.App.<Method>), so nothing listens
// on a port; with --browser the same methods answer POST /api/call/<Method> on
// a loopback port, behind the page's token. Both run the review page's own
// code (reviewServer): the same marks, the same timeline, the same write path.
type App struct {
	rs *reviewServer
}

func newApp(rs *reviewServer) *App { return &App{rs: rs} }

type queueResult struct {
	Items    []any  `json:"items"`
	Host     string `json:"host"`
	ViewOnly string `json:"view_only,omitempty"`
}

// Queue is everything waiting on the owner: open pairs, stale checks, newer
// results, ruling changes, the agents' questions and the dead links in their
// rulings.
func (a *App) Queue() (*queueResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Under rs.mu throughout: the items read rs.facts, which a concurrent
	// Queue, Sync or owner write replaces (Wails runs each call on its own
	// goroutine).
	a.rs.mu.Lock()
	defer a.rs.mu.Unlock()
	a.rs.loadFacts() // another tool or machine may have changed the store
	return &queueResult{Items: a.rs.queue(ctx), Host: store.ShortHost(), ViewOnly: a.rs.viewOnly}, nil
}

// Decide makes one mark on a finding, exactly as the review page does.
func (a *App) Decide(id, action string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.decide(ctx, id, action, false)
}

// Answer makes one mark with the owner's optional note in their own words.
// "wrong" (neither is true) retires both facts of a pair.
func (a *App) Answer(id, action, comment string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.answer(ctx, id, action, comment, false)
}

// Comment leaves the owner's note on a finding without answering it.
func (a *App) Comment(id, comment string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.answer(ctx, id, "note", comment, false)
}

// Timeline is "What git says" under a finding.
func (a *App) Timeline(id string) (*gitev.Timeline, error) {
	if a.rs.tl == nil {
		return nil, errors.New("no timeline here")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t, err := a.rs.timelineFor(ctx, id)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errors.New("reading git took too long on this machine; try again in a moment")
	}
	return t, err
}

// FactTimeline is "What git says" about one fact.
func (a *App) FactTimeline(ref string) (*gitev.Timeline, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t, err := a.rs.factTimeline(ctx, ref)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errors.New("reading git took too long on this machine; try again in a moment")
	}
	return t, err
}

// Meta is what the filters and forms offer.
func (a *App) Meta() (*appMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.meta(ctx)
}

// Browse lists and searches the facts.
func (a *App) Browse(q browseQuery) (*browseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.browse(ctx, q)
}

// Fact is one fact with its frontmatter, links, history and what may be done to it.
func (a *App) Fact(ref string) (*factDetail, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.rs.factDetail(ctx, ref)
}

func writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

// Edit saves the edit form as one commit.
func (a *App) Edit(ref string, d factDraft) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.editFact(ctx, ref, d)
}

// Retire gives the fact a retired date: it stays in the history and stops reaching agents.
func (a *App) Retire(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.setRetired(ctx, ref, true)
}

// Restore clears the retired date.
func (a *App) Restore(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.setRetired(ctx, ref, false)
}

// Undo puts the fact back to before the owner's last change to it here.
func (a *App) Undo(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.undoLast(ctx, ref)
}

// Mine makes a fact the agents wrote their rule, on their word: it records their
// decision and the fact rests on it.
func (a *App) Mine(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.endorse(ctx, ref)
}

// NotMine says a fact agents believe the owner prefers is an agent's, not their rule.
func (a *App) NotMine(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.disown(ctx, ref)
}

// UndoNotMine takes back their last Not mine on a fact.
func (a *App) UndoNotMine(ref string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.undoNotMine(ctx, ref)
}

// NewRuling writes the owner's own words as a ruling.
func (a *App) NewRuling(d rulingDraft) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.newRuling(ctx, d)
}

// ApplyLinkFix writes the curator's fix for one dead link into a ruling.
func (a *App) ApplyLinkFix(id string) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.applyLinkFix(ctx, id)
}

// Sync pulls what other machines wrote.
func (a *App) Sync() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.rs.mu.Lock()
	defer a.rs.mu.Unlock()
	err := a.rs.s.Sync(ctx)
	a.rs.snapReset()
	a.rs.loadFacts()
	if err != nil {
		return "", fmt.Errorf("sync failed, showing this machine's copy: %s", shortErr(err))
	}
	return "Up to date with the server.", nil
}

// call runs one App method by name with its arguments as a JSON array, the
// way the Wails bridge does. Only exported methods of App can be reached.
func (a *App) call(name string, raw []byte) (any, error) {
	m := reflect.ValueOf(a).MethodByName(name)
	if !m.IsValid() || name == "" || strings.ToLower(name[:1]) == name[:1] {
		return nil, fmt.Errorf("no method %q", name)
	}
	var args []json.RawMessage
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, errors.New("arguments must be a JSON array")
		}
	}
	t := m.Type()
	if len(args) != t.NumIn() {
		return nil, fmt.Errorf("%s takes %d argument(s), got %d", name, t.NumIn(), len(args))
	}
	in := make([]reflect.Value, len(args))
	for i := range args {
		v := reflect.New(t.In(i))
		if err := json.Unmarshal(args[i], v.Interface()); err != nil {
			return nil, fmt.Errorf("argument %d of %s: %v", i+1, name, err)
		}
		in[i] = v.Elem()
	}
	out := m.Call(in)
	if len(out) == 0 {
		return nil, nil
	}
	if e, ok := out[len(out)-1].Interface().(error); ok && e != nil {
		return nil, e
	}
	if len(out) == 1 {
		return nil, nil
	}
	return out[0].Interface(), nil
}

// appAssets are the files of the page besides the page itself.
var appAssets = []string{"/fonts/", "/app.css", "/desk.css", "/app.js", "/api.js", "/desk.js", "/folders.js", "/theme.js", "/icon.svg"}

// uiHandler serves the app page and its files. With a token it also answers
// /api/call/<Method> (tacit review, tacit app --browser); the desktop window
// calls the bindings. With a token the page itself is served only to the URL
// that carries it (ownerURL), so the token never reaches a client that was
// not handed that URL: an agent that can reach the port cannot read the
// token from the page.
func (a *App) uiHandler(token string) http.Handler {
	return pageHandler("app.html", token, appAssets, func(mux *http.ServeMux) {
		if token == "" {
			return
		}
		mux.HandleFunc("/api/call/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodPost || !tokenOK(r.Header.Get("X-Tacit-Token"), token) {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
				return
			}
			body := http.MaxBytesReader(w, r.Body, 1<<20)
			var raw json.RawMessage
			if err := json.NewDecoder(body).Decode(&raw); err != nil && err.Error() != "EOF" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
				return
			}
			res, err := a.call(strings.TrimPrefix(r.URL.Path, "/api/call/"), raw)
			if err != nil {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"result": res})
		})
	})
}

// tokenQuery is the query parameter of the owner page's URL that carries
// its token.
const tokenQuery = "t"

// tokenOK compares a presented token with the run's, in constant time; an
// empty run token (the desktop window) never matches.
func tokenOK(got, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// pageHandler serves page (with the token in its tacit-token meta) at / and
// the assets beside it, plus what api registers. With a token, / needs
// ?t=<token>: anything else gets a short refusal, never the page.
func pageHandler(page, token string, assets []string, api func(*http.ServeMux)) http.Handler {
	ui, _ := fs.Sub(reviewUI, "reviewui")
	body, _ := fs.ReadFile(ui, page)
	body = []byte(strings.Replace(string(body), "{{TOKEN}}", token, 1))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if token != "" && !tokenOK(r.URL.Query().Get(tokenQuery), token) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "This page opens only from the link tacit printed or opened when it started. Run it again to get a new link.\n")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	})
	files := http.FileServer(http.FS(ui))
	for _, p := range assets {
		mux.Handle(p, files)
	}
	if api != nil {
		api(mux)
	}
	return mux
}

// newToken is one run's token: 16 random bytes, hex.
func newToken() string {
	var tb [16]byte
	rand.Read(tb[:])
	return hex.EncodeToString(tb[:])
}

// ownerURL is the URL that opens the owner's page: the token rides on it.
func ownerURL(addr, token string) string {
	return "http://" + addr + "/?" + tokenQuery + "=" + token
}

// stdoutIsTerminal reports whether this process prints to a terminal. An
// agent's shell tool reads a pipe, so the owner page's link (which carries
// the token) is printed only to a terminal, where the owner sits.
var stdoutIsTerminal = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// openBrowser opens the owner page's URL (a variable for tests).
var openBrowser = openURL

// announceOwner prints the line that says where the owner's page is and
// returns its URL with the token. The token is printed only to a terminal.
func announceOwner(w io.Writer, name, addr, token string, tty bool) string {
	url := ownerURL(addr, token)
	if tty {
		fmt.Fprintf(w, "tacit %s: %s (Ctrl-C to stop)\n", name, url)
	} else {
		fmt.Fprintf(w, "tacit %s: http://%s/ opened in your browser; the link with its token is shown only on a terminal (Ctrl-C to stop)\n", name, addr)
	}
	return url
}

// serveOwner listens on addr (loopback only) and serves h, the owner's page
// (tacit review, tacit app --browser). The URL with the run's token goes to
// the browser it opens, and is printed only on a terminal: an agent running
// the command through a pipe sees the address without the token, and the
// page and its API refuse it. With --no-open and no terminal there is no
// one to give the link to, so it refuses to start.
func serveOwner(name, addr, token string, noOpen bool, h http.Handler) int {
	if noOpen && !stdoutIsTerminal() {
		fmt.Fprintf(os.Stderr, "%s: --no-open needs a terminal to print the page's link to (the link carries the page's token, which is never printed where an agent reads it)\n", name)
		return 2
	}
	ln, err := listenLoopback(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	url := announceOwner(os.Stdout, name, ln.Addr().String(), token, stdoutIsTerminal())
	if !noOpen {
		openBrowser(runtime.GOOS, url)
	}
	if err := http.Serve(ln, browserHandler(h)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// runApp implements `tacit app [--browser [--addr 127.0.0.1:7450] [--no-open]]`.
func runApp(args []string) int {
	fsf := flag.NewFlagSet("app", flag.ExitOnError)
	browser := fsf.Bool("browser", false, "serve the app page in a browser tab instead of the desktop window")
	addr := fsf.String("addr", "127.0.0.1:7450", "with --browser: listen address (loopback only)")
	noOpen := fsf.Bool("no-open", false, "with --browser: do not open a browser")
	fsf.Parse(args)
	if *browser {
		if host, _, err := net.SplitHostPort(*addr); err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			fmt.Fprintln(os.Stderr, "app: --addr must be a loopback address")
			return 2
		}
	}
	if !*browser && !desktopBuild {
		fmt.Fprintln(os.Stderr, "app: this tacit was built without the desktop window; build it with -tags desktop,production,webkit2_41 (see README), or run tacit app --browser")
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "app: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "app: sync failed, showing local state:", err)
	}
	cancel()
	rs := &reviewServer{s: s, tl: newTimelineBuilder(s, time.Time{})}
	rs.viewOnly = viewOnlyReason(context.Background(), s)
	rs.loadFacts()
	app := newApp(rs)
	token := newToken()
	// Pull what other machines wrote, every two minutes, under the store lock.
	go func() {
		for range time.Tick(2 * time.Minute) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rs.mu.Lock()
			if err := s.Sync(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "app: sync:", err)
			}
			rs.mu.Unlock()
			cancel()
		}
	}()
	if !*browser {
		if err := runDesktop(app); err != nil {
			fmt.Fprintln(os.Stderr, "app:", err)
			return 1
		}
		return 0
	}
	return serveOwner("app", *addr, token, *noOpen, app.uiHandler(token))
}

// listenLoopback listens on addr only when it is a loopback address.
func listenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return nil, errors.New("--addr must be a loopback address")
	}
	return net.Listen("tcp", addr)
}

// browserHandler is what browser mode serves: only to a loopback Host (a
// DNS-rebound name is refused), under the review page's content policy.
func browserHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hh, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			hh = r.Host
		}
		if hh != "127.0.0.1" && hh != "localhost" && hh != "[::1]" && hh != "::1" {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

// desktopEntry is ~/.local/share/applications/tacit.desktop.
func desktopEntry(exe, storeEnv string) string {
	q := func(s string) string {
		if strings.ContainsAny(s, " \t\"'\\$`") {
			return `"` + strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(s) + `"`
		}
		return s
	}
	cmd := q(exe) + " app"
	var env []string
	if storeEnv != "" {
		env = append(env, q("TACIT_STORE="+storeEnv))
	}
	if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
		env = append(env, q("TACIT_SETTINGS="+cfg))
	}
	if len(env) > 0 {
		cmd = "env " + strings.Join(env, " ") + " " + cmd
	}
	return "[Desktop Entry]\nType=Application\nName=tacit\nComment=What needs you, and every fact your agents read\nExec=" + cmd + "\nIcon=tacit\nTerminal=false\nCategories=Development;Utility;\nStartupWMClass=" + filepath.Base(exe) + "\n"
}

// installApp writes the desktop entry and the icon under home. It never runs
// anything.
func installApp(home, exe, storeEnv string) ([]string, error) {
	icon, err := fs.ReadFile(reviewUI, "reviewui/icon.svg")
	if err != nil {
		return nil, err
	}
	files := []struct {
		path string
		data []byte
	}{
		{filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps", "tacit.svg"), icon},
		{filepath.Join(home, ".local", "share", "applications", "tacit.desktop"), []byte(desktopEntry(exe, storeEnv))},
	}
	var out []string
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return out, err
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return out, err
		}
		out = append(out, f.path)
	}
	return out, nil
}
