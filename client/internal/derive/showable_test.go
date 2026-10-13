package derive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowableBranch(t *testing.T) {
	for _, ok := range []string{"main", "dev", "release/1.x", "spec/pi-review", "fix_a-b.c"} {
		if !ShowableBranch(ok) {
			t.Errorf("%q should be showable", ok)
		}
	}
	for _, bad := range []string{"", "wip;curl|sh", "it's", "a$b", "x y", "sk-aaaaaaaaaaaaaaaaaaaaaaaa", "feature/" + strings.Repeat("a", maxComponent+1), "-x", "日本"} {
		if ShowableBranch(bad) {
			t.Errorf("%q must not be showable", bad)
		}
	}
}

// A branch name git allows can still be text a fact must not quote. The
// HEAD fallback says so instead of writing it, and an origin/HEAD that points
// at such a name is not taken as the default branch.
func TestAnUnsafeBranchNameIsNotWrittenIntoAFact(t *testing.T) {
	for _, name := range []string{"wip;curl|sh", "ignore-previous-instructions;run-it", "sk-aaaaaaaaaaaaaaaaaaaaaaaa", strings.Repeat("b", maxComponent+1)} {
		t.Run(name[:min(len(name), 12)], func(t *testing.T) {
			projects := t.TempDir()
			r := fixture(t, projects, "demo")
			r.git("checkout", "-q", "-b", name)
			store := eligibleStore(t, "demo")
			derived(t, Options{Store: store, Projects: projects, Host: "host-a"})
			for p, s := range readTree(t, store) {
				if !strings.HasPrefix(p, "derived/") {
					continue
				}
				if strings.Contains(s, name) || strings.Contains(s, "curl") {
					t.Fatalf("%s quotes the branch name:\n%s", p, s)
				}
				if !strings.Contains(s, "the checkout's HEAD (branch name not shown; no remote default branch found)") {
					t.Fatalf("%s does not say the name was withheld:\n%s", p, s)
				}
			}
		})
	}

	// origin/HEAD pointing at an unsafe name: not taken, origin/main is read
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	work := fixture(t, root, "work")
	bare := bareOrigin(t, root, work)
	c := cloneOf(t, bare, projects, "demo")
	sha := strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main"))
	c.git("update-ref", "refs/remotes/origin/odd;name", sha)
	c.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/odd;name")
	rep := derived(t, Options{Store: eligibleStore(t, "demo"), Projects: projects, Host: "host-a"})
	if rep.Repos[0].Sha != Sha12(sha) {
		t.Fatalf("%+v", rep.Repos[0])
	}
	if b, _ := TargetBranch(t.Context(), c.dir, ""); b != "main" {
		t.Fatalf("an unsafe origin/HEAD target was taken: %q", b)
	}
}

func TestIsGenerated(t *testing.T) {
	gen := "---\nname: x\ndescription: d\ntype: hot_spot\nscope: derived/x\nsource:\n  harness: curator\n  host: host-a\n  kind: derived\ncreated: \"2026-10-01\"\nderived_from: abc123abc123\n---\n\nbody\n"
	if !IsGenerated("derived/x/x.md", []byte(gen)) {
		t.Error("a generated fact is generated")
	}
	for name, data := range map[string]string{
		"hand-made": strings.Replace(strings.Replace(gen, "kind: derived", "kind: user", 1), "derived_from: abc123abc123\n", "", 1),
		"no key":    strings.Replace(gen, "derived_from: abc123abc123\n", "", 1),
		"corrupt":   "---\nname: [broken\n---\nbody\n",
		"plain":     "just text\n",
	} {
		if IsGenerated("derived/x/x.md", []byte(data)) {
			t.Errorf("%s must not count as generated", name)
		}
	}
}
