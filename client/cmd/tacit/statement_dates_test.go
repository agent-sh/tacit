package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/statement"
)

// dayOf is the local day of an RFC 3339 time, as the steps date statements.
func dayOf(t *testing.T, at string) string {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	return tm.Local().Format("2006-01-02")
}

// datesRig is rulingsRig after the audit, in the statements format, with the
// owner's turns in HOME dated, a statement an older binary dated by its run
// (2026-10-25, its words line with no said day), and a gate that keeps every
// one-off.
func datesRig(t *testing.T) (*rig, map[string]string) {
	t.Helper()
	r, c := rulingsRig(t)
	if _, err := rulingsApply(ctxMinute(t), r.s, c, false, false); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "2026-10-25T00:00:00Z"}.Encode(), 0o644)
	at := map[string]string{
		"release": "2026-09-03T12:00:00Z",
		"rename":  "2026-09-01T12:00:00Z",
		"again":   "2026-09-02T12:00:00Z",
		"runner":  "2026-08-30T12:00:00Z",
		"notes":   "2026-08-25T12:00:00Z",
	}
	runner := "the nightly build stays on the old runner until the migration ends"
	old := &fact.Fact{Path: "user/old-statement.md", Meta: fact.Meta{Name: "old-statement", Description: "where the nightly build runs", Type: fact.StatementType, Scope: "user",
		Source:  fact.Source{Harness: "curator", Host: "host-a", Kind: "observed", OwnerWords: runner, OwnerWordsFrom: "claude:theirs"},
		Created: "2026-10-25"}, Body: fact.StatementBody("2026-10-25", runner, "where the nightly build runs", "")}
	writeFactFile(t, r.clone, old)
	// Superseded since: its day is still the record of when they said it.
	notes := "the release notes stay short and plain for every tag we cut"
	sup := &fact.Fact{Path: "user/old-superseded.md", Meta: fact.Meta{Name: "old-superseded", Description: "how release notes read", Type: fact.StatementType, Scope: "user",
		Source:  fact.Source{Harness: "curator", Host: "host-a", Kind: "observed", OwnerWords: notes, OwnerWordsFrom: "claude:theirs"},
		Created: "2026-10-25", SupersededBy: "old-statement"}, Body: fact.StatementBody("2026-10-25", notes, "how release notes read", "")}
	writeFactFile(t, r.clone, sup)
	qp := filepath.Join(r.clone, filepath.FromSlash(ledger.QuotesPath))
	ql, _ := os.ReadFile(qp)
	ql = append(withNewline(ql), ledger.Quote{Name: "old-statement", Quote: runner, From: "claude:theirs", TurnHash: ledger.TurnHash(ownerwords.Normalize(runner)), At: "2026-10-25", Words: true}.Line()...)
	ql = append(ql, ledger.Quote{Name: "old-superseded", Quote: notes, From: "claude:theirs", TurnHash: ledger.TurnHash(ownerwords.Normalize(notes)), At: "2026-10-25", Words: true}.Line()...)
	os.WriteFile(qp, ql, 0o644)
	r.commitAs("format, and a statement an older binary dated by its run")

	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u")
	os.MkdirAll(proj, 0o755)
	line := func(at, text string) string {
		return `{"type":"user","entrypoint":"cli","timestamp":"` + at + `","message":{"content":"` + text + `"}}`
	}
	os.WriteFile(filepath.Join(proj, "theirs.jsonl"), []byte(strings.Join([]string{
		line(at["rename"], "please never rename public endpoints without asking me, ok?"),
		line(at["release"], "dont tag a release on fridays without cheking the changelog"),
		line(at["runner"], runner),
		line(at["notes"], notes),
		line("2026-07-01T12:00:00Z", "Dont reset the shared cache on friday, those are mine"),
	}, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "again.jsonl"), []byte(line(at["again"], "as before: never rename public endpoints without asking me")+"\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("TACIT_STORE", r.clone)
	prev := oneOffsRun
	t.Cleanup(func() { oneOffsRun = prev })
	oneOffsRun = func(_ context.Context, prompt string) (string, float64, string, error) {
		var out []string
		for n := 1; strings.Contains(prompt, fmt.Sprintf(`<item n="%d">`, n)); n++ {
			out = append(out, fmt.Sprintf(`{"n": %d, "keep": true, "category": "instruction", "reason": "how work is done"}`, n))
		}
		return "[" + strings.Join(out, ",") + "]", 0.01, "opus", nil
	}
	return r, at
}

// A one-off ruling becomes the owner's statement dated by the turn they said
// it in (their transcript's time), never by the day the ruling was curated
// or the step ran; its words line records that day. A statement an older
// binary dated by its run is corrected to its turn's day, its words line
// gains the day, and the log says so. A dry run reports both and writes
// nothing; a second run writes nothing (idempotent); lint is clean.
func TestOneOffsDateStatementsByTheOwnersTurn(t *testing.T) {
	r, at := datesRig(t)
	head := r.serverHead()
	dry := filepath.Join(t.TempDir(), "dry.json")
	if code := runOneOffs([]string{"--dry-run", "--digests", "", "--json", dry}); code != 0 {
		t.Fatalf("dry run exit %d", code)
	}
	var rep oneOffsReport
	if json.Unmarshal(readBytes(t, dry), &rep) != nil || r.serverHead() != head {
		t.Fatal("no report, or the dry run wrote")
	}
	to := map[string]string{}
	for _, d := range rep.Redates {
		if d.By == "turn" {
			to[d.Name] = d.To
		}
	}
	if rep.Redated != 2 || rep.RedatedLedger != 2 || len(rep.Redates) != 2 || to["old-statement"] != dayOf(t, at["runner"]) || to["old-superseded"] != dayOf(t, at["notes"]) {
		t.Fatalf("dry run's redates: %d, %d, %+v", rep.Redated, rep.RedatedLedger, rep.Redates)
	}
	for _, it := range rep.Items {
		if it.Name == "typo-fixed" && it.SaidOn != dayOf(t, at["release"]) {
			t.Fatalf("typo-fixed said on %q", it.SaidOn)
		}
	}
	if code := runOneOffs([]string{"--digests", "", "--verdicts", dry}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	by := factsByName(t, r.clone)
	quotes := storeQuotes(r.clone)
	for name, want := range map[string]string{"typo-fixed-said-once": dayOf(t, at["release"]), "old-statement": dayOf(t, at["runner"]), "old-superseded": dayOf(t, at["notes"])} {
		st := by[name]
		if st == nil || st.Meta.Type != fact.StatementType || st.Meta.Source.OwnerWords == "" {
			t.Fatalf("%s: %+v", name, st)
		}
		day, _, ok := fact.StatementLine(st.Body)
		q := quotes[ledger.Quote{Name: name, Quote: st.Meta.Source.OwnerWords, From: st.Meta.Source.OwnerWordsFrom, Words: true}.Key()]
		if !ok || day != want || st.Meta.Created != want || q.Said != want {
			t.Fatalf("%s: dated %q, created %q, its words line said %q; want %s", name, day, st.Meta.Created, q.Said, want)
		}
	}
	log := statement.ReadLog(readBytes(t, filepath.Join(r.clone, filepath.FromSlash(ledger.StatementsPath))))
	redated := 0
	for _, e := range log {
		if e.Kind == statement.LogRedated && e.Name == "old-statement" && strings.Contains(e.Why, "not 2026-10-25") {
			redated++
		}
	}
	if redated != 1 {
		t.Fatalf("statements log: %+v", log)
	}
	if findings, err := lint.Run(r.clone); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}
	head = r.serverHead()
	if code := runOneOffs([]string{"--digests", ""}); code != 0 || r.serverHead() != head {
		t.Fatalf("a second run wrote: exit %d", code)
	}
}

// The rulings audit never decides a statement: its body quotes the owner's
// words, which as a candidate proved it a ruling (kind user, owner_quote in
// place of owner_words), a change lint L20 refuses, so the audit wrote
// nothing (the rebuild's "lint rejects the change"). After one-offs the audit
// runs clean and leaves every statement as it was.
func TestRulingsAuditLeavesStatements(t *testing.T) {
	r, _ := datesRig(t)
	if code := runOneOffs([]string{"--digests", ""}); code != 0 {
		t.Fatalf("one-offs exit %d", code)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	before := map[string]fact.Source{}
	for n, f := range factsByName(t, r.clone) {
		if f.Meta.Type == fact.StatementType {
			before[n] = f.Meta.Source
		}
	}
	if len(before) < 2 {
		t.Fatalf("statements: %v", before)
	}
	c, err := ownerwords.LoadAll(os.Getenv("HOME"), "")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := rulingsApply(ctxMinute(t), r.s, c, false, false)
	if err != nil {
		t.Fatalf("the audit after one-offs: %v", err)
	}
	for _, v := range rep.Verdicts {
		if _, ok := before[v.Name]; ok {
			t.Errorf("the audit decided statement %s: %+v", v.Name, v)
		}
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	for n, f := range factsByName(t, r.clone) {
		if src, ok := before[n]; ok && f.Meta.Source != src {
			t.Errorf("%s changed: %+v -> %+v", n, src, f.Meta.Source)
		}
	}
	if findings, err := lint.Run(r.clone); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}
}

// The statements step corrects a statement whose day disagrees with the day
// its words line records (no transcript needed), reads it with that day, logs
// it, and the next run changes nothing.
func TestStatementsStepRedatesFromTheLedger(t *testing.T) {
	r, _ := datesRig(t)
	ql := filepath.Join(r.clone, filepath.FromSlash(ledger.QuotesPath))
	b, _ := os.ReadFile(ql)
	q := ledger.Quotes(b)[ledger.Quote{Name: "old-statement", Quote: "the nightly build stays on the old runner until the migration ends", From: "claude:theirs", Words: true}.Key()]
	q.Said = "2026-08-29"
	os.WriteFile(ql, append(withNewline(b), q.Line()...), 0o644)
	// Judged before: the step needs no model call for it.
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.StatementsPath)), statement.Entry{Kind: statement.LogJudged, At: "2026-10-25", Name: "old-statement", Says: statement.Instruction}.Line(), 0o644)
	r.commitAs("the day it was said, recorded")
	prev := statementsRun
	t.Cleanup(func() { statementsRun = prev })
	calls := 0
	statementsRun = func(context.Context, string, string) (string, float64, error) { calls++; return "[]", 0, nil }
	sr, errs := statementsStep(ctxMinute(t), r.s, statementsOpts{model: "opus"}, &curate.Report{})
	if errs != 0 || sr.Redated != 1 || calls != 0 {
		t.Fatalf("step: %+v, %d error(s), %d call(s)", sr, errs, calls)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	st := factsByName(t, r.clone)["old-statement"]
	if day, _, _ := fact.StatementLine(st.Body); day != "2026-08-29" || st.Meta.Created != "2026-08-29" {
		t.Fatalf("not redated: %+v\n%s", st.Meta, st.Body)
	}
	if findings, err := lint.Run(r.clone); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}
	head := r.serverHead()
	if sr, errs := statementsStep(ctxMinute(t), r.s, statementsOpts{model: "opus"}, &curate.Report{}); errs != 0 || sr.Redated != 0 || r.serverHead() != head {
		t.Fatalf("a second run: %+v, %d error(s)", sr, errs)
	}
}

// With its time nearly up the statements step starts no model call: the
// statements it has not read wait for the next run, counted, and the run is
// no error (a deadline in the middle of the model calls lost every
// judgement with the write).
func TestStatementsStepLeavesTheRestWhenTimeIsUp(t *testing.T) {
	r, _ := datesRig(t)
	prev := statementsRun
	t.Cleanup(func() { statementsRun = prev })
	calls := 0
	statementsRun = func(context.Context, string, string) (string, float64, error) { calls++; return "[]", 0, nil }
	ctx, cancel := context.WithTimeout(context.Background(), stmtWriteReserve-time.Minute)
	defer cancel()
	head := r.serverHead()
	sr, errs := statementsStep(ctx, r.s, statementsOpts{model: "opus"}, &curate.Report{})
	if errs != 0 || calls != 0 || sr.Left < 1 || sr.Judged != 0 || r.serverHead() != head {
		t.Fatalf("step: %+v, %d error(s), %d call(s)", sr, errs, calls)
	}
	if code := runStatements([]string{"--max-runtime", "5m"}); code != 2 {
		t.Fatalf("a max runtime inside the write's reserve: exit %d", code)
	}
}
