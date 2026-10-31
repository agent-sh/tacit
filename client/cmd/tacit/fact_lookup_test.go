package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A name the store holds twice is looked up as the live curated fact it
// stands for (tacit_fact, `tacit fact`, `tacit resolve`). The walk meets
// inbox/ before every curated folder and projects/ before user/, so an
// agent's note of the same name, or a retired copy another tool left in a
// second folder, must not answer for it. With no live copy, a curated fact
// that is replaced or retired comes before a note.
func TestFindFactTakesTheLiveCuratedFactOfAName(t *testing.T) {
	dir := t.TempDir()
	put := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	note := func(name, desc string) string {
		return "---\nname: " + name + "\ndescription: \"" + desc + "\"\ntype: unclassified\nscope: user\nsource:\n  harness: claude\n  host: box\n  kind: observed\ncreated: 2026-10-20\n---\n\n" + desc + ".\n"
	}
	put("projects/demo/cache-rule.md", rotFile("cache-rule", "projects/demo", "design_decision", "The cache is cleared weekly", "The cache is cleared weekly.", "2026-09-01", "retired: 2026-10-01\n"))
	put("user/cache-rule.md", rotFile("cache-rule", "user", "design_decision", "The cache is cleared before every build", "The cache is cleared before every build.", "2026-10-26", ""))
	put("inbox/claude@box/deploy-rule.md", note("deploy-rule", "Deploys wait for a green canary, says an agent"))
	put("system/deploy-rule.md", rotFile("deploy-rule", "system", "design_decision", "Deploys wait for the canary", "Deploys wait for the canary.", "2026-10-02", ""))
	put("inbox/claude@box/lint-rule.md", note("lint-rule", "Lint runs on save, says an agent"))
	put("projects/demo/lint-rule.md", rotFile("lint-rule", "projects/demo", "design_decision", "Lint runs before the push", "Lint runs before the push.", "2026-09-03", "retired: 2026-10-03\n"))
	for name, want := range map[string]string{
		"cache-rule":  "user/cache-rule.md",
		"deploy-rule": "system/deploy-rule.md",
		"lint-rule":   "projects/demo/lint-rule.md",
	} {
		p, ok := findFact(dir, name)
		if rel, _ := filepath.Rel(dir, p); !ok || filepath.ToSlash(rel) != want {
			t.Errorf("findFact(%s) = %q, want %s", name, p, want)
		}
	}
	out, ok, err := factText(dir, "cache-rule")
	if !ok || err != nil || !strings.Contains(out, "before every build") || strings.Contains(out, "weekly") {
		t.Errorf("tacit_fact cache-rule: %v %v\n%s", ok, err, out)
	}
}
