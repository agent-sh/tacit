package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/store"
)

// TestMain lets git run this test binary as `tacit credential <harness>`:
// a clone's credential helper is the running binary (store.helperExe).
func TestMain(m *testing.M) {
	if os.Getenv("TACIT_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// publicServer stands in for the server's public path: every request needs
// a valid token of one enrolled key (401 otherwise), receive-pack is refused
// (403), and the rest is git http-backend over CGI serving bare.
type publicServer struct {
	*httptest.Server
	mu        sync.Mutex
	authed    int
	anonymous int
	pushes    int
}

func newPublicServer(t *testing.T, bare, keyid string, pub ed25519.PublicKey) *publicServer {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"},
	}
	ps := &publicServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if strings.Contains(r.URL.RawQuery, "git-receive-pack") || strings.HasSuffix(r.URL.Path, "git-receive-pack") {
			ps.pushes++
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != keyid || !validToken(keyid, p, pub) {
			ps.anonymous++
			w.Header().Set("WWW-Authenticate", `Basic realm="tacit"`)
			http.Error(w, "unauthorized: the public path needs a reader key token", http.StatusUnauthorized)
			return
		}
		ps.authed++
		if strings.Contains(r.URL.RawQuery, "git-receive-pack") || strings.HasSuffix(r.URL.Path, "git-receive-pack") {
			http.Error(w, "the public path is read-only: push over the tailnet", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(": connected head=0\n\n"))
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(ps.Close)
	return ps
}

// validToken checks a token the way TacitServer.Auth.Token does (without
// the replay check): ts within 60 s, signature over the message.
func validToken(keyid, token string, pub ed25519.PublicKey) bool {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return false
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if d := time.Now().Unix() - sec; d > 60 || d < -60 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, []byte("tacit-v1\n"+keyid+"\n"+parts[0]+"\n"+parts[1]), sig)
}

// The Mac's read-only setup over the public URL: `tacit clone --read-only
// --as reader` clones with the reader key's token on every request, the
// clone syncs with it, the follower signs the feed with it, `tacit authcheck
// reader` asks fetch's question, and nothing in the clone can push.
func TestReadOnlyCloneAsReaderOverThePublicPath(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TACIT_TEST_MAIN", "1")
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	t.Setenv("TACIT_CONFIG", cfg)
	storeDir := filepath.Join(root, "store")
	t.Setenv("TACIT_STORE", storeDir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "local"))

	id := "reader@" + shortHost(mustHostname())
	key, err := auth.Generate(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	// The bare repo is named "git" so that <server>/git is its URL.
	src, _ := reviewFixture(t)
	bare := filepath.Join(root, "srv", "git")
	git(t, root, "clone", "-q", "--bare", src, bare)
	ps := newPublicServer(t, bare, id, key.Priv.Public().(ed25519.PublicKey))

	// Without a key there is nothing to clone with.
	if code := runClone([]string{"--read-only", "--as", "nokey", ps.URL + "/git"}); code != 2 {
		t.Fatalf("clone with no key: exit %d, want 2", code)
	}
	if code := runClone([]string{"--read-only", "--as", "reader", ps.URL + "/git"}); code != 0 {
		t.Fatalf("clone: exit %d", code)
	}
	s := &store.Store{Dir: storeDir}
	ctx := context.Background()
	if !s.ReadOnly(ctx) || s.ReadAs(ctx) != "reader" {
		t.Fatalf("readonly=%v as=%q", s.ReadOnly(ctx), s.ReadAs(ctx))
	}
	if _, err := os.Stat(filepath.Join(storeDir, "system", "a.md")); err != nil {
		t.Fatal("the clone has no facts:", err)
	}
	ps.mu.Lock()
	cloned := ps.authed
	ps.mu.Unlock()
	if cloned == 0 {
		t.Fatal("the clone never sent the reader token")
	}

	// A new commit on the server reaches the clone through a signed fetch.
	work := filepath.Join(root, "work")
	git(t, root, "clone", "-q", bare, work)
	os.WriteFile(filepath.Join(work, "system", "new.md"), []byte(factFile("new")), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "new")
	git(t, work, "push", "-q", "origin", "HEAD:main")
	if err := s.Sync(ctx); err != nil {
		t.Fatal("sync:", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "system", "new.md")); err != nil {
		t.Fatal("sync did not bring the new fact")
	}
	if ahead, err := s.RemoteAhead(ctx); err != nil || ahead {
		t.Fatalf("RemoteAhead = %v, %v", ahead, err)
	}

	// The follower signs the feed with the clone's key.
	u, p, err := cloneAuth(s)
	if err != nil || u != id {
		t.Fatalf("cloneAuth = %q, %v", u, err)
	}
	req, _ := http.NewRequest(http.MethodGet, eventsURL(ps.URL+"/git"), nil)
	req.SetBasicAuth(u, p)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v %v", resp, err)
	}
	resp.Body.Close()

	// authcheck reader asks what a fetch asks, and the public path takes it.
	if code := runAuthcheck([]string{"reader"}); code != authOK {
		t.Fatalf("authcheck reader: exit %d", code)
	}

	// Nothing in the clone pushes: git has no transport for its push URL.
	os.WriteFile(filepath.Join(storeDir, "system", "evil.md"), []byte(factFile("evil")), 0o644)
	git(t, storeDir, "add", "-A")
	git(t, storeDir, "commit", "-q", "-m", "evil")
	cmd := exec.Command("git", "push", "origin", "HEAD:main")
	cmd.Dir = storeDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a push from the read-only clone succeeded:\n%s", out)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.pushes != 0 {
		t.Fatalf("the server saw %d receive-pack request(s) from a read-only clone", ps.pushes)
	}
}

// The answers authcheck reads from the public path.
func TestAuthAnswerPublicPath(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   int
	}{
		{429, "too many failed requests from 1.2.3.4; wait a minute", authRetry},
		{403, "the public path is read-only: push over the tailnet", authStop},
		{401, "writer token rejected: unknown_key", authRetry},
		{401, "unauthorized: the public path needs a reader key token", authRetry},
		{401, "writer token rejected: bad_signature", authStop},
	} {
		if got := authAnswer("reader@host-b", c.status, c.body); got != c.want {
			t.Errorf("%d %q: exit %d, want %d", c.status, c.body, got, c.want)
		}
	}
}
