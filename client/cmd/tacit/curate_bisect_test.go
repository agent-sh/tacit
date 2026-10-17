package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// verdictsFor answers n pairs with "no".
func verdictsFor(n int) string {
	v := make([]string, n)
	for i := range v {
		v[i] = "no"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func judgedPairsFor(t *testing.T, names ...string) ([]curate.Pair, []judgedPair, []*fact.Fact) {
	t.Helper()
	dir := t.TempDir()
	existing := curatedFact(t, dir, "old-wombat", "wombat relay listens on port 6601", "2026-10-01", "note")
	nf := curatedFact(t, dir, "new-wombat", "wombat relay listens on port 6602", "2026-10-14", "note")
	var pairs []curate.Pair
	var refs []judgedPair
	for _, n := range names {
		pairs = append(pairs, curate.Pair{ExistingName: existing.Meta.Name, ExistingDesc: existing.Meta.Description, NewName: n, NewDesc: "pair " + n})
		refs = append(refs, judgedPair{newIdx: 0, existing: &search.Hit{Name: existing.Meta.Name, Path: existing.Path, Description: existing.Meta.Description}})
	}
	return pairs, refs, []*fact.Fact{nf}
}

func TestJudgeInvalidChunkIsSplitAndAskedAgain(t *testing.T) {
	pairs, refs, facts := judgedPairsFor(t, "p1", "p2", "p3", "p4")
	previous := judgeRun
	defer func() { judgeRun = previous }()
	var sizes []int
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		n := strings.Count(prompt, "pair p")
		sizes = append(sizes, n)
		if n == 4 {
			return verdictsFor(3), .1, nil // one verdict short
		}
		return verdictsFor(n), .1, nil
	}
	rep := curate.Report{}
	review, written, err := judgePairs(context.Background(), &store.Store{Dir: t.TempDir()}, pairs, refs, facts, false, &rep)
	if err != nil || len(review) != 0 || len(written) != 0 || rep.JudgeUnchecked != 0 || rep.JudgeCalls != 3 {
		t.Fatalf("split retry: %v %v %v %+v sizes %v", review, written, err, rep, sizes)
	}
	if len(sizes) != 3 || sizes[0] != 4 || sizes[1] != 2 || sizes[2] != 2 {
		t.Fatalf("chunk sizes %v, want [4 2 2]", sizes)
	}
}

func TestJudgePairWithNoValidVerdictGoesToReview(t *testing.T) {
	pairs, refs, facts := judgedPairsFor(t, "good", "bad")
	previous := judgeRun
	defer func() { judgeRun = previous }()
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		if strings.Contains(prompt, "pair bad") {
			return `["maybe"]`, .1, nil
		}
		return verdictsFor(strings.Count(prompt, "pair good")), .1, nil
	}
	rep := curate.Report{}
	review, _, err := judgePairs(context.Background(), &store.Store{Dir: t.TempDir()}, pairs, refs, facts, false, &rep)
	if err != nil || rep.JudgeUnchecked != 1 || len(review) != 1 || !strings.Contains(review[0], "CONFLICT CHECK FAILED") {
		t.Fatalf("unjudged pair: %v %v %+v", review, err, rep)
	}
}

// curateFixture sets up a fresh store with the given notes and a fake curator
// CLI that answers by which notes the prompt carries: answer(prompt) picks one.
func curateFixture(t *testing.T, notes map[string]string, answers map[string]string) (home string, s *store.Store) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
	t.Setenv("TACIT_INDEX", filepath.Join(home, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	t.Setenv("TACIT_CURATE_RUNNER", "claude")
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s = &store.Store{Dir: filepath.Join(home, "store")}
	for name, desc := range notes {
		if _, err := addFact(context.Background(), s, "muse", fact.Meta{Name: name, Description: desc, Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed"}}, desc+"."); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture prints the answer whose key names every note it finds in
	// the prompt ("both" when it finds more than one).
	script := "#!/bin/sh\nin=$(cat)\nprintf 'call\\n' >> \"$FIXTURE_CALLS\"\nkey=\"\"\nn=0\n"
	for name := range notes {
		script += "case \"$in\" in *" + name + "*) key=" + name + "; n=$((n+1));; esac\n"
	}
	script += "[ $n -gt 1 ] && key=both\neval \"printf '%s\\\\n' \\\"\\$ANSWER_$(echo $key | tr - _)\\\"\"\n"
	for key, answer := range answers {
		response, _ := json.Marshal(map[string]any{"result": answer, "total_cost_usd": .01})
		t.Setenv("ANSWER_"+strings.ReplaceAll(key, "-", "_"), string(response))
	}
	fake := filepath.Join(home, "fake-cli")
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Setenv("FIXTURE_CALLS", filepath.Join(home, "calls"))
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func entryJSON(origin, name, desc string) string {
	b, _ := json.Marshal([]map[string]string{{"origin": origin, "name": name, "description": desc, "type": "machine_fact", "scope": "system", "kind": "observed", "body": desc + "."}})
	return string(b)
}

func callCount(t *testing.T, home string) int {
	calls, _ := os.ReadFile(filepath.Join(home, "calls"))
	return strings.Count(string(calls), "call")
}

func TestCuratorSplitsABatchWithAnInvalidAnswer(t *testing.T) {
	home, s := curateFixture(t,
		map[string]string{"alpha-note": "The quokka socket driver uses port 7781", "beta-note": "Weekly backups of the marmot cache run on Sundays"},
		map[string]string{
			"both":       "malformed",
			"alpha_note": entryJSON("alpha-note", "quokka-socket-port", "The quokka socket driver uses port 7781"),
			"beta_note":  entryJSON("beta-note", "marmot-backup-day", "Weekly backups of the marmot cache run on Sundays"),
		})
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	all, _, err := fact.Walk(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	curated := 0
	for _, f := range all {
		if !f.InInbox() {
			curated++
		}
	}
	if curated != 2 || callCount(t, home) != 3 {
		t.Fatalf("curated %d facts in %d calls, want 2 in 3", curated, callCount(t, home))
	}
}

func TestCuratorSetsAsideANoteThatFailsAloneRepeatedly(t *testing.T) {
	home, _ := curateFixture(t,
		map[string]string{"gamma-note": "The quokka socket driver uses port 7781"},
		map[string]string{"gamma_note": "malformed"})
	for i := 0; i < maxNoteFailures; i++ {
		if code := runCurate(nil); code != 1 {
			t.Fatalf("run %d exit %d", i+1, code)
		}
	}
	state := loadCuratorState(filepath.Join(home, ".local/state/tacit/curator.json"))
	if state.Failed["gamma-note"] != maxNoteFailures {
		t.Fatalf("failed count %d", state.Failed["gamma-note"])
	}
	runCurate(nil)
	if callCount(t, home) != maxNoteFailures {
		t.Fatalf("set-aside note was sent again: %d calls", callCount(t, home))
	}
}
