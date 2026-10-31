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
	"github.com/agent-sh/tacit/client/internal/ledger"
)

// The guards and readers around a name two files hold: each test pins one
// thing a review of the change found.

// appendDecisionLine adds one record to the clone's decision log and pushes it.
func appendDecisionLine(t *testing.T, r *rig, d decision) {
	t.Helper()
	p := filepath.Join(r.clone, filepath.FromSlash(ledger.DecisionsPath))
	old, _ := os.ReadFile(p)
	b, _ := json.Marshal(d)
	if err := os.WriteFile(p, append(append(old, b...), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.clone, "add", "-A")
	gitIn(t, r.clone, "commit", "-q", "-m", "a decision")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
}

// A keep that retires the file walked last leaves the name reading as the
// live file in the review server and in tacit app, so a finding about the
// name is not read against the retired copy.
func TestAKeepLeavesTheNameReadingAsTheLiveFile(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "Both say port 7781."}}); code != 0 {
		t.Fatalf("review apply exited %d", code)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	if f := rs.facts["lantern-proxy-port"]; f == nil || f.Path != it.A.Path {
		t.Fatalf("the review server reads the name as %v, want %s", f, it.A.Path)
	}
	sn, err := rs.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f := sn.lookup("lantern-proxy-port"); f == nil || f.Path != it.A.Path {
		t.Fatalf("tacit app reads the name as %v, want %s", f, it.A.Path)
	}
}

// The owner's note sent with an answer names the two files, not the name
// twice or an empty link.
func TestTheOwnersNoteOnANameFindingNamesTheFiles(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	rs := &reviewServer{s: r.s}
	if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "right", Comment: "the user copy says which scheduler"}}); err != nil || errs[0] != nil {
		t.Fatalf("the owner's answer: %v %v", err, errs)
	}
	notes, _ := filepath.Glob(filepath.Join(r.clone, "inbox", "human@*", "*.md"))
	if len(notes) != 1 {
		t.Fatalf("owner notes %v", notes)
	}
	b, _ := os.ReadFile(notes[0])
	if !strings.Contains(string(b), "keep the file "+it.B.Path+", retire "+it.A.Path) || strings.Contains(string(b), "[[]]") {
		t.Fatalf("the note's answer:\n%s", b)
	}
}

// With a third file of the name nothing can say which file replaced a
// copy: no mark is taken, and tacit questions says it is the owner's.
func TestANameThreeFilesHoldTakesNoMark(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	writeFact(t, r.clone, fact.Meta{Name: "lantern-proxy-port", Description: "The lantern proxy answers on port 7781 for the scheduler", Type: "machine_fact", Scope: "machines/host-b"}, "A third copy.\n")
	r.commitIndex(r.clone, "a third file")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	if len(it.Allowed) != 0 || !it.Crowded || len(it.Also) != 1 || it.Also[0] == it.A.Path || it.Also[0] == it.B.Path {
		t.Fatalf("allowed %v, crowded %v, also %v", it.Allowed, it.Crowded, it.Also)
	}
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "x"}}); code == 0 {
		t.Fatal("an agent marked a name three files hold")
	}
	rs := &reviewServer{s: r.s}
	if _, errs, _ := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "left"}}); errs[0] == nil {
		t.Fatal("the page marked a name three files hold")
	}
	rs.loadFacts()
	for _, q := range openQuestions(rs, nil) {
		if q.ID == it.ID && !strings.Contains(q.Owners, "more than two files") {
			t.Errorf("question %+v", q)
		}
	}
}

// The owner's keep of a name endorses the file they kept, never the copy it
// retired into the name.
func TestTheRulingsAuditEndorsesNoCopyRetiredIntoItsName(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	rs := &reviewServer{s: r.s}
	if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "right"}}); err != nil || errs[0] != nil {
		t.Fatalf("the owner's keep: %v %v", err, errs)
	}
	facts, a, err := auditorFor(context.Background(), r.clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Meta.Name == "lantern-proxy-port" && f.Path == it.A.Path {
			if v := a.decide(f); v.Outcome == ruledEndorsed || v.Endorsed != "" {
				t.Fatalf("the retired copy is endorsed: %+v", v)
			}
			return
		}
	}
	t.Fatal("the copy is gone")
}

// The agent guards: an owner's word on the name elsewhere, their note on the
// finding, their words in the file; and no closure takes the finding.
func TestTheAgentGuardsOnANameTwoFilesHold(t *testing.T) {
	keep := func(it reviewItem, path string) []map[string]string {
		return []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": path, "evidence": "The two say one thing."}}
	}
	t.Run("the owner kept the name in another answer", func(t *testing.T) {
		r := nameFixture(t)
		stubScrump(t)
		appendDecisionLine(t, r, decision{ID: "aaaaaaaaaaaa", Action: "right", A: "another-fact", B: "lantern-proxy-port", At: "2026-10-20T00:00:00Z"})
		if code := runDedupe([]string{"--names"}); code != 0 {
			t.Fatalf("dedupe --names exited %d", code)
		}
		it := nameItemFor(t, r, "lantern-proxy-port")
		if code := applyVerdicts(t, keep(it, it.B.Path)); code == 0 {
			t.Fatal("an agent marked a name the owner kept")
		}
		if got := supersededOnServer(t, r, it.A.Path); got != "" {
			t.Fatalf("retired: %q", got)
		}
	})
	t.Run("the owner left a note on the finding", func(t *testing.T) {
		r := nameFixture(t)
		stubScrump(t)
		if code := runDedupe([]string{"--names"}); code != 0 {
			t.Fatalf("dedupe --names exited %d", code)
		}
		it := nameItemFor(t, r, "lantern-proxy-port")
		appendDecisionLine(t, r, decision{ID: it.ID, Action: "note", A: it.A.Name, B: it.B.Name, At: "2026-10-20T00:00:00Z", Comment: "check the unit file first"})
		if code := applyVerdicts(t, keep(it, it.B.Path)); code != 0 {
			t.Fatalf("review apply exited %d", code)
		}
		if got := supersededOnServer(t, r, it.A.Path); got != "" {
			t.Fatalf("retired against their note: %q", got)
		}
		if it = nameItemFor(t, r, "lantern-proxy-port"); it.Ask == nil {
			t.Fatal("no question for the owner")
		}
	})
	t.Run("the file to retire holds their words", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		words := fact.Source{Kind: "observed", OwnerWords: "rerun it with the deploy tool", OwnerWordsFrom: testOwnerSession}
		writeFact(t, r.clone, fact.Meta{Name: "deploy-rerun", Description: "Rerun a failed deploy with the deploy tool", Type: "user_standard", Scope: "machines/host-a"}, "x\n")
		writeFact(t, r.clone, fact.Meta{Name: "deploy-rerun", Description: "They said to rerun a failed deploy with the deploy tool", Type: "user_standard", Scope: "user", Source: words}, "They said to rerun it with the deploy tool.\n")
		r.commitIndex(r.clone, "words")
		gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("TACIT_STORE", r.clone)
		t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
		stubScrump(t)
		if code := runDedupe([]string{"--names"}); code != 0 {
			t.Fatalf("dedupe --names exited %d", code)
		}
		it := nameItemFor(t, r, "deploy-rerun")
		if code := applyVerdicts(t, keep(it, it.A.Path)); code != 0 {
			t.Fatalf("review apply exited %d", code)
		}
		if got := supersededOnServer(t, r, it.B.Path); got != "" {
			t.Fatalf("their words were retired: %q", got)
		}
		if it = nameItemFor(t, r, "deploy-rerun"); it.Ask == nil || !strings.Contains(it.Ask.Question, "rerun it with the deploy tool") {
			t.Fatalf("ask %+v", it.Ask)
		}
	})
	t.Run("no closure takes it", func(t *testing.T) {
		r := nameFixture(t)
		stubScrump(t)
		if code := runDedupe([]string{"--names"}); code != 0 {
			t.Fatalf("dedupe --names exited %d", code)
		}
		it := nameItemFor(t, r, "lantern-proxy-port")
		head := r.serverHead()
		var code int
		msg := captureStderr(t, func() {
			code = runQuestionsClose([]string{it.ID, "--harness", "claude", "--answer", "keep:" + it.B.Path, "--proof", "file", "--evidence", "the two files", "--reason", "they say one thing"})
		})
		if code == 0 || !strings.Contains(msg, "tacit review apply") {
			t.Fatalf("tacit questions close on a name finding: exit %d, %q", code, msg)
		}
		if r.serverHead() != head {
			t.Fatal("the refused closure pushed")
		}
	})
}

// A pair whose file the same run retires as a copy is not raised, a
// generated derived fact is never one of a pair, and --names points no chain.
func TestDedupeLeavesOutWhatItRetiresAndPointsNoChainWithNames(t *testing.T) {
	const ws = "machines/host-a"
	withExtras := func(t *testing.T) *rig {
		r := nameFixture(t)
		// The machine file of lantern-proxy-port restates lantern-port-proxy,
		// which the full run keeps (by name, all else equal).
		writeFact(t, r.clone, fact.Meta{Name: "lantern-port-proxy", Description: "The lantern proxy listens on port 7781 on host-a", Type: "machine_fact", Scope: ws}, "Seen in its unit file.\n")
		// A chain: chain-a was replaced by chain-b, which chain-c replaced.
		writeFact(t, r.clone, fact.Meta{Name: "chain-c", Description: "The quokka queue drains at midnight", Type: "machine_fact", Scope: ws}, "x\n")
		writeFact(t, r.clone, fact.Meta{Name: "chain-b", Description: "The quokka queue drains at eleven", Type: "machine_fact", Scope: ws, SupersededBy: "chain-c"}, "x\n")
		writeFact(t, r.clone, fact.Meta{Name: "chain-a", Description: "The quokka queue drains at ten", Type: "machine_fact", Scope: ws, SupersededBy: "chain-b"}, "x\n")
		r.commitIndex(r.clone, "a copy and a chain")
		gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
		return r
	}
	t.Run("names alone", func(t *testing.T) {
		r := withExtras(t)
		if code := runDedupe([]string{"--names"}); code != 0 {
			t.Fatalf("dedupe --names exited %d", code)
		}
		if got := supersededOnServer(t, r, ws+"/chain-a.md"); got != "chain-b" {
			t.Fatalf("dedupe --names pointed the chain: %q", got)
		}
		if got := supersededOnServer(t, r, ws+"/lantern-proxy-port.md"); got != "" {
			t.Fatalf("dedupe --names retired a copy: %q", got)
		}
	})
	t.Run("the full run", func(t *testing.T) {
		r := withExtras(t)
		if code := runDedupe(nil); code != 0 {
			t.Fatalf("dedupe exited %d", code)
		}
		if got := supersededOnServer(t, r, ws+"/lantern-proxy-port.md"); got != "lantern-port-proxy" {
			t.Fatalf("the copy: superseded_by %q", got)
		}
		page := r.onServer("review/" + time.Now().Format("2006-01-02") + "-names.md")
		if strings.Contains(page, "[[lantern-proxy-port]]") || !strings.Contains(page, "[[deploy-by-tool]]") {
			t.Fatalf("names raised:\n%s", page)
		}
		if got := supersededOnServer(t, r, ws+"/chain-a.md"); got != "chain-c" {
			t.Fatalf("the full run left the chain: %q", got)
		}
	})
	derived := &fact.Fact{Path: "derived/widget/deploy-by-tool.md", Meta: fact.Meta{Name: "deploy-by-tool", Scope: "derived/widget"}}
	live := &fact.Fact{Path: "user/deploy-by-tool.md", Meta: fact.Meta{Name: "deploy-by-tool", Scope: "user"}}
	if ps := heldTwice([]*fact.Fact{derived, live}); len(ps) != 0 {
		t.Fatalf("a derived fact made a pair: %v", ps)
	}
}

// A copy retired into its own name never stands for the name again: not
// when the owner then retires the file kept, and tacit resolve still takes
// the name.
func TestACopyRetiredIntoItsNameNeverStandsForIt(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "Both say port 7781."}}); code != 0 {
		t.Fatalf("review apply exited %d", code)
	}
	rs := &reviewServer{s: r.s}
	if res, err := rs.setRetired(context.Background(), it.A.Path, true); err != nil || !res.Saved {
		t.Fatalf("the owner's retire of the kept file: %+v %v", res, err)
	}
	if p, ok := findFact(r.clone, "lantern-proxy-port"); !ok || !strings.HasSuffix(filepath.ToSlash(p), it.A.Path) {
		t.Errorf("the name reads as %s, want the kept file %s", p, it.A.Path)
	}
	rs.loadFacts()
	if f := rs.facts["lantern-proxy-port"]; f == nil || f.Path != it.A.Path {
		t.Errorf("the review server reads the name as %v", f)
	}
	if res, err := rs.undoLast(context.Background(), it.A.Path); err != nil || !res.Saved {
		t.Errorf("the owner's undo of their retire: %+v %v", res, err)
	}
	if code := runResolve([]string{"conflict", "lantern-proxy-port", "deploy-by-tool"}); code == 2 {
		t.Error("tacit resolve refuses the name after a keep")
	}
}

// tacit app's actions on a file whose name another file holds name that
// file: Retire sends the path the card opened, not the name.
func TestTheAppActsOnTheFileItShowsForAName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	r := nameFixture(t)
	writeFact(t, r.clone, fact.Meta{Name: "solo-fact", Description: "One file holds this name", Type: "machine_fact", Scope: "user"}, "x\n")
	r.commitIndex(r.clone, "a unique name")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	rs := &reviewServer{s: r.s}
	const path = "user/lantern-proxy-port.md"
	d, err := rs.factDetail(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if solo, err := rs.factDetail(context.Background(), "solo-fact"); err != nil || solo.Ref != "solo-fact" || d.Ref != path {
		t.Fatalf("refs %q and %q (%v)", d.Ref, solo.Ref, err)
	}
	raw, _ := json.Marshal(d)
	script := `
const fs = require("fs");
const src = fs.readFileSync("reviewui/api.js", "utf8") + ";" + fs.readFileSync("reviewui/app.js", "utf8") + ";" + fs.readFileSync("reviewui/desk.js", "utf8");
const els = {};
const el = (id) => els[id] || (els[id] = { id, innerHTML: "", textContent: "", value: "", hidden: false, dataset: {}, style: {}, querySelectorAll: () => [], querySelector: () => null, contains: () => false, addEventListener() {}, removeAttribute() {}, setAttribute() {}, focus() { document.activeElement = this; }, scrollIntoView() {} });
const document = { querySelector: () => ({ content: "tok" }), querySelectorAll: () => [], getElementById: el, addEventListener() {}, documentElement: { dataset: {} }, body: { dataset: {}, style: {} }, activeElement: null };
const D = ` + string(raw) + `;
const calls = [];
const answer = (m) => m === "Queue" ? { items: [], host: "h" } : m === "Meta" ? { host: "h", scopes: ["user"], repos: [], types: [], kinds: [], owned_types: [], facts: 3, inbox: 0 }
  : m === "Fact" ? D : m === "Retire" ? { saved: true } : { events: [], rows: [] };
const fetchStub = async (url, opts) => { const m = String(url).split("/").pop(); calls.push({ m, args: opts && opts.body ? JSON.parse(opts.body) : null }); return { ok: true, json: async () => ({ result: answer(m) }) }; };
const body = src + ";return (async () => {" + process.argv[1] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "els", "calls", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, els, calls).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	checks := `
  await new Promise((r) => setTimeout(r, 30));
  desk.detail = D; desk.mode = "show";
  await retire(true);
  return JSON.stringify(calls.filter((c) => c.m === "Retire").map((c) => c.args[0]));
`
	out, err := exec.Command(node, "-e", script, checks).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `["`+path+`"]` {
		t.Fatalf("Retire was sent %s, want the file %s", got, path)
	}
}
