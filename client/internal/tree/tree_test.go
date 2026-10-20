package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func mk(path, typ, desc, date string) *fact.Fact {
	dir := filepath.ToSlash(filepath.Dir(path))
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	return &fact.Fact{Path: path, Meta: fact.Meta{Name: name, Description: desc, Type: typ, Scope: dir, Created: date, Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed"}}, Body: desc + "\n", Audited: true}
}

func ruling(f *fact.Fact) *fact.Fact {
	f.Meta.Source.Kind, f.Meta.Source.OwnerQuote, f.Meta.Source.OwnerQuoteFrom, f.QuoteBacked = "user", "always", "claude:s1", true
	return f
}

func sample() []*fact.Fact {
	old := mk("projects/widget/old-port.md", "machine_fact", "The widget server listened on port 7000", "2026-01-01")
	old.Meta.SupersededBy = "widget-port"
	return []*fact.Fact{
		mk("system/release-tags.md", "user_standard", "Release tags are signed", "2026-02-01"),
		ruling(mk("user/past-tense-notes.md", "user_standard", "Release notes are written in the past tense", "2026-03-01")),
		mk("user/likes-tables.md", "user", "The owner reads tables before prose", "2026-03-05"),
		mk("projects/widget/widget-port.md", "machine_fact", "The widget server listens on port 7781", "2026-04-01"),
		mk("projects/widget/widget-flaky-test.md", "past_mistake", "The widget scheduler test is flaky under load; rerun with -count=1", "2026-04-02"),
		old,
		mk("projects/gadget/gadget-build.md", "design_decision", "Gadget builds with make, not the IDE", "2026-04-03"),
		mk("machines/box/box-gpu.md", "machine_fact", "box has four GPUs", "2026-04-04"),
		mk("derived/widget/widget-hot-spots.md", "hot_spot", "Most-changed files of widget", "2026-04-05"),
		{Path: "inbox/claude@box/raw-note.md", Meta: fact.Meta{Name: "raw-note", Description: "an inbox note", Type: "unclassified", Scope: "user"}},
	}
}

func TestGenerateWritesOneIndexPerCuratedFolder(t *testing.T) {
	gen := Generate(sample(), "")
	var paths []string
	for p := range gen {
		paths = append(paths, p)
	}
	want := []string{"_index.md", "derived/_index.md", "derived/widget/_index.md", "machines/_index.md", "machines/box/_index.md", "projects/_index.md", "projects/gadget/_index.md", "projects/widget/_index.md", "system/_index.md", "user/_index.md"}
	if strings.Join(sortStrings(paths), " ") != strings.Join(want, " ") {
		t.Fatalf("index files %v, want %v", sortStrings(paths), want)
	}
	root := gen["_index.md"]
	for _, s := range []string{"# store root", "8 facts (1 owner ruling)", "- [projects/](projects/_index.md) 3 facts", "- [user/](user/_index.md) 2 facts (1 owner ruling)"} {
		if !strings.Contains(root, s) {
			t.Errorf("root index lacks %q:\n%s", s, root)
		}
	}
	if strings.Contains(root, "inbox") {
		t.Errorf("the inbox is in the tree:\n%s", root)
	}
	w := gen["projects/widget/_index.md"]
	for _, s := range []string{"`widget-port` (machine_fact): The widget server listens on port 7781", "`widget-flaky-test` (past_mistake)", "1 superseded or retired fact here is not listed"} {
		if !strings.Contains(w, s) {
			t.Errorf("widget index lacks %q:\n%s", s, w)
		}
	}
	if strings.Contains(w, "old-port") {
		t.Errorf("a superseded fact is listed:\n%s", w)
	}
	u := gen["user/_index.md"]
	if i, j := strings.Index(u, "past-tense-notes"), strings.Index(u, "likes-tables"); i < 0 || j < 0 || i > j || !strings.Contains(u, "(user_standard, owner ruling)") {
		t.Errorf("the ruling is not first and marked:\n%s", u)
	}
	// Generation is a function of the facts.
	if again := Generate(sample(), ""); again["_index.md"] != root || again["user/_index.md"] != u {
		t.Error("two generations differ")
	}
	// An empty store has the root index alone.
	if e := Generate(nil, ""); len(e) != 1 || !strings.Contains(e["_index.md"], "no live facts") {
		t.Errorf("empty store: %v", e)
	}
}

func TestGenerateBoundsAFolder(t *testing.T) {
	var fs []*fact.Fact
	for i := range MaxFacts + 30 {
		fs = append(fs, mk(fmt.Sprintf("projects/big/f-%03d.md", i), "history", fmt.Sprintf("Event %d happened in the big repo", i), fmt.Sprintf("2026-05-%02d", 1+i%28)))
	}
	fs = append(fs, ruling(mk("projects/big/the-rule.md", "user_standard", "Always rebase", "2020-01-01")))
	for i := range MaxFolders + 5 {
		fs = append(fs, mk(fmt.Sprintf("projects/r%03d/x.md", i), "history", "x", "2026-01-01"))
	}
	gen := Generate(fs, "")
	big := gen["projects/big/_index.md"]
	if n := strings.Count(big, "\n- `"); n != MaxFacts {
		t.Errorf("%d fact lines, want %d", n, MaxFacts)
	}
	if !strings.Contains(big, "... and 31 more facts here, not listed") || !strings.Contains(big, "`the-rule` (user_standard, owner ruling)") {
		t.Errorf("truncation or ruling line missing:\n%s", big[:400])
	}
	p := gen["projects/_index.md"]
	if n := strings.Count(p, "\n- ["); n != MaxFolders {
		t.Errorf("%d folder lines, want %d", n, MaxFolders)
	}
	if !strings.Contains(p, fmt.Sprintf("- [big/](big/_index.md) %d facts", MaxFacts+31)) || !strings.Contains(p, "and 6 more folders") {
		t.Errorf("folder list:\n%s", p[:300])
	}
	for rel, c := range gen {
		if len(c) > 24<<10 {
			t.Errorf("%s is %d bytes", rel, len(c))
		}
	}
}

func TestPlanApplyCheck(t *testing.T) {
	dir := t.TempDir()
	gen := Generate(sample(), "")
	if bad, _ := Check(dir, gen); len(bad) != 0 {
		t.Fatalf("a store from before the tree is not checked for missing indexes: %v", bad)
	}
	// A stale index in a folder that holds no facts.
	if err := os.MkdirAll(filepath.Join(dir, "projects", "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "projects", "gone", FileName), []byte("old"), 0o644)
	// inbox/ holds none and is never looked at.
	os.MkdirAll(filepath.Join(dir, "inbox", "claude@box"), 0o755)
	os.WriteFile(filepath.Join(dir, "inbox", "claude@box", FileName), []byte("not ours"), 0o644)
	write, remove, err := Plan(dir, gen)
	if err != nil {
		t.Fatal(err)
	}
	if len(write) != len(gen) || len(remove) != 1 || remove[0] != "projects/gone/_index.md" {
		t.Fatalf("plan: %d writes, removes %v", len(write), remove)
	}
	if err := Apply(dir, write, remove); err != nil {
		t.Fatal(err)
	}
	if bad, _ := Check(dir, gen); len(bad) != 0 {
		t.Fatalf("after apply: %v", bad)
	}
	if w, r, _ := Plan(dir, gen); len(w)+len(r) != 0 {
		t.Fatalf("a second plan changes %d, removes %v", len(w), r)
	}
	// Drift: a hand edit, a missing file once the tree exists.
	os.WriteFile(filepath.Join(dir, "user", FileName), []byte("edited"), 0o644)
	os.Remove(filepath.Join(dir, "system", FileName))
	bad, _ := Check(dir, gen)
	if len(bad) != 2 || !strings.Contains(strings.Join(bad, "\n"), "system/_index.md: missing") || !strings.Contains(strings.Join(bad, "\n"), "user/_index.md: differs") {
		t.Fatalf("drift: %v", bad)
	}
}

func readOpts(t *testing.T) ReadOptions {
	t.Helper()
	dir := t.TempDir()
	gen := Generate(sample(), "")
	w, r, _ := Plan(dir, gen)
	if err := Apply(dir, w, r); err != nil {
		t.Fatal(err)
	}
	return ReadOptions{Store: dir, Repos: []string{"Widget", "widget-svc"}, Host: "box"}
}

func blocks(out string) []string {
	var bs []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "=== ") {
			bs = append(bs, strings.TrimSuffix(strings.TrimPrefix(l, "=== "), " ==="))
		}
	}
	return bs
}

func TestReadReturnsTheIndexesInScopeDownDepthLevels(t *testing.T) {
	o := readOpts(t)
	for _, c := range []struct {
		path  string
		depth int
		want  string
	}{
		{"", 0, "store root: _index.md"},
		{"", 1, "store root: _index.md|system: system/_index.md|user: user/_index.md|projects: projects/_index.md|machines: machines/_index.md|derived: derived/_index.md"},
		{"", 3, "store root: _index.md|system: system/_index.md|user: user/_index.md|projects: projects/_index.md|projects/widget: projects/widget/_index.md|machines: machines/_index.md|machines/box: machines/box/_index.md|derived: derived/_index.md|derived/widget: derived/widget/_index.md"},
		{"projects", 1, "projects: projects/_index.md|projects/widget: projects/widget/_index.md"},
		{"system", 2, "system: system/_index.md"},
		{"projects/widget/", 0, "projects/widget: projects/widget/_index.md"},
	} {
		out, err := Read(o, c.path, c.depth)
		if err != nil {
			t.Fatalf("Read(%q, %d): %v", c.path, c.depth, err)
		}
		if got := strings.Join(blocks(out), "|"); got != c.want {
			t.Errorf("Read(%q, %d) = %s\nwant %s", c.path, c.depth, got, c.want)
		}
		if strings.Contains(out, "gadget") {
			t.Errorf("Read(%q, %d) shows another repository:\n%s", c.path, c.depth, out)
		}
	}
	out, _ := Read(o, "projects", 0)
	if !strings.Contains(out, "1 folder outside this session's scope not shown") || !strings.Contains(out, "- [widget/]") {
		t.Errorf("projects folder filtering:\n%s", out)
	}
}

func TestReadRefusesPathsOutOfTheStoreOrTheSession(t *testing.T) {
	o := readOpts(t)
	for _, p := range []string{"../etc", "/etc", "projects/../..", "inbox", "inbox/claude@box", "review", "keys", "projects/gadget", "machines/other", ".git", "projects\\widget", "C:/x"} {
		if out, err := Read(o, p, 1); err == nil {
			t.Errorf("Read(%q) answered:\n%s", p, out)
		}
	}
	if _, err := Read(o, "", MaxDepth+1); err == nil {
		t.Error("a depth past MaxDepth was taken")
	}
	if _, err := Read(ReadOptions{Store: t.TempDir()}, "", 3); err == nil || !strings.Contains(err.Error(), "never generated") {
		t.Errorf("a store with no index: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// An index file that is a symlink out of the store is refused.
	outside := filepath.Join(t.TempDir(), "secret.md")
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.Remove(filepath.Join(o.Store, "system", FileName))
	if err := os.Symlink(outside, filepath.Join(o.Store, "system", FileName)); err != nil {
		t.Fatal(err)
	}
	if out, err := Read(o, "system", 0); err == nil || strings.Contains(out, "secret") {
		t.Errorf("a symlinked index out of the store was read: %v\n%s", err, out)
	}
}

func TestReadAddsTheLocalLayer(t *testing.T) {
	o := readOpts(t)
	local := t.TempDir()
	lf := []*fact.Fact{mk("local/curated/projects/widget/local-port.md", "machine_fact", "On this machine widget uses port 9000", "2026-06-01")}
	lgen := Generate(lf, "local/curated/")
	if _, ok := lgen["projects/widget/_index.md"]; !ok {
		t.Fatalf("local tree: %v", lgen)
	}
	w, r, _ := Plan(local, lgen)
	Apply(local, w, r)
	o.Local = local
	out, err := Read(o, "projects/widget", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(blocks(out), "|"); got != "projects/widget: projects/widget/_index.md|projects/widget (this machine's local layer): projects/widget/_index.md" || !strings.Contains(out, "local-port") {
		t.Errorf("local layer:\n%s", out)
	}
}
