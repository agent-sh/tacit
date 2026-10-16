package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/results"
	"github.com/agent-sh/tacit/client/internal/search"
)

// withFailingGit puts a git first in PATH that behaves like git except that a
// call with one of the arguments exits 128, as a killed or timed-out read does.
func withFailingGit(t *testing.T, args ...string) (restore func()) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	var cases []string
	for _, a := range args {
		cases = append(cases, a)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in\n    " + strings.Join(cases, "|") + ") echo 'fatal: simulated failure' >&2; exit 128;;\n  esac\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+orig)
	return func() { t.Setenv("PATH", orig) }
}

// A git read that failed or was cut gives a partial list of series, and a
// partial list would retire the facts of every file not read. The repo fails
// instead, and the store keeps what it has.
func TestResultsGitReadFailureKeepsWhatTheStoreHas(t *testing.T) {
	for _, failing := range []string{"--batch-check", "--name-only", "--batch"} {
		t.Run(failing, func(t *testing.T) {
			r := resultsRig(t, nil)
			ctx := ctxMinute(t)
			if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
				t.Fatal(err)
			}
			before, head := strings.Join(r.resultFacts(), ","), r.serverHead()
			if before == "" {
				t.Fatal("the first run wrote nothing")
			}
			restore := withFailingGit(t, failing)
			res, err := resultsStep(ctx, r.s, r.resultsOpts(), false)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			if res.Report.Count(derive.Failed) != 1 {
				t.Fatalf("the repo must be reported as failed: %+v", res.Report.Repos)
			}
			if got := strings.Join(r.resultFacts(), ","); got != before || r.serverHead() != head {
				t.Fatalf("a failed read changed the store:\nbefore %s\nafter  %s", before, got)
			}
			if len(res.Findings) != 0 {
				t.Fatalf("findings from a failed read: %d", len(res.Findings))
			}
			// the next good run finds everything as it was
			if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
				t.Fatal(err)
			}
			if r.serverHead() != head {
				t.Fatal("a good run after a failed one rewrote facts")
			}
		})
	}
}

// A read that a bound cut (the byte budget) has the rows it read and not the
// files it did not: it writes what it read and removes nothing.
func TestResultsCutReadRemovesNothing(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	before, head := strings.Join(r.resultFacts(), ","), r.serverHead()
	if before == "" {
		t.Fatal("the first run wrote nothing")
	}
	o := r.resultsOpts()
	o.ReadBudget = 1 // nothing fits: every file of the repo is cut
	res, err := resultsStep(ctx, r.s, o, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.resultFacts(), ","); got != before || r.serverHead() != head {
		t.Fatalf("a cut read removed or rewrote facts:\nbefore %s\nafter  %s", before, got)
	}
	cut := false
	for _, rr := range res.Report.Repos {
		cut = cut || strings.Contains(rr.Detail, "cut by a bound")
	}
	if !cut {
		t.Fatalf("the cut is not reported: %+v", res.Report.Repos)
	}
}

// B of a result-superseded pair is a generated file the next run rewrites. The
// marks that would patch it (left, query) are not offered, and the decision
// that is offered patches the saved fact only.
func TestResultSupersededMarksNeverPatchTheGeneratedFact(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	items := map[string]reviewItem{}
	for _, it := range rs.items() {
		items[it.A.Name] = it
	}
	saved, ruling := items["bench-q9-plain-speed"], items["bench-owner-q9-speed"]
	if saved.Kind != kindResultSup || ruling.Kind != kindRulingChange {
		t.Fatalf("kinds %q %q", saved.Kind, ruling.Kind)
	}
	for _, it := range []reviewItem{saved, ruling} {
		for _, m := range it.Allowed {
			if m == "left" || m == "query" {
				t.Fatalf("%s offers %q, which patches the generated fact: %v", it.A.Name, m, it.Allowed)
			}
		}
		if strings.Join(it.Allowed, ",") != "both,right" {
			t.Fatalf("%s allowed %v", it.A.Name, it.Allowed)
		}
	}
	resultFile := "derived/bench/" + saved.B.Name + ".md"
	before := r.serverFile(resultFile)
	if _, err := rs.decide(ctx, saved.ID, "left", false); err == nil {
		t.Fatal("left on a generated fact must be refused")
	}
	if _, err := rs.decide(ctx, saved.ID, "query", false); err == nil {
		t.Fatal("query on a generated fact must be refused")
	}
	if _, err := rs.decide(ctx, saved.ID, "right", false); err != nil {
		t.Fatal(err)
	}
	if r.serverFile(resultFile) != before {
		t.Fatal("the result fact was patched by a mark")
	}
	if !strings.Contains(r.serverFile("projects/bench/bench-q9-plain-speed.md"), "superseded_by: "+saved.B.Name) {
		t.Fatal("the saved fact is not retired by the result")
	}
	// the next run keeps the answer: the saved fact is superseded, the result fact stays
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.serverFile("projects/bench/bench-q9-plain-speed.md"), "superseded_by: "+saved.B.Name) {
		t.Fatal("the next run lost the owner's answer")
	}
	r.assertServerLintsClean()
}

// When the linter or the server refuses a repo, its findings are not pushed
// pointing at result facts that do not exist, and the next healthy run raises
// them.
func TestResultsFindingsOfARepoThatDidNotWriteAreNotPushed(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	real := lintRun
	defer func() { lintRun = real }()
	lintRun = func(dir string) ([]lint.Finding, error) {
		fs, err := real(dir)
		if _, serr := os.Stat(filepath.Join(dir, "derived", "bench")); serr == nil {
			fs = append(fs, lint.Finding{Rule: "L9", Path: "derived/bench/" + results.FactName("bench", "results.jsonl", "toks") + ".md", Msg: "test"})
		}
		return fs, err
	}
	res, err := resultsStep(ctx, r.s, r.resultsOpts(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Count(derive.Failed) != 1 {
		t.Fatalf("report %+v", res.Report.Repos)
	}
	for _, f := range r.serverFiles() {
		if (strings.HasPrefix(f, "review/") && strings.HasSuffix(f, ".md")) || strings.HasPrefix(f, "derived/bench/bench-results-") {
			t.Fatalf("%s was pushed for a repo that failed", f)
		}
	}
	r.assertClean("after the linter refused")
	lintRun = real
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.serverFile("review/2026-10-10-results-bench.md"), "may replace [[bench-q9-plain-speed]]") {
		t.Fatal("the healthy run did not raise the findings")
	}
	r.assertServerLintsClean()
}

// Real scrump on shapes that are not tokens: a heading with "password: x", and
// one where the cut to 90 characters falls inside the secret. Neither the fact
// body, the name, the applies_to path nor the review lines carry it.
func TestResultsRedactionCoversWhatIsStrippedAndCut(t *testing.T) {
	requireScrump(t)
	r := newRig(t, rigOpts{repos: []string{"bench"}})
	repo := filepath.Join(r.projects, "bench")
	doc := func(v string) string {
		return "# Notes\n\n## Deploy box password: hunter2hunter2xyz\n\n| Row | Ours |\n|---|---:|\n| alpha-1 | " + v + " r/s |\n| beta-2 | 5,000 r/s |\n\n" +
			"## " + strings.Repeat("a", 74) + " password: hunter3hunter3xyz\n\n| Row | Ours |\n|---|---:|\n| gamma-3 | " + v + " r/s |\n| delta-4 | 5,000 r/s |\n"
	}
	commitResults(t, repo, "2026-09-01T12:00:00Z", "first", map[string]string{"docs/NOTES.md": doc("1,000")})
	commitResults(t, repo, "2026-09-10T12:00:00Z", "second", map[string]string{"docs/NOTES.md": doc("1,200")})
	o := r.resultsOpts()
	o.Red = digest.RedactAll
	if _, err := resultsStep(ctxMinute(t), r.s, o, false); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range r.serverFiles() {
		if !strings.HasPrefix(f, "derived/bench/bench-results-") {
			continue
		}
		n++
		all := f + "\n" + r.serverFile(f)
		for _, secret := range []string{"hunter2", "hunter3", "hunte"} {
			if strings.Contains(all, secret) {
				t.Fatalf("%q reached %s:\n%s", secret, f, all)
			}
		}
	}
	if n == 0 {
		t.Fatal("no result fact written")
	}
}

// 24 facts a repo and 20 rows a fact, the cuts the SPEC states.
func TestResultsCapsFactsPerRepoAndRowsPerFact(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"bench"}})
	repo := filepath.Join(r.projects, "bench")
	files1, files2 := map[string]string{}, map[string]string{}
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("logs/f%02d.jsonl", i)
		row := func(day string, v int) string {
			return fmt.Sprintf(`{"ts":"%sT10:00:00Z","cell":"cell-%02d","toks":%d.5}`+"\n", day, i, v)
		}
		files1[name] = row("2026-08-30", 100+i)
		files2[name] = row("2026-08-30", 100+i) + row("2026-09-09", 150+i)
	}
	tbl := func(base int) string {
		var b strings.Builder
		b.WriteString("# Big\n\n## Wide table\n\n| Row | Ours |\n|---|---:|\n")
		for i := 0; i < 25; i++ {
			fmt.Fprintf(&b, "| row-%02d-x | %d r/s |\n", i, base+i)
		}
		return b.String()
	}
	files1["docs/BIG.md"], files2["docs/BIG.md"] = tbl(1000), tbl(2000)
	commitResults(t, repo, "2026-09-01T12:00:00Z", "first", files1)
	commitResults(t, repo, "2026-09-10T12:00:00Z", "second", files2)
	if _, err := resultsStep(ctxMinute(t), r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	var facts []string
	for _, f := range r.serverFiles() {
		if strings.HasPrefix(f, "derived/bench/bench-results-") {
			facts = append(facts, f)
		}
	}
	if len(facts) != results.MaxFactsPerRepo {
		t.Fatalf("%d result facts, the cap is %d", len(facts), results.MaxFactsPerRepo)
	}
	// the 25-row table has the most changed rows, so it is among the 24
	found := false
	for _, f := range facts {
		body := r.serverFile(f)
		if !strings.Contains(body, "row-00-x") {
			continue
		}
		found = true
		if rows := strings.Count(body, "\n- row-"); rows != results.MaxRowsPerFact {
			t.Fatalf("%d rows shown, the cap is %d:\n%s", rows, results.MaxRowsPerFact, body)
		}
		if !strings.Contains(body, "5 more rows of this group are not shown") {
			t.Fatalf("the cut is not said:\n%s", body)
		}
	}
	if !found {
		t.Fatal("the table fact is missing")
	}
	r.assertServerLintsClean()
}

// The host that produced a repo's result facts is the only one that rewrites
// them; --take-over claims them.
func TestResultsForeignHostIsSkippedUntilTakenOver(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	// the facts were produced elsewhere
	for _, f := range r.resultFacts() {
		p := filepath.Join(r.clone, f)
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), "host: host-a", "host: other", 1)), 0o644)
	}
	r.commitIndex(r.clone, "produced on another host")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	head := r.serverHead()
	// a newer result: this host must not rewrite the other host's facts
	repo := filepath.Join(r.projects, "bench")
	prev, _ := os.ReadFile(filepath.Join(repo, "results.jsonl"))
	commitResults(t, repo, "2026-09-21T12:00:00Z", "newer", map[string]string{"results.jsonl": string(prev) + logRows("2026-09-20", "ccc3333", map[string]string{"q9-plain-short": "180.0", "q9-plain-long": "120.5", "q9-spec-short": "301.2"})})
	res, err := resultsStep(ctx, r.s, r.resultsOpts(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Count(derive.Skipped) != 1 || r.serverHead() != head {
		t.Fatalf("a foreign host's facts were touched: %+v", res.Report.Repos)
	}
	o := r.resultsOpts()
	o.TakeOver = true
	if _, err := resultsStep(ctx, r.s, o, false); err != nil {
		t.Fatal(err)
	}
	if body := r.resultFactWith("180.0 tok/s"); !strings.Contains(body, "host: host-a") {
		t.Fatalf("--take-over did not claim the facts:\n%s", body)
	}
}

// tacit curate runs the results pass after the derived facts.
func TestCurateRunsTheResultsPass(t *testing.T) {
	stubScrump(t)
	r := resultsRig(t, nil)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("TACIT_PROJECTS", r.projects)
	t.Setenv("TACIT_DERIVE_CONFIG", filepath.Join(r.root, "derive.yaml"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(r.root, "cache"))
	t.Setenv("HOME", r.root)
	if code := runCurate([]string{"--max-batches", "1"}); code != 0 {
		t.Fatalf("curate exited %d", code)
	}
	if len(r.resultFacts()) == 0 {
		t.Fatal("curate wrote no result facts")
	}
	hot := false
	for _, f := range r.serverFiles() {
		hot = hot || f == "derived/bench/bench-hot-spots.md"
	}
	if !hot {
		t.Fatal("curate wrote no derived facts either")
	}
	// os.UserCacheDir honours XDG_CACHE_HOME on Linux and uses $HOME/Library/Caches on macOS.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "tacit", "results")); err != nil {
		t.Fatalf("the series cache was not written: %v", err)
	}
}

// A prompt that names files still gets the hot-spot fact, not a result fact;
// the facts of groups nobody quotes (result_ref) are never printed by the hook
// but search finds them.
func TestHookPrefersHotSpotsAndNeverPrintsUnquotedResults(t *testing.T) {
	r := resultsRig(t, nil)
	repo := filepath.Join(r.projects, "bench")
	other := "# Other\n\n## Unquoted table\n\n| Case | Ours |\n|---|---:|\n| omega-9 | %s r/s |\n| sigma-8 | 4,100 r/s |\n"
	commitResults(t, repo, "2026-09-11T12:00:00Z", "other table", map[string]string{"docs/OTHER.md": fmt.Sprintf(other, "1,100")})
	commitResults(t, repo, "2026-09-12T12:00:00Z", "other table, newer", map[string]string{"docs/OTHER.md": fmt.Sprintf(other, "1,300")})
	ctx := ctxMinute(t)
	if _, err := r.step(ctx); err != nil { // hot spots and the module map
		t.Fatal(err)
	}
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, f := range r.resultFacts() {
		if strings.Contains(r.serverFile(f), "type: result_ref") {
			ref = strings.TrimSuffix(filepath.Base(f), ".md")
		}
	}
	if ref == "" {
		t.Fatal("the unquoted table should be a result_ref fact")
	}
	head, _ := r.s.Head(ctx)
	ix, err := search.Open(ctx, r.clone, head)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sit := search.Situation{Repo: "bench", Host: "host-a"}
	got, err := promptHits(ctx, ix, sit, "which files are the hot spots in bench and what changes with src/scheduler/worker.go", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	var derivedNames []string
	for _, h := range got {
		if isDerived(h) {
			derivedNames = append(derivedNames, h.Name)
		}
	}
	if len(derivedNames) != 1 || derivedNames[0] != "bench-hot-spots" {
		t.Fatalf("the hot-spot fact must win a file-naming prompt, got %s", names(got))
	}
	// a prompt written to match the unquoted table exactly: the hook does not print it
	q := "omega-9 sigma-8 unquoted table Case Ours r/s"
	hook, _ := promptHits(ctx, ix, sit, q, map[string]bool{})
	for _, h := range hook {
		if h.Name == ref {
			t.Fatalf("the hook printed a result_ref fact: %s", names(hook))
		}
	}
	// search and the MCP tool still find it
	sit.Query, sit.Derived = q, true
	all, err := ix.Query(ctx, sit, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names(all), ref) {
		t.Fatalf("search must still find %s: %s", ref, names(all))
	}
}

// A repo whose HEAD moved for reasons that touch no result gives the same
// facts byte for byte (nothing to push), and a second run reads the series from
// the cache instead of git.
func TestResultsUnchangedRepoCostsNothing(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	g := gitev.New(r.projects)
	o := r.resultsOpts()
	o.Git, o.CacheDir = g, filepath.Join(r.root, "series-cache")
	if _, err := resultsStep(ctx, r.s, o, false); err != nil {
		t.Fatal(err)
	}
	head, runs := r.serverHead(), g.Runs()
	if _, err := resultsStep(ctx, r.s, o, false); err != nil {
		t.Fatal(err)
	}
	if used := g.Runs() - runs; used > 4 {
		t.Fatalf("a second run of an unchanged repo ran %d git commands; it should read the cache", used)
	}
	// HEAD moves, no result changes
	commitResults(t, filepath.Join(r.projects, "bench"), "2026-09-22T12:00:00Z", "docs only", map[string]string{"docs/notes.md": "text\n"})
	if _, err := resultsStep(ctx, r.s, o, false); err != nil {
		t.Fatal(err)
	}
	if r.serverHead() != head {
		t.Fatalf("a commit that touched no result rewrote result facts:\n%s", gitIn(t, r.bare, "diff", "--stat", head, "main"))
	}
}
