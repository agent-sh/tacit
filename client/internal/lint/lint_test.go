package lint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/tree"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func good(name, scope string) string {
	return "---\nname: " + name + "\ndescription: d\ntype: machine_fact\nscope: " + scope + "\nsource:\n  harness: claude\n  host: host-a\n  kind: observed\ncreated: 2026-10-07\n---\n\nbody\n"
}

func rules(fs []Finding) []string {
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Rule] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func TestCleanStorePasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, "machines/host-a/a.md", good("a", "machines/host-a"))
	write(t, root, "projects/memra/b.md", strings.Replace(good("b", "projects/memra"), "body", "see [[a]]", 1))
	write(t, root, "inbox/claude@host-a/c.md", strings.Replace(good("c", "projects/memra"), "type: machine_fact", "type: unclassified", 1))
	// A retired fact (a dated verdict that it is outdated) is clean and not indexed.
	write(t, root, "machines/host-a/r.md", strings.Replace(good("r", "machines/host-a"), "created:", "retired: 2026-10-09\ncreated:", 1))
	facts, _, _ := fact.Walk(root)
	write(t, root, "MEMORY.md", index.Generate(facts))
	fs, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("expected clean, got %v", fs)
	}
}

func TestEachRuleFires(t *testing.T) {
	root := t.TempDir()
	write(t, root, "machines/host-a/ok.md", good("ok", "machines/host-a"))
	// L1 missing fields and bad frontmatter
	write(t, root, "machines/host-a/l1.md", "---\nname: l1\n---\nbody\n")
	write(t, root, "machines/host-a/l1b.md", "no frontmatter\n")
	// L2 name != stem, not slug
	write(t, root, "machines/host-a/l2.md", good("Other_Name", "machines/host-a"))
	// L3 unknown type, unclassified outside inbox
	write(t, root, "machines/host-a/l3.md", strings.Replace(good("l3", "machines/host-a"), "machine_fact", "unclassified", 1))
	write(t, root, "machines/host-a/l3b.md", strings.Replace(good("l3b", "machines/host-a"), "machine_fact", "rumor", 1))
	// L4 dir != scope, invalid scope, bad inbox path
	write(t, root, "machines/host-a/l4.md", good("l4", "projects/memra"))
	write(t, root, "machines/host-a/l4b.md", good("l4b", "projects"))
	write(t, root, "inbox/l4c.md", good("l4c", "user"))
	// L5 dates
	write(t, root, "machines/host-a/l5.md", strings.Replace(good("l5", "machines/host-a"), "2026-10-07", "yesterday", 1))
	// L6 unresolved link (curated), L7 superseded_by
	write(t, root, "machines/host-a/l6.md", strings.Replace(good("l6", "machines/host-a"), "body", "[[nope]]", 1))
	write(t, root, "machines/host-a/l7.md", strings.Replace(good("l7", "machines/host-a"), "created:", "superseded_by: nope\ncreated:", 1))
	// L8 web in system
	write(t, root, "system/l8.md", strings.Replace(good("l8", "system"), "kind: observed", "kind: web", 1))
	// L9 derived_from rules
	write(t, root, "derived/memra/l9.md", good("l9", "derived/memra"))
	write(t, root, "machines/host-a/l9b.md", strings.Replace(good("l9b", "machines/host-a"), "created:", "derived_from: abc\ncreated:", 1))
	// L14 retired is a date, and not on an inbox fact
	write(t, root, "machines/host-a/l14.md", strings.Replace(good("l14", "machines/host-a"), "created:", "retired: yesterday\ncreated:", 1))
	write(t, root, "inbox/claude@host-a/l14b.md", strings.Replace(good("l14b", "user"), "created:", "retired: 2026-10-09\ncreated:", 1))
	// L11 long description, empty body
	write(t, root, "machines/host-a/l11.md", strings.Replace(good("l11", "machines/host-a"), "description: d", "description: "+strings.Repeat("x", 201), 1))
	write(t, root, "machines/host-a/l11b.md", strings.Replace(good("l11b", "machines/host-a"), "\nbody\n", "\n", 1))
	// L12 enums
	write(t, root, "machines/host-a/l12.md", strings.Replace(good("l12", "machines/host-a"), "harness: claude", "harness: vim", 1))
	// L10 stale index
	write(t, root, "MEMORY.md", "stale\n")

	fs, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"L1", "L10", "L11", "L12", "L14", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9"}
	got := rules(fs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rules fired %v, want %v\n%v", got, want, fs)
	}
	for _, f := range fs {
		if strings.HasSuffix(f.Path, "ok.md") {
			t.Errorf("clean file flagged: %v", f)
		}
	}
	// inbox links are not L6
	write(t, root, "inbox/claude@host-a/x.md", strings.Replace(good("x", "user"), "body", "[[missing]]", 1))
	fs, _ = Run(root)
	for _, f := range fs {
		if f.Rule == "L6" && strings.HasPrefix(f.Path, "inbox/") {
			t.Errorf("inbox link flagged: %v", f)
		}
	}
}

// L16: once a store has its root index, every folder index must equal the
// generated one; a store from before the tree is checked only for the index
// files it holds.
func TestFolderIndexesMatchTheFacts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "machines/host-a/a.md", good("a", "machines/host-a"))
	write(t, root, "projects/memra/b.md", good("b", "projects/memra"))
	facts, _, _ := fact.Walk(root)
	write(t, root, "MEMORY.md", index.Generate(facts))
	if fs, _ := Run(root); len(fs) != 0 {
		t.Fatalf("a store from before the tree: %v", fs)
	}
	gen := tree.Generate(facts, "")
	for rel, c := range gen {
		write(t, root, rel, c)
	}
	if fs, _ := Run(root); len(fs) != 0 {
		t.Fatalf("generated indexes: %v", fs)
	}
	write(t, root, "projects/memra/_index.md", "edited by hand\n")
	os.Remove(filepath.Join(root, "machines", "_index.md"))
	write(t, root, "projects/gone/_index.md", "stale\n")
	fs, _ := Run(root)
	var got []string
	for _, f := range fs {
		if f.Rule != "L16" {
			t.Errorf("unexpected %s", f)
		}
		got = append(got, f.Path)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "machines/_index.md,projects/gone/_index.md,projects/memra/_index.md" {
		t.Fatalf("L16 on %v", got)
	}
}
