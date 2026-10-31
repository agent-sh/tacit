package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

func curatedFact(t *testing.T, root, name, desc, created, origin string) *fact.Fact {
	t.Helper()
	f := &fact.Fact{Path: "machines/host-a/" + name + ".md", Meta: fact.Meta{
		Name: name, Description: desc, Type: "machine_fact", Scope: "machines/host-a",
		Source:  fact.Source{Harness: "curator", Host: "host-a", Kind: "observed"},
		Created: created, Origin: origin,
	}, Body: desc + "\n"}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, f.Path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// conflictFixture is a clone with filler so bm25 scores clear the floor;
// facts adds the facts under test.
func conflictFixture(t *testing.T, facts func(clone string)) *store.Store {
	t.Helper()
	root := t.TempDir()
	clone, _ := newPair(t, root)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	facts(clone)
	for i := 0; i < 40; i++ {
		curatedFact(t, clone, fmt.Sprintf("filler-%d", i), fmt.Sprintf("filler fact %d about the garden hose and watering can", i), "2026-10-01", "")
	}
	return &store.Store{Dir: clone}
}

// flakyIndex fails its first fails queries, then answers from a real index.
type flakyIndex struct {
	real  conflictIndex
	fails *int
}

func (f flakyIndex) Query(ctx context.Context, sit search.Situation, limit int, inbox bool) ([]search.Hit, error) {
	if *f.fails > 0 {
		*f.fails--
		return nil, errors.New("unable to open database file (14)")
	}
	return f.real.Query(ctx, sit, limit, inbox)
}
func (f flakyIndex) Close() error { return f.real.Close() }

func withFlakyIndex(t *testing.T, fails int) (opens *int) {
	t.Helper()
	old := openConflictIndex
	opens = new(int)
	left := fails
	openConflictIndex = func(ctx context.Context, dir, head string) (conflictIndex, error) {
		*opens++
		ix, err := search.Open(ctx, dir, head)
		if err != nil {
			return nil, err
		}
		return flakyIndex{real: ix, fails: &left}, nil
	}
	t.Cleanup(func() { openConflictIndex = old })
	return opens
}

// A query fails on an open index. One failure opens the index again and the
// check goes on; a second stops it with an error, so the curator writes
// nothing (before, it logged the error and wrote the fact unchecked, once
// for every failed query).
func TestConflictPairsReopensOnceThenFailsLoud(t *testing.T) {
	s := conflictFixture(t, func(clone string) {
		curatedFact(t, clone, "old-quokka", "quokka scheduler zebra runs on port 7781 behind the lantern proxy", "2026-10-01", "n0")
	})
	nf := &fact.Fact{Meta: fact.Meta{Name: "new", Scope: "machines/host-a", Description: "quokka scheduler zebra now runs on port 7790 behind the lantern proxy"}}

	opens := withFlakyIndex(t, 1)
	pairs, _, err := conflictPairs(context.Background(), s, []*fact.Fact{nf}, "host-a", true, false, nil)
	if err != nil || len(pairs) != 1 || pairs[0].ExistingName != "old-quokka" || *opens != 2 {
		t.Fatalf("one failed query: pairs %v, err %v, opens %d", pairs, err, *opens)
	}

	withFlakyIndex(t, 2)
	if _, _, err := conflictPairs(context.Background(), s, []*fact.Fact{nf}, "host-a", true, false, nil); err == nil {
		t.Fatal("a query that fails again after reopening returned no error")
	}
}

// --recheck-conflicts pairs each of the day's facts with its closest older
// fact: never itself, never a newer one even when that one ranks first, and a
// "supersedes" against a same-day sibling is a review line, never a
// retirement. Dry writes nothing.
func TestRecheckConflictsPairsTodaysFactsWithOlderOnes(t *testing.T) {
	s := conflictFixture(t, func(clone string) {
		curatedFact(t, clone, "old-quokka", "quokka scheduler zebra listens on port 7781 behind the lantern proxy", "2026-10-01", "n0")
		curatedFact(t, clone, "today-quokka", "quokka scheduler zebra now runs on port 7790 behind the lantern proxy", "2026-10-11", "n1")
		// Closer to today's fact than the old one is, and newer.
		curatedFact(t, clone, "later-quokka", "quokka scheduler zebra now runs on port 7790 behind the lantern proxy again", "2026-10-12", "n2")
		// Same day, its own subject, close to nothing else.
		curatedFact(t, clone, "today-heron", "heron uploader pushes parquet shards to the walrus bucket nightly", "2026-10-11", "n3")
		curatedFact(t, clone, "today-heron-twin", "heron uploader pushes parquet shards to the walrus bucket every night", "2026-10-11", "n4")
	})
	var prompts []string
	old := judgeRun
	judgeRun = func(ctx context.Context, model, prompt string) (string, float64, error) {
		prompts = append(prompts, prompt)
		return `["supersedes", "supersedes"]`, 0.01, nil
	}
	defer func() { judgeRun = old }()

	out := captureStdout(t, func() {
		if code := recheckConflicts(context.Background(), s, "host-a", "2026-10-11", "2026-10-11", 0, true); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	if len(prompts) != 1 {
		t.Fatalf("judge calls %d, want 1", len(prompts))
	}
	p := prompts[0]
	if !strings.Contains(p, "EXISTING (old-quokka)") || !strings.Contains(p, "NEW (today-quokka)") || strings.Contains(p, "later-quokka") {
		t.Fatalf("prompt pairs:\n%s", p)
	}
	if strings.Count(p, "EXISTING (") != 2 || !strings.Contains(p, "today-heron-twin") {
		t.Fatalf("want the quokka pair and one heron pair (not both directions):\n%s", p)
	}
	if !strings.Contains(out, "[[today-quokka]]") || !strings.Contains(out, "duplicate in one run: [[today-heron") {
		t.Fatalf("output:\n%s", out)
	}
	if strings.Contains(out, "supersedes: [[today-heron") {
		t.Fatalf("a same-day sibling was routed to retirement:\n%s", out)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, "machines/host-a/old-quokka.md"))
	if strings.Contains(string(b), "superseded_by") {
		t.Fatal("dry run retired a fact")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "review")); err == nil {
		t.Fatal("dry run wrote review/")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func TestConflictPairsUseFactHostAcrossMachines(t *testing.T) {
	s := conflictFixture(t, func(clone string) {
		curatedFact(t, clone, "old-quokka", "quokka scheduler zebra listens on port 7781 behind the lantern proxy", "2026-10-01", "n0")
	})
	nf := &fact.Fact{Meta: fact.Meta{Name: "new", Scope: "machines/host-a", Description: "quokka scheduler zebra now runs on port 7790 behind the lantern proxy"}}
	pairs, _, err := conflictPairs(context.Background(), s, []*fact.Fact{nf}, "host-b", false, false, nil)
	if err != nil || len(pairs) != 1 || pairs[0].ExistingName != "old-quokka" {
		t.Fatalf("remote machine fact was not checked: %v %v", pairs, err)
	}
}
