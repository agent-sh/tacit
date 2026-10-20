package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/stale"
	"github.com/agent-sh/tacit/client/internal/store"
)

const staleEngine = "// engine\nconst MAX_RETRIES: u32 = 3;\nconst BACKOFF_MS: u32 = 200;\nfn run() {\n    retry(MAX_RETRIES);\n    sleep(BACKOFF_MS);\n}\nconst KEEP_ALIVE_SECS: u32 = 30;\n"

func gitDay(t *testing.T, dir, day string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	when := day + "T12:00:00+00:00"
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.test", "GIT_AUTHOR_DATE="+when,
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.test", "GIT_COMMITTER_DATE="+when)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitDay(t *testing.T, repo, day, msg string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	gitDay(t, repo, day, "add", "-A")
	gitDay(t, repo, day, "commit", "-q", "-m", msg)
}

func staleFact(name, kind, typ, body string) string {
	evidence := ""
	if kind == "user" {
		// A ruling: the owner's words, found in one of their sessions.
		evidence = "  owner_quote: \"" + strings.ReplaceAll(body, "`", "") + "\"\n  owner_quote_from: " + testOwnerSession + "\n"
	}
	return "---\nname: " + name + "\ndescription: \"fact " + name + "\"\ntype: " + typ + "\nscope: projects/x\napplies_to:\n  repos: [\"x\"]\nsource:\n  harness: curator\n  host: h\n  kind: " + kind + "\n" + evidence + "created: 2026-10-01\n---\n\n" + body + "\n"
}

// staleFixture is a store clone with three curated facts about repo x and a
// projects directory holding x: engine-retries (observed) and owner-rule (the
// owner's ruling) name MAX_RETRIES, which a later commit renames;
// steady-keepalive names KEEP_ALIVE_SECS, which nothing touches. The engine
// file is also edited by two unrelated commits.
type staleFx struct {
	rs       *reviewServer
	projects string
	repo     string
	bare     string
	b        *gitev.Builder
	root     string
}

func newStaleFixture(t *testing.T) *staleFx {
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "projects", "x"), 0o755)
	write := func(name, kind, typ, body string) {
		os.WriteFile(filepath.Join(seed, "projects", "x", name+".md"), []byte(staleFact(name, kind, typ, body)), 0o644)
	}
	write("engine-retries", "observed", "design_decision", "The engine in `src/engine.rs` retries `MAX_RETRIES` times, three, with a `BACKOFF_MS` pause.")
	write("owner-rule", "user", "design_decision", "Never raise `MAX_RETRIES` without asking.")
	write("steady-keepalive", "observed", "machine_fact", "The keep alive is `KEEP_ALIVE_SECS` seconds in `src/engine.rs`.")
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	dir := filepath.Join(root, "one")
	git(t, root, "clone", "-q", bare, dir)
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")

	projects := filepath.Join(root, "projects")
	repo := filepath.Join(projects, "x")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	commitDay(t, repo, "2026-09-20", "engine: first version", map[string]string{"src/engine.rs": staleEngine})
	// Two later commits edit the hot file without touching what the facts name.
	commitDay(t, repo, "2026-10-03", "engine: add a helper", map[string]string{"src/engine.rs": staleEngine + "fn helper() {}\n"})
	commitDay(t, repo, "2026-10-04", "engine: add another helper", map[string]string{"src/engine.rs": staleEngine + "fn helper() {}\nfn helper2() {}\n"})
	rs := &reviewServer{s: &store.Store{Dir: dir}}
	rs.loadFacts()
	b := &gitev.Builder{G: gitev.New(projects), StoreDir: dir, Redact: noRedact}
	rs.tl = b
	return &staleFx{rs: rs, projects: projects, repo: repo, bare: bare, b: b, root: root}
}

// change makes the commit that overtakes engine-retries and owner-rule.
func (fx *staleFx) change(t *testing.T, day string) {
	t.Helper()
	fx.rename(t, day, "MAX_RETRIES", "RETRY_LIMIT", "engine: name the retry limit")
}

// changeBackoff is a later, second thing overtaking engine-retries.
func (fx *staleFx) changeBackoff(t *testing.T, day string) {
	t.Helper()
	fx.rename(t, day, "BACKOFF_MS", "PAUSE_MS", "engine: name the pause")
}

func (fx *staleFx) rename(t *testing.T, day, from, to, msg string) {
	t.Helper()
	body, _ := os.ReadFile(filepath.Join(fx.repo, "src", "engine.rs"))
	commitDay(t, fx.repo, day, msg, map[string]string{"src/engine.rs": strings.ReplaceAll(string(body), from, to)})
}

func (fx *staleFx) opts() staleOptions {
	return staleOptions{Builder: fx.b, Workers: 2}
}

func flaggedNames(fs []*stale.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

func headOf(t *testing.T, dir string) string {
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// The check flags the facts whose subject a later commit changed, and only
// those: the fact whose identifier nothing touched is not flagged though two
// commits edited the file it names. It writes lines in review/<date>.md,
// pushes them, and does not touch a fact.
func TestStaleStepFlagsWhatGitOvertookAndNothingElse(t *testing.T) {
	fx := newStaleFixture(t)
	s := fx.rs.s
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	before := map[string]string{}
	for _, n := range []string{"engine-retries", "owner-rule", "steady-keepalive"} {
		b, _ := os.ReadFile(filepath.Join(s.Dir, "projects", "x", n+".md"))
		before[n] = string(b)
	}

	rep, err := staleStep(ctx, s, fx.opts())
	if err != nil {
		t.Fatal(err)
	}
	if got := flaggedNames(rep.Findings); got != "engine-retries,owner-rule" {
		t.Fatalf("flagged %q (skips %v), want engine-retries and owner-rule", got, rep.Skips)
	}
	if rep.Skips[stale.SkipQuiet] != 1 {
		t.Errorf("steady-keepalive should be quiet: %v", rep.Skips)
	}
	review, err := os.ReadFile(filepath.Join(s.Dir, "review", time.Now().Format("2006-01-02")+".md"))
	if err != nil || !strings.Contains(string(review), "- stale? [[engine-retries]] (projects/x) @") || strings.Contains(string(review), "steady-keepalive") {
		t.Fatalf("review file: %v\n%s", err, review)
	}
	// Pushed: the server's main holds the commit, and no fact changed.
	if remote := strings.TrimSpace(git(t, fx.bare, "rev-parse", "main")); remote != headOf(t, s.Dir) {
		t.Fatalf("server main %s, clone %s: not pushed", remote, headOf(t, s.Dir))
	}
	if log := git(t, s.Dir, "log", "-1", "--format=%s"); !strings.Contains(log, "2 stale? finding(s)") {
		t.Errorf("commit: %s", log)
	}
	for n, was := range before {
		if b, _ := os.ReadFile(filepath.Join(s.Dir, "projects", "x", n+".md")); string(b) != was {
			t.Errorf("the check changed fact %s", n)
		}
	}

	// A second run finds the same two open and adds nothing: no new commit.
	head := headOf(t, s.Dir)
	rep, err = staleStep(ctx, s, fx.opts())
	if err != nil || len(rep.Findings) != 0 || rep.Open != 2 || headOf(t, s.Dir) != head {
		t.Fatalf("second run: findings %s open %d err %v", flaggedNames(rep.Findings), rep.Open, err)
	}
}

// `tacit curate` runs the staleness step after the derived-facts step, also
// when no inbox note is pending, and its findings reach the server.
func TestCurateRunsTheStaleStepWhenNothingIsPending(t *testing.T) {
	requireScrump(t)
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", fx.rs.s.Dir)
	t.Setenv("TACIT_PROJECTS", fx.projects)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_DERIVE_CONFIG", filepath.Join(home, "derive.yaml"))
	if code := runCurate([]string{"--max-batches", "0"}); code != 0 {
		t.Fatalf("curate exited %d", code)
	}
	remote := git(t, fx.bare, "show", "main:review/"+time.Now().Format("2006-01-02")+".md")
	if !strings.Contains(remote, "- stale? [[engine-retries]] (projects/x) @") || !strings.Contains(remote, "- stale? [[owner-rule]]") || strings.Contains(remote, "steady-keepalive") {
		t.Fatalf("the server's review file:\n%s", remote)
	}
	// Derived facts come first in the same run: its commit is older than the stale one.
	log := git(t, fx.bare, "log", "--format=%s", "-3", "main")
	if !strings.Contains(log, "stale? finding(s)") || !strings.Contains(log, "derived facts") {
		t.Fatalf("log:\n%s", log)
	}
	// A dry run of curate does not run the scan at all (it can take most of an
	// hour) and writes nothing, the cursor included.
	head := headOf(t, fx.rs.s.Dir)
	statePath := filepath.Join(home, ".local", "state", "tacit", "stale.json")
	before := mustRead(t, statePath)
	start := time.Now()
	if code := runCurate([]string{"--dry-run", "--max-batches", "0"}); code != 0 || headOf(t, fx.rs.s.Dir) != head {
		t.Fatalf("curate --dry-run exited %d or moved the store", code)
	}
	if string(mustRead(t, statePath)) != string(before) {
		t.Fatal("curate --dry-run moved the stale cursor")
	}
	if code := curateStale(context.Background(), fx.rs.s, true); code != 0 {
		t.Fatalf("curateStale in a dry run: %d", code)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("curate --dry-run took %v", d)
	}
}

// A run that runs out of time reaches the facts it did not reach first the
// next time, and a dry run moves no cursor.
func TestStaleCursorContinuesWhereTheLastRunStopped(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	statePath := filepath.Join(t.TempDir(), "stale.json")
	o := fx.opts()
	o.StatePath = statePath

	// No time at all: nothing is reached, nothing is remembered.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rep := computeStale(gone, fx.rs.s, o)
	if rep.Skips[stale.SkipUnreached] != 3 || len(rep.checked) != 0 {
		t.Fatalf("a spent budget: skips %v checked %v", rep.Skips, rep.checked)
	}
	rep.remember(o, true)
	if st := loadStaleState(statePath); len(st.Checked) != 0 {
		t.Fatalf("unreached facts were remembered: %v", st.Checked)
	}

	// A dry run orders by the cursor but writes none.
	dry := o
	dry.Dry = true
	rep = computeStale(context.Background(), fx.rs.s, dry)
	rep.remember(dry, true)
	if st := loadStaleState(statePath); len(st.Checked) != 0 {
		t.Fatalf("a dry run moved the cursor: %v", st.Checked)
	}

	// A real run remembers the three and puts them behind a newer fact.
	if _, err := staleStep(context.Background(), fx.rs.s, o); err != nil {
		t.Fatal(err)
	}
	st := loadStaleState(statePath)
	if len(st.Checked) != 3 {
		t.Fatalf("checked %v", st.Checked)
	}
	ins := []stale.Input{{Fact: fx.rs.facts["steady-keepalive"]}, {Fact: fx.rs.facts["engine-retries"]}, {Fact: fx.rs.facts["owner-rule"]}}
	st.Checked["engine-retries"] = "2026-10-09T22:00:00Z"
	st.Checked["owner-rule"] = "2026-10-08T22:00:00Z"
	delete(st.Checked, "steady-keepalive")
	byCursor(ins, st)
	if got := ins[0].Fact.Meta.Name + "," + ins[1].Fact.Meta.Name + "," + ins[2].Fact.Meta.Name; got != "steady-keepalive,owner-rule,engine-retries" {
		t.Fatalf("order %s: never checked first, then the longest ago", got)
	}
}

// A dry run reads and reports and changes nothing, not even the sync.
func TestStaleDryRunWritesNothing(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	o := fx.opts()
	o.Dry = true
	head := headOf(t, fx.rs.s.Dir)
	rep, err := staleStep(context.Background(), fx.rs.s, o)
	if err != nil || len(rep.Findings) != 2 {
		t.Fatalf("dry run: %v %s", err, flaggedNames(rep.Findings))
	}
	if headOf(t, fx.rs.s.Dir) != head || git(t, fx.rs.s.Dir, "status", "--porcelain", "--untracked-files=all") != "" {
		t.Fatal("a dry run changed the store")
	}
}

// The strongest findings go first and --max holds the rest back for the next
// run, which finds them again.
func TestStaleMaxHoldsBackTheRest(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	o := fx.opts()
	o.Max = 1
	ctx := context.Background()
	rep, err := staleStep(ctx, fx.rs.s, o)
	if err != nil || len(rep.Findings) != 1 || rep.Held != 1 {
		t.Fatalf("max 1: %v findings %s held %d", err, flaggedNames(rep.Findings), rep.Held)
	}
	rep, err = staleStep(ctx, fx.rs.s, o)
	if err != nil || len(rep.Findings) != 1 || rep.Held != 0 || rep.Open != 1 {
		t.Fatalf("next run: %v findings %s held %d open %d", err, flaggedNames(rep.Findings), rep.Held, rep.Open)
	}
}

// What the page shows: both findings are single-fact items with their marks,
// they ride /api/items next to the pairs, and the timeline of one is built
// from that one fact (no side on its dates).
func TestPageListsStaleFindingsWithTheirTimeline(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx := context.Background()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	items := fx.rs.staleItems()
	if len(items) != 2 {
		t.Fatalf("%d stale items", len(items))
	}
	var eng staleItem
	for _, it := range items {
		if it.A.Name == "engine-retries" {
			eng = it
		}
	}
	if !eng.Single || eng.Kind != kindStale || eng.Flag.N != 1 || !eng.NeedsOwner || strings.Join(eng.Allowed, ",") != "current,retire" || len(eng.Flag.Detail) < 2 {
		t.Fatalf("item %+v", eng)
	}
	b, err := json.Marshal(map[string]any{"items": fx.rs.allItems()})
	if err != nil || !strings.Contains(string(b), `"single":true`) || !strings.Contains(string(b), `"kind":"stale"`) {
		t.Fatalf("api/items: %v\n%s", err, b)
	}
	tl, err := fx.rs.timelineFor(ctx, eng.ID)
	if err != nil {
		t.Fatal(err)
	}
	var commit, head bool
	for _, e := range tl.Events {
		commit = commit || (e.Kind == gitev.KindCommit && e.Text == "engine: name the retry limit" && e.Repo == "x")
		head = head || e.Kind == gitev.KindHead
		if e.Kind == gitev.KindFact && e.Side != "" {
			t.Errorf("a one-fact timeline put a side on %+v", e)
		}
	}
	if !commit || !head {
		t.Errorf("timeline events %+v", tl.Events)
	}
}

// The page's own script lists and sets a single-fact finding: the name, why
// git flagged it, the two marks and the timeline section.
func TestPageRendersAStaleFinding(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	if _, err := staleStep(context.Background(), fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	var raw []byte
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" {
			raw, _ = json.Marshal(it)
		}
	}
	script := `
const src = require("fs").readFileSync(process.argv[1], "utf8");
const mk = () => new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => "" : k === "then" ? undefined : mk()), apply: () => mk(), set: () => true });
const inner = { innerHTML: "", querySelectorAll: () => [] };
const proofs = { innerHTML: "", contains: () => false, querySelectorAll: () => [], querySelector: () => null };
const tl = { innerHTML: "", removeAttribute() {}, querySelector: () => ({ textContent: "" }) };
const document = { querySelector: () => ({ content: "tok" }), getElementById: (id) => (id === "sheet-inner" ? inner : id === "proofs" ? proofs : id === "timeline" ? tl : mk()), addEventListener() {}, documentElement: { dataset: {} } };
const fetchStub = async (url) => (String(url).startsWith("/api/items") ? { ok: true, json: async () => ({ items: [], host: "h" }) } : { ok: true, json: async () => ({ repos: ["x"], terms: [], events: [] }) });
const body = src + ";return (async () => {" + process.argv[2] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "inner", "proofs", "tl", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, inner, proofs, tl).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	checks := `
  await new Promise((r) => setTimeout(r, 50));
  const it = ` + string(raw) + `;
  it.b = it.a; items = [it]; view = [it]; cur = 0;
  sheet(); await new Promise((r) => setTimeout(r, 50));
  return JSON.stringify({ sheet: inner.innerHTML, tl: tl.innerHTML });
`
	out, err := exec.Command(node, "-e", script, "reviewui/app.js", checks).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct{ Sheet, Tl string }
	if err := json.Unmarshal(out[strings.LastIndex(strings.TrimSpace(string(out)), "\n")+1:], &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The question in plain words, the fact with its name under Details, what
	// git deleted as a sentence, the two answers and the git section.
	for _, want := range []string{"Git says something this fact names was deleted. Is it still true?", "engine-retries", "was deleted in x on 6 Oct", "“engine: name the retry limit”", "Still true", "Outdated, retire it", `id="timeline"`, "What git shows"} {
		if !strings.Contains(got.Sheet, want) {
			t.Errorf("the sheet lacks %q:\n%s", want, got.Sheet)
		}
	}
	if strings.Contains(got.Sheet, `id="g-b"`) {
		t.Error("a one-fact finding drew a second galley")
	}
}

// The verifier gets the timeline with the finding, never a ruling, and its
// verdicts land as marks: keep re-dates the fact, retire retires it, fix and
// owner stay with the owner.
func TestVerifierGetsTheTimelineAndItsVerdictsLand(t *testing.T) {
	stubScrump(t)
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	var engID string
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" {
			engID = it.ID
		}
	}
	var prompt string
	call := func(_ context.Context, _, p, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		prompt = p
		return []verifyVerdict{{ID: engID, Mark: "retire", Evidence: "abc: MAX_RETRIES is five now, the fact says three"}}, "raw", 0.1, nil
	}
	r := verifyRun{model: "m", max: 50, per: 12, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir(), tl: fx.b}
	n, _, err := r.run(ctx, fx.rs.s, call)
	if err != nil || n != 1 {
		t.Fatalf("run: settled %d err %v", n, err)
	}
	for _, want := range []string{"engine-retries", `"kind":"stale"`, `"changes":1`, "engine: name the retry limit", "curated into the store"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the verifier input lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "owner-rule") {
		t.Error("an owner ruling was sent to the verifier")
	}
	fx.rs.loadFacts()
	f := fx.rs.facts["engine-retries"]
	if f.Meta.Retired == "" || f.Meta.Updated == "" {
		t.Fatalf("retire did not retire: %+v", f.Meta)
	}
	if got := string(mustRead(t, filepath.Join(fx.rs.s.Dir, "projects", "x", "engine-retries.md"))); !strings.Contains(got, "retired: "+f.Meta.Retired) {
		t.Fatalf("file:\n%s", got)
	}
	if idx := index.Generate(factsOf(t, fx.rs.s.Dir)); strings.Contains(idx, "engine-retries") || !strings.Contains(idx, "steady-keepalive") {
		t.Errorf("a retired fact is still indexed:\n%s", idx)
	}
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" && (it.Decision == nil || it.Decision.By != "agent" || it.Decision.Evidence == "" || it.NeedsOwner || it.State != "retired") {
			t.Errorf("decision %+v state %s", it.Decision, it.State)
		}
	}
	// The retired fact is no longer a candidate; the ruling stays open and unsent.
	prompt = ""
	if n, _, err := r.run(ctx, fx.rs.s, call); err != nil || n != 0 || prompt != "" {
		t.Fatalf("second run sent %d findings (%q): %v", n, prompt, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func factsOf(t *testing.T, dir string) []*fact.Fact {
	t.Helper()
	fs, _, err := fact.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// An agent never retires an owner ruling: its verdict becomes a question and
// the file is not written. The owner can retire their own ruling from the page,
// and reopen puts the file back byte for byte.
func TestRulingNeverRetiredByAnAgentBecomesAnAsk(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	rulingPath := filepath.Join(fx.rs.s.Dir, "projects", "x", "owner-rule.md")
	orig := mustRead(t, rulingPath)
	var id, sha string
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "owner-rule" {
			id, sha = it.ID, it.Flag.Sha
		}
	}
	_, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, A: "owner-rule", B: sha, Mark: "retire", By: "agent", Evidence: "git says so"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("retire: %v %v", err, errs)
	}
	var got staleItem
	for _, it := range fx.rs.staleItems() {
		if it.ID == id {
			got = it
		}
	}
	if got.Decision != nil || got.Ask == nil || !got.NeedsOwner || !strings.Contains(got.Ask.Question, "Does your ruling owner-rule still hold?") || got.A.Retired != "" {
		t.Fatalf("retire on a ruling: decision %+v ask %+v", got.Decision, got.Ask)
	}
	if string(mustRead(t, rulingPath)) != string(orig) {
		t.Fatal("an agent's retire wrote into the ruling")
	}
	// One question is enough: another agent mark is refused.
	if _, errs, _ := fx.rs.applyOps(ctx, []op{{ID: id, A: "owner-rule", Mark: "retire", By: "agent", Evidence: "again"}}); errs[0] == nil {
		t.Fatal("an agent marked a finding that carries a question")
	}
	// The owner's own mark is theirs: retire applies, and reopen restores exactly.
	if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, Mark: "retire"}}); err != nil || errs[0] != nil {
		t.Fatalf("owner retire: %v %v", err, errs)
	}
	fx.rs.loadFacts()
	if fx.rs.facts["owner-rule"].Meta.Retired == "" {
		t.Fatal("the owner could not retire their own ruling")
	}
	if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, Mark: "reopen"}}); err != nil || errs[0] != nil {
		t.Fatalf("reopen: %v %v", err, errs)
	}
	if string(mustRead(t, rulingPath)) != string(orig) {
		t.Fatalf("reopen did not restore the ruling:\n%s", mustRead(t, rulingPath))
	}
}

// Nothing an agent says about a ruling is written: a fix or a "still true"
// is a question for the owner too, since only the owner can clear a flag on their word.
func TestAgentFixOrKeepOnARulingIsAlsoAnAsk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, mark := range []string{"fix", "current"} {
		fx := newStaleFixture(t)
		fx.change(t, "2026-10-06")
		if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
			t.Fatal(err)
		}
		fx.rs.loadFacts()
		var id string
		for _, it := range fx.rs.staleItems() {
			if it.A.Name == "owner-rule" {
				id = it.ID
			}
		}
		path := filepath.Join(fx.rs.s.Dir, "projects", "x", "owner-rule.md")
		orig := mustRead(t, path)
		if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, A: "owner-rule", Mark: mark, By: "agent", Evidence: "git says so"}}); err != nil || errs[0] != nil {
			t.Fatalf("%s: %v %v", mark, err, errs)
		}
		for _, it := range fx.rs.staleItems() {
			if it.ID == id && (it.Decision != nil || it.Ask == nil || !it.NeedsOwner) {
				t.Fatalf("%s on a ruling: decision %+v ask %+v", mark, it.Decision, it.Ask)
			}
		}
		if string(mustRead(t, path)) != string(orig) {
			t.Fatalf("%s wrote into the ruling", mark)
		}
	}
}

// An agent's keep, fix and owner verdicts, and a reopen of a retirement.
func TestAgentStaleVerdictsKeepFixOwnerAndReopen(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	var id, sha string
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" {
			id, sha = it.ID, it.Flag.Sha
		}
	}
	path := filepath.Join(fx.rs.s.Dir, "projects", "x", "engine-retries.md")
	orig := mustRead(t, path)
	apply := func(o op) error {
		_, errs, err := fx.rs.applyOps(ctx, []op{o})
		if err != nil {
			return err
		}
		return errs[0]
	}
	// No evidence, no mark; a verdict naming another fact is refused.
	if err := apply(op{ID: id, A: "engine-retries", B: sha, Mark: "current", By: "agent"}); err == nil {
		t.Error("an agent mark without evidence was accepted")
	}
	if err := apply(op{ID: id, A: "owner-rule", B: sha, Mark: "current", By: "agent", Evidence: "x"}); err == nil {
		t.Error("a verdict for another fact was accepted")
	}
	// fix leaves the finding with the owner and the fact as it was.
	if err := apply(op{ID: id, A: "engine-retries", B: sha, Mark: "fix", By: "agent", Evidence: "abc changed it. Fix needed: five, not three"}); err != nil {
		t.Fatal(err)
	}
	var it staleItem
	for _, x := range fx.rs.staleItems() {
		if x.ID == id {
			it = x
		}
	}
	if it.Decision == nil || it.Decision.Action != "fix" || !it.NeedsOwner || strings.Join(it.Allowed, ",") != "current,retire" || string(mustRead(t, path)) != string(orig) {
		t.Fatalf("fix: %+v", it)
	}
	// The owner overrules it with retire; reopen undoes the retirement and
	// leaves the file as it was.
	if err := apply(op{ID: id, Mark: "retire"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, path)), "retired: ") {
		t.Fatal("retire did not write")
	}
	if err := apply(op{ID: id, Mark: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, path)) != string(orig) {
		t.Fatalf("reopen:\n%s", mustRead(t, path))
	}
	// A reopened finding is open again, and still true re-dates the fact: the
	// commit is spent, the next run flags nothing until a newer commit lands.
	if err := apply(op{ID: id, Mark: "current"}); err != nil {
		t.Fatal(err)
	}
	rep, err := staleStep(ctx, fx.rs.s, fx.opts())
	if err != nil || flaggedNames(rep.Findings) != "" {
		t.Fatalf("after still true: %q %v", flaggedNames(rep.Findings), err)
	}
}

// An owner's "still true" re-dates the fact; the next commit that changes
// what it names flags it again as a new finding.
func TestStillTrueIsSpentUntilANewCommitLands(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	for _, it := range fx.rs.staleItems() {
		if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: it.ID, Mark: "current"}}); err != nil || errs[0] != nil {
			t.Fatalf("%v %v", err, errs)
		}
	}
	rep, err := staleStep(ctx, fx.rs.s, fx.opts())
	if err != nil || len(rep.Findings) != 0 {
		t.Fatalf("flagged after still true: %q %v", flaggedNames(rep.Findings), err)
	}
	// A commit dated after the marks (the marks carry the real clock).
	fx.changeBackoff(t, time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
	rep, err = staleStep(ctx, fx.rs.s, fx.opts())
	if err != nil || flaggedNames(rep.Findings) != "engine-retries" {
		t.Fatalf("after a new commit: %q %v (skips %v)", flaggedNames(rep.Findings), err, rep.Skips)
	}
	fx.rs.loadFacts()
	open := 0
	for _, it := range fx.rs.staleItems() {
		if it.NeedsOwner {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("%d open stale findings after the new commit, want 1 (engine-retries; the ruling was settled)", open)
	}
}

func TestStaleWorkersFromTheEnvironment(t *testing.T) {
	for v, want := range map[string]int{"": 4, "1": 1, " 2 ": 2, "0": 4, "-3": 4, "many": 4} {
		t.Setenv("TACIT_STALE_WORKERS", v)
		if got := staleWorkers(); got != want {
			t.Errorf("TACIT_STALE_WORKERS=%q: %d, want %d", v, got, want)
		}
	}
}
