package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// A fact that pointed at a copy dedupe retires points at the fact kept:
// the chain of replacements ends at the live fact in one step.
func TestDedupeRepointsAChainToTheKeptFact(t *testing.T) {
	r := dedupeFixture(t)
	writeFact(t, r.clone, fact.Meta{Name: "cwd-older", Description: "An older wording the migration moved", Type: "machine_fact", Scope: "machines/host-a", SupersededBy: "cwd-resets-again"}, "x\n")
	r.commitIndex(r.clone, "a moved fact")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	if got := supersededOnServer(t, r, "machines/host-a/cwd-resets-again.md"); got != "cwd-resets" {
		t.Fatalf("the copy: %q", got)
	}
	if got := supersededOnServer(t, r, "machines/host-a/cwd-older.md"); got != "cwd-resets" {
		t.Fatalf("the older fact still points at the copy: %q", got)
	}
	r.assertClean("after dedupe")
}

// The facts of a folder that is no project move where their own evidence
// puts them: the checkout the paths they name are in, else the checkout the
// session that wrote them worked in; with no evidence, or two repositories,
// they stay (quiet).
func TestMigrateRefilesANotAProjectFolderByItsEvidence(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"),
		"TACIT_LOCAL": filepath.Join(home, "overlay"), "TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": "off"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	for _, n := range []string{"tool-a", "tool-b"} {
		dir := filepath.Join(home, "projects", n)
		os.MkdirAll(dir, 0o755)
		gitIn(t, dir, "init", "-q", "-b", "main")
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	for _, n := range []string{"tool-a", "tool-b"} {
		putRecordFile(t, s.Dir, testRec(record.KindRepo, "projects/"+n))
	}
	scratch := testRec(record.KindFolder, "projects/scratch")
	scratch.Status, scratch.Was[0].Status, scratch.Was[0].Why = record.StatusDead, record.StatusDead, assess.NotProject+"a scratch directory of agents"
	putRecordFile(t, s.Dir, scratch)
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	repoid.RecordWorks(reg, []repoid.WorkItem{{Root: filepath.Join(home, "projects", "tool-b"), Session: "s-b", By: "edit"}})
	put := func(name, session, body string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: "projects/scratch/" + name + ".md", Meta: fact.Meta{Name: name, Description: "note " + name + " about the build", Type: "history", Scope: "projects/scratch",
			AppliesTo: fact.AppliesTo{Repos: []string{"scratch"}}, Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed", Session: session}, Created: "2026-10-01"}, Body: body})
	}
	put("by-path", "s-x", "The generator lives in ~/projects/tool-a/gen/main.go.\n")
	put("by-session", "s-b", "The release script needs a clean tree.\n")
	put("no-evidence", "s-x", "Something about the weather.\n")
	put("two-repos", "s-x", "Compare ~/projects/tool-a/x with ~/projects/tool-b/y.\n")
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "a scratch folder")
	if _, err := migrateStep(context.Background(), s, migrateBatch, migrateCommits, false, nil); err != nil {
		t.Fatal(err)
	}
	facts, _, _ = fact.Walk(s.Dir)
	at := map[string]string{}
	for _, f := range facts {
		if f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			o := f.Meta.Origin
			if o == "" || f.Meta.Scope == "projects/scratch" {
				o = f.Meta.Name
			}
			at[o] = f.Meta.Scope
			if f.Meta.Scope != "projects/scratch" && strings.Join(f.Meta.AppliesTo.Repos, ",") == "scratch" {
				t.Errorf("%s keeps the old folder's name in applies_to.repos", f.Meta.Name)
			}
		}
	}
	want := map[string]string{"by-path": "projects/tool-a", "by-session": "projects/tool-b", "no-evidence": "projects/scratch", "two-repos": "projects/scratch"}
	for n, w := range want {
		if at[n] != w {
			t.Errorf("%s is in %q, want %q (%v)", n, at[n], w, at)
		}
	}
}

// A supersedes verdict code's checks refute goes back to the judge once
// with the checks; a second one is a review line and retires nothing. A
// changed answer is taken.
func TestJudgeRefusesASupersedeTheChecksRefute(t *testing.T) {
	s := &store.Store{Dir: t.TempDir()}
	nf := &fact.Fact{Meta: fact.Meta{Name: "new-path", Description: "Edit cmd/old/main.go for the port", Scope: "projects/widget"}}
	nf2 := &fact.Fact{Meta: fact.Meta{Name: "new-path-2", Description: "Edit cmd/old2/main.go for the port", Scope: "projects/widget"}}
	fails := []check.Result{{Kind: check.KindPath, Claim: "path cmd/old/main.go exists in widget", Source: "git:HEAD", Output: "no tracked path", Verdict: check.Fails}}
	holds := []check.Result{{Kind: check.KindPath, Claim: "path cmd/serve/main.go exists in widget", Source: "git:HEAD", Output: "tracked", Verdict: check.Holds}}
	pairs := []curate.Pair{{ExistingName: "old-path", NewName: "new-path"}, {ExistingName: "old-path-2", NewName: "new-path-2"}}
	refs := []judgedPair{
		{newIdx: 0, existing: &search.Hit{Name: "old-path", Path: "projects/widget/old-path.md"}, otherIdx: -1, newChecks: fails, otherChecks: holds},
		{newIdx: 1, existing: &search.Hit{Name: "old-path-2", Path: "projects/widget/old-path-2.md"}, otherIdx: -1, newChecks: fails, otherChecks: holds},
	}
	var prompts []string
	prev := judgeRun
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return `["supersedes", "supersedes"]`, 0, nil
		}
		return `["supersedes", "no"]`, 0, nil
	}
	t.Cleanup(func() { judgeRun = prev })
	rep := &curate.Report{}
	review, written, err := judgePairs(context.Background(), s, pairs, refs, []*fact.Fact{nf, nf2}, false, rep)
	if err != nil || len(written) != 0 {
		t.Fatalf("%v, wrote %v", err, written)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "CODE REFUSED YOUR \"supersedes\"") {
		t.Fatalf("%d prompts", len(prompts))
	}
	if len(review) != 1 || !strings.Contains(review[0], "- CHECKED: the judge said [[new-path]] replaces [[old-path]] twice") || !strings.Contains(review[0], "no tracked path") {
		t.Fatalf("review %q", review)
	}
	if rep.CheckRefused != 2 {
		t.Fatalf("report %+v", rep)
	}
}

// A session reads a project folder's facts in the place its record's roots
// name: on this machine, the longest root that holds the working directory,
// of a live folder that is not quiet.
func TestRootFolderOnlyThisHostsLongestLiveRoot(t *testing.T) {
	home := t.TempDir()
	host := store.ShortHost()
	recs := map[string]*record.Record{
		"projects/lab":     {Kind: record.KindFolder, Name: "lab", Status: record.StatusActive, Roots: []record.Root{{Host: host, Path: "~/work/lab"}}},
		"projects/lab-sub": {Kind: record.KindFolder, Name: "lab-sub", Status: record.StatusActive, Roots: []record.Root{{Host: host, Path: "~/work/lab/sub"}}},
		"projects/remote":  {Kind: record.KindFolder, Name: "remote", Status: record.StatusActive, Roots: []record.Root{{Host: "other-host", Path: "~/work/remote"}}},
		"projects/gone":    {Kind: record.KindFolder, Name: "gone", Status: record.StatusDead, Roots: []record.Root{{Host: host, Path: "~/work/gone"}}},
		"projects/moved":   {Kind: record.KindFolder, Name: "moved", Status: record.StatusActive, SuppressedBy: "projects/lab", Roots: []record.Root{{Host: host, Path: "~/work/moved"}}},
	}
	for cwd, want := range map[string]string{
		"work/lab": "lab", "work/lab/a/b": "lab", "work/lab/sub/c": "lab-sub", "work/labx": "", "work/remote": "", "work/gone": "", "work/moved": "", "": "",
	} {
		c := ""
		if cwd != "" {
			c = filepath.Join(home, cwd)
		}
		if got := rootFolder(recs, host, home, c); got != want {
			t.Errorf("%s: %q, want %q", cwd, got, want)
		}
	}
}

// The model's roots are checked: only this machine's, only folders that
// exist; none is fine (code fills them in from its evidence).
func TestAssessRefusesRootsCodeCannotConfirm(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "projects", "lab"), 0o755)
	a := &assessor{home: home, host: "box"}
	ok := assess.Decision{Roots: []record.Root{{Host: "box", Path: "~/projects/lab"}}}
	if ps := a.checkRoots(ok); len(ps) != 0 {
		t.Fatalf("an existing root refused: %v", ps)
	}
	bad := assess.Decision{Roots: []record.Root{{Host: "box", Path: "~/projects/nowhere"}, {Host: "other", Path: "~/projects/lab"}}}
	if ps := a.checkRoots(bad); len(ps) != 2 || !strings.Contains(ps[0], "is no folder") || !strings.Contains(ps[1], "another machine's") {
		t.Fatalf("problems %v", ps)
	}
	if ps := a.checkRoots(assess.Decision{}); len(ps) != 0 {
		t.Fatalf("no roots refused: %v", ps)
	}
}
