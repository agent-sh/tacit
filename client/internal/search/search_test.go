package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, name, scope, desc, body, extra string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	content := "---\nname: " + name + "\ndescription: " + desc + "\ntype: machine_fact\nscope: " + scope + "\nsource:\n  harness: curator\n  host: host-a\n  kind: observed\ncreated: 2026-10-08\n" + extra + "---\n\n" + body + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQueryScopesWeightsAndSupersede(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "machines/host-a/findproc-loose-match.md", "findproc-loose-match", "machines/host-a", "zapproc -f and findproc -f can match the calling shell and exit 137; use procwait instead", "Long body about waiting.", "")
	write(t, root, "projects/api/prepush-node-path.md", "prepush-node-path", "projects/api", "api pre-push runs eslint; prefix PATH=$HOME/.local/node/bin", "body", "")
	write(t, root, "projects/other/noise.md", "noise", "projects/other", "zapproc findproc procwait shell exit", "zapproc findproc procwait", "")
	write(t, root, "machines/host-a/old.md", "old", "machines/host-a", "findproc old advice superseded", "findproc", "superseded_by: findproc-loose-match\n")
	write(t, root, "machines/host-a/outdated.md", "outdated", "machines/host-a", "findproc outdated advice retired by a staleness verdict", "findproc", "retired: 2026-10-09\n")
	write(t, root, "inbox/claude@host-a/raw.md", "raw", "machines/host-a", "findproc raw note from a session", "findproc zapproc", "")
	write(t, root, "user/body-only.md", "body-only", "user", "unrelated description", "zapproc findproc zapproc findproc zapproc findproc procwait shell exit 137", "")

	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ctx := context.Background()
	ix, err := Open(ctx, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	hits, err := ix.Query(ctx, Situation{Repo: "atlas", Host: "host-a", Command: "zapproc -f 'check.py'"}, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.Name
	}
	if len(hits) == 0 || hits[0].Name != "findproc-loose-match" {
		t.Fatalf("expected the host-a fact first, got %v", names)
	}
	for _, n := range names {
		if n == "noise" || n == "old" || n == "raw" || n == "outdated" {
			t.Fatalf("scope, supersede or inbox filter failed: %v", names)
		}
	}
	// description outranks body-only matches
	if idx(names, "body-only") >= 0 && idx(names, "body-only") < idx(names, "findproc-loose-match") {
		t.Fatalf("body-only outranked description match: %v", names)
	}

	hits, _ = ix.Query(ctx, Situation{Repo: "atlas", Host: "host-a", Query: "findproc"}, 5, true)
	found := false
	for _, h := range hits {
		if h.Name == "raw" && h.Inbox {
			found = true
		}
	}
	if !found {
		t.Fatalf("inbox fact not included when asked: %v", hits)
	}
	block := Block(hits, "# Facts")
	if !strings.Contains(block, "raw (unreviewed)") || strings.Count(block, "\n") != len(hits)+1 {
		t.Fatalf("block:\n%s", block)
	}

	// Index is reused for the same sha and rebuilt for a new one.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "index", fmt.Sprintf("index-v%d-abc123.db", schemaVersion))); err != nil {
		t.Fatal(err)
	}
	ix2, err := Open(ctx, root, "def456")
	if err != nil {
		t.Fatal(err)
	}
	ix2.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "index", fmt.Sprintf("index-v%d-abc123.db", schemaVersion))); err == nil {
		t.Fatal("old index not pruned")
	}
}

// Derived facts are in scope only when the situation asks for them, and only
// for its own repo.
func TestDerivedScopeOnlyWhenAsked(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "projects/atlas/worker-note.md", "worker-note", "projects/atlas", "the atlas worker restarts on a config change", "body", "")
	write(t, root, "derived/atlas/atlas-hot-spots.md", "atlas-hot-spots", "derived/atlas", "Hot spots in atlas, crates/atlas-server/src/worker.rs 35", "worker.rs changes with lib.rs", "derived_from: abc123\n")
	write(t, root, "derived/other/other-hot-spots.md", "other-hot-spots", "derived/other", "Hot spots in other, worker.rs 35", "worker.rs", "derived_from: abc123\n")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ctx := context.Background()
	ix, err := Open(ctx, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	names := func(sit Situation) string {
		hits, err := ix.Query(ctx, sit, 8, false)
		if err != nil {
			t.Fatal(err)
		}
		var ns []string
		for _, h := range hits {
			ns = append(ns, h.Name)
		}
		return strings.Join(ns, ",")
	}
	base := Situation{Repo: "atlas", Host: "host-a", Query: "worker rs"}
	if got := names(base); got != "worker-note" {
		t.Fatalf("default scope must not include derived facts: %s", got)
	}
	both := base
	both.Derived = true
	got := names(both)
	if !strings.Contains(got, "worker-note") || !strings.Contains(got, "atlas-hot-spots") || strings.Contains(got, "other-hot-spots") {
		t.Fatalf("Derived scope: %s", got)
	}
	only := base
	only.OnlyDerived = true
	if got := names(only); got != "atlas-hot-spots" {
		t.Fatalf("OnlyDerived: %s", got)
	}
	only.Repo = ""
	if got := names(only); got != "" {
		t.Fatalf("OnlyDerived without a repo must return nothing: %s", got)
	}
}

func TestTerms(t *testing.T) {
	got := Terms("Run ./job.sh in the background and wait for result.txt; don't use zapproc -f")
	want := []string{"run", "job", "background", "wait", "result", "txt", "zapproc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("terms %v", got)
	}
}

func idx(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// The owner's search reaches every fact (inbox, retired, superseded, any
// scope), matches a half-typed word, and needs all words before any one.
func TestRankSearchesEverythingWithPrefixes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "machines/host-a/findproc-loose-match.md", "findproc-loose-match", "machines/host-a", "zapproc -f and findproc -f match the shell; use procwait", "body", "")
	write(t, root, "projects/other/noise.md", "noise", "projects/other", "procwait alone", "nothing else", "")
	write(t, root, "machines/host-a/outdated.md", "outdated", "machines/host-a", "findproc advice retired", "findproc", "retired: 2026-10-09\n")
	write(t, root, "inbox/claude@host-a/raw.md", "raw", "machines/host-a", "findproc raw note", "zapproc", "")
	write(t, root, "projects/other/freeze-note.md", "freeze-note", "projects/other", "the deployment waits for review", "x", "")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ctx := context.Background()
	ix, err := Open(ctx, root, "rank1")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	paths := func(rs []Ranked) string {
		var out []string
		for _, r := range rs {
			out = append(out, filepath.Base(r.Path))
		}
		return strings.Join(out, ",")
	}
	rs, err := ix.Rank(ctx, "findpr", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"findproc-loose-match.md", "outdated.md", "raw.md"} {
		if !strings.Contains(paths(rs), want) {
			t.Fatalf("prefix search lacks %s: %s", want, paths(rs))
		}
	}
	// The prompt index stems; a half-typed word still finds its fact here
	// ("deploy" stems to "deploi", "deployment" to "deploy").
	if rs, _ = ix.Rank(ctx, "deploy", 10, nil); paths(rs) != "freeze-note.md" {
		t.Fatalf("prefix of a stemmed word: %s", paths(rs))
	}
	rs, _ = ix.Rank(ctx, "findproc procwait", 10, nil)
	if got := paths(rs); got != "findproc-loose-match.md" {
		t.Fatalf("all words first: %s", got)
	}
	rs, _ = ix.Rank(ctx, "procwait zebra", 10, nil)
	if got := paths(rs); !strings.Contains(got, "noise.md") || !strings.Contains(got, "findproc-loose-match.md") {
		t.Fatalf("any word when no fact has all: %s", got)
	}
	// Only an inbox note has both words. A caller that keeps curated facts
	// gets the curated facts with either word, not nothing.
	write(t, root, "projects/other/only-zapproc.md", "only-zapproc", "projects/other", "zapproc alone here", "x", "")
	ix2, err := Open(ctx, root, "rank2")
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	if rs, _ = ix2.Rank(ctx, "raw zapproc", 10, nil); paths(rs) != "raw.md" {
		t.Fatalf("both words, no filter: %s", paths(rs))
	}
	curated := func(p string) bool { return !strings.HasPrefix(p, "inbox/") }
	rs, _ = ix2.Rank(ctx, "raw zapproc", 10, curated)
	if got := paths(rs); !strings.Contains(got, "only-zapproc.md") || strings.Contains(got, "raw.md") {
		t.Fatalf("the filter must decide the fallback: %s", got)
	}
}

// A filter that drops the top hits does not under-fill the result: Rank pages
// past its first page until it has limit rows the caller keeps.
func TestRankPagesPastTheLimitForTheFilter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	for i := 0; i < 600; i++ {
		n := fmt.Sprintf("note-%03d", i)
		write(t, root, "inbox/claude@host-a/"+n+".md", n, "machines/host-a", "findproc findproc findproc note", "findproc findproc", "")
	}
	write(t, root, "machines/host-a/curated-one.md", "curated-one", "machines/host-a", "a long curated fact that mentions findproc once among many other words here", "body", "")
	write(t, root, "machines/host-a/curated-two.md", "curated-two", "machines/host-a", "another curated fact with findproc and plenty of other words to lower its score", "body", "")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ctx := context.Background()
	ix, err := Open(ctx, root, "rank3")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	curated := func(p string) bool { return !strings.HasPrefix(p, "inbox/") }
	rs, err := ix.Rank(ctx, "findproc", 2, curated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d curated rows, want 2: %v", len(rs), rs)
	}
	all, _ := ix.Rank(ctx, "findproc", 2000, nil)
	if len(all) != 602 {
		t.Fatalf("unfiltered: %d rows", len(all))
	}
}
