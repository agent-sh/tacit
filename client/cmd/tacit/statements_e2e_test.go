package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/statement"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The owner's words in four sessions, end to end on a fresh store: the
// dreamer keeps each as a statement (verbatim, dated, a line of context), the
// split writes them as statements once code finds the words in the owner's
// own turns, and the statements step reads each once. "the real name is
// pikadon" is one statement that a check agrees with (the checkout's
// directory is pikadon, ginza its origin's name): a decision. The release
// branch, said in two separate sessions, is a decision by repeat. "the real
// name of widget is gizmo" is refuted by the check (gizmo is a symlink to
// widget): it stays a statement, flagged, an optional review item, and a
// session in widget reads it in the owner's words with the flag, never as a
// RULE line; the decisions are rules now.
func TestStatementsEndToEnd(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"owner/ginza": "owner/ginza", "owner/widget": "owner/widget"}, map[string]string{"owner": "User"})
	for k, v := range map[string]string{"USERPROFILE": w.home, "TACIT_HERMES_DB": "off", "TACIT_SETTINGS": "", "TACIT_DREAM_MAX_RUNTIME": "",
		"TACIT_DREAM_RUNNER": "claude", "TACIT_CURATE_RUNNER": "claude", "TACIT_JUDGE_RUNNER": "claude"} {
		t.Setenv(k, v)
	}
	pk := w.git("pikadon", "https://github.com/owner/ginza.git")
	widget := w.git("widget", "https://github.com/owner/widget.git")
	if err := os.Symlink("widget", filepath.Join(w.p, "gizmo")); err != nil {
		t.Fatal(err)
	}
	// Each said on its own day, weeks before the run: a statement is dated by
	// the owner's turn, never by the day the dreamer or the curator ran.
	said := map[string]struct{ id, cwd, words, context, day string }{
		"pk":   {"11111111-0000-4000-8000-000000000001", pk, "the real name is pikadon, ginza is a name for now until we move it to public repo", "what the lab repository is called", "2026-09-14"},
		"rel1": {"11111111-0000-4000-8000-000000000002", widget, "we ship widget from the release branch, never from main", "where widget releases are cut", "2026-09-20"},
		"rel2": {"11111111-0000-4000-8000-000000000003", widget, "widget releases go out from the release branch", "cutting the widget release", "2026-10-02"},
		"gz":   {"11111111-0000-4000-8000-000000000004", widget, "the real name of widget is gizmo", "what the widget repository is called", "2026-10-05"},
	}
	offsets := map[string]int64{}
	for _, v := range said {
		offsets[ownerSession(t, w.home, v.cwd, v.id, v.words, v.day+"T12:00:00Z")] = 0
	}
	if err := dream.SaveState(filepath.Join(platform.StateDir(w.home), "dreamer.json"), &dream.State{Offsets: offsets}); err != nil {
		t.Fatal(err)
	}
	prevDream := dreamRun
	t.Cleanup(func() { dreamRun = prevDream })
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		for _, v := range said {
			if strings.Contains(prompt, v.words) {
				b, _ := json.Marshal([]map[string]any{{"description": "The owner said: " + v.words, "type": "design_decision", "kind": "statement",
					"quote": v.words, "context": v.context, "relevance": map[string]any{"keep": true, "category": "decision", "reason": "a choice later work follows"}, "applies_to": map[string]any{}}})
				return string(b), .01, 10, nil
			}
		}
		return `[]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream exit %d", code)
	}
	noteOf := map[string]*fact.Fact{}
	for _, f := range factsList(t, w.s.Dir) {
		if f.InInbox() && f.Meta.Source.Session != "" {
			noteOf[f.Meta.Source.Session] = f
		}
	}
	if len(noteOf) != 4 || !strings.Contains(noteOf[said["pk"].id].Body, "Said once; context: what the lab repository is called") {
		t.Fatalf("dreamed notes %v", noteOf)
	}

	// The split: one statement entry per note.
	var entries []map[string]any
	for k, v := range said {
		scope := "projects/widget"
		if k == "pk" {
			scope = "projects/pikadon"
		}
		e := entry(noteOf[v.id].Meta.Name, k+"-statement", v.context, "design_decision", scope)
		e["kind"], e["owner_quote"], e["context"], e["body"] = "statement", v.words, v.context, "the owner's reading of "+k
		e["relevance"] = map[string]any{"keep": true, "category": "decision", "reason": "a choice later work follows"}
		entries = append(entries, e)
	}
	b, _ := json.Marshal(entries)
	r, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	t.Setenv("FIXTURE_SPLIT", string(r))
	fake := filepath.Join(w.home, "fake-cli")
	os.WriteFile(fake, []byte("#!/bin/sh\nin=$(cat)\ncase \"$in\" in\n*\"OWNER RULINGS already extracted\"*) printf '%s\\n' '{\"result\":\"[]\",\"total_cost_usd\":0.01}' ;;\n*) printf '%s\\n' \"$FIXTURE_SPLIT\" ;;\nesac\n"), 0o700)
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	prevJudge := judgeRun
	t.Cleanup(func() { judgeRun = prevJudge })
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		out := make([]string, strings.Count(prompt, "\nEXISTING ("))
		for i := range out {
			out[i] = "no"
		}
		b, _ := json.Marshal(out)
		return string(b), .01, nil
	}
	prevA, prevM, prevO, prevU, prevS := curateAssessStep, curateMigrateStep, curateOverviewStep, curateUpkeep, curateStaleStep
	t.Cleanup(func() {
		curateAssessStep, curateMigrateStep, curateOverviewStep, curateUpkeep, curateStaleStep = prevA, prevM, prevO, prevU, prevS
	})
	curateAssessStep = func(context.Context, *store.Store, string, bool, *curate.Report) int { return 0 }
	curateMigrateStep = func(context.Context, *store.Store, bool) int { return 0 }
	curateOverviewStep = func(context.Context, *store.Store, string, bool, *curate.Report) int { return 0 }
	curateUpkeep = func(context.Context, *store.Store, string, bool, *curate.Report) {}
	curateStaleStep = func(context.Context, *store.Store, bool) int { return 0 }

	// The statements step's model: what each says, the same point, the
	// claims its words make.
	prevS2 := statementsRun
	t.Cleanup(func() { statementsRun = prevS2 })
	var stmtPrompts []string
	blockRx := regexp.MustCompile(`(?ms)^### statement=(\S+) .*?\nyou said once \([^)]*\): "([^"]*)"(.*?)(?:\n\n|\z)`)
	earlierRx := regexp.MustCompile(`(?m)^earlier (\S+) `)
	statementsRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		stmtPrompts = append(stmtPrompts, prompt)
		var out []statement.Judgement
		for _, m := range blockRx.FindAllStringSubmatch(prompt, -1) {
			j := statement.Judgement{Name: m[1], Says: statement.Choice}
			switch m[2] {
			case said["pk"].words:
				j.Claims = []check.Claim{{Kind: "canonical", Name: "pikadon", Other: "ginza"}}
			case said["gz"].words:
				j.Claims = []check.Claim{{Kind: "canonical", Name: "gizmo", Other: "widget"}}
			case said["rel2"].words, said["rel1"].words:
				for _, e := range earlierRx.FindAllStringSubmatch(m[3], -1) {
					if strings.HasPrefix(e[1], "rel") {
						j.Same = append(j.Same, statement.Relation{Name: e[1], Relation: statement.Same})
					}
				}
			}
			out = append(out, j)
		}
		b, _ := json.Marshal(out)
		return string(b), 0.02, nil
	}
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate exit %d", code)
	}

	by := factsByName(t, w.s.Dir)
	for _, k := range []string{"pk", "rel1", "rel2", "gz"} {
		f := by[k+"-statement"]
		if f == nil || f.Meta.Type != fact.StatementType || f.Meta.Source.Kind != "observed" || f.Meta.Source.OwnerWords != said[k].words {
			t.Fatalf("%s: %+v", k, f)
		}
		if day, words, ok := fact.StatementLine(f.Body); !ok || words != said[k].words || day != said[k].day || f.Meta.Created != said[k].day || !strings.Contains(f.Body, fact.GlossLabel+"the owner's reading of "+k) {
			t.Fatalf("%s: dated %q (created %s), said on %s; body %q", k, day, f.Meta.Created, said[k].day, f.Body)
		}
	}
	// The one a check agrees with, and the repeated one, are decisions.
	dec := func(k string) *fact.Fact {
		n := by[k+"-statement"].Meta.SupersededBy
		if n == "" {
			t.Fatalf("%s was not promoted", k)
		}
		return by[n]
	}
	pkd := dec("pk")
	if pkd.Meta.Type != "design_decision" || pkd.Meta.Source.Kind != "user" || pkd.Meta.Source.OwnerQuote != said["pk"].words || !strings.Contains(pkd.Body, "a check agrees") || !fact.IsRuling(pkd) {
		t.Fatalf("pikadon's decision %+v\n%s", pkd.Meta, pkd.Body)
	}
	rd := dec("rel2")
	if by["rel1-statement"].Meta.SupersededBy != rd.Meta.Name || !strings.Contains(rd.Body, "said in 2 separate sessions") || !fact.IsRuling(rd) ||
		!strings.HasPrefix(strings.TrimLeft(rd.Body, "\n"), "You decided ("+said["rel2"].day+"): ") || !strings.Contains(rd.Body, "[[rel1-statement]] ("+said["rel1"].day+", ") {
		t.Fatalf("the release decision %+v\n%s", rd.Meta, rd.Body)
	}
	// The refuted one stays a statement, flagged, an optional review item.
	gz := by["gz-statement"]
	if gz.Meta.SupersededBy != "" || len(fact.CheckFlags(gz.Body)) != 1 {
		t.Fatalf("gizmo's statement %+v\n%s", gz.Meta, gz.Body)
	}
	review, _ := os.ReadFile(filepath.Join(w.s.Dir, "review", time.Now().Format("2006-01-02")+"-statements.md"))
	if !strings.Contains(string(review), "- check? [[gz-statement]] (projects/widget) outcome=misread") || !strings.Contains(string(review), "nothing waits on your answer") {
		t.Fatalf("review:\n%s", review)
	}
	log := statement.ReadLog(mustRead(t, filepath.Join(w.s.Dir, ledger.StatementsPath)))
	kinds := map[string]int{}
	for _, e := range log {
		kinds[e.Kind]++
	}
	if kinds[statement.LogJudged] != 4 || kinds[statement.LogPromoted] != 2 || kinds[statement.LogFlagged] != 1 {
		t.Fatalf("log %v", kinds)
	}

	// Delivery in widget: the statement in their words with the flag, the
	// decision as a rule.
	out := hookOut(t, startPayload(widget, "r1"), "session-start")
	if !strings.Contains(out, `- gz-statement: you said once (`) || !strings.Contains(out, `"the real name of widget is gizmo"; context: what the widget repository is called (a check disagrees, maybe misread)`) {
		t.Fatalf("the statement's line:\n%s", out)
	}
	if strings.Contains(out, "RULE gz-statement") || !strings.Contains(out, "RULE "+rd.Meta.Name) {
		t.Fatalf("rules:\n%s", out)
	}

	// A flagged statement is not left for the owner: the second run reads it
	// again, alone, with the check that disagrees; the reading stands (the
	// same claim), so nothing is flagged twice.
	n := len(stmtPrompts)
	reviewText := func() string {
		b, _ := os.ReadFile(filepath.Join(w.s.Dir, "review", time.Now().Format("2006-01-02")+"-statements.md"))
		return string(b)
	}
	before := reviewText()
	sr2, errs := statementsStep(context.Background(), w.s, statementsOpts{model: "opus"}, &curate.Report{})
	if errs != 0 || len(stmtPrompts) != n+1 || sr2.Reread != 1 || sr2.Flagged != 0 {
		t.Fatalf("a second run: %d errors, %d prompts, %+v", errs, len(stmtPrompts)-n, sr2)
	}
	p := stmtPrompts[n]
	if !strings.Contains(p, "### statement=gz-statement ") || strings.Count(p, "### statement=") != 1 || !strings.Contains(p, "an earlier reading's claim was checked by code and the check disagrees: ") {
		t.Fatalf("the re-read prompt:\n%s", p)
	}
	if reviewText() != before {
		t.Fatal("the re-read flagged the statement again")
	}
	// The third run, the same week, asks and writes nothing.
	head := gitIn(t, w.s.Dir, "rev-parse", "HEAD")
	n = len(stmtPrompts)
	if _, errs := statementsStep(context.Background(), w.s, statementsOpts{model: "opus"}, &curate.Report{}); errs != 0 || len(stmtPrompts) != n || gitIn(t, w.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatalf("a third run: %d errors, %d prompts", errs, len(stmtPrompts)-n)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// ownerSession writes one Claude Code session the owner typed words in, at
// at (RFC 3339), in cwd, whose agent edits a file there.
func ownerSession(t *testing.T, home, cwd, id, words, at string) string {
	t.Helper()
	line := func(v map[string]any) string {
		v["timestamp"] = at
		// A cwd outside the temporary directory: the owner never starts a
		// session there (ownerwords.AgentCwd); the edit's path places it.
		v["sessionId"], v["userType"], v["entrypoint"], v["isSidechain"], v["cwd"] = id, "external", "cli", false, "/home/u/projects"
		b, _ := json.Marshal(v)
		return string(b)
	}
	f := filepath.Join(home, ".claude", "projects", "p", id+".jsonl")
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte(strings.Join([]string{
		line(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "typed", "message": map[string]any{"role": "user", "content": words}}),
		line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Noted."},
			map[string]any{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": filepath.ToSlash(filepath.Join(cwd, "main.go"))}}}}}),
		strings.Repeat(" ", 5000),
	}, "\n")+"\n"), 0o600)
	return f
}

// The Mac's statements stay in its overlay: a statement of the owner's
// words on a read-only clone with the local layer is written, read and
// logged in the overlay only; the clone and the server are untouched.
func TestLocalStatementsStayInTheOverlay(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	q := "never deploy the walrus service on fridays without a rollback plan"
	n := dream.ToFact(dream.Extracted{Description: "The owner said not to deploy walrus on Fridays without a rollback plan", Type: "design_decision", Kind: "statement", Quote: dream.Quote(q), Context: "a walrus deploy"}, "", host, "s9")
	if _, err := persistLocalDreamWindow(r.s, []*fact.Fact{n}, nil); err != nil {
		t.Fatal(err)
	}
	r.split(map[string]any{"origin": n.Meta.Name, "name": "walrus-friday", "description": "a walrus deploy", "type": "design_decision", "scope": "user", "kind": "statement",
		"owner_quote": q, "context": "a walrus deploy", "body": "no Friday deploys of walrus without a rollback", "applies_to": map[string]any{}})
	r.judge()
	prev := statementsRun
	t.Cleanup(func() { statementsRun = prev })
	asked := 0
	statementsRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		asked++
		if !strings.Contains(prompt, "### statement=walrus-friday") {
			t.Errorf("prompt:\n%s", prompt)
		}
		return `[{"name":"walrus-friday","says":"instruction"}]`, 0.01, nil
	}
	if code := runCurate(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fb, err := os.ReadFile(filepath.Join(r.local, "curated", "user", "walrus-friday.md"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fact.Parse("local/curated/user/walrus-friday.md", fb)
	if f.Meta.Type != fact.StatementType || f.Meta.Source.OwnerWords != q || asked != 1 {
		t.Fatalf("the local statement %+v (asked %d)", f.Meta, asked)
	}
	if lb, err := os.ReadFile(filepath.Join(r.local, "curated", filepath.FromSlash(ledger.StatementsPath))); err != nil || !strings.Contains(string(lb), `"name":"walrus-friday"`) {
		t.Fatalf("the overlay's statements log: %v %s", err, lb)
	}
	r.untouched()
}
