package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/rotation"
)

// rotFile is a curated fact file with its own dates.
func rotFile(name, scope, typ, desc, body, created, extra string) string {
	return "---\nname: " + name + "\ndescription: \"" + desc + "\"\ntype: " + typ + "\nscope: " + scope + "\nsource:\n  harness: curator\n  host: host-a\n  kind: observed\n" + extra + "created: " + created + "\n---\n\n" + body + "\n"
}

// fakeRotator answers rotation prompts: answer gives each fact's verdict
// from its item (so it can cite the proof code gathered); a retry prompt
// gets retry's verdicts. Every prompt is kept.
type fakeRotator struct {
	answer, retry func(it rotation.Item) (rotation.Verdict, bool)
	prompts       []string
}

func (f *fakeRotator) run(_ context.Context, _, prompt string) (string, float64, error) {
	f.prompts = append(f.prompts, prompt)
	pick := f.answer
	if strings.Contains(prompt, "CODE REFUSED THESE VERDICTS") && f.retry != nil {
		pick = f.retry
	}
	body := prompt[strings.Index(prompt, "\nFacts:\n")+len("\nFacts:\n"):]
	if i := strings.Index(body, "\nYOUR PREVIOUS ANSWER:"); i >= 0 {
		body = body[:i]
	}
	var out []rotation.Verdict
	for _, l := range strings.Split(body, "\n") {
		var it rotation.Item
		if json.Unmarshal([]byte(l), &it) != nil || it.Name == "" {
			continue
		}
		if v, ok := pick(it); ok {
			v.Name = it.Name
			out = append(out, v)
		}
	}
	b, _ := json.Marshal(out)
	return string(b), 0.12, nil
}

func proofOf(it rotation.Item, factName string) string {
	for _, p := range it.Proof {
		if p.Fact == factName {
			return p.ID
		}
	}
	return ""
}

// rotationRig is a writer store with the format marker and one fact for
// every outcome, all never checked.
func rotationRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, rigOpts{})
	files := map[string]string{
		"system/fine.md":         rotFile("fine", "system", "design_decision", "Builds run in the build queue", "Builds run in the build queue.", "2026-09-01", ""),
		"system/pin-tool.md":     rotFile("pin-tool", "system", "design_decision", "The tool is pinned at v0.7.17", "We pin the tool at v0.7.17 for every repo.", "2026-09-02", ""),
		"system/pin-tool-new.md": rotFile("pin-tool-new", "system", "history", "The tool moved to v0.7.19", "The tool was bumped to v0.7.19 across the pinned repos.", "2026-10-05", ""),
		"system/queue-old.md":    rotFile("queue-old", "system", "design_decision", "Jobs go to the zebra queue", "Jobs go to the zebra queue.", "2026-09-03", ""),
		"system/queue-new.md":    rotFile("queue-new", "system", "design_decision", "Jobs go to the zebra queue v2 now", "Jobs go to the zebra queue v2 since October.", "2026-10-06", ""),
		"system/dead-flag.md":    rotFile("dead-flag", "system", "design_decision", "Set the quokka flag for builds", "Set the quokka flag for builds.", "2026-09-04", ""),
		"system/dup-a.md":        rotFile("dup-a", "system", "design_decision", "Lint runs with the walrus profile", "Lint runs with the walrus profile.", "2026-09-05", ""),
		"system/dup-b.md":        rotFile("dup-b", "system", "design_decision", "The walrus profile is the lint profile", "The walrus lint profile is the one to use.", "2026-09-06", ""),
		"system/unsure.md":       rotFile("unsure", "system", "design_decision", "The narwhal cache is warm by nine", "The narwhal cache is warm by nine.", "2026-09-07", ""),
		"system/badcite.md":      rotFile("badcite", "system", "design_decision", "The otter job runs nightly", "The otter job runs nightly.", "2026-09-08", ""),
		"system/skipped.md":      rotFile("skipped", "system", "design_decision", "The ibis report goes out weekly", "The ibis report goes out weekly.", "2026-09-09", ""),
		"system/recent.md":       rotFile("recent", "system", "design_decision", "The lynx service is checked", "The lynx service is checked.", "2026-09-10", "last_checked: "+time.Now().AddDate(0, 0, -2).Format("2006-01-02")+"\n"),
	}
	for rel, body := range files {
		os.MkdirAll(filepath.Join(r.clone, filepath.Dir(rel)), 0o755)
		os.WriteFile(filepath.Join(r.clone, rel), []byte(body), 0o644)
	}
	os.MkdirAll(filepath.Join(r.clone, "user"), 0o755)
	os.WriteFile(filepath.Join(r.clone, "user", "yak-fridays-fine.md"), []byte(rotFile("yak-fridays-fine", "user", "history", "The yak service deploys fine on Fridays now", "The yak service deploys on Fridays since the canary landed.", "2026-10-09", "")), 0o644)
	writeFact(t, r.clone, fact.Meta{Name: "owner-rule", Description: "Never deploy the yak service on Fridays", Type: "user_standard", Scope: "user", Source: ownerSource("never deploy the yak service on fridays")}, "Never deploy the yak service on Fridays.\n")
	os.MkdirAll(filepath.Join(r.clone, "review"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	r.commitIndex(r.clone, "facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_STATE_DIR", filepath.Join(r.root, "state"))
	return r
}

func runRotationStep(t *testing.T, r *rig, o rotationOpts) rotationStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep := &curate.Report{ModelCalls: &modelrun.Trace{}}
	st, errs := rotationStep(ctx, r.s, o, rep)
	if errs > 0 {
		t.Fatalf("rotation errors: %d", errs)
	}
	return st
}

// The rotation judges every fact due, oldest first, applies each outcome
// as code allows, asks the owner instead of changing their fact, stamps
// last_checked on every fact the model answered, and writes one commit per
// batch; a second run finds nothing due.
func TestRotationAppliesCheckedVerdictsAndStamps(t *testing.T) {
	stubScrump(t) // Synthetic proof; exercise redaction without an installed scrump.
	r := rotationRig(t)
	fr := &fakeRotator{answer: func(it rotation.Item) (rotation.Verdict, bool) {
		first := ""
		if len(it.Proof) > 0 {
			first = it.Proof[0].ID
		}
		switch it.Name {
		case "pin-tool":
			return rotation.Verdict{Outcome: rotation.Fix, Was: "v0.7.17", Now: "v0.7.19", Cites: []string{proofOf(it, "pin-tool-new")}, Evidence: "a newer fact says it moved"}, true
		case "queue-old":
			return rotation.Verdict{Outcome: rotation.Superseded, SupersededBy: "queue-new", Cites: []string{proofOf(it, "queue-new")}, Evidence: "the newer fact replaces it"}, true
		case "dead-flag":
			return rotation.Verdict{Outcome: rotation.Stale, Cites: []string{first}, Evidence: "the flag is gone"}, first != ""
		case "dup-a":
			return rotation.Verdict{Outcome: rotation.Compress, Compress: []string{"dup-b"}, Cites: []string{proofOf(it, "dup-b")}, Evidence: "both say one thing",
				New: &rotation.NewFact{Name: "walrus-lint-profile", Description: "Lint runs with the walrus profile", Type: "design_decision", Body: "Lint runs with the walrus profile ([[dup-a]], [[dup-b]])."}}, true
		case "owner-rule":
			return rotation.Verdict{Outcome: rotation.Superseded, SupersededBy: "yak-fridays-fine", Cites: []string{proofOf(it, "yak-fridays-fine")}, Evidence: "a newer fact says Fridays are fine now"}, true
		case "unsure":
			return rotation.Verdict{Outcome: rotation.Unknown}, true
		case "badcite":
			return rotation.Verdict{Outcome: rotation.Stale, Cites: []string{"p99"}, Evidence: "made up"}, true
		case "skipped":
			return rotation.Verdict{}, false
		}
		return rotation.Verdict{Outcome: rotation.Still}, true
	}, retry: func(it rotation.Item) (rotation.Verdict, bool) {
		if it.Name == "badcite" {
			return rotation.Verdict{Outcome: rotation.Still}, true
		}
		return rotation.Verdict{}, false
	}}
	prev := rotationRun
	rotationRun = fr.run
	t.Cleanup(func() { rotationRun = prev })
	head := strings.TrimSpace(gitIn(t, r.clone, "rev-parse", "HEAD"))

	st := runRotationStep(t, r, rotationOpts{model: "m", age: 30, batch: 50})
	if st.Due != 13 || st.Checked != 12 || st.Unanswered != 1 || st.Fixed != 1 || st.Superseded != 1 || st.Stale != 1 || st.Compressed != 1 || st.Asked != 1 || st.Unknown != 1 || st.Batches != 1 {
		t.Fatalf("stats %+v", st)
	}
	if len(fr.prompts) != 2 || !strings.Contains(fr.prompts[1], "- badcite: cites \"p99\"") {
		t.Fatalf("prompts: %d\n%s", len(fr.prompts), fr.prompts[len(fr.prompts)-1])
	}
	// One commit for the batch, named as the rotation.
	if n := strings.TrimSpace(gitIn(t, r.clone, "rev-list", "--count", head+"..HEAD")); n != "1" {
		t.Fatalf("%s commits", n)
	}
	subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%s"))
	if !strings.HasPrefix(subj, gitev.RotationPrefix+" 12 checked") {
		t.Fatalf("subject %q", subj)
	}
	today := time.Now().Format("2006-01-02")
	read := func(rel string) *fact.Fact {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(r.clone, rel))
		if err != nil {
			t.Fatal(err)
		}
		f, err := fact.Parse(rel, b)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, n := range []string{"fine", "pin-tool", "queue-old", "dead-flag", "dup-a", "unsure", "badcite"} {
		if f := read("system/" + n + ".md"); f.Meta.LastChecked != today {
			t.Errorf("%s not stamped: %q", n, f.Meta.LastChecked)
		}
	}
	if f := read("system/skipped.md"); f.Meta.LastChecked != "" {
		t.Error("a fact the model left out was stamped")
	}
	if f := read("system/pin-tool.md"); !strings.HasSuffix(f.Body, "> superseded "+today+": v0.7.17 -> v0.7.19\n") || !strings.Contains(f.Body, "We pin the tool at v0.7.17 for every repo.") || f.Meta.Updated != today {
		t.Errorf("fix:\n%s", f.Body)
	}
	if f := read("system/queue-old.md"); f.Meta.SupersededBy != "queue-new" {
		t.Error("superseded not written")
	}
	if f := read("system/dead-flag.md"); f.Meta.Retired != today {
		t.Error("stale not retired")
	}
	j := read("system/walrus-lint-profile.md")
	if j.Meta.Scope != "system" || !strings.Contains(j.Body, "[[dup-a]]") {
		t.Errorf("compress: %+v", j.Meta)
	}
	for _, n := range []string{"dup-a", "dup-b"} {
		if f := read("system/" + n + ".md"); f.Meta.SupersededBy != "walrus-lint-profile" {
			t.Errorf("%s not pointed at the compressed fact", n)
		}
	}
	// The owner's rule: unchanged words, a question for them, a stamp.
	rule := read("user/owner-rule.md")
	if rule.Meta.Retired != "" || rule.Meta.LastChecked != today || !strings.Contains(rule.Body, "Never deploy the yak service on Fridays.") {
		t.Errorf("the owner's rule changed: %+v", rule.Meta)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	var asked bool
	for _, it := range rs.checkItems() {
		if it.A.Name == "owner-rule" && it.NeedsOwner && strings.Join(it.Allowed, ",") == "current,retire" && len(it.Flag.Detail) > 0 {
			asked = true
			// An agent may not answer it; the owner may.
			if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, A: "owner-rule", Mark: "current", By: "agent", Evidence: "x"}}); err != nil || errs[0] == nil {
				t.Errorf("an agent answered the owner's question: %v %v", err, errs)
			}
			if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "current"}}); err != nil || errs[0] != nil {
				t.Errorf("the owner could not answer: %v %v", err, errs)
			}
		}
	}
	if !asked {
		t.Fatalf("no question for the owner: %+v", rs.checkItems())
	}
	dl, _ := os.ReadFile(filepath.Join(r.clone, filepath.FromSlash(ledger.DecisionsPath)))
	for _, want := range []string{`"action":"fix"`, `"action":"superseded"`, `"action":"stale"`, `"action":"compress"`, `"action":"ask"`, `"by":"curator"`} {
		if !strings.Contains(string(dl), want) {
			t.Errorf("decisions lack %s", want)
		}
	}
	if fs, _ := lint.Run(r.clone); len(fs) > 0 {
		t.Fatalf("lint: %v", fs)
	}
	// Stamped facts rest: a second run asks only about the one left out and
	// the fact the compress wrote.
	fr.prompts = nil
	st = runRotationStep(t, r, rotationOpts{model: "m", age: 30, batch: 50})
	if st.Due != 2 || len(fr.prompts) == 0 || !strings.Contains(fr.prompts[0], `"name":"skipped"`) || !strings.Contains(fr.prompts[0], `"name":"walrus-lint-profile"`) {
		t.Fatalf("second run: %+v", st)
	}
	// A stamp does not move the fact's effective date.
	b := newTimelineBuilder(r.s, time.Time{})
	f := read("system/fine.md")
	if e := b.Effective(context.Background(), gitev.FactRef{Fact: f}); e.Why != "created" {
		t.Errorf("the stamp moved the effective date: %+v", e)
	}
}

// Without the store's format marker the rotation writes nothing.
func TestRotationWaitsForTheFormatMarker(t *testing.T) {
	r := newRig(t, rigOpts{})
	os.MkdirAll(filepath.Join(r.clone, "system"), 0o755)
	os.WriteFile(filepath.Join(r.clone, "system", "x.md"), []byte(rotFile("x", "system", "design_decision", "x", "x", "2026-09-01", "")), 0o644)
	r.commitIndex(r.clone, "x")
	called := false
	prev := rotationRun
	rotationRun = func(context.Context, string, string) (string, float64, error) { called = true; return "[]", 0, nil }
	t.Cleanup(func() { rotationRun = prev })
	st := runRotationStep(t, r, rotationOpts{model: "m", age: 30})
	if called || st.Due != 0 {
		t.Fatalf("ran without the marker: %+v", st)
	}
}

// A fact that claims the owner's authority without their words is checked
// first, whatever its age, and the rotation may reword it as an agent's
// note: one new fact in the same folder supersedes it. A reword that still
// claims their authority, or quotes words of theirs the fact does not, is
// refused; so is a reword of a fact that claims nothing.
func TestRotationRewordsClaimsOfTheOwnersAuthorityFirst(t *testing.T) {
	stubScrump(t)
	r := rotationRig(t)
	os.WriteFile(filepath.Join(r.clone, "system", "claim-tabs.md"), []byte(rotFile("claim-tabs", "system", "user_standard", "Owner ruling: build logs use tabs", "The owner said build logs use tabs.", "2026-10-20", "")), 0o644)
	r.commitIndex(r.clone, "claim")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	tries := 0
	var fr *fakeRotator
	fr = &fakeRotator{answer: func(it rotation.Item) (rotation.Verdict, bool) {
		switch it.Name {
		case "claim-tabs":
			if !it.ClaimsOwner || len(it.Proof) == 0 || it.Proof[0].Kind != "attribution" {
				t.Errorf("the claim is not marked: %+v", it)
			}
			tries++
			return rotation.Verdict{Outcome: rotation.Reword, Cites: []string{it.Proof[0].ID}, Evidence: "no words of theirs",
				New: &rotation.NewFact{Name: "build-logs-tabs", Description: "The owner said: \"build logs always use tabs and never spaces\"", Body: "Build logs use tabs ([[claim-tabs]])."}}, true
		}
		return rotation.Verdict{Outcome: rotation.Still}, true
	}, retry: func(it rotation.Item) (rotation.Verdict, bool) {
		if it.Name == "claim-tabs" {
			if p := fr.prompts[len(fr.prompts)-1]; !strings.Contains(p, "still claims the owner's authority") || !strings.Contains(p, "never put words in the owner's mouth") {
				t.Errorf("refusals: %s", p)
			}
			return rotation.Verdict{Outcome: rotation.Reword, Cites: []string{it.Proof[0].ID}, Evidence: "no words of theirs",
				New: &rotation.NewFact{Name: "build-logs-tabs", Description: "Build logs use tabs (an agent noted it)", Body: "Build logs use tabs ([[claim-tabs]])."}}, true
		}
		return rotation.Verdict{Outcome: rotation.Still}, true
	}}
	prev := rotationRun
	rotationRun = fr.run
	t.Cleanup(func() { rotationRun = prev })
	st := runRotationStep(t, r, rotationOpts{model: "m", age: 30, batch: 50, max: 1})
	if st.Claims != 1 || !strings.Contains(fr.prompts[0], `"name":"claim-tabs"`) {
		t.Fatalf("the claim is not first: %+v\n%s", st, fr.prompts[0])
	}
	if st.Reworded != 1 || tries != 1 {
		t.Fatalf("stats %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(r.clone, "system", "claim-tabs.md"))
	old, _ := fact.Parse("system/claim-tabs.md", b)
	nb, err := os.ReadFile(filepath.Join(r.clone, "system", "build-logs-tabs.md"))
	if err != nil || old.Meta.SupersededBy != "build-logs-tabs" {
		t.Fatalf("not reworded: %v %+v", err, old.Meta)
	}
	nf, _ := fact.Parse("system/build-logs-tabs.md", nb)
	if nf.Meta.Type != "user_standard" || nf.Meta.Source.Kind != "observed" || claimsOwner(nf) {
		t.Fatalf("the new fact: %+v", nf.Meta)
	}
	// A reword of a fact that claims nothing is refused.
	it := rotation.Item{Name: "fine", Proof: []rotation.Proof{{ID: "p1", Kind: "git"}}}
	if ps := rotation.Check(rotation.Verdict{Outcome: rotation.Reword, Cites: []string{"p1"}, Evidence: "x", New: &rotation.NewFact{Name: "fine-2", Description: "x", Body: "[[fine]]"}}, it, rotation.Env{Taken: func(string) bool { return false }}); len(ps) == 0 {
		t.Fatal("a reword of a plain fact passed")
	}
	if fs, _ := lint.Run(r.clone); len(fs) > 0 {
		t.Fatalf("lint: %v", fs)
	}
}
