package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

func TestBackoffDelayDoublesToTheCap(t *testing.T) {
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Fatalf("after %d failed run(s): %s, want %s", i+1, got, w)
		}
	}
}

// fakeClock sets the guard's clock; the returned func moves it.
func fakeClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	prev := backoffNow
	backoffNow = func() time.Time { return now }
	t.Cleanup(func() { backoffNow = prev })
	return func(d time.Duration) { now = now.Add(d) }
}

// fakeProbe makes the probe answer with err() and counts the calls.
func fakeProbe(t *testing.T, err func() error) *int {
	t.Helper()
	n := 0
	prev := probeRoute
	probeRoute = func(context.Context, string, string) (float64, error) { n++; return .001, err() }
	t.Cleanup(func() { probeRoute = prev })
	return &n
}

// A failed run schedules the next try; a run before it pays for nothing and
// sends no probe; a scheduled run does not wait but probes; a failed probe
// doubles the wait; a passed probe lets the run pay, and a run that completes
// ends the backoff.
func TestRunGuardSchedulesProbesAndResets(t *testing.T) {
	advance := fakeClock(t)
	down := true
	probes := fakeProbe(t, func() error {
		if down {
			return modelrun.ProviderFailure(503, "", "unavailable")
		}
		return nil
	})
	path := filepath.Join(t.TempDir(), "curate-backoff.json")
	ctx := context.Background()

	g := loadGuard("curate", path)
	if !g.admit(ctx, false) || *probes != 0 {
		t.Fatal("a guard with no failure refused or probed")
	}
	g.failed("judge", "opus", modelrun.ProviderFailure(503, "", "x"))
	g.finish(runOutcome{Attempted: 1})
	st := loadGuard("curate", path).st
	if st.Failures != 1 || st.Role != "judge" || st.Reason != "judge: capacity" || !st.Next.Equal(backoffNow().Add(30*time.Minute)) {
		t.Fatalf("after one failed run: %+v", st)
	}

	advance(10 * time.Minute)
	g = loadGuard("curate", path)
	if g.admit(ctx, false) || *probes != 0 {
		t.Fatal("a run before the next try was admitted or probed")
	}
	g.finish(runOutcome{})
	if st := loadGuard("curate", path).st; st.Skipped != 1 || st.Failures != 1 {
		t.Fatalf("skipped run: %+v", st)
	}

	// The daily run does not wait; its probe fails: the wait doubles.
	g = loadGuard("curate", path)
	if g.admit(ctx, true) || *probes != 1 {
		t.Fatalf("scheduled run with the route down: probes %d", *probes)
	}
	g.finish(runOutcome{})
	if st := loadGuard("curate", path).st; st.Failures != 2 || !st.Next.Equal(backoffNow().Add(time.Hour)) || st.Reason != "judge: probe: capacity" {
		t.Fatalf("after a failed probe: %+v", st)
	}

	advance(time.Hour)
	down = false
	g = loadGuard("curate", path)
	if !g.admit(ctx, false) || *probes != 2 {
		t.Fatal("the route answers its probe but the run was refused")
	}
	g.finish(runOutcome{Attempted: 1, Done: 1})
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a completed run left the backoff in place")
	}
}

// Within a run: after a route failure the next batch pays only once a probe
// passes, and a failed probe stops the run, which counts as failed even when
// an earlier batch completed.
func TestRunGuardStopsTheRunWhenTheRouteStaysDown(t *testing.T) {
	fakeClock(t)
	down := true
	probes := fakeProbe(t, func() error {
		if down {
			return modelrun.ProviderFailure(529, "", "overloaded")
		}
		return nil
	})
	path := filepath.Join(t.TempDir(), "b.json")
	ctx := context.Background()
	g := loadGuard("curate", path)
	if !g.next(ctx) || *probes != 0 {
		t.Fatal("next probed with no failure")
	}
	g.failed("curate", "opus", modelrun.ProviderFailure(529, "", "x"))
	down = false
	if !g.next(ctx) || *probes != 1 {
		t.Fatal("a passed probe did not let the run go on")
	}
	g.failed("curate", "opus", modelrun.ProviderFailure(529, "", "x"))
	down = true
	if g.next(ctx) || *probes != 2 {
		t.Fatal("a failed probe did not stop the run")
	}
	if g.next(ctx) || *probes != 2 {
		t.Fatal("a stopped run probed again")
	}
	g.finish(runOutcome{Attempted: 3, Done: 1})
	if st := loadGuard("curate", path).st; st.Failures != 1 || st.Role != "curate" {
		t.Fatalf("a run stopped by its route: %+v", st)
	}

	// Lone notes' invalid answers and nothing pending are no failure.
	path2 := filepath.Join(t.TempDir(), "c.json")
	g = loadGuard("curate", path2)
	g.finish(runOutcome{Attempted: 1})
	g.finish(runOutcome{})
	if _, err := os.Stat(path2); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a run with no failure of its own recorded a backoff")
	}
}

func TestRouteFailureIsNotAnInvalidAnswer(t *testing.T) {
	ctx := context.Background()
	if routeFailure(ctx, nil) || routeFailure(ctx, modelrun.ErrInvalidResult) || routeFailure(ctx, &curate.InvalidEntries{}) {
		t.Fatal("an invalid answer counted as the route's")
	}
	if !routeFailure(ctx, modelrun.ProviderFailure(401, "", "x")) || !routeFailure(ctx, errors.New("runner missing")) {
		t.Fatal("a route failure was not counted")
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if routeFailure(done, context.Canceled) {
		t.Fatal("the run's own deadline counted as the route's")
	}
}

func TestSplitCacheKeepsAnswersPerNote(t *testing.T) {
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "splits.json")
	note := func(name, desc string) *fact.Fact {
		return &fact.Fact{Path: "inbox/dreamer@h/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Scope: "system"}, Body: desc}
	}
	a, b := note("a", "alpha uses port 1"), note("b", "beta uses port 2")
	c := loadSplitCache(path, now)
	if c.has([]*fact.Fact{a}) {
		t.Fatal("empty cache has a note")
	}
	c.putSplit([]*fact.Fact{a, b}, []curate.Entry{{Origin: "a", Description: "alpha uses port 1"}}, "opus", now)
	c = loadSplitCache(path, now)
	if !c.has([]*fact.Fact{a, b}) {
		t.Fatal("the saved answer was not read back")
	}
	es, _, done := c.split([]*fact.Fact{a, b})
	if len(es) != 1 || es[0].Origin != "a" || done {
		t.Fatalf("split %v rulings done %v", es, done)
	}
	c.putRulings([]*fact.Fact{a, b}, nil)
	if _, _, done := c.split([]*fact.Fact{a, b}); !done {
		t.Fatal("the rulings pass's answer was not kept")
	}
	// A changed note is split again; the batches put cached notes first.
	b2 := note("b", "beta uses port 3")
	if c.has([]*fact.Fact{a, b2}) {
		t.Fatal("a changed note kept its old answer")
	}
	bs, n := c.batches([]*fact.Fact{b2, a})
	if n != 1 || len(bs) != 2 || bs[0][0].Meta.Name != "a" {
		t.Fatalf("batches %v, %d cached", bs, n)
	}
	// An answer the checks refuse now is dropped.
	refuse := func([]curate.Entry) []curate.Problem { return []curate.Problem{{Entry: 1, Field: "scope", Rule: "x"}} }
	if _, _, _, ok := cachedSplitFor(c, []*fact.Fact{a}, refuse); ok || c.has([]*fact.Fact{a}) {
		t.Fatal("a refused cached answer was used or kept")
	}
	// Old answers expire.
	c.putSplit([]*fact.Fact{a}, nil, "opus", now)
	if loadSplitCache(path, now.AddDate(0, 0, splitCacheDays+1)).has([]*fact.Fact{a}) {
		t.Fatal("an answer past splitCacheDays was kept")
	}
}

// failingCLI replaces quickHome's stand-in CLI with one that fails every call
// with a provider error while the file down exists, and logs every prompt.
func failingCLI(t *testing.T, home string) (down string) {
	t.Helper()
	down = filepath.Join(home, "route-down")
	t.Setenv("FIXTURE_DOWN", down)
	fake := filepath.Join(home, "fake-cli-down")
	script := `#!/bin/sh
in=$(cat)
printf '%s\n\036\n' "$in" >> "$FIXTURE_PROMPTS"
if [ -e "$FIXTURE_DOWN" ]; then
  echo 'API Error: 503 Service Unavailable' >&2
  exit 1
fi
case "$in" in
*"connectivity check"*) printf '%s\n' '{"result":"OK","total_cost_usd":0.001}' ;;
*) printf '%s\n' "$FIXTURE_FRESH" ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	return down
}

// The quick curate on a route that fails: the first run fails and backs off;
// a run before the next try sends nothing; one after it sends only the probe
// while the route is down; once the probe passes the run curates and the
// backoff ends. tacit doctor shows the state.
func TestQuickCurateBacksOffWhileTheRouteFails(t *testing.T) {
	home, prompts := quickHome(t)
	advance := fakeClock(t)
	down := failingCLI(t, home)
	os.WriteFile(down, nil, 0o600)
	statePath := filepath.Join(platform.StateDir(home), "curate-backoff.json")
	quick := []string{"--quick", "--min-interval", "1ns"}

	if code := runCurate(quick); code != 1 {
		t.Fatalf("failing run exit %d", code)
	}
	if n := len(prompts()); n != 1 {
		t.Fatalf("first run: %d prompt(s)", n)
	}
	var st jobBackoff
	if b, err := os.ReadFile(statePath); err != nil || json.Unmarshal(b, &st) != nil || st.Failures != 1 || st.Role != "curate" {
		t.Fatalf("backoff after a failed run: %+v %v", st, err)
	}
	if status, detail := backoffLine(statePath); status != "warn" || !strings.Contains(detail, "1 failed run(s)") {
		t.Fatalf("doctor: %s %s", status, detail)
	}

	// Before the next try: no model call at all.
	advance(10 * time.Minute)
	if code := runCurate(quick); code != 0 || len(prompts()) != 1 {
		t.Fatalf("before the next try: exit %d, %d prompt(s)", code, len(prompts()))
	}
	// After it, the route still down: only the probe.
	advance(30 * time.Minute)
	if code := runCurate(quick); code != 1 {
		t.Fatalf("probe-failed run exit %d", code)
	}
	ps := prompts()
	if len(ps) != 2 || !strings.Contains(ps[1], "connectivity check") {
		t.Fatalf("after the next try: %d prompt(s), last %q", len(ps), ps[len(ps)-1])
	}
	if b, _ := os.ReadFile(statePath); json.Unmarshal(b, &st) != nil || st.Failures != 2 || st.Probes != 1 || st.Skipped != 1 {
		t.Fatalf("after a failed probe: %+v", st)
	}
	// The route is back: probe, split, written; the backoff is gone.
	advance(2 * time.Hour)
	os.Remove(down)
	if code := runCurate(quick); code != 0 {
		t.Fatalf("recovered run exit %d", code)
	}
	ps = prompts()
	if len(ps) != 4 || !strings.Contains(ps[2], "connectivity check") || !strings.Contains(ps[3], "name=fresh-note") {
		t.Fatalf("recovered run prompts: %d", len(ps))
	}
	if _, ok := findFact(filepath.Join(home, "store"), "fresh-note-fact"); !ok {
		t.Fatal("the fresh note's fact was not written after the route came back")
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the backoff outlived a completed run")
	}
}

// A batch whose split was paid for and whose conflict judge then failed is
// not split again: the next run takes the cached answer and pays only the
// judge.
func TestFailedJudgeKeepsThePaidSplit(t *testing.T) {
	home, prompts := quickHome(t)
	advance := fakeClock(t)
	// Two facts of one scope that share most of their terms: the judge is asked.
	two, _ := json.Marshal([]map[string]any{
		{"origin": "fresh-note", "name": "quokka-driver-port", "description": "The quokka fixture driver uses port 7790 on the build host", "type": "machine_fact", "scope": "system", "kind": "observed", "body": "The quokka fixture driver uses port 7790 on the build host."},
		{"origin": "fresh-note", "name": "quokka-driver-config", "description": "The quokka fixture driver port 7790 is set in quokka.toml on the build host", "type": "machine_fact", "scope": "system", "kind": "observed", "body": "The quokka fixture driver port 7790 is set in quokka.toml on the build host."},
	})
	r, _ := json.Marshal(map[string]any{"result": string(two), "total_cost_usd": .5})
	t.Setenv("FIXTURE_FRESH", string(r))
	judgeDown := true
	judged := 0
	prev := judgeRun
	t.Cleanup(func() { judgeRun = prev })
	probed := false
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		judged++
		if strings.Contains(prompt, "connectivity check") {
			probed = !judgeDown
			if judgeDown {
				return "", 0, modelrun.ProviderFailure(503, "", "unavailable")
			}
			return "OK", .001, nil
		}
		if judgeDown {
			return "", 0, modelrun.ProviderFailure(503, "", "unavailable")
		}
		n := strings.Count(prompt, "NEW:")
		vs := make([]string, max(n, 1))
		for i := range vs {
			vs[i] = "no"
		}
		b, _ := json.Marshal(vs)
		return string(b), .01, nil
	}
	quick := []string{"--quick", "--min-interval", "1ns"}
	if code := runCurate(quick); code != 1 {
		t.Fatalf("judge-failed run exit %d", code)
	}
	if judged == 0 {
		t.Skip("the fixture's facts made no conflict pair")
	}
	if n := len(prompts()); n != 1 {
		t.Fatalf("first run: %d split prompt(s)", n)
	}
	cache := loadSplitCache(filepath.Join(platform.StateDir(home), "curate-splits.json"), time.Now())
	if len(cache.Notes["fresh-note"].Entries) != 2 {
		t.Fatalf("the paid split was not kept: %+v", cache.Notes)
	}
	advance(31 * time.Minute)
	judgeDown = false
	if code := runCurate(quick); code != 0 {
		t.Fatalf("recovered run exit %d", code)
	}
	// No second split; the probe went through the judge's call, as the
	// judge does.
	if ps := prompts(); len(ps) != 1 {
		t.Fatalf("recovered run: %d split prompt(s)", len(ps))
	}
	if !probed {
		t.Fatal("the judge route was not probed before the run paid")
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	if _, ok := findFact(s.Dir, "quokka-driver-port"); !ok {
		t.Fatal("the cached split's fact was not written")
	}
	var rep curate.Report
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(home), "curator.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if json.Unmarshal([]byte(lines[len(lines)-1]), &rep) != nil || rep.SplitsReused != 1 || rep.Probes != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(platform.StateDir(home), "curate-splits.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a written batch's answer stayed in the cache")
	}
}

// The dreamer on a failing route: the first window fails, the probe fails,
// the other windows are not paid for and their cursors stay; an idle run
// before the next try sends nothing; the full run probes, and once the route
// answers it dreams every window and ends the backoff.
func TestDreamBacksOffWhileTheRouteFails(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", filepath.Join(root, "store"))
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	advance := fakeClock(t)
	down := true
	probes := fakeProbe(t, func() error {
		if down {
			return modelrun.ProviderFailure(401, "", "expired")
		}
		return nil
	})
	offsets := map[string]int64{}
	var files []string
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(home, ".claude/projects/demo/"+n+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(`{"type":"user","message":{"content":"Check the fixture controller `+n+` and record what fixes its error."}}`+"\n"+`{"type":"assistant","cwd":"/home/u/projects/demo","message":{"content":[{"type":"text","text":"The fixture driver `+n+` requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
		offsets[p] = 0
		files = append(files, p)
	}
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: offsets})
	old := dreamRun
	t.Cleanup(func() { dreamRun = old })
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		if down {
			return "", 0, 0, modelrun.ProviderFailure(401, "", "expired")
		}
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 1 {
		t.Fatalf("failing dream exit %d", code)
	}
	if calls != 1 || *probes != 1 {
		t.Fatalf("a failing route was paid for %d window(s), %d probe(s)", calls, *probes)
	}
	backoff := filepath.Join(platform.StateDir(home), "dream-backoff.json")
	if status, detail := backoffLine(backoff); status != "warn" || !strings.Contains(detail, "dream: authentication") {
		t.Fatalf("dream backoff: %s %s", status, detail)
	}
	st, _ := dream.LoadState(statePath)
	for _, f := range files {
		if st.Offsets[f] != 0 {
			t.Fatal("a cursor moved past a window that was not dreamed")
		}
	}
	// An idle run before the next try: nothing.
	advance(5 * time.Minute)
	if code := runDream([]string{"--idle", "1ns"}); code != 0 || calls != 1 || *probes != 1 {
		t.Fatalf("idle run in backoff: exit %d, %d call(s), %d probe(s)", code, calls, *probes)
	}
	// The full run probes; the route is back.
	down = false
	if code := runDream(nil); code != 0 {
		t.Fatalf("recovered dream exit %d", code)
	}
	if calls != 4 || *probes != 2 {
		t.Fatalf("recovered dream: %d call(s), %d probe(s)", calls, *probes)
	}
	if _, err := os.Stat(backoff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the dream backoff outlived a completed run")
	}
}
