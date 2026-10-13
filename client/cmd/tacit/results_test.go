package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/results"
	"github.com/agent-sh/tacit/client/internal/search"
)

// gitAt commits in dir at a fixed date, so the series a fixture repo yields
// do not depend on the day the test runs.
func gitAt(t *testing.T, dir, date string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitResults(t *testing.T, repo, date, msg string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitAt(t, repo, date, "add", "-A")
	gitAt(t, repo, date, "commit", "-q", "-m", msg)
}

func logRows(day, code string, vals map[string]string) string {
	var b strings.Builder
	for _, cell := range []string{"q9-plain-short", "q9-plain-long", "q9-spec-short"} {
		fmt.Fprintf(&b, `{"ts":"%sT10:00:00Z","git":"%s","cell":"%s","toks":%s}`+"\n", day, code, cell, vals[cell])
	}
	return b.String()
}

func table(set, get string) string {
	return "# Bench\n\n## Results, localhost\n\n| Scenario | Ours | C (ref) |\n|---|---:|---:|\n" +
		"| SET 100B conc=10 | " + set + " r/s | 8,775 r/s |\n| GET 100B conc=10 | " + get + " r/s | 8,627 r/s |\n| SET 16KiB conc=10 | 4,100 r/s | 4,300 r/s |\n"
}

// resultsRig is a store whose repo "bench" has two committed results for
// the same benchmark: a row log and a table, the second commit newer.
func resultsRig(t *testing.T, extra func(r *rig)) *rig {
	t.Helper()
	r := newRig(t, rigOpts{repos: []string{"bench"}, filler: 400})
	repo := filepath.Join(r.projects, "bench")
	first := logRows("2026-08-30", "aaa1111", map[string]string{"q9-plain-short": "138.5", "q9-plain-long": "120.4", "q9-spec-short": "281.0"})
	commitResults(t, repo, "2026-09-01T12:00:00Z", "bench: first results", map[string]string{"results.jsonl": first, "RESULTS.md": table("6,821", "7,245")})
	second := first + logRows("2026-09-09", "bbb2222", map[string]string{"q9-plain-short": "161.3", "q9-plain-long": "120.5", "q9-spec-short": "301.2"})
	commitResults(t, repo, "2026-09-10T12:00:00Z", "bench: newer results", map[string]string{"results.jsonl": second, "RESULTS.md": table("7,305", "7,245")})

	write := func(name, kind, text string) {
		src := fact.Source{Kind: kind}
		if kind == "user" {
			src = ownerSource(strings.TrimPrefix(text, "Owner: ")) // their words, on record
		}
		m := fact.Meta{Name: name, Description: text, Type: "design_decision", Scope: "projects/bench", Source: src}
		writeFact(t, r.clone, m, text)
	}
	write("bench-q9-plain-speed", "observed", "bench q9 plain short decode ran 138.5 tok/s on the host-a")
	write("bench-owner-q9-floor", "user", "Owner: q9 plain short must stay above 138.5 tok/s on the host-a")
	write("bench-owner-q9-speed", "user", "Owner: q9 plain short decodes 138.5 tok/s on the host-a")
	write("bench-q9-both", "observed", "bench q9 plain short went from 138.5 to 161.3 tok/s")
	write("bench-q9-arrow", "observed", "bench q9 plain short went 138.5 -> 161.3 tok/s")
	write("bench-q9-now", "observed", "bench q9 plain short decode is 161.3 tok/s now")
	write("bench-q9-spec-long", "observed", "bench q9 spec long ran 138.5 tok/s")
	write("bench-q9-dated", "observed", "On 2026-09-02 bench q9 plain short ran 138.5 tok/s, the first published figure.")
	write("bench-q9-small-number", "observed", "bench q9 plain short ran 14 tok/s once, when the card was shared")
	write("bench-set-conc10", "observed", "bench SET 100B conc=10 reached 6,821 r/s against the C reference")
	if extra != nil {
		extra(r)
	}
	r.commitIndex(r.clone, "seed facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	return r
}

func (r *rig) resultsOpts() results.Options {
	return results.Options{Store: r.clone, Projects: r.projects, Host: "host-a", Date: "2026-10-10", Red: func(xs []string) ([]string, error) { return xs, nil }}
}

func (r *rig) serverFile(rel string) string {
	r.t.Helper()
	return gitIn(r.t, r.bare, "show", "main:"+rel)
}

func (r *rig) resultFacts() []string {
	var out []string
	for _, f := range r.serverFiles() {
		if strings.HasPrefix(f, "derived/bench/bench-results-") {
			out = append(out, f)
		}
	}
	return out
}

func (r *rig) resultFactWith(substr string) string {
	r.t.Helper()
	for _, f := range r.resultFacts() {
		if body := r.serverFile(f); strings.Contains(body, substr) {
			return body
		}
	}
	r.t.Fatalf("no result fact holds %q among %v", substr, r.resultFacts())
	return ""
}

// Two committed results for one benchmark: the store ends with the newer
// one, the older one is shown as what it replaced, the saved fact that
// quotes it is flagged, an owner ruling is only put to the owner, a different
// setup and a dated record stand, and nothing the step did is lint.
func TestResultsStepSnapshotAndFlags(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	ruling := r.serverFile("projects/bench/bench-owner-q9-speed.md")
	saved := r.serverFile("projects/bench/bench-q9-plain-speed.md")
	seed := r.serverHead()

	res, err := resultsStep(ctx, r.s, r.resultsOpts(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Count(derive.Failed) != 0 || len(res.Findings) == 0 {
		t.Fatalf("report %+v findings %d", res.Report.Repos, len(res.Findings))
	}
	if a := gitIn(t, r.bare, "log", "-1", "--format=%an|%s", "main"); !strings.HasPrefix(a, "curator|curator: derived facts for") {
		t.Fatalf("commit %q", a)
	}
	if r.serverHead() == seed {
		t.Fatal("nothing was pushed")
	}

	// the log: the newest row, labelled, with the commit and the line
	toks := r.resultFactWith("cell=q9-plain-short")
	for _, want := range []string{"161.3 tok/s", "run count not stated (one row)", "results.jsonl:4", "2 dated rows recorded since 2026-08-30", "written by commit "} {
		if !strings.Contains(toks, want) {
			t.Fatalf("log fact lacks %q:\n%s", want, toks)
		}
	}
	// the table: the new number, and the old one as what it replaced, with its commit
	tbl := r.resultFactWith("SET 100B conc=10")
	for _, want := range []string{"7305", "Replaced 6821", "RESULTS.md:"} {
		if !strings.Contains(tbl, want) {
			t.Fatalf("snapshot fact lacks %q:\n%s", want, tbl)
		}
	}
	// only the new number is the headline
	for _, f := range r.resultFacts() {
		body := r.serverFile(f)
		desc := body[:strings.Index(body, "\n---\n")]
		if strings.Contains(desc, "138.5") || strings.Contains(desc, "6821") {
			t.Fatalf("an older value reached the description of %s:\n%s", f, desc)
		}
	}

	// the findings
	review := r.serverFile("review/2026-10-10-results-bench.md")
	nameLog := results.FactName("bench", "results.jsonl", "toks")
	nameTbl := results.FactName("bench", "RESULTS.md", "Results, localhost / Scenario")
	for _, want := range []string{
		"- result-superseded? [[" + nameLog + "]] may replace [[bench-q9-plain-speed]]",
		"- RULING CHANGE? new [[" + nameLog + "]] may supersede owner ruling [[bench-owner-q9-speed]]",
		"- result-superseded? [[" + nameTbl + "]] may replace [[bench-set-conc10]]",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("review lacks %q:\n%s", want, review)
		}
	}
	// a floor is not a claim about the measurement, and a fact that states both values is about the current one
	for _, not := range []string{"bench-q9-now", "bench-q9-spec-long", "bench-q9-dated", "bench-q9-small-number", "bench-owner-q9-floor", "bench-q9-both", "bench-q9-arrow"} {
		if strings.Contains(review, "[["+not+"]]") {
			t.Fatalf("%s must not be flagged:\n%s", not, review)
		}
	}
	// a flag edits nothing: the saved fact and the ruling are byte for byte what they were
	if got := r.serverFile("projects/bench/bench-owner-q9-speed.md"); got != ruling || strings.Contains(got, "superseded_by") {
		t.Fatal("the owner's ruling was touched")
	}
	if got := r.serverFile("projects/bench/bench-q9-plain-speed.md"); got != saved {
		t.Fatal("the saved fact was edited")
	}
	r.assertClean("after a push")
	r.assertServerLintsClean()
	if mem := r.serverFile("MEMORY.md"); !strings.Contains(mem, "bench-results-") {
		t.Fatalf("MEMORY.md lacks the result facts:\n%s", mem)
	}

	// same commits: nothing new, no second finding
	head := r.serverHead()
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	if r.serverHead() != head {
		t.Fatal("a second run pushed")
	}
}

// A retired fact (outdated per git, no longer delivered) is not a saved fact
// that quotes a number: it neither names a series nor is flagged.
func TestResultsIgnoresRetiredSavedFacts(t *testing.T) {
	r := resultsRig(t, func(r *rig) {
		m := fact.Meta{Name: "bench-q9-retired", Description: "bench q9 plain short ran 138.5 tok/s, retired", Type: "design_decision", Scope: "projects/bench", Source: fact.Source{Kind: "observed"}, Retired: "2026-10-09"}
		writeFact(t, r.clone, m, "bench q9 plain short ran 138.5 tok/s, retired")
	})
	if _, err := resultsStep(ctxMinute(t), r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	review := r.serverFile("review/2026-10-10-results-bench.md")
	if !strings.Contains(review, "[[bench-q9-plain-speed]]") {
		t.Fatalf("the live saved fact is flagged (control):\n%s", review)
	}
	if strings.Contains(review, "[[bench-q9-retired]]") {
		t.Fatalf("a retired fact was flagged:\n%s", review)
	}
}

// A newer commit replaces the number in the same fact; a saved fact that
// quoted the value it replaced is a new finding, and the finding already
// raised is not raised twice.
func TestResultsStepFollowsANewerCommit(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	before := r.resultFactWith("161.3 tok/s")
	repo := filepath.Join(r.projects, "bench")
	prev, _ := os.ReadFile(filepath.Join(repo, "results.jsonl"))
	third := string(prev) + logRows("2026-09-20", "ccc3333", map[string]string{"q9-plain-short": "180.0", "q9-plain-long": "120.5", "q9-spec-short": "301.2"})
	commitResults(t, repo, "2026-09-21T12:00:00Z", "bench: newer still", map[string]string{"results.jsonl": third})

	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	after := r.resultFactWith("180.0 tok/s")
	if after == before || strings.Contains(after[:strings.Index(after, "\n---\n")], "161.3") {
		t.Fatalf("the fact still shows the replaced value first:\n%s", after)
	}
	if !strings.Contains(after, "The newest different value before it was 161.3") {
		t.Fatalf("the replaced value is not shown as such:\n%s", after)
	}
	review := r.serverFile("review/2026-10-10-results-bench.md")
	// the finding raised in the first run is still the only one for that pair
	nameLog := results.FactName("bench", "results.jsonl", "toks")
	if n := strings.Count(review, "may replace [[bench-q9-plain-speed]]"); n != 1 {
		t.Fatalf("raised %d times:\n%s", n, review)
	}
	// the fact that quoted 161.3 was current; now it is out of date
	if !strings.Contains(review, "- result-superseded? [["+nameLog+"]] may replace [[bench-q9-now]]") {
		t.Fatalf("the saved fact that quoted the replaced 161.3 is not flagged:\n%s", review)
	}
	r.assertServerLintsClean()
}

// The review page reads the finding as a pair: A the saved fact, B the result.
func TestResultsFindingsAreReviewPairs(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	var kinds = map[string]string{}
	for _, p := range parseReviewFiles(r.clone) {
		kinds[p.a] = p.kind + ">" + p.b
	}
	nameLog := results.FactName("bench", "results.jsonl", "toks")
	if got := kinds["bench-q9-plain-speed"]; got != kindResultSup+">"+nameLog {
		t.Fatalf("saved fact: %q (all %v)", got, kinds)
	}
	if got := kinds["bench-owner-q9-speed"]; got != kindRulingChange+">"+nameLog {
		t.Fatalf("ruling: %q", got)
	}
}

// --dry-run and --out change nothing in the store.
func TestResultsDryRunTouchesNothing(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	head := r.serverHead()
	res, err := resultsStep(ctx, r.s, r.resultsOpts(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Report.Changed()) == 0 || len(res.Findings) == 0 {
		t.Fatalf("a dry run still reports what it would do: %+v", res.Report.Repos)
	}
	if r.serverHead() != head {
		t.Fatal("a dry run pushed")
	}
	r.assertClean("after a dry run")
}

// Redaction runs on every string from the repo before anything is clipped:
// a secret-shaped setup value reaches neither a fact nor a finding, and a
// redactor that fails writes nothing.
func TestResultsRedactBeforeClipAndFailClosed(t *testing.T) {
	requireScrump(t)
	secret := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)
	r := newRig(t, rigOpts{repos: []string{"bench"}})
	repo := filepath.Join(r.projects, "bench")
	row := func(day string, v string) string {
		return fmt.Sprintf(`{"ts":"%sT10:00:00Z","cell":"%s","toks":%s}`+"\n", day, secret, v)
	}
	commitResults(t, repo, "2026-09-01T12:00:00Z", "first", map[string]string{"results.jsonl": row("2026-08-30", "100.5")})
	commitResults(t, repo, "2026-09-10T12:00:00Z", "second", map[string]string{"results.jsonl": row("2026-08-30", "100.5") + row("2026-09-09", "120.5")})
	writeFact(t, r.clone, fact.Meta{Name: "bench-secret-cell", Description: "the cell " + secret + " ran 100.5 tok/s", Type: "design_decision", Scope: "projects/bench"}, "x")
	r.commitIndex(r.clone, "seed")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")

	o := r.resultsOpts()
	o.Red = digest.RedactAll
	if _, err := resultsStep(ctxMinute(t), r.s, o, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.serverFiles() {
		if !strings.HasPrefix(f, "derived/bench/") && !strings.HasPrefix(f, "review/") {
			continue
		}
		if body := r.serverFile(f); strings.Contains(body, "ghp_") {
			t.Fatalf("a secret reached %s:\n%s", f, body)
		}
	}
	if len(r.resultFacts()) == 0 {
		t.Fatal("the step wrote no result fact")
	}

	// a redactor that cannot run: no fact, a failed repo, nothing pushed
	r2 := resultsRig(t, nil)
	head := r2.serverHead()
	o2 := r2.resultsOpts()
	o2.Red = func([]string) ([]string, error) { return nil, errors.New("scrump is down") }
	res, err := resultsStep(ctxMinute(t), r2.s, o2, false)
	if err == nil && res.Report.Count(derive.Failed) == 0 {
		t.Fatal("a failing redactor must fail the repo")
	}
	if r2.serverHead() != head {
		t.Fatal("something was pushed with redaction down")
	}
}

// Result facts reach the prompt hook through the repo's derived scope, one at
// most, and never SessionStart.
func TestResultFactsReachThePromptHookNotSessionStart(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	head, err := r.s.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := search.Open(ctx, r.clone, head)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sit := search.Situation{Repo: "bench", Host: "host-a"}
	got, err := promptHits(ctx, ix, sit, "what is the newest q9 plain short decode tok/s cell in results.jsonl for bench", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	var derived []string
	for _, h := range got {
		if isDerived(h) {
			derived = append(derived, h.Name)
		}
	}
	if len(derived) != 1 || !strings.Contains(derived[0], "bench-results-") {
		t.Fatalf("the prompt hook returned %s", names(got))
	}
	start, err := sessionStartHits(ctx, ix, sit)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range start {
		if isDerived(h) {
			t.Fatalf("SessionStart carries %s", h.Name)
		}
	}
	// the hook line says where it came from
	block := search.Block(got, "tacit")
	if !strings.Contains(block, "(computed from git)") {
		t.Fatalf("block:\n%s", block)
	}
}

// A result fact that a saved fact names with superseded_by is not removed
// when its series leaves the repo: the link would dangle (lint L7).
func TestResultFactsAFactPointsAtAreKept(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	name := results.FactName("bench", "results.jsonl", "toks")
	if _, ok := markSuperseded(r.clone, "projects/bench/bench-q9-plain-speed.md", name); !ok {
		t.Fatal("could not mark the saved fact")
	}
	r.commitIndex(r.clone, "owner answered")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")

	repo := filepath.Join(r.projects, "bench")
	gitAt(t, repo, "2026-09-25T12:00:00Z", "rm", "-q", "results.jsonl")
	gitAt(t, repo, "2026-09-25T12:00:00Z", "commit", "-q", "-m", "drop the log")
	res, err := resultsStep(ctx, r.s, r.resultsOpts(), false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.serverFiles() {
		if strings.HasSuffix(f, name+".md") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the fact the saved fact points at was removed; report %+v", res.Report.Repos)
	}
	r.assertServerLintsClean()

	// an unreferenced one is removed: drop the table too
	gitAt(t, repo, "2026-09-26T12:00:00Z", "rm", "-q", "RESULTS.md")
	gitAt(t, repo, "2026-09-26T12:00:00Z", "commit", "-q", "-m", "drop the table")
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.serverFiles() {
		if strings.Contains(f, "results-results-localhost") {
			t.Fatalf("an unreferenced result fact outlived its series: %s", f)
		}
	}
	r.assertServerLintsClean()
}

// The derive step owns hot spots and the module map. A result fact under the
// same derived/<repo>/ is generated too, but it is not stale hot spot output:
// derive must neither remove it nor skip the repo because of it.
func TestDeriveLeavesResultFactsAlone(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	before := r.resultFacts()
	if len(before) == 0 {
		t.Fatal("the results step wrote nothing")
	}
	rep, err := r.step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 {
		t.Fatalf("derive wrote the repo's own two facts: %+v", rep.Repos)
	}
	after := r.resultFacts()
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("derive changed the result facts:\nbefore %v\nafter  %v", before, after)
	}
	hot := false
	for _, f := range r.serverFiles() {
		if f == "derived/bench/bench-hot-spots.md" {
			hot = true
		}
	}
	if !hot {
		t.Fatal("derive wrote no hot spots")
	}
	r.assertServerLintsClean()
}
