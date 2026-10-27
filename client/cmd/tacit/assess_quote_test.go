package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

const (
	stopWords  = "we are stoping development of oldtool. bank what you have in one branch, take down all servers"
	pauseWords = "we are banking our work and releasing boxes, no more oldtool work for now"
)

// quoteHome is assessHome with oldtool, the owner's repository (commits on
// the given days), and their session claude:s9 of 28 days ago: the stop
// order, then, typed while the agent worked and after the reminder Claude
// Code put in front of it, the same pause "for now".
func quoteHome(t *testing.T, commitDays ...int) (string, *store.Store) {
	home, s := assessHome(t)
	var cs [][2]string
	for _, d := range commitDays {
		cs = append(cs, [2]string{daysAgo(d), "cmd/run.go"})
	}
	datedRepo(t, home, "oldtool", cs...)
	at := time.Now().AddDate(0, 0, -28).UTC()
	typed := func(ts time.Time, text string) string {
		b, _ := json.Marshal(text)
		return `{"parentUuid":"ca33","isSidechain":false,"promptSource":"typed","turnOrigin":"human","type":"user","message":{"role":"user","content":` + string(b) + `},"uuid":"4192","timestamp":"` + ts.Format(time.RFC3339) + `","userType":"external","entrypoint":"cli","cwd":"/home/u/projects/oldtool","sessionId":"s9","version":"2.1.302","origin":{"kind":"human"}}`
	}
	queued := func(ts time.Time, text string) string {
		b, _ := json.Marshal(text)
		return `{"parentUuid":"8d5d","isSidechain":false,"attachment":{"type":"queued_command","prompt":` + string(b) + `,"commandMode":"prompt","origin":{"kind":"human"},"humanTurn":true},"type":"attachment","uuid":"728b","timestamp":"` + ts.Format(time.RFC3339) + `","entrypoint":"cli","sessionId":"s9","version":"2.1.302"}`
	}
	tr := filepath.Join(home, ".claude", "projects", "-home-u-projects-oldtool", "s9.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o700)
	os.WriteFile(tr, []byte(strings.Join([]string{
		typed(at, stopWords),
		queued(at.Add(13*time.Minute), "<system-reminder>\nThe user started your suggested background task task_c187 in a separate cloud session. It is running independently.\n</system-reminder>\n\n"+pauseWords),
	}, "\n")+"\n"), 0o600)
	return home, s
}

func quoteDecision(status, quote string) assess.Decision {
	d := aDecision("projects/oldtool", "repo", status, "the owner's repository; they stopped it in claude:s9")
	d.OwnerQuote, d.OwnerQuoteFrom = quote, "claude:s9"
	return d
}

var quoteOnly = map[string]bool{"projects/oldtool": true}

// The owner stopped oldtool "for now", 28 days ago, and nobody worked on it
// since. Dead on those words is refused (no words of permanence; a pause
// the model itself flagged); dormant on them stands, dated by the day they
// were said, with the owner's words after the reminder shown up front.
func TestAssessQuoteStopIsDormant(t *testing.T) {
	_, s := quoteHome(t, 40, 35, 30)
	dead := quoteDecision("dead", stopWords)
	paused := quoteDecision("dead", pauseWords)
	paused.OwnerQuoteTemporal, paused.OwnerQuoteFinal = "for now", "no more oldtool work"
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/oldtool": dead}, fixed: map[string]assess.Decision{"projects/oldtool": paused}}
	fa.install(t)
	rep := &curate.Report{}
	assessStep(context.Background(), s, assessOpts{model: "opus", only: quoteOnly}, rep)
	if rep.AssessRecords != 0 || rep.AssessRefused != 1 || len(fa.prompts) != 2 {
		t.Fatalf("dead on a stop and on a pause: report %+v, %d prompts", rep, len(fa.prompts))
	}
	if !strings.Contains(fa.prompts[0], `owner's turn (claude:s9): "we are banking our work and releasing boxes, no more oldtool work for now"`) {
		t.Errorf("the owner's words after the reminder:\n%s", fa.prompts[0])
	}
	if !strings.Contains(fa.prompts[1], "owner_quote_final") || !strings.Contains(fa.prompts[1], "stopping, banking or pausing work is dormant") {
		t.Errorf("the refusal:\n%s", fa.prompts[1][strings.Index(fa.prompts[1], "CODE REFUSED"):])
	}
	if p := assess.CheckQuoteStatus(assess.Candidate{}, paused); len(p) != 1 || !strings.Contains(p[0], `the words limit themselves in time ("for now")`) {
		t.Errorf("a pause the model flagged: %q", p)
	}
	fa = &fakeAssessor{first: map[string]assess.Decision{"projects/oldtool": quoteDecision("dormant", pauseWords)}}
	fa.install(t)
	rep = &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: quoteOnly}, rep); errs != 0 || rep.AssessRecords != 1 {
		t.Fatalf("dormant: errs %d report %+v", errs, rep)
	}
	recs, _ := record.Walk(s.Dir)
	r := recs["projects/oldtool"]
	if r == nil || r.Status != record.StatusDormant || r.Was[0].By != record.ByOwner || r.Was[0].Quote != pauseWords ||
		!strings.HasPrefix(r.Was[0].Why, "code: dormant on the owner's words of "+daysAgo(28)+", claude:s9") {
		t.Fatalf("oldtool: %+v", r)
	}
	ql, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
	if !strings.Contains(string(ql), `"said":"`+daysAgo(28)+`"`) {
		t.Errorf("the ledger line names the day they said it:\n%s", ql)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
	// --recheck-quoted asks about it again; the same reading writes nothing new.
	n := len(fa.prompts)
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: quoteOnly, recheckQuoted: true}, &curate.Report{}); errs != 0 || len(fa.prompts) != n+1 ||
		!strings.Contains(fa.prompts[n], "asked again: its dormant status rests on the owner's words (claude:s9)") {
		t.Fatalf("recheck: errs %d, %d prompts", errs, len(fa.prompts)-n)
	}
}

// Dead stands on words that end it; and when the owner really worked on the
// folder after the day of the words, its dates stand, whatever the words
// say, and a review line says so.
func TestAssessQuoteWorkAfterTheWords(t *testing.T) {
	_, s := quoteHome(t, 40, 35, 3)
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/oldtool": quoteDecision("dormant", pauseWords)}}
	fa.install(t)
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: quoteOnly}, rep); errs != 0 || rep.AssessRecords != 1 {
		t.Fatalf("errs %d report %+v", errs, rep)
	}
	recs, _ := record.Walk(s.Dir)
	r := recs["projects/oldtool"]
	if r == nil || r.Status != record.StatusActive || r.Was[0].By != record.ByOpus || r.Was[0].Quote != "" ||
		!strings.Contains(r.Was[0].Why, "Owner activity after the "+daysAgo(28)+" words (claude:s9") {
		t.Fatalf("oldtool: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "review", time.Now().Format("2006-01-02")+"-assess.md")); !strings.Contains(string(b), "the owner worked on it after that day, so its dates stand") {
		t.Errorf("review:\n%s", b)
	}
	// Words that end it, dead: the record is the owner's, dead.
	home, s2 := quoteHome(t, 40, 35, 30)
	end := quoteDecision("dead", stopWords+", drop the repo")
	tr := filepath.Join(home, ".claude", "projects", "-home-u-projects-oldtool", "s9.jsonl")
	b, _ := os.ReadFile(tr)
	os.WriteFile(tr, []byte(strings.Replace(string(b), stopWords, stopWords+", drop the repo", 1)), 0o600)
	end.OwnerQuoteFinal = "drop the repo"
	fa = &fakeAssessor{first: map[string]assess.Decision{"projects/oldtool": end}}
	fa.install(t)
	s = s2
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: quoteOnly}, &curate.Report{}); errs != 0 {
		t.Fatalf("errs %d", errs)
	}
	recs, _ = record.Walk(s.Dir)
	if r := recs["projects/oldtool"]; r == nil || r.Status != record.StatusDead || r.Was[0].By != record.ByOwner || !strings.HasPrefix(r.Was[0].Why, "code: dead on the owner's words of "+daysAgo(28)) {
		t.Fatalf("dead on words that end it: %+v", r)
	}
}
