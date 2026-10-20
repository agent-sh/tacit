package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// dedupeFixture is a store with a real remote holding three wordings of one
// machine fact, an owner ruling with an agent's copy of it, a fact holding
// the owner's words, and two facts that differ in a port.
func dedupeFixture(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, rigOpts{})
	const ws = "machines/host-a"
	cwd := "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd"
	writeFact(t, r.clone, fact.Meta{Name: "cwd-resets", Description: cwd, Type: "machine_fact", Scope: ws, AppliesTo: fact.AppliesTo{Hosts: []string{"host-a"}}}, cwd+"\n")
	writeFact(t, r.clone, fact.Meta{Name: "cwd-resets-again", Description: "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd", Type: "machine_fact", Scope: ws}, "Do not rely on an earlier cd.\n")
	writeFact(t, r.clone, fact.Meta{Name: "cwd-resets-home", Description: "The Bash tool resets cwd to ~/work after every call; start each command with cd", Type: "machine_fact", Scope: ws}, "Seen on every call.\n")
	rule := "Never retry a failed deploy by hand in a loop; rerun it with the deploy tool and a timeout"
	writeFact(t, r.clone, fact.Meta{Name: "deploy-rule", Description: rule, Type: "user_standard", Scope: "user", Source: ownerSource("never retry a failed deploy by hand in a loop")}, "The owner said \"never retry a failed deploy by hand in a loop\".\n")
	writeFact(t, r.clone, fact.Meta{Name: "deploy-copy", Description: "Never retry a failed deploy by hand in a loop, rerun it with the deploy tool and a timeout", Type: "user_standard", Scope: "user"}, "An agent's note of the same rule.\n")
	words := fact.Source{Kind: "observed", OwnerWords: "rerun it with the deploy tool", OwnerWordsFrom: testOwnerSession}
	writeFact(t, r.clone, fact.Meta{Name: "deploy-words", Description: "never retry a failed deploy by hand in a loop; rerun it with the deploy tool and a timeout", Type: "user_standard", Scope: "user", Source: words}, "They said to rerun it with the deploy tool.\n")
	writeFact(t, r.clone, fact.Meta{Name: "lantern-port", Description: "The lantern proxy for the quokka scheduler listens on port 7781 on host-a", Type: "machine_fact", Scope: ws}, "x\n")
	writeFact(t, r.clone, fact.Meta{Name: "lantern-port-new", Description: "The lantern proxy for the quokka scheduler listens on port 7790 on host-a", Type: "machine_fact", Scope: ws}, "x\n")
	r.commitIndex(r.clone, "dedupe fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return r
}

func supersededOnServer(t *testing.T, r *rig, rel string) string {
	t.Helper()
	f, err := fact.Parse(rel, []byte(r.onServer(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return f.Meta.SupersededBy
}

// tacit dedupe: the dry run changes nothing; the real run retires each copy
// with superseded_by the fact kept, in one curator commit that lints clean,
// leaves the ruling, the fact with the owner's words and the changed port
// alone, and a second run pushes nothing.
func TestDedupeSupersedesCopiesInOneCuratorCommit(t *testing.T) {
	r := dedupeFixture(t)
	head := r.serverHead()
	if code := runDedupe([]string{"--dry-run"}); code != 0 || r.serverHead() != head {
		t.Fatalf("dedupe --dry-run exited %d or moved the server", code)
	}
	r.assertClean("after dedupe --dry-run")
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	want := map[string]string{
		"machines/host-a/cwd-resets.md":       "",
		"machines/host-a/cwd-resets-again.md": "cwd-resets",
		"machines/host-a/cwd-resets-home.md":  "cwd-resets",
		"user/deploy-rule.md":                 "",
		"user/deploy-copy.md":                 "deploy-rule",
		"user/deploy-words.md":                "",
		"machines/host-a/lantern-port.md":     "",
		"machines/host-a/lantern-port-new.md": "",
	}
	for rel, by := range want {
		if got := supersededOnServer(t, r, rel); got != by {
			t.Errorf("%s: superseded_by %q, want %q", rel, got, by)
		}
	}
	subjects := gitIn(t, r.bare, "log", "--format=%s", head+"..main")
	if n := strings.Count(strings.TrimSpace(subjects), "\n") + 1; n != 1 || !strings.HasPrefix(subjects, "curator: dedupe: 3 near-duplicate fact(s)") {
		t.Fatalf("want one dedupe commit, got:\n%s", subjects)
	}
	if got := lintRules(r.serverLint()); got != "" {
		t.Fatalf("lint after dedupe:\n%s", got)
	}
	// Only the frontmatter lines change.
	if body := r.onServer("machines/host-a/cwd-resets-home.md"); !strings.Contains(body, "Seen on every call.") || !strings.Contains(body, "updated: ") {
		t.Fatalf("copy rewritten:\n%s", body)
	}
	r.assertClean("after dedupe")
	head = r.serverHead()
	if code := runDedupe(nil); code != 0 || r.serverHead() != head {
		t.Fatalf("a second dedupe exited %d or pushed", code)
	}
}

// A fact of the split that only restates a curated fact is not written: its
// note is recorded as absorbed by that fact, and the run's other facts are
// written as before.
func TestCurateAbsorbsAFactThatRestatesACuratedOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home := t.TempDir()
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
	s := &store.Store{Dir: filepath.Join(home, "store")}
	cwd := "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd"
	writeFact(t, s.Dir, fact.Meta{Name: "cwd-resets", Description: cwd, Type: "machine_fact", Scope: "machines/host-a"}, cwd+"\n")
	all, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, all)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "curator: seed")
	for _, n := range []string{"n1", "n2"} {
		if _, err := addFact(context.Background(), s, "muse", fact.Meta{Name: n, Description: "a session note " + n, Type: "unclassified", Scope: "machines/host-a", Source: fact.Source{Kind: "observed"}}, "Several things happened in "+n+"."); err != nil {
			t.Fatal(err)
		}
	}
	split := `[{"origin":"n1","name":"bash-cwd-resets","description":"Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd","type":"machine_fact","scope":"machines/host-a","kind":"observed","body":"Do not rely on an earlier cd."},` +
		`{"origin":"n2","name":"zebra-receipts","description":"The zebra crossing tool writes its receipts under receipts/zebra in the repo root","type":"machine_fact","scope":"machines/host-a","kind":"observed","body":"Look there first."}]`
	response, _ := json.Marshal(map[string]any{"result": split, "total_cost_usd": .01})
	fake := filepath.Join(home, "fake-cli")
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Setenv("FIXTURE_RESPONSE", string(response))
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$FIXTURE_RESPONSE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := runCurate([]string{"--max-batches", "1"}); code != 0 {
		t.Fatalf("curate exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "machines/host-a/bash-cwd-resets.md")); err == nil {
		t.Fatal("a second copy of cwd-resets was written")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "machines/host-a/zebra-receipts.md")); err != nil {
		t.Fatal("the new fact was not written:", err)
	}
	state := loadCuratorState(filepath.Join(home, ".local/state/tacit/curator.json"))
	if state.Restated["n1"] != "cwd-resets" || state.Empty["n1"] != "" || state.Restated["n2"] != "" {
		t.Fatalf("absorbed note not recorded: restated %v empty %v", state.Restated, state.Empty)
	}
	if log, _ := os.ReadFile(filepath.Join(home, ".local/state/tacit/curator.log")); !strings.Contains(string(log), `"absorbed":1`) {
		t.Fatalf("the run's report does not count the absorbed fact:\n%s", log)
	}
	if st := gitIn(t, s.Dir, "status", "--porcelain"); st != "" {
		t.Fatalf("the store is dirty:\n%s", st)
	}
}

// A fact an earlier batch of the same run wrote absorbs a later restatement.
func TestCurateAbsorbsAcrossBatchesOfOneRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home := t.TempDir()
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
	s := &store.Store{Dir: filepath.Join(home, "store")}
	// Each note fills most of a batch, so the two are split apart.
	for _, n := range []string{"n1", "n2"} {
		if _, err := addFact(context.Background(), s, "muse", fact.Meta{Name: n, Description: "a session note " + n, Type: "unclassified", Scope: "user", Source: fact.Source{Kind: "observed"}}, strings.Repeat("Several things happened. ", 1200)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(origin, name, desc string) string {
		return `[{"origin":"` + origin + `","name":"` + name + `","description":"` + desc + `","type":"machine_fact","scope":"user","kind":"observed","body":"Look there first."}]`
	}
	r1, _ := json.Marshal(map[string]any{"result": entry("n1", "zebra-receipts", "The zebra crossing tool writes its receipts under receipts/zebra in the repo root"), "total_cost_usd": .01})
	r2, _ := json.Marshal(map[string]any{"result": entry("n2", "zebra-receipts-again", "zebra crossing tool writes receipts under receipts/zebra in the root of the repo"), "total_cost_usd": .01})
	fake := filepath.Join(home, "fake-cli")
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Setenv("FIXTURE_R1", string(r1))
	t.Setenv("FIXTURE_R2", string(r2))
	script := "#!/bin/sh\nin=$(cat)\ncase \"$in\" in *'note name=n1 '*) printf '%s\\n' \"$FIXTURE_R1\";; *) printf '%s\\n' \"$FIXTURE_R2\";; esac\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := runCurate([]string{"--max-batches", "2"}); code != 0 {
		t.Fatalf("curate exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "user/zebra-receipts.md")); err != nil {
		t.Fatal("the first batch's fact was not written:", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "user/zebra-receipts-again.md")); err == nil {
		t.Fatal("the second batch wrote a copy")
	}
	state := loadCuratorState(filepath.Join(home, ".local/state/tacit/curator.json"))
	if state.Restated["n2"] != "zebra-receipts" {
		t.Fatalf("n2 not absorbed by the first batch's fact: %v", state.Restated)
	}
}
