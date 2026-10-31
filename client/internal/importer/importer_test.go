package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeFS answers DirExists from a set of directories.
func fakeFS(dirs ...string) func(string) bool {
	set := map[string]bool{}
	for _, d := range dirs {
		for p := d; p != "/" && p != "."; p = filepath.Dir(p) {
			set[p] = true
		}
	}
	return func(p string) bool { return set[filepath.Clean(p)] }
}

func TestClaudeImport(t *testing.T) {
	home := t.TempDir()
	mem := filepath.Join(home, ".claude/projects/-home-u-projects-billing/memory")
	write(t, filepath.Join(mem, "MEMORY.md"), "# index\n")
	write(t, filepath.Join(mem, "feedback_small_commits.md"), "---\nname: feedback_small_commits\ndescription: \"Keep commits small\"\nmetadata:\n  type: feedback\n  originSessionId: abc\n  modified: 2026-09-01T10:00:00.000Z\n---\n\nSmall commits. See [[host_a_facts]]. Shell: `[[ $n == 0 ]]` and df[[\"a\",\"b\"]] and [[ ]].\n")
	// Invalid YAML: unquoted description with ": " inside.
	write(t, filepath.Join(mem, "deploy-audit-217.md"), "---\nname: deploy-audit-217\ndescription: billing audit (billing#217): what refuses, what warns\nmetadata:\n  type: project\n---\n\nBody text.\n")
	// No frontmatter at all.
	write(t, filepath.Join(mem, "plain.md"), "# Plain note\n\nSome fact.\n")
	// Stem collision after slugging.
	write(t, filepath.Join(mem, "feedback-small-commits.md"), "---\nname: x\ndescription: twin\nmetadata:\n  type: feedback\n---\n\nTwin body.\n")
	// Symlinked file is skipped.
	os.Symlink("/etc/hostname", filepath.Join(mem, "link.md"))

	store := t.TempDir()
	o := Options{Store: store, Host: "host-a", Home: "/home/u", DirExists: fakeFS("/home/u/projects/billing/.git")}
	r := Claude(o, mem)
	if len(r.Errors) != 0 || r.Written != 4 || r.Skipped != 2 {
		t.Fatalf("result %v %v", r, r.Errors)
	}
	r2 := Claude(o, mem)
	if r2.Written != 0 || r2.Unchanged != 4 {
		t.Fatalf("second import %v", r2)
	}
	facts, perrs, err := fact.Walk(store)
	if err != nil || len(perrs) != 0 {
		t.Fatalf("walk: %v %v", err, perrs)
	}
	byName := map[string]*fact.Fact{}
	for _, f := range facts {
		byName[f.Meta.Name] = f
	}
	// Sorted order gives feedback-small-commits.md the plain slug and the
	// underscore twin a hash suffix; find the original by its source path.
	var f *fact.Fact
	for _, c := range facts {
		if strings.HasSuffix(c.Meta.Source.ImportedFrom, "/feedback_small_commits.md") {
			f = c
		}
	}
	if f == nil || !strings.HasPrefix(f.Meta.Name, "feedback-small-commits-") {
		t.Fatalf("original not kept apart from its twin; have %v", keys(byName))
	}
	// A feedback memory holds no quote of theirs: it is the agent's note of what the owner
	// wants, so it is observed (before 2026-10-11 every feedback memory was kind
	// user).
	if f.Meta.Scope != "projects/billing" || f.Meta.Source.Kind != "observed" || f.Meta.Source.OriginalType != "feedback" || f.Meta.Created != "2026-09-01" || f.Meta.Type != "unclassified" {
		t.Fatalf("meta %+v", f.Meta)
	}
	if !strings.Contains(f.Body, "[[host-a-facts]]") || !strings.Contains(f.Body, "[[ $n == 0 ]]") || !strings.Contains(f.Body, `df[["a","b"]]`) || !strings.Contains(f.Body, "[[ ]]") {
		t.Fatalf("link rewrite wrong: %q", f.Body)
	}
	if !strings.HasPrefix(f.Path, "inbox/import@host-a/-home-u-projects-billing/") {
		t.Fatalf("path %q", f.Path)
	}
	if e := byName["deploy-audit-217"]; e == nil || !strings.HasPrefix(e.Meta.Description, "billing audit (billing#217)") || e.Meta.Source.OriginalType != "project" {
		t.Fatalf("loose frontmatter fallback failed: %+v", e)
	}
	if p := byName["plain"]; p == nil || p.Meta.Description != "Plain note" {
		t.Fatalf("plain import: %+v", p)
	}
	if tw := byName["feedback-small-commits"]; tw == nil || !strings.Contains(tw.Body, "Twin body") {
		t.Fatalf("twin missing or overwritten: %+v", tw)
	}
	if fs, _ := lint.Run(store); len(fs) != 0 {
		t.Fatalf("imported store does not lint clean: %v", fs)
	}
}

func TestHostCannotEscapeInbox(t *testing.T) {
	store := t.TempDir()
	mem := t.TempDir()
	write(t, filepath.Join(mem, "a.md"), "fact\n")
	for _, host := range []string{"/../../escaped", "..", "Box", "a b", ""} {
		r := Claude(Options{Store: store, Host: host, Home: "/home/u"}, mem)
		if len(r.Errors) == 0 || r.Written != 0 {
			t.Errorf("host %q accepted: %v", host, r)
		}
	}
	entries, _ := os.ReadDir(store)
	if len(entries) != 0 {
		t.Fatalf("store written to: %v", entries)
	}
}

func TestClaudeMetaLoose(t *testing.T) {
	cm := claudeMetaLoose([]byte("name: x\ndescription: \"quoted: with colon\"\nmetadata:\n  type: feedback\n  modified: 2026-09-01T00:00:00Z\n"))
	if cm.Description != "quoted: with colon" || cm.Metadata.Type != "feedback" || cm.Metadata.Modified != "2026-09-01T00:00:00Z" {
		t.Fatalf("%+v", cm)
	}
	cm = claudeMetaLoose([]byte("description: \"Talk\" is the word\n"))
	if cm.Description != "\"Talk\" is the word" {
		t.Fatalf("opening quote stripped: %q", cm.Description)
	}
	cm = claudeMetaLoose([]byte("description: |\n  first line: a\n  second\nmetadata:\n  type: project\n"))
	if cm.Description != "first line: a second" || cm.Metadata.Type != "project" {
		t.Fatalf("block scalar: %+v", cm)
	}
}

func TestRewriteLinks(t *testing.T) {
	in := "[[feedback_small_commits]] [[Some Fact|alias]] [[ $x == 1 ]] df[[\"a\",\"b\"]] [[ ]] [[a\nb]] [[host-a-facts#sec]]"
	want := "[[feedback-small-commits]] [[Some Fact|alias]] [[ $x == 1 ]] df[[\"a\",\"b\"]] [[ ]] [[a\nb]] [[host-a-facts#sec]]"
	if got := rewriteLinks(in); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestCodexImport(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "memory_summary.md"), "v1\n\n## User Profile\n\nDana is an engineer.\nWorks on databases.\n\n## User preferences\n\n- Run the full test suite before a release. [claude]\n- Answer in short paragraphs and\n  lead with the result.\n\n## General Tips\n\n- Search MEMORY.md first.\n\n## What's in Memory\n\n- ignored\n")
	write(t, filepath.Join(dir, "raw_memories.md"), "- raw\n")
	store := t.TempDir()
	o := Options{Store: store, Host: "host-a", Home: "/home/u"}
	r := Codex(o, dir)
	if len(r.Errors) != 0 || r.Written != 4 {
		t.Fatalf("result %v %v", r, r.Errors)
	}
	facts, _, _ := fact.Walk(store)
	count := map[string]int{}
	for _, f := range facts {
		count[f.Meta.Source.OriginalType]++
		switch f.Meta.Source.OriginalType {
		case "user-preferences":
			// Codex's summary of their preference, no words of theirs: observed.
			if f.Meta.Source.Kind != "observed" || strings.Contains(f.Body, "[claude]") {
				t.Errorf("preference: %+v %q", f.Meta.Source, f.Body)
			}
		case "general-tips":
			if f.Meta.Source.Kind != "observed" {
				t.Errorf("tip kind %q", f.Meta.Source.Kind)
			}
		case "user-profile":
			if f.Meta.Name != "codex-user-profile" || !strings.Contains(f.Body, "Works on databases") {
				t.Errorf("profile %+v %q", f.Meta, f.Body)
			}
		}
	}
	if count["user-preferences"] != 2 || count["general-tips"] != 1 || count["user-profile"] != 1 {
		t.Fatalf("sections: %v", count)
	}
	if fs, _ := lint.Run(store); len(fs) != 0 {
		t.Fatalf("lint: %v", fs)
	}
	if r2 := Codex(o, dir); r2.Written != 0 || r2.Unchanged != 4 {
		t.Fatalf("second import %v", r2)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Feedback_Small Commit!": "feedback-small-commit", "---": "fact", "a": "a"} {
		if got := Slug(in, 80); got != want {
			t.Errorf("Slug(%q)=%q want %q", in, got, want)
		}
	}
	if got := Slug(strings.Repeat("ab-", 40), 20); len(got) > 20 || strings.HasSuffix(got, "-") {
		t.Errorf("cap: %q", got)
	}
}

func TestScopeFromClaudeProject(t *testing.T) {
	o := Options{Home: "/home/u", DirExists: fakeFS(
		"/home/u/projects/webapp/.git",
		"/home/u/projects/rust-cache/.git",
		"/home/u/projects/agent-sh/agnix/.git",
		"/home/u/projects/queue-kit/queue-kit/.git",
		"/home/u/projects/webapp/wt-x",
		"/home/u/.cache/claude-scratch",
		"/home/u/Documents/Codex",
	)}
	for in, want := range map[string]string{
		"-home-u":                              "user",
		"-home-u-projects":                     "user",
		"-home-u-projects-webapp":              "projects/webapp",
		"-home-u-projects-webapp-wt-x":         "projects/webapp",
		"-home-u-projects-rust-cache":          "projects/rust-cache",
		"-home-u-projects-agent-sh-agnix":      "projects/agnix",
		"-home-u-projects-queue-kit-queue-kit": "projects/queue-kit",
		"-home-u-projects-gone-repo":           "projects/gone-repo",
		"-home-u--cache-claude-scratch":        "user",
		"-home-u-Documents-Codex":              "user",
		"-tmp-x":                               "user",
	} {
		if got := o.scopeFromClaudeProject(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func keys(m map[string]*fact.Fact) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
