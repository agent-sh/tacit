package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/store"
)

func ownerFactFile(name, scope, kind, typ, body string) string {
	evidence := ""
	if kind == "user" {
		evidence = "  endorsed: " + mineID(name) + "\n" // a ruling: the owner said it is theirs (markAudited records the decision)
	}
	return "---\nname: " + name + "\ndescription: \"fact " + name + "\"\ntype: " + typ + "\nscope: " + scope + "\nsource:\n  harness: curator\n  host: h\n  kind: " + kind + "\n" + evidence + "created: 2026-10-01\n---\n\n" + body + "\n"
}

// ownerFixture is a bare remote seeded with two plain facts, an owner ruling
// with a dead link (and its LINK? line), an inbox note, a generated fact, and a
// stale? finding and a pair in the review file.
func ownerFixture(t *testing.T) (bare string, clone func(string) *reviewServer) {
	t.Helper()
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	root := t.TempDir()
	bare = filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	files := map[string]string{
		"system/a.md":                  factFile("a"),
		"system/b.md":                  factFile("b"),
		"projects/x/rule.md":           ownerFactFile("rule", "projects/x", "user", "user_standard", "Always ask first. See [[gone-fact]]."),
		"projects/x/engine-retries.md": ownerFactFile("engine-retries", "projects/x", "observed", "design_decision", "The engine retries three times."),
		"inbox/claude@h/note.md":       "---\nname: note\ndescription: an agent's note about x\ntype: unclassified\nscope: projects/x\nsource:\n  harness: claude\n  host: h\n  kind: observed\ncreated: 2026-10-02\n---\n\nThe owner said: never push on Fridays; the engine waits.\n",
		"derived/x/x-hot-spots.md":     "---\nname: x-hot-spots\ndescription: hot spots of x\ntype: hot_spot\nscope: derived/x\nsource:\n  harness: curator\n  host: h\n  kind: derived\ncreated: 2026-10-01\nderived_from: abc123\n---\n\nsrc/a.go 3 commits\n",
		"review/2026-10-01.md":         "- supersedes? [[b]] may replace [[a]]\n- stale? [[engine-retries]] (projects/x) @abc1234 n=1 since=2026-10-01\n  2026-10-02 abc1234 x: rename - MAX_RETRIES (gone at HEAD)\n\n## link check\n\n- LINK? [[rule]] (projects/x) links to [[gone-fact]], and no fact is named gone-fact; this is an owner ruling, so the curator left it; it would write plain text\n",
	}
	for rel, body := range files {
		os.MkdirAll(filepath.Join(seed, filepath.Dir(rel)), 0o755)
		os.WriteFile(filepath.Join(seed, rel), []byte(body), 0o644)
	}
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	clone = func(name string) *reviewServer {
		dir := filepath.Join(root, name)
		git(t, root, "clone", "-q", bare, dir)
		git(t, dir, "config", "user.email", "t@t")
		git(t, dir, "config", "user.name", "t")
		rs := &reviewServer{s: &store.Store{Dir: dir}}
		rs.loadFacts()
		return rs
	}
	return bare, clone
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func readRel(t *testing.T, rs *reviewServer, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func clean(t *testing.T, rs *reviewServer) {
	t.Helper()
	if out := git(t, rs.s.Dir, "status", "--porcelain"); out != "" {
		t.Fatalf("clone left dirty:\n%s", out)
	}
}

func draftOf(t *testing.T, rs *reviewServer, ref string) factDraft {
	t.Helper()
	d, err := rs.factDetail(ctxT(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	return factDraft{Rev: d.Rev, Description: d.Description, Body: d.Body, Scope: d.Scope, Type: d.Type, AppliesTo: d.AppliesTo}
}

// An edit is one commit on the server that names the owner as the editor;
// undo puts the bytes back exactly, and a second undo redoes.
func TestOwnerEditLandsAndUndoRestoresExactly(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	orig := readRel(t, rs, "system/a.md")
	d := draftOf(t, rs, "a")
	d.Description, d.Body = "a says something new", "the new body of a"
	res, err := rs.editFact(ctx, "a", d)
	if err != nil || !res.Saved {
		t.Fatalf("edit: %+v %v", res, err)
	}
	if subj := strings.TrimSpace(git(t, bare, "log", "-1", "--format=%s")); subj != "owner: edit a (description, body)" {
		t.Fatalf("server subject %q", subj)
	}
	now := readRel(t, rs, "system/a.md")
	if !strings.Contains(now, "description: a says something new") || !strings.Contains(now, "the new body of a") || !strings.Contains(now, "updated: \""+today()+"\"") {
		t.Fatalf("edited file:\n%s", now)
	}
	d2, _ := rs.factDetail(ctx, "a")
	if !d2.Undo.Can || d2.History[0].Who != "you" {
		t.Fatalf("undo view %+v, history %+v", d2.Undo, d2.History)
	}
	if res, err := rs.undoLast(ctx, "a"); err != nil || !res.Saved {
		t.Fatalf("undo: %+v %v", res, err)
	}
	if readRel(t, rs, "system/a.md") != orig {
		t.Fatal("undo did not restore the bytes")
	}
	if res, err := rs.undoLast(ctx, "a"); err != nil || !res.Saved {
		t.Fatalf("redo: %+v %v", res, err)
	}
	if readRel(t, rs, "system/a.md") != now {
		t.Fatal("a second undo did not redo the edit")
	}
	clean(t, rs)
}

// A change the linter refuses is not saved: the clone and the server stay as
// they were, and the findings come back for the form, which keeps the draft.
func TestOwnerEditLintFailureKeepsTheStoreAndReturnsFindings(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("one")
	head := strings.TrimSpace(git(t, bare, "rev-parse", "main"))
	orig := readRel(t, rs, "system/a.md")
	d := draftOf(t, rs, "a")
	d.Body = "now links to [[no-such-fact]]"
	res, err := rs.editFact(ctxT(t), "a", d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Saved || len(res.Lint) != 1 || !strings.Contains(res.Lint[0], "L6") || !strings.Contains(res.Lint[0], "no-such-fact") {
		t.Fatalf("result %+v", res)
	}
	if readRel(t, rs, "system/a.md") != orig || strings.TrimSpace(git(t, bare, "rev-parse", "main")) != head {
		t.Fatal("a refused edit changed the store")
	}
	clean(t, rs)
	// The ruling's own dead link is old news: editing the ruling is allowed.
	r := draftOf(t, rs, "rule")
	r.Description = "always ask first"
	if res, err := rs.editFact(ctxT(t), "rule", r); err != nil || !res.Saved {
		t.Fatalf("a finding the store already had blocked the edit: %+v %v", res, err)
	}
}

// A scope change moves the file in one commit; undo moves it back.
func TestOwnerScopeChangeMovesTheFileAndUndoMovesItBack(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	orig := readRel(t, rs, "system/b.md")
	d := draftOf(t, rs, "b")
	d.Scope = "projects/x"
	if res, err := rs.editFact(ctx, "b", d); err != nil || !res.Saved {
		t.Fatalf("move: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(rs.s.Dir, "system", "b.md")); !os.IsNotExist(err) {
		t.Fatal("the old path is still there")
	}
	if !strings.Contains(readRel(t, rs, "projects/x/b.md"), "scope: projects/x") {
		t.Fatal("the new file lacks its scope")
	}
	if msg := git(t, rs.s.Dir, "log", "-1", "--format=%B"); !strings.Contains(msg, movedTrailer+"system/b.md") {
		t.Fatalf("message %q", msg)
	}
	if res, err := rs.undoLast(ctx, "b"); err != nil || !res.Saved {
		t.Fatalf("undo: %+v %v", res, err)
	}
	if readRel(t, rs, "system/b.md") != orig {
		t.Fatal("undo did not put the file back at its old path")
	}
	if _, err := os.Stat(filepath.Join(rs.s.Dir, "projects", "x", "b.md")); !os.IsNotExist(err) {
		t.Fatal("undo left the moved copy")
	}
	clean(t, rs)
}

// Retire sets retired and leaves the file in place, out of MEMORY.md; restore
// clears it. Undo is refused when the last change was not the owner's.
func TestOwnerRetireRestoreAndUndoGuard(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	if _, err := rs.undoLast(ctx, "a"); err == nil || !strings.Contains(err.Error(), "not one you made here") {
		t.Fatalf("undo of someone else's change: %v", err)
	}
	if res, err := rs.setRetired(ctx, "a", true); err != nil || !res.Saved {
		t.Fatalf("retire: %+v %v", res, err)
	}
	if !strings.Contains(readRel(t, rs, "system/a.md"), "retired: "+today()) {
		t.Fatal("no retired line")
	}
	if strings.Contains(readRel(t, rs, "MEMORY.md"), "[a](") {
		t.Fatal("a retired fact is still in the index")
	}
	if res, _ := rs.setRetired(ctx, "a", true); res.Saved || !strings.Contains(res.Note, "Already retired") {
		t.Fatalf("second retire: %+v", res)
	}
	if res, err := rs.setRetired(ctx, "a", false); err != nil || !res.Saved {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if strings.Contains(readRel(t, rs, "system/a.md"), "retired:") || !strings.Contains(readRel(t, rs, "MEMORY.md"), "[a](") {
		t.Fatal("restore did not bring the fact back")
	}
	if fs, _ := lint.Run(rs.s.Dir); len(newFindings(nil, fs)) != 1 { // the ruling's dead link only
		t.Fatalf("lint after retire and restore: %v", fs)
	}
	clean(t, rs)
}

// Inbox notes and generated facts are not the owner's to change here.
func TestOwnerWritesRefuseInboxAndGenerated(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	for _, ref := range []string{"inbox/claude@h/note.md", "x-hot-spots"} {
		if _, err := rs.setRetired(ctx, ref, true); err == nil {
			t.Errorf("retire %s was allowed", ref)
		}
		if _, err := rs.editFact(ctx, ref, factDraft{Description: "x", Body: "y", Scope: "system", Type: "reference"}); err == nil {
			t.Errorf("edit %s was allowed", ref)
		}
	}
	d, _ := rs.factDetail(ctx, "inbox/claude@h/note.md")
	if d.CanEdit || d.CanRetire || !d.Inbox {
		t.Fatalf("inbox detail %+v", d)
	}
	clean(t, rs)
}

// A fact that changed after the form opened is not overwritten.
func TestOwnerEditRefusesAStaleForm(t *testing.T) {
	_, clone := ownerFixture(t)
	one, two := clone("one"), clone("two")
	ctx := ctxT(t)
	d := draftOf(t, one, "a")
	other := draftOf(t, two, "a")
	other.Body = "edited on the other machine"
	if res, err := two.editFact(ctx, "a", other); err != nil || !res.Saved {
		t.Fatalf("%+v %v", res, err)
	}
	d.Body = "edited here from an old copy"
	head := strings.TrimSpace(git(t, one.s.Dir, "rev-parse", "HEAD"))
	// The review's repro: the refusal, then the same kept draft sent again
	// (the page used to re-arm it with the new rev). Every send of the old rev
	// is refused, and so is a draft with no rev at all.
	for i := 0; i < 2; i++ {
		res, err := one.editFact(ctx, "a", d)
		if err != nil || res.Saved || !res.Changed || !strings.Contains(res.Note, "changed since you opened it") {
			t.Fatalf("stale form, send %d: %+v %v", i+1, res, err)
		}
	}
	norev := d
	norev.Rev = ""
	if res, err := one.editFact(ctx, "a", norev); err != nil || !res.Changed {
		t.Fatalf("a draft without a rev: %+v %v", res, err)
	}
	if !strings.Contains(readRel(t, one, "system/a.md"), "edited on the other machine") {
		t.Fatal("the other machine's edit was lost")
	}
	// Only the owner's explicit "keep mine" sends the new rev, and then it saves.
	now := draftOf(t, one, "a")
	d.Rev = now.Rev
	if res, err := one.editFact(ctx, "a", d); err != nil || !res.Saved {
		t.Fatalf("keep mine over the new version: %+v %v", res, err)
	}
	if strings.TrimSpace(git(t, one.s.Dir, "rev-parse", "HEAD")) == head {
		t.Fatal("no commit after keep mine")
	}
	clean(t, one)
}

// An owner commit names its files: something else staged in the clone does
// not ride along under the owner's name.
func TestOwnerCommitNamesItsFiles(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("one")
	os.WriteFile(filepath.Join(rs.s.Dir, "system", "b.md"), []byte(strings.Replace(factFile("b"), "body of b", "staged by someone else", 1)), 0o644)
	git(t, rs.s.Dir, "add", "system/b.md")
	d := draftOf(t, rs, "a")
	d.Body = "the owner's own edit"
	if res, err := rs.editFact(ctxT(t), "a", d); err != nil || !res.Saved {
		t.Fatalf("%+v %v", res, err)
	}
	files := git(t, bare, "show", "--name-only", "--format=", "main")
	if strings.Contains(files, "system/b.md") || !strings.Contains(files, "system/a.md") {
		t.Fatalf("the owner commit holds:\n%s", files)
	}
	if staged := git(t, rs.s.Dir, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "system/b.md" {
		t.Fatalf("the other change should stay staged, got %q", staged)
	}
}

// Two machines writing at once both land: the other machine pushes after this
// one synced and before it pushed, so its push loses the race, it syncs and
// computes its change again, and its clone ends clean.
func TestOwnerConcurrentWritersBothLand(t *testing.T) {
	bare, clone := ownerFixture(t)
	one, two := clone("one"), clone("two")
	ctx := ctxT(t)
	da, db := draftOf(t, one, "a"), draftOf(t, two, "b")
	da.Body, db.Body = "a from one", "b from two"
	var otherErr error
	one.beforePush = func() {
		var res *saveResult
		res, otherErr = two.editFact(ctx, "b", db)
		if otherErr == nil && !res.Saved {
			otherErr = errors.New("the other writer did not save")
		}
	}
	res, err := one.editFact(ctx, "a", da)
	if err != nil || otherErr != nil || !res.Saved {
		t.Fatalf("%+v %v; other: %v", res, err, otherErr)
	}
	if res.tries != 2 {
		t.Fatalf("the lost race should run the locked part twice, ran %d", res.tries)
	}
	log := git(t, bare, "log", "--format=%s", "main")
	for _, want := range []string{"owner: edit a (body)", "owner: edit b (body)"} {
		if !strings.Contains(log, want) {
			t.Fatalf("server log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(readRel(t, one, "system/b.md"), "b from two") {
		t.Fatal("the retry did not build on the other machine's commit")
	}
	clean(t, one)
}

// Queue reads rs.facts under rs.mu while syncs and loads replace it (run with -race).
func TestQueueUnderConcurrentReloads(t *testing.T) {
	_, clone := ownerFixture(t)
	app := newApp(clone("one"))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); app.Queue() }()
		go func() {
			defer wg.Done()
			app.rs.mu.Lock()
			app.rs.loadFacts()
			app.rs.mu.Unlock()
		}()
	}
	wg.Wait()
}

// Browser mode serves only a loopback Host, under the content policy, and
// listens only on a loopback address.
func TestBrowserModeIsLoopbackOnly(t *testing.T) {
	h := browserHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for host, want := range map[string]int{"127.0.0.1:7450": 200, "localhost:7450": 200, "[::1]:7450": 200, "evil.example:7450": 403, "evil.example": 403, "127.0.0.1.evil.example": 403} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
		if want == 200 {
			for k, v := range map[string]string{"Content-Security-Policy": "default-src 'self'", "X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
				if !strings.Contains(rec.Header().Get(k), v) {
					t.Errorf("Host %s: %s = %q", host, k, rec.Header().Get(k))
				}
			}
		}
	}
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:0", "example.com:80"} {
		if ln, err := listenLoopback(addr); err == nil {
			ln.Close()
			t.Errorf("listened on %s", addr)
		}
	}
	ln, err := listenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

// A new ruling is the owner's words as a curated user standard in the scope the owner
// chose; a name in use and a derived scope are refused.
func TestOwnerNewRuling(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	res, err := rs.newRuling(ctx, rulingDraft{Name: "no-friday-push", Body: "Never push on Fridays. Weekends are for nobody.", Scope: "projects/x", Origin: "note"})
	if err != nil || !res.Saved {
		t.Fatalf("%+v %v", res, err)
	}
	got := readRel(t, rs, "projects/x/no-friday-push.md")
	for _, want := range []string{"type: user_standard", "kind: user", "harness: human", "description: Never push on Fridays.", "origin: note", "- x", "Never push on Fridays. Weekends are for nobody."} {
		if !strings.Contains(got, want) {
			t.Errorf("ruling lacks %q:\n%s", want, got)
		}
	}
	if subj := strings.TrimSpace(git(t, bare, "log", "-1", "--format=%s")); subj != "owner: new ruling no-friday-push (projects/x)" {
		t.Fatalf("subject %q", subj)
	}
	if _, err := rs.newRuling(ctx, rulingDraft{Name: "a", Body: "x", Scope: "system"}); err == nil {
		t.Error("a name in use was accepted")
	}
	if _, err := rs.newRuling(ctx, rulingDraft{Name: "fresh", Body: "x", Scope: "derived/x"}); err == nil {
		t.Error("a derived scope was accepted")
	}
	if _, err := rs.newRuling(ctx, rulingDraft{Name: "note", Body: "x", Scope: "user"}); err == nil {
		t.Error("an inbox note's name was taken by an unrelated ruling")
	}
	d, _ := rs.factDetail(ctx, "no-friday-push")
	if !d.Ruling || d.Undo.Can {
		t.Fatalf("detail %+v", d.Undo)
	}
	clean(t, rs)
}

// The queue holds the pairs, the stale checks and the dead links in rulings;
// applying a link fix writes the curator's text into the ruling and settles it.
func TestQueueHoldsLinkAndStaleAndAppliesALinkFix(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	app := newApp(rs)
	q, err := app.Queue()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	var link linkItem
	for _, it := range q.Items {
		switch x := it.(type) {
		case reviewItem:
			kinds[x.Kind] = true
		case staleItem:
			kinds[x.Kind] = true
		case linkItem:
			kinds[x.Kind] = true
			link = x
		}
	}
	for _, k := range []string{kindSupersedeAsk, kindStale, kindLink} {
		if !kinds[k] {
			t.Fatalf("queue lacks %s: %v", k, kinds)
		}
	}
	if link.State != "open" || !link.NeedsOwner || link.Link.Target != "gone-fact" || link.Link.Would != "plain text" || !contains(link.Allowed, "apply") {
		t.Fatalf("link item %+v", link)
	}
	if res, err := app.ApplyLinkFix(link.ID); err != nil || !res.Saved {
		t.Fatalf("%+v %v", res, err)
	}
	if body := readRel(t, rs, "projects/x/rule.md"); !strings.Contains(body, "See gone-fact.") || strings.Contains(body, "[[") {
		t.Fatalf("ruling after the fix:\n%s", body)
	}
	for _, it := range rs.linkItems(ctxT(t)) {
		if it.State != "settled" || it.NeedsOwner {
			t.Fatalf("after the fix: %+v", it)
		}
	}
	clean(t, rs)
}

func rowNames(rows []factRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

// The list filters by place, status, rulings, repo and type, and searches with
// the full-text index.
func TestBrowseFiltersAndSearch(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	if _, err := rs.setRetired(ctx, "b", true); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q    browseQuery
		want string
	}{
		{browseQuery{Rulings: true}, "rule"},
		{browseQuery{Status: "retired"}, "b"},
		{browseQuery{Where: "inbox"}, "note"},
		{browseQuery{Repo: "x", Type: "hot_spot"}, "x-hot-spots"},
		{browseQuery{Q: "retri"}, "engine-retries"},
		{browseQuery{Q: "fridays", Where: "all"}, "note"},
		{browseQuery{Q: "fridays"}, ""},
		// Only the inbox note has both words: Curated falls back to either.
		{browseQuery{Q: "fridays engine"}, "engine-retries"},
	}
	for _, c := range cases {
		res, err := rs.browse(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		if got := rowNames(res.Rows); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.q, got, c.want)
		}
	}
}

// Browser mode calls the same methods the window binds, behind the token;
// unexported methods and wrong arguments are refused.
func TestAppHTTPCallsTheBoundMethods(t *testing.T) {
	_, clone := ownerFixture(t)
	app := newApp(clone("one"))
	srv := httptest.NewServer(app.uiHandler("tok"))
	defer srv.Close()
	call := func(method, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/call/"+method, strings.NewReader(body))
		req.Header.Set("X-Tacit-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := call("Browse", "wrong", `[{}]`); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	code, out := call("Browse", "tok", `[{"rulings":true}]`)
	if code != http.StatusOK || !strings.Contains(func() string { b, _ := json.Marshal(out); return string(b) }(), `"name":"rule"`) {
		t.Fatalf("Browse: %d %v", code, out)
	}
	for _, m := range []string{"call", "uiHandler", "Nope"} {
		if code, _ := call(m, "tok", `[]`); code != http.StatusConflict {
			t.Errorf("%s: %d", m, code)
		}
	}
	if code, out := call("Fact", "tok", `[]`); code != http.StatusConflict || !strings.Contains(out["error"].(string), "takes 1 argument") {
		t.Errorf("wrong arity: %d %v", code, out)
	}
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	page.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(page.String(), `content="tok"`) || !strings.Contains(page.String(), "/desk.js") {
		t.Fatal("the page lacks its token or script")
	}
}

// install app writes a desktop entry that runs `<exe> app` and its icon, and
// nothing else.
func TestInstallAppWritesTheDesktopEntry(t *testing.T) {
	home := t.TempDir()
	paths, err := installApp(home, "/opt/tacit bin/tacit", "/s/store")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths %v", paths)
	}
	entry, _ := os.ReadFile(filepath.Join(home, ".local", "share", "applications", "tacit.desktop"))
	for _, want := range []string{"[Desktop Entry]", "Type=Application", `Exec=env TACIT_STORE=/s/store "/opt/tacit bin/tacit" app`, "Icon=tacit", "StartupWMClass=tacit\n"} {
		if !strings.Contains(string(entry), want) {
			t.Errorf("entry lacks %q:\n%s", want, entry)
		}
	}
	icon, _ := os.ReadFile(filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps", "tacit.svg"))
	if !bytes.HasPrefix(icon, []byte("<svg")) {
		t.Fatal("no icon")
	}
}

// The app's script renders a fact with its links, history and actions, the
// edit form with the draft, and a dead link in a ruling, under node.
func TestAppPageRendersAFactAnEditFormAndADeadLink(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	_, clone := ownerFixture(t)
	rs := clone("one")
	d, err := rs.factDetail(ctxT(t), "rule")
	if err != nil {
		t.Fatal(err)
	}
	links := rs.linkItems(ctxT(t))
	rawD, _ := json.Marshal(d)
	rawL, _ := json.Marshal(links[0])
	script := `
const fs = require("fs");
const src = fs.readFileSync("reviewui/api.js", "utf8") + ";" + fs.readFileSync("reviewui/app.js", "utf8") + ";" + fs.readFileSync("reviewui/desk.js", "utf8");
const mk = () => new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => "" : k === "then" ? undefined : mk()), apply: () => mk(), set: () => true });
const els = {};
const el = (id) => els[id] || (els[id] = { id, innerHTML: "", textContent: "", value: "", hidden: false, dataset: {}, style: {}, querySelectorAll: () => [], querySelector: () => null, contains: () => false, addEventListener() {}, removeAttribute() {}, setAttribute() {}, focus() { document.activeElement = this; }, scrollIntoView() {} });
const document = { querySelector: () => ({ content: "tok" }), querySelectorAll: () => [], getElementById: el, addEventListener() {}, documentElement: { dataset: {} }, body: { dataset: {}, style: {} }, activeElement: null };
const D = ` + string(rawD) + `;
const calls = [];
const answer = (m) => m === "Queue" ? { items: [], host: "h" } : m === "Meta" ? { host: "h", scopes: ["system", "projects/x"], repos: ["x"], types: [], kinds: [], owned_types: ["user_standard", "reference"], facts: 5, inbox: 1 }
  : m === "Fact" ? D : m === "Edit" ? { changed: true, name: D.name, note: "this fact changed since you opened it" } : { events: [] };
const fetchStub = async (url, opts) => { const m = String(url).split("/").pop(); calls.push({ m, args: opts && opts.body ? JSON.parse(opts.body) : null }); return { ok: true, json: async () => ({ result: answer(m) }) }; };
const body = src + ";return (async () => {" + process.argv[1] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "els", "calls", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, els, calls).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	checks := `
  await new Promise((r) => setTimeout(r, 30));
  desk.detail = ` + string(rawD) + `;
  factSheet(); const fact = els["fact-sheet"].innerHTML;
  // A draft started on an older version ("old") of a fact the store has changed.
  // The draft is keyed by name, so a remote scope move keeps it reachable; its
  // base is the version it was started on, which differs from the store's only
  // in the body (the other machine's change); the owner changed the description.
  desk.mode = "edit"; desk.drafts.set(D.name, Object.assign({}, { rev: "old", base: { description: D.description, body: "Always ask first.", scope: D.scope, type: D.type }, over: "", description: "kept draft", body: "b", scope: "projects/x", type: "user_standard", applies_to: { paths: [], symbols: [], repos: ["x"], hosts: [], commands: [] } }));
  desk.detail = Object.assign({}, D, { path: "projects/moved/" + D.name + ".md", scope: "projects/moved" });
  editSheet();
  desk.detail = D;
  const edit = els["fact-sheet"].innerHTML;
  const focused = document.activeElement && document.activeElement.id;
  // Save sends the draft's own rev, never the open version's, and the refusal
  // leaves the draft beside the new version.
  await saveEdit();
  const sent = calls.filter((c) => c.m === "Edit").map((c) => c.args[1].rev);
  const after = els["fact-sheet"].innerHTML;
  const keptRev = desk.drafts.get(D.name).rev;
  // Only keep mine re-arms the draft with the new rev, and then Save is offered.
  keepMine();
  const armed = { rev: readEdit().rev, html: els["fact-sheet"].innerHTML };
  const it = ` + string(rawL) + `; it.b = it.a; items = [it]; view = [it]; cur = 0;
  sheet(); const link = els["sheet-inner"].innerHTML;
  return JSON.stringify({ fact, edit, link, sent, after, keptRev, armedRev: armed.rev, armed: armed.html, newRev: D.rev, focused });
`
	cmd := exec.Command(node, "-e", script, checks)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Fact, Edit, Link, After, KeptRev, ArmedRev, Armed, NewRev, Focused string
		Sent                                                               []string
	}
	if err := json.Unmarshal(out[strings.LastIndex(strings.TrimSpace(string(out)), "\n")+1:], &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Your rule", "Always ask first", `class="dead"`, "gone-fact", "Edit", "Retire", "Changes to this fact", "What git shows", "Details"} {
		if !strings.Contains(got.Fact, want) {
			t.Errorf("fact sheet lacks %q:\n%s", want, got.Fact)
		}
	}
	for _, want := range []string{"kept draft", `id="e-scope"`, "In the store now", "Keep my draft", "Take the new version", "Always ask first", "nothing is saved until you choose"} {
		if !strings.Contains(strings.ToLower(got.Edit), strings.ToLower(want)) {
			t.Errorf("edit form over a changed fact lacks %q:\n%s", want, got.Edit)
		}
	}
	if got.Focused != "act-mine" {
		t.Errorf("in a conflict focus belongs on Keep my draft, got %q", got.Focused)
	}
	if !strings.Contains(got.Edit, `<mark class="theirs">Always ask first. See`) || strings.Contains(got.Edit, `<mark class="theirs">fact rule`) {
		t.Errorf("only the other side's changes are marked:\n%s", got.Edit)
	}
	if strings.Contains(got.Edit, `id="act-save"`) {
		t.Error("Save is offered before the owner chose between their draft and the new version")
	}
	if len(got.Sent) != 1 || got.Sent[0] != "old" || got.KeptRev != "old" || !strings.Contains(got.After, "In the store now") {
		t.Errorf("save sent revs %v, kept %q; after:\n%s", got.Sent, got.KeptRev, got.After)
	}
	if got.ArmedRev != got.NewRev || !strings.Contains(got.Armed, `id="act-save"`) || strings.Contains(got.Armed, "In the store now") || !strings.Contains(got.Armed, "The version your draft replaces") || !strings.Contains(got.Armed, "<details open>") {
		t.Errorf("keep mine: rev %q (new %q):\n%s", got.ArmedRev, got.NewRev, got.Armed)
	}
	for _, want := range []string{"Your rule links to a fact that does not exist.", "Apply the fix", "Edit the rule", "gone-fact", "keeps the words and drops the link"} {
		if !strings.Contains(got.Link, want) {
			t.Errorf("link sheet lacks %q:\n%s", want, got.Link)
		}
	}
}
