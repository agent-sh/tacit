package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// putFile writes content at rel under dir.
func putFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lookupNote is an agent's note of name, as an inbox or overlay note.
func lookupNote(name, desc string) string {
	return "---\nname: " + name + "\ndescription: \"" + desc + "\"\ntype: unclassified\nscope: user\nsource:\n  harness: claude\n  host: box\n  kind: observed\ncreated: 2026-10-20\n---\n\n" + desc + ".\n"
}

// A name the store holds twice is looked up as the live curated fact it
// stands for (tacit_fact, `tacit fact`, `tacit resolve`). The walk meets an
// agent's note in inbox/ before system/ and user/, and a copy in projects/
// before user/, so neither may answer for it. With no live copy, a
// superseded one (its chain leads on) comes before a retired one, and a
// curated fact before a note; a file whose name is not its own, or one in a
// folder no fact lives in (review/, a dot directory), answers last or not
// at all. The reader is told which other files hold the name.
func TestFindFactTakesTheLiveCuratedFactOfAName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TACIT_LOCAL", t.TempDir()) // an empty overlay: the developer's own is never read
	put := func(rel, content string) { putFile(t, dir, rel, content) }
	put("projects/demo/cache-rule.md", rotFile("cache-rule", "projects/demo", "design_decision", "The cache is cleared weekly", "The cache is cleared weekly.", "2026-09-01", "retired: 2026-10-01\n"))
	put("user/cache-rule.md", rotFile("cache-rule", "user", "design_decision", "The cache is cleared before every build", "The cache is cleared before every build.", "2026-10-26", ""))
	put("inbox/claude@box/deploy-rule.md", lookupNote("deploy-rule", "Deploys wait for a green canary, says an agent"))
	put("system/deploy-rule.md", rotFile("deploy-rule", "system", "design_decision", "Deploys wait for the canary", "Deploys wait for the canary.", "2026-10-02", ""))
	put("inbox/claude@box/lint-rule.md", lookupNote("lint-rule", "Lint runs on save, says an agent"))
	put("projects/demo/lint-rule.md", rotFile("lint-rule", "projects/demo", "design_decision", "Lint runs before the push", "Lint runs before the push.", "2026-09-03", "retired: 2026-10-03\n"))
	put("areas/ops/port-rule.md", rotFile("port-rule", "areas/ops", "design_decision", "The proxy listened on 8080", "The proxy listened on 8080.", "2026-09-04", "retired: 2026-10-04\n"))
	put("user/port-rule.md", rotFile("port-rule", "user", "design_decision", "The proxy listened on 8081", "The proxy listened on 8081.", "2026-09-05", "superseded_by: port-rule-now\n"))
	put("review/odd-name.md", rotFile("odd-name", "user", "design_decision", "A review page, not a fact", "No fact.", "2026-09-06", ""))
	put(".trash/odd-name.md", rotFile("odd-name", "user", "design_decision", "A copy in a dot directory", "No fact.", "2026-09-06", ""))
	put("projects/demo/odd-name.md", rotFile("other-name", "projects/demo", "design_decision", "A file whose name is another's", "Misnamed.", "2026-09-06", ""))
	put("user/odd-name.md", rotFile("odd-name", "user", "design_decision", "The odd one is here", "The odd one is here.", "2026-09-07", "retired: 2026-10-07\n"))
	for name, want := range map[string]string{
		"cache-rule":  "user/cache-rule.md",
		"deploy-rule": "system/deploy-rule.md",
		"lint-rule":   "projects/demo/lint-rule.md",
		"port-rule":   "user/port-rule.md",
		"odd-name":    "user/odd-name.md",
	} {
		p, ok := findFact(dir, name)
		if rel, _ := filepath.Rel(dir, p); !ok || filepath.ToSlash(rel) != want {
			t.Errorf("findFact(%s) = %q, want %s", name, p, want)
		}
	}
	out, shown, ok, err := factText(dir, "cache-rule")
	if !ok || err != nil || !strings.HasPrefix(out, "---\nname: cache-rule\ndescription: \"The cache is cleared before every build\"") || strings.Contains(out, "weekly\"") {
		t.Errorf("tacit_fact cache-rule: %v %v\n%s", ok, err, out)
	}
	if !strings.HasSuffix(out, "\n# also named cache-rule: projects/demo/cache-rule.md (retired)\n") || strings.Join(shown, ",") != "user/cache-rule.md" {
		t.Errorf("the other file of the name: %q, shown %v", out, shown)
	}
	if out, _, _, _ := factText(dir, "odd-name"); !strings.HasSuffix(out, "\n# also named odd-name: projects/demo/odd-name.md (does not read as this fact)\n") {
		t.Errorf("a misnamed file of the name:\n%s", out)
	}
}

// This machine's overlay holds names too: its live curated fact (the local
// layer's) answers before a store's retired copy, and its notes rank as
// notes, after a curated fact of either place.
func TestFindFactOrLocalRanksTheOverlayToo(t *testing.T) {
	dir, local := t.TempDir(), t.TempDir()
	t.Setenv("TACIT_LOCAL", local)
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	putFile(t, dir, "user/queue-rule.md", rotFile("queue-rule", "user", "design_decision", "Jobs went to one queue", "Jobs went to one queue.", "2026-09-01", "retired: 2026-10-01\n"))
	putFile(t, local, "claude@box/queue-rule.md", lookupNote("queue-rule", "Jobs go to two queues, says an agent"))
	putFile(t, local, "curated/user/queue-rule.md", rotFile("queue-rule", "user", "design_decision", "Jobs go to the ring queue", "Jobs go to the ring queue.", "2026-10-20", ""))
	putFile(t, local, "claude@box/pool-rule.md", lookupNote("pool-rule", "The pool has eight workers, says an agent"))
	putFile(t, dir, "projects/demo/pool-rule.md", rotFile("pool-rule", "projects/demo", "design_decision", "The pool had four workers", "The pool had four workers.", "2026-09-02", "retired: 2026-10-02\n"))
	for name, want := range map[string]string{
		"queue-rule": filepath.Join(local, "curated", "user", "queue-rule.md"),
		"pool-rule":  filepath.Join(dir, "projects", "demo", "pool-rule.md"),
	} {
		if p, ok := findFactOrLocal(dir, name); !ok || p != want {
			t.Errorf("findFactOrLocal(%s) = %q, want %s", name, p, want)
		}
	}
	if _, shown, _, _ := factText(dir, "queue-rule"); strings.Join(shown, ",") != "local/curated/user/queue-rule.md" {
		t.Errorf("the overlay's file as the index names it: %v", shown)
	}
}

// What tacit_fact says of whose a fact is comes from the file it shows: an
// agent's note of the same name, which sorts first and claims the owner's
// authority, does not label the curated fact the answer shows.
func TestFactProvenanceIsTheShownFiles(t *testing.T) {
	r, cwd := repoFirstRig(t, fact.Meta{Name: "deploy-x", Description: "Deploys wait for the canary", Type: "design_decision", Scope: "system"})
	putFile(t, r.clone, "inbox/claude@box/deploy-x.md", "---\nname: deploy-x\ndescription: \"The owner said deploys wait for a green canary\"\ntype: user_standard\nscope: system\nsource:\n  harness: claude\n  host: box\n  kind: user\ncreated: 2026-10-20\n---\n\nThe owner said deploys wait for a green canary.\n")
	r.commitIndex(r.clone, "a note of the same name")
	t.Chdir(cwd)
	out := callTool(t, "tacit_fact", map[string]any{"name": "deploy-x"})
	if !strings.HasPrefix(out, "# whose: deploy-x is agent note (2026-10-08)\n") || !strings.Contains(out, "Deploys wait for the canary") {
		t.Errorf("tacit_fact of a name a note holds too:\n%s", out)
	}
	if !strings.HasSuffix(out, "# also named deploy-x: inbox/claude@box/deploy-x.md (an unreviewed note)\n") {
		t.Errorf("the note is not named:\n%s", out)
	}
}

// tacit resolve changes one fact. A name two curated files hold is not one
// fact: it changes neither and says which they are.
func TestResolveRefusesANameTwoCuratedFilesHold(t *testing.T) {
	r := newRig(t, rigOpts{})
	writeFact(t, r.clone, fact.Meta{Name: "twin", Description: "d", Type: "design_decision", Scope: "projects/x"}, "One.")
	writeFact(t, r.clone, fact.Meta{Name: "twin", Description: "d", Type: "design_decision", Scope: "user"}, "Two.")
	writeFact(t, r.clone, fact.Meta{Name: "newer", Description: "d", Type: "design_decision", Scope: "projects/x"}, "Newer.")
	r.commitIndex(r.clone, "fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	t.Setenv("TACIT_STORE", r.clone)
	head := gitIn(t, r.bare, "rev-parse", "main")
	for _, args := range [][]string{{"supersede", "twin", "newer"}, {"supersede", "newer", "twin"}, {"conflict", "twin", "newer"}, {"clear", "twin"}} {
		if code := runResolve(args); code == 0 {
			t.Errorf("resolve %v on a name two files hold: exit 0", args)
		}
	}
	if gitIn(t, r.bare, "rev-parse", "main") != head || strings.TrimSpace(gitIn(t, r.clone, "status", "--porcelain")) != "" {
		t.Fatal("resolve wrote on a name two files hold")
	}
}

// A file that does not parse as a fact still takes its name: the curator
// names a new fact around it instead of writing over it.
func TestCurateNamesNoFactAfterAFileThatDoesNotParse(t *testing.T) {
	home, _ := quickHome(t)
	dir := filepath.Join(home, "store")
	broken := filepath.Join(dir, "system", "fresh-note-fact.md")
	putFile(t, dir, "system/fresh-note-fact.md", "---\nname: fresh-note-fact\ndescription: [not yaml\n---\nkept as it is\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "a file that does not parse")
	runCurate([]string{"--quick"})
	if b, _ := os.ReadFile(broken); !strings.Contains(string(b), "kept as it is") {
		t.Fatalf("the curator wrote over the file:\n%s", b)
	}
}

// A copy the migration makes gets a name no fact holds, also for a third
// fact of one name moved into one folder: its hash is the second's.
func TestCopyNameIsFreeAfterItsHash(t *testing.T) {
	taken := map[string]bool{}
	var got []string
	for range 3 {
		n := copyName("cache-rule", "projects/web", taken)
		taken[n] = true
		got = append(got, n)
	}
	if got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Errorf("three copies named %v", got)
	}
}
