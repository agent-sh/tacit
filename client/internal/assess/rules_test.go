package assess

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/record"
)

func repoCheck(name, verdict, renamed string) *check.RepoCheck {
	rc := &check.RepoCheck{Name: name, Verdict: verdict, Renamed: renamed}
	if renamed != "" {
		rc.Forge = "o/" + renamed
	}
	rc.Results = []check.Result{{Kind: check.KindRepo, Claim: "repository " + name + " exists", Source: "forge:github", Output: "not found: o/" + name, Verdict: verdict}}
	return rc
}

func TestEnforceRefusesAStatusForANameThatExistsNowhere(t *testing.T) {
	c := Candidate{Folder: "projects/widgit", Name: repoCheck("widgit", check.Fails, ""),
		Real: []RealRepo{{Name: "widget", Why: []string{"a near name"}}}}
	env := Checked{Repo: func(n string) check.RepoCheck { return *repoCheck(n, check.Holds, "") }}
	for _, d := range []Decision{
		{Kind: "repo", Status: "active"}, {Kind: "repo", Status: "dead"}, {Kind: "folder", Status: "active"}, {Kind: "none", Status: "dead"},
	} {
		ps := Enforce(c, d, env)
		if len(ps) != 1 || !strings.Contains(ps[0], "exists nowhere") || !strings.Contains(ps[0], "projects/widget") || !strings.Contains(ps[0], "not found: o/widgit") {
			t.Errorf("%+v: %v", d, ps)
		}
	}
	for _, d := range []Decision{
		{Kind: "repo", Status: "active", SuppressedBy: "projects/widget"}, {Kind: "repo", Status: "needs-review"},
	} {
		if ps := Enforce(c, d, env); len(ps) != 0 {
			t.Errorf("%+v refused: %v", d, ps)
		}
	}
	// With no candidate in the evidence, "not a project" is open too.
	c.Real = nil
	if ps := Enforce(c, Decision{Kind: "none", Status: "dead"}, env); len(ps) != 0 {
		t.Errorf("none with no candidate: %v", ps)
	}
	// A folder that exists on disk (paths in the evidence) is no absent name.
	c.Evidence = []record.Evidence{{Host: "box", Paths: []string{"~/projects/widgit"}}}
	if ps := Enforce(c, Decision{Kind: "folder", Status: "active"}, env); len(ps) != 0 {
		t.Errorf("a real folder: %v", ps)
	}
	// Unknown (no forge) refutes nothing.
	u := Candidate{Folder: "projects/widgit", Name: repoCheck("widgit", check.Unknown, "")}
	if ps := Enforce(u, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 0 {
		t.Errorf("unknown: %v", ps)
	}
}

func TestEnforceFollowsTheForgesRenames(t *testing.T) {
	env := Checked{
		Repo: func(n string) check.RepoCheck {
			if n == "old-name" {
				return *repoCheck(n, check.Fails, "new-name")
			}
			if n == "nowhere" {
				return *repoCheck(n, check.Fails, "")
			}
			return *repoCheck(n, check.Holds, "")
		},
		Record: func(f string) *record.Record { return &record.Record{Kind: record.KindRepo} },
	}
	// The current name never folds into the old one.
	cur := Candidate{Folder: "projects/new-name", Name: repoCheck("new-name", check.Holds, "")}
	ps := Enforce(cur, Decision{Kind: "repo", Status: "active", SuppressedBy: "projects/old-name"}, env)
	if len(ps) != 1 || !strings.Contains(ps[0], "renamed to new-name") {
		t.Fatalf("current into old: %v", ps)
	}
	// The old name gets no status of its own; folding it into the new one is fine.
	old := Candidate{Folder: "projects/old-name", Name: repoCheck("old-name", check.Fails, "new-name")}
	if ps := Enforce(old, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 1 || !strings.Contains(ps[0], "projects/new-name") {
		t.Fatalf("old active: %v", ps)
	}
	if ps := Enforce(old, Decision{Kind: "repo", Status: "active", SuppressedBy: "projects/new-name"}, env); len(ps) != 0 {
		t.Fatalf("old into new: %v", ps)
	}
	// Never into a name that exists nowhere.
	other := Candidate{Folder: "projects/thing", Name: repoCheck("thing", check.Holds, "")}
	if ps := Enforce(other, Decision{Kind: "repo", Status: "active", SuppressedBy: "projects/nowhere"}, env); len(ps) != 1 || !strings.Contains(ps[0], "exists nowhere") {
		t.Fatalf("into nowhere: %v", ps)
	}
}

func TestNamePatternsAndNearNames(t *testing.T) {
	if NamePattern("KV-013-g1") != "kv-#-g#" || NamePattern("kv-150-g2") != "kv-#-g#" || NamePattern("pikadon") != "pikadon" {
		t.Fatal("patterns")
	}
	for _, tc := range []struct {
		a, b string
		near bool
	}{
		{"valkey-reviewer-bot", "valkey-review-bot", true},
		{"valkey-reviewer", "valkey-review-bot", true},
		{"hermes-android", "hermes-android-app", true},
		{"valkey", "valkey-glide", true},
		{"tacit", "pikadon", false},
		{"kv-146-g1", "pikadon", false},
		{"a", "b", true},
	} {
		if _, ok := Near(tc.a, tc.b); ok != tc.near {
			t.Errorf("Near(%s, %s) = %v", tc.a, tc.b, ok)
		}
	}
}

func TestSortPutsTargetsFirstAndPatternsTogether(t *testing.T) {
	absent := repoCheck("x", check.Fails, "")
	cands := []Candidate{
		{Folder: "projects/kv-150-g2", Pattern: "kv-#-g#", Name: absent},
		{Folder: "projects/alpha", Pattern: "alpha"},
		{Folder: "projects/kv-013-g1", Pattern: "kv-#-g#"},
		{Folder: "projects/kv-140-g2", Pattern: "kv-#-g#", Name: absent},
		{Folder: "projects/zeta", Pattern: "zeta"},
	}
	SortCandidates(cands, func(string) string { return "" })
	var got []string
	for _, c := range cands {
		got = append(got, c.Folder)
	}
	want := "projects/alpha projects/kv-013-g1 projects/zeta projects/kv-140-g2 projects/kv-150-g2"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %v", got)
	}
}

func TestSimilarLinesGroupDecisions(t *testing.T) {
	recs := []*record.Record{
		{Folder: "projects/kv-1-g1", Kind: "repo", Status: "active", SuppressedBy: "projects/pikadon", Was: []record.Entry{{Why: "a worktree of pikadon"}}},
		{Folder: "projects/kv-2-g1", Kind: "repo", Status: "active", SuppressedBy: "projects/pikadon", Was: []record.Entry{{Why: "same"}}},
		{Folder: "projects/kv-3-g1", Kind: "repo", Status: "needs-review", Was: []record.Entry{{Why: "unclear"}}},
	}
	ls := SimilarLines("the same name pattern kv-#-g#", recs)
	if len(ls) != 3 || !strings.Contains(ls[1], "2 suppressed into projects/pikadon") || !strings.Contains(ls[2], "1 repo, needs-review") {
		t.Fatalf("%q", ls)
	}
}

func TestEnforceOneLiveFolderPerRepository(t *testing.T) {
	same := []check.Result{{Kind: check.KindRepo, Claim: "ginza and pikadon are one repository", Source: "checkouts", Output: "ginza is another name of the checkout pikadon", Verdict: check.Holds, Value: "pikadon"}}
	c := Candidate{Folder: "projects/ginza", Name: repoCheck("ginza", check.Holds, ""), Same: same}
	recs := map[string]*record.Record{"projects/pikadon": {Folder: "projects/pikadon", Kind: record.KindRepo, Status: "active"}}
	env := Checked{Repo: func(n string) check.RepoCheck { return *repoCheck(n, check.Holds, "") }, Record: func(f string) *record.Record { return recs[f] }}
	ps := Enforce(c, Decision{Kind: "repo", Status: "active"}, env)
	if len(ps) != 1 || !strings.Contains(ps[0], "same repository as projects/pikadon") || !strings.Contains(ps[0], "another name of the checkout pikadon") {
		t.Fatalf("a second live folder: %v", ps)
	}
	if ps := Enforce(c, Decision{Kind: "none", Status: "dead"}, env); len(ps) != 1 {
		t.Fatalf("none for another name of a project: %v", ps)
	}
	for _, d := range []Decision{{Kind: "repo", Status: "active", SuppressedBy: "projects/pikadon"}, {Kind: "repo", Status: "needs-review"}} {
		if ps := Enforce(c, d, env); len(ps) != 0 {
			t.Errorf("%+v refused: %v", d, ps)
		}
	}
	// The other way round: pikadon folded into ginza leaves ginza the live one.
	recs["projects/pikadon"].SuppressedBy = "projects/ginza"
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 0 {
		t.Fatalf("the winner refused: %v", ps)
	}
	// No record of the other name yet: nothing to fold into.
	delete(recs, "projects/pikadon")
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 0 {
		t.Fatalf("no other record: %v", ps)
	}
}
