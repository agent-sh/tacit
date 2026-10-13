package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/gitev"
)

// timelineFixture is the review fixture plus a projects directory holding
// repo x, which fact a names a path in and which changed that path after
// both facts were written.
func timelineFixture(t *testing.T) (rs *reviewServer, projects string, ab reviewItem) {
	_, clone := reviewFixture(t)
	rs = clone("one")
	projects = t.TempDir()
	repo := filepath.Join(projects, "x")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	t.Setenv("GIT_AUTHOR_DATE", "2026-10-05T12:00:00+00:00")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-05T12:00:00+00:00")
	os.MkdirAll(filepath.Join(repo, "src"), 0o755)
	os.WriteFile(filepath.Join(repo, "src", "engine.rs"), []byte("fn main() {}\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "engine: drop the old loop")
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "paths: []", `paths: ["src/engine.rs"]`, 1)), 0o644)
	rs.loadFacts()
	return rs, projects, itemFor(t, rs, "a", "b")
}

func noRedact(in []string) ([]string, error) { return in, nil }

// GET /api/timeline gives the page one proof's timeline, to the page that
// holds the token and by GET only.
func TestReviewServesTheTimeline(t *testing.T) {
	rs, projects, ab := timelineFixture(t)
	rs.token = "page-token"
	get := func(id, token, method string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/timeline?id="+id, nil)
		if token != "" {
			req.Header.Set("X-Tacit-Token", token)
		}
		rs.handleTimeline(rr, req)
		return rr
	}
	if rr := get(ab.ID, "", http.MethodGet); rr.Code != http.StatusForbidden {
		t.Fatalf("a request without the token answered %d", rr.Code)
	}
	if rr := get(ab.ID, "guess", http.MethodGet); rr.Code != http.StatusForbidden {
		t.Fatalf("a wrong token answered %d", rr.Code)
	}
	if rr := get(ab.ID, "page-token", http.MethodPost); rr.Code != http.StatusForbidden {
		t.Fatalf("POST answered %d", rr.Code)
	}
	if rr := get(ab.ID, "page-token", http.MethodGet); rr.Code != http.StatusNotFound {
		t.Fatalf("a server without a builder answered %d", rr.Code)
	}
	rs.tl = &gitev.Builder{G: gitev.New(projects), StoreDir: rs.s.Dir, Redact: noRedact}
	rr := get(ab.ID, "page-token", http.MethodGet)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var tl gitev.Timeline
	if err := json.Unmarshal(rr.Body.Bytes(), &tl); err != nil {
		t.Fatal(err)
	}
	var commit, head, fact bool
	for _, e := range tl.Events {
		commit = commit || (e.Kind == gitev.KindCommit && e.Text == "engine: drop the old loop" && e.Repo == "x" && e.Ref != "")
		head = head || e.Kind == gitev.KindHead
		fact = fact || e.Kind == gitev.KindFact
	}
	if !commit || !head || !fact || tl.Incomplete {
		t.Errorf("events %+v", tl.Events)
	}
	if rr := get("000000000000", "page-token", http.MethodGet); rr.Code != http.StatusConflict {
		t.Errorf("unknown finding answered %d", rr.Code)
	}
	// Asked again, the second answer comes from memory.
	runs := rs.tl.G.Runs()
	get(ab.ID, "page-token", http.MethodGet)
	if rs.tl.G.Runs() != runs {
		t.Errorf("the second request ran git again")
	}
}

// The handler's own deadline is an answer, not silence: 504 with a message,
// which the page shows. Only a page that has left (its request context ended)
// gets nothing.
func TestTimelineDeadlineIsAnErrorAndAPageThatLeftGetsNothing(t *testing.T) {
	rs, projects, ab := timelineFixture(t)
	rs.token = "tok"
	var calls int32
	release := make(chan struct{})
	defer close(release)
	rs.tl = blockingBuilder(rs, projects, &calls, release)
	rs.tlTimeout = 100 * time.Millisecond
	req := func(ctx context.Context) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/timeline?id="+ab.ID, nil).WithContext(ctx)
		r.Header.Set("X-Tacit-Token", "tok")
		return r
	}
	rr := httptest.NewRecorder()
	rs.handleTimeline(rr, req(context.Background()))
	var body struct{ Error string }
	if rr.Code != http.StatusGatewayTimeout || json.Unmarshal(rr.Body.Bytes(), &body) != nil || body.Error == "" {
		t.Fatalf("the handler's own deadline answered %d %q", rr.Code, rr.Body.String())
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rr = httptest.NewRecorder()
	rs.handleTimeline(rr, req(gone))
	if rr.Body.Len() != 0 {
		t.Errorf("a page that left was sent %q", rr.Body.String())
	}
}

// The page never turns an answer without a timeline into "nothing to
// compare": a body without events, or an error status, is an error that is
// kept for 20 s, a real timeline for ten minutes. This runs the page's own
// script against stubs.
func TestPageTreatsAnAnswerWithoutEventsAsAnError(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	// The page's script runs inside a function with stub DOM and fetch; the
	// checks are appended to it (its let and const names are in scope).
	script := `
const src = require("fs").readFileSync(process.argv[1], "utf8");
const mk = () => new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => "" : k === "then" ? undefined : mk()), apply: () => mk(), set: () => true });
const sub = { textContent: "" };
const tl = { innerHTML: "", removeAttribute() {}, querySelector: () => sub };
const tst = { answer: async () => ({}), sub, tl };
const document = { querySelector: () => ({ content: "tok" }), getElementById: (id) => (id === "timeline" ? tl : mk()), addEventListener() {}, documentElement: { dataset: {} } };
const fetchStub = async (url) => (String(url).startsWith("/api/items") ? { ok: true, json: async () => ({ items: [], host: "h" }) } : tst.answer());
const body = src + ";return (async () => {" + process.argv[2] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "tst", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, tst).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	checks := `
  await new Promise((r) => setTimeout(r, 50)); // let the first load() finish, it resets the list
  view = [{ id: "x" }]; cur = 0;
  const out = {};
  const ask = async (a) => { tlCache.clear(); tst.sub.textContent = ""; tst.tl.innerHTML = ""; tst.answer = a; await timeline({ id: "x" }); const c = tlCache.get("x"); return { sub: tst.sub.textContent, html: tst.tl.innerHTML, ttl: c && c.ttl }; };
  out.empty = await ask(async () => ({ ok: true, json: async () => ({}) }));
  out.unreadable = await ask(async () => ({ ok: true, json: async () => { throw new Error("not json"); } }));
  out.timeout = await ask(async () => ({ ok: false, json: async () => ({ error: "reading git took too long" }) }));
  out.good = await ask(async () => ({ ok: true, json: async () => ({ repos: [], terms: [], events: [] }) }));
  out.partial = await ask(async () => ({ ok: true, json: async () => ({ repos: ["r"], terms: [], events: [], incomplete: true }) }));
  out.summary = await ask(async () => ({ ok: true, json: async () => ({ repos: ["r"], terms: [], events: [{ when: "2026-10-03", kind: "commit", repo: "r", ref: "abc1234", text: "drop it" }], summary: ["In r, nothing they name changed since 1 Oct."] }) }));
  return JSON.stringify(out);
`
	out, err := exec.Command(node, "-e", script, "reviewui/app.js", checks).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got map[string]struct {
		Sub, Html string
		Ttl       int
	}
	if err := json.Unmarshal(out[strings.LastIndex(strings.TrimSpace(string(out)), "\n")+1:], &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, k := range []string{"empty", "unreadable", "timeout"} {
		r := got[k]
		if !strings.Contains(r.Sub, "could not be read") || strings.Contains(r.Html, "no repo") || r.Html != "" || r.Ttl != 20000 {
			t.Errorf("%s: %+v, want an error line, no timeline painted, a 20 s hold", k, r)
		}
	}
	if !strings.Contains(got["timeout"].Sub, "took too long") {
		t.Errorf("the server's message is lost: %+v", got["timeout"])
	}
	if g := got["good"]; !strings.Contains(g.Html, "What git shows") || g.Ttl != 600000 {
		t.Errorf("a real answer: %+v", g)
	}
	if p := got["partial"]; !strings.Contains(p.Html, "partial view") || p.Ttl != 20000 {
		t.Errorf("a partial answer: %+v", p)
	}
	// The plain summary leads; the commits are folded under it.
	if g := got["summary"]; !strings.Contains(g.Html, `<ul class="sum"><li>In r, nothing they name changed since 1 Oct.</li></ul>`) || !strings.Contains(g.Html, `<details class="full"><summary>Full git history</summary>`) || !strings.Contains(g.Html, "drop it") {
		t.Errorf("summary: %+v", g)
	}
}

// blockingBuilder is a builder whose redactor counts its calls (one per
// build) and holds every build until release is closed.
func blockingBuilder(rs *reviewServer, projects string, calls *int32, release chan struct{}) *gitev.Builder {
	return &gitev.Builder{G: gitev.New(projects), StoreDir: rs.s.Dir, Redact: func(in []string) ([]string, error) {
		atomic.AddInt32(calls, 1)
		<-release
		return in, nil
	}}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waiters(rs *reviewServer) (n, flights int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, f := range rs.tlFlights {
		n += f.waiters
	}
	return n, len(rs.tlFlights)
}

// Holding j through the proofs asks for many timelines; requests for one
// proof share one build, and a build nobody waits for any more is stopped
// and not kept.
func TestTimelineRequestsShareOneBuildAndALeftBuildIsNotKept(t *testing.T) {
	rs, projects, ab := timelineFixture(t)
	var calls int32
	release := make(chan struct{})
	rs.tl = blockingBuilder(rs, projects, &calls, release)

	// Six requests for one proof, one build.
	type res struct {
		t   *gitev.Timeline
		err error
	}
	out := make(chan res, 6)
	for i := 0; i < 6; i++ {
		go func() {
			tl, err := rs.timelineFor(context.Background(), ab.ID)
			out <- res{tl, err}
		}()
	}
	waitFor(t, "six waiters on one flight", func() bool { n, f := waiters(rs); return n == 6 && f == 1 })
	close(release)
	for i := 0; i < 6; i++ {
		if r := <-out; r.err != nil || r.t == nil {
			t.Fatalf("request %d: %v", i, r.err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("%d builds for six requests of one proof", n)
	}
	// Complete, so remembered: no new build.
	if _, err := rs.timelineFor(context.Background(), ab.ID); err != nil || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("a finished timeline was built again (%d builds, %v)", atomic.LoadInt32(&calls), err)
	}

	// A request that goes away stops its build, and what that build returns
	// is not remembered.
	rs2, projects2, ab2 := timelineFixture(t)
	var calls2 int32
	release2 := make(chan struct{})
	rs2.tl = blockingBuilder(rs2, projects2, &calls2, release2)
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() { _, err := rs2.timelineFor(ctx, ab2.ID); gone <- err }()
	waitFor(t, "the first build to start", func() bool { return atomic.LoadInt32(&calls2) == 1 })
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the request that left got %v", err)
	}
	if _, f := waiters(rs2); f != 0 {
		t.Fatalf("%d flights left with nobody waiting", f)
	}
	next := make(chan error, 1)
	go func() { _, err := rs2.timelineFor(context.Background(), ab2.ID); next <- err }()
	waitFor(t, "a new build for the next request", func() bool { return atomic.LoadInt32(&calls2) == 2 })
	close(release2)
	if err := <-next; err != nil {
		t.Fatal(err)
	}
	// The cancelled build finished too, but only the complete one is kept.
	waitFor(t, "both builds to finish", func() bool { _, f := waiters(rs2); return f == 0 })
	if _, err := rs2.timelineFor(context.Background(), ab2.ID); err != nil || atomic.LoadInt32(&calls2) != 2 {
		t.Fatalf("builds %d, want the memo of the second to serve (%v)", atomic.LoadInt32(&calls2), err)
	}
}

// A timeline with a read that failed is returned, marked incomplete, and not
// remembered: the next request reads again.
func TestIncompleteTimelineIsNotRemembered(t *testing.T) {
	rs, projects, ab := timelineFixture(t)
	// Repo y is a checkout whose git directory is gone: every read fails.
	os.MkdirAll(filepath.Join(projects, "y"), 0o755)
	os.WriteFile(filepath.Join(projects, "y", ".git"), []byte("gitdir: /nonexistent/tacit-test\n"), 0o644)
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `repos: ["x"]`, `repos: ["y", "x"]`, 1)), 0o644)
	rs.loadFacts()
	var calls int32
	rs.tl = &gitev.Builder{G: gitev.New(projects), StoreDir: rs.s.Dir, Redact: func(in []string) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return in, nil
	}}
	for i := 1; i <= 2; i++ {
		tl, err := rs.timelineFor(context.Background(), ab.ID)
		if err != nil || !tl.Incomplete {
			t.Fatalf("read %d: incomplete %v err %v", i, tl != nil && tl.Incomplete, err)
		}
		if n := atomic.LoadInt32(&calls); int(n) != i {
			t.Fatalf("after read %d the builder ran %d times: an incomplete timeline was remembered", i, n)
		}
	}
}

func TestParseAsOf(t *testing.T) {
	got, err := parseAsOf("2026-10-08T18:20:00-05:00")
	if err != nil || got.UTC().Format(time.RFC3339) != "2026-10-08T23:20:00Z" {
		t.Errorf("RFC 3339: %v %v", got, err)
	}
	for _, in := range []string{"2026-10-09", "2026-10-09 02:20"} {
		got, err := parseAsOf(in)
		if err != nil || !strings.HasPrefix(got.Format("2006-01-02 15:04"), in) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseAsOf("yesterday"); err == nil {
		t.Error("a word was taken for a time")
	}
}
