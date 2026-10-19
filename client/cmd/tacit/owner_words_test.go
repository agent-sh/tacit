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

// giveWords puts words of theirs into a fact of the clone at dir, as the rulings
// audit records them (source.owner_words and a words entry in the quote
// ledger), and pushes.
func giveWords(t *testing.T, dir, rel, words string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	f, err := fact.Parse(rel, mustRead(t, p))
	if err != nil {
		t.Fatal(err)
	}
	f.Body = strings.TrimSpace(f.Body) + "\nHe said \"" + words + "\".\n"
	f.Meta.Source.OwnerWords, f.Meta.Source.OwnerWordsFrom = words, testOwnerSession
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, b, 0o644)
	ql := filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath))
	old, _ := os.ReadFile(ql)
	q := ledger.Quote{Name: f.Meta.Name, Quote: words, From: testOwnerSession, TurnHash: "fixture", At: "2026-10-11", Words: true}
	os.WriteFile(ql, append(old, q.Line()...), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "their words in "+f.Meta.Name)
	git(t, dir, "push", "-q", "origin", "HEAD:main")
}

// renderNotMine draws the fact page for d (a fact the owner said is not theirs) under
// node and checks it shows their recorded words and their Not mine, and that its
// Undo Not mine button calls UndoNotMine.
func renderNotMine(t *testing.T, d *factDetail) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Log("no node: the fact page is not drawn")
		return
	}
	raw, _ := json.Marshal(d)
	script := `
const fs = require("fs");
const src = fs.readFileSync("reviewui/api.js", "utf8") + ";" + fs.readFileSync("reviewui/app.js", "utf8") + ";" + fs.readFileSync("reviewui/desk.js", "utf8");
const els = {};
const el = (id) => els[id] || (els[id] = { id, innerHTML: "", textContent: "", value: "", hidden: false, dataset: {}, style: {}, querySelectorAll: () => [], querySelector: () => null, contains: () => false, addEventListener() {}, removeAttribute() {}, setAttribute() {}, focus() {}, scrollIntoView() {} });
const document = { querySelector: () => ({ content: "tok" }), querySelectorAll: () => [], getElementById: el, addEventListener() {}, documentElement: { dataset: {} }, body: { dataset: {}, style: {} }, activeElement: null };
const D = ` + string(raw) + `;
const calls = [];
const answer = (m) => m === "Queue" ? { items: [], host: "h" } : m === "Meta" ? { host: "h", scopes: [], repos: [], types: [], kinds: [], owned_types: [], facts: 1, inbox: 0 } : m === "Fact" ? D : m === "UndoNotMine" ? { saved: true } : { events: [], rows: [] };
const fetchStub = async (url, opts) => { const m = String(url).split("/").pop(); calls.push({ m, args: opts && opts.body ? JSON.parse(opts.body) : null }); return { ok: true, json: async () => ({ result: answer(m) }) }; };
const body = src + ";return (async () => {" + process.argv[1] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "els", "calls", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, els, calls).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	checks := `
  await new Promise((r) => setTimeout(r, 30));
  desk.detail = D; desk.mode = "show";
  factSheet(); const html = els["fact-sheet"].innerHTML;
  await els["act-undonotmine"].onclick();
  return JSON.stringify({ html, undo: calls.filter((c) => c.m === "UndoNotMine").map((c) => c.args[0]) });
`
	out, err := exec.Command(node, "-e", script, checks).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		HTML string
		Undo []string
	}
	if err := json.Unmarshal(out[strings.LastIndex(strings.TrimSpace(string(out)), "\n")+1:], &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"You said not yours", "You said this is not yours", "keep the hooks quiet", `id="act-undonotmine"`, "Undo Not mine"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("fact page lacks %q:\n%s", want, got.HTML)
		}
	}
	if strings.Contains(got.HTML, `id="act-notmine"`) {
		t.Error("Not mine offered again on a fact the owner already said is not theirs")
	}
	if len(got.Undo) != 1 || got.Undo[0] != d.Name {
		t.Errorf("Undo Not mine called %v", got.Undo)
	}
}

// pairItem is the finding on facts x and y, in either order.
func pairItem(t *testing.T, rs *reviewServer, x, y string) reviewItem {
	t.Helper()
	rs.loadFacts()
	for _, it := range rs.items() {
		if (it.A.Name == x && it.B.Name == y) || (it.A.Name == y && it.B.Name == x) {
			return it
		}
	}
	t.Fatalf("no finding on %s and %s", x, y)
	return reviewItem{}
}

// The verifier reads words of theirs in a fact as owner_words, with the rule
// for them. A mark that would retire that fact comes back to them as a
// question; a mark that retires nothing lands, so their words alone ask nothing,
// and a fact with no words of theirs is retired as before.
func TestVerifierMayNotRetireAgainstHisWords(t *testing.T) {
	needRedaction(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	giveWords(t, rs.s.Dir, "system/a.md", "keep the hooks quiet")
	rs.loadFacts()
	if v := rs.view("a"); v.OwnerWords != "keep the hooks quiet" || v.Ruling || v.guarded() {
		t.Fatalf("view of a: %+v", v)
	}
	pushReview(t, rs, "- CONTRADICTION?: [[a]] (system) vs [[d]] (system)\n- CONTRADICTION?: [[b]] (system) vs [[d]] (system)\n")
	ab, ac, cd := pairItem(t, rs, "a", "b"), pairItem(t, rs, "c", "a"), pairItem(t, rs, "c", "d")
	ad, bd := pairItem(t, rs, "a", "d"), pairItem(t, rs, "b", "d")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var prompt string
	r := &verifyRun{model: "m", per: 10, max: 10, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir()}
	_, _, err := r.run(ctx, rs.s, func(ctx context.Context, model, p, cwd string, budget float64) ([]verifyVerdict, string, float64, error) {
		prompt = p
		return []verifyVerdict{
			{ID: ab.ID, Mark: "keep", Keep: "b", Evidence: "README.md:3 says b"},
			{ID: ac.ID, Mark: "both", Evidence: "different subjects"},
			{ID: cd.ID, Mark: "keep", Keep: "d", Evidence: "README.md:9 says d"},
			{ID: ad.ID, Mark: "query", Evidence: "README.md:9 says d, their words say a"},
			{ID: bd.ID, Mark: "query", Evidence: "the docs disagree"},
		}, "", 0.01, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"owner_words":"keep the hooks quiet"`) || !strings.Contains(prompt, "never retire it against their words") || !strings.Contains(prompt, "ask only on a real conflict") {
		t.Fatalf("the verifier was not told their words:\n%s", prompt)
	}
	if after := pairItem(t, rs, "a", "b"); after.Decision != nil || after.Ask == nil || after.Ask.Proposed != "b" || !strings.Contains(after.Ask.Question, "keep the hooks quiet") || strings.Contains(read(t, rs, "a"), "superseded_by") {
		t.Fatalf("an agent retired a against their words: decision %+v ask %+v", after.Decision, after.Ask)
	}
	if after := pairItem(t, rs, "c", "a"); after.Ask != nil || after.Decision == nil || after.Decision.Action != "both" {
		t.Fatalf("their words alone made a question: decision %+v ask %+v", after.Decision, after.Ask)
	}
	if after := pairItem(t, rs, "c", "d"); after.Ask != nil || after.Decision == nil || !strings.Contains(read(t, rs, "c"), "superseded_by: d") {
		t.Fatalf("a fact with no words of theirs was not retired: decision %+v ask %+v", after.Decision, after.Ask)
	}
	// A conflict with their words is theirs: "query" becomes a question and a
	// keeps no conflicts_with; a conflict between agents' facts lands.
	if after := pairItem(t, rs, "a", "d"); after.Decision != nil || after.Ask == nil || !strings.Contains(after.Ask.Question, "keep the hooks quiet") || strings.Contains(read(t, rs, "a"), "conflicts_with") {
		t.Fatalf("a conflict with their words landed: decision %+v ask %+v", after.Decision, after.Ask)
	}
	if after := pairItem(t, rs, "b", "d"); after.Ask != nil || after.Decision == nil || after.Decision.Action != "query" {
		t.Fatalf("a conflict between agents' facts did not land: decision %+v ask %+v", after.Decision, after.Ask)
	}
}

// A stale check's "retire" on a fact with words of theirs is a question too.
func TestStaleVerifierMayNotRetireAgainstHisWords(t *testing.T) {
	needRedaction(t)
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	giveWords(t, fx.rs.s.Dir, "projects/x/engine-retries.md", "keep the retries at three")
	fx.rs.loadFacts()
	var engID string
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" {
			engID = it.ID
		}
	}
	if engID == "" {
		t.Fatal("no stale finding on engine-retries")
	}
	var prompt string
	call := func(_ context.Context, _, p, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		prompt = p
		return []verifyVerdict{{ID: engID, Mark: "retire", Evidence: "abc: MAX_RETRIES is five now"}}, "raw", 0.1, nil
	}
	r := verifyRun{model: "m", max: 50, per: 12, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir(), tl: fx.b}
	if _, _, err := r.run(ctx, fx.rs.s, call); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"owner_words":"keep the retries at three"`) || !strings.Contains(prompt, "never retire it against their words") {
		t.Fatalf("the stale verifier was not told their words:\n%s", prompt)
	}
	fx.rs.loadFacts()
	if f := fx.rs.facts["engine-retries"]; f.Meta.Retired != "" {
		t.Fatal("retired against their words")
	}
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" && (it.Decision != nil || it.Ask == nil || !strings.Contains(it.Ask.Question, "keep the retries at three")) {
			t.Fatalf("decision %+v ask %+v", it.Decision, it.Ask)
		}
	}
}
