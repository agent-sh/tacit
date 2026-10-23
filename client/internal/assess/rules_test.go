package assess

import (
	"encoding/json"
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

func TestFingerprintIgnoresTheReasonTheRecordAndNewOwnerTurns(t *testing.T) {
	c := Candidate{Folder: "projects/x", Reason: "a", Context: []string{"facts: 2 live", `owner's turn (claude:s1): "x is fine"`}}
	fp := Fingerprint(c)
	d := c
	d.Reason, d.Old = "b", &record.Record{Kind: "repo", Status: "needs-review"}
	d.Context = []string{"facts: 2 live", `owner's turn (claude:s9): "x again"`}
	if Fingerprint(d) != fp {
		t.Fatal("the fingerprint moved with nothing about the folder changed")
	}
	d.Context = []string{"facts: 3 live"}
	if Fingerprint(d) == fp {
		t.Fatal("new evidence kept the fingerprint")
	}
}

func TestFingerprintKeepsVerdictsAndNamesNotWording(t *testing.T) {
	c := Candidate{Folder: "projects/x", Name: repoCheck("x", check.Fails, ""), Real: []RealRepo{{Name: "a", Why: []string{"2 facts"}}, {Name: "b", Why: []string{"near"}}}, ProposedKey: []string{"a"}}
	fp := Fingerprint(c)
	d := c
	d.Name = repoCheck("x", check.Fails, "")
	d.Name.Results[0].Output = "not found: o/x, p/x"
	// Proposals the live file system makes (b, near), and the cut, move nothing.
	d.Real = []RealRepo{{Name: "a", Why: []string{"3 facts"}}, {Name: "z", Why: []string{"near"}}}
	if Fingerprint(d) != fp {
		t.Fatal("wording moved the fingerprint")
	}
	d.ProposedKey = []string{"a", "c"}
	if Fingerprint(d) == fp {
		t.Fatal("a new candidate from stable evidence kept it")
	}
	d.ProposedKey = c.ProposedKey
	d.Name = repoCheck("x", check.Holds, "")
	if Fingerprint(d) == fp {
		t.Fatal("a new verdict kept it")
	}
}

// Find the ancestor: a folder with ancestor candidates stands alone only on
// independence evidence it cites; without any, it must fold.
func TestEnforceFindsTheAncestor(t *testing.T) {
	env := Checked{Repo: func(n string) check.RepoCheck { return *repoCheck(n, check.Holds, "") }}
	c := Candidate{Folder: "projects/wt-portfolio-rework", Name: repoCheck("wt-portfolio-rework", check.Unknown, ""),
		Ancestors: []Ancestor{{Name: "portfolio", Folder: "projects/portfolio", Sources: []string{SourceName}, Why: []string{"its name contains portfolio"}}}}
	for _, d := range []Decision{
		{Kind: "repo", Status: "active"}, {Kind: "repo", Status: "needs-review"}, {Kind: "none", Status: "dead"},
		{Kind: "folder", Status: "dormant", Independent: Keys{"commits"}},
	} {
		ps := Enforce(c, d, env)
		if len(ps) != 1 || !strings.Contains(ps[0], "presumed part of") || !strings.Contains(ps[0], "set suppressed_by to projects/portfolio") {
			t.Errorf("%+v: %v", d, ps)
		}
	}
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active", SuppressedBy: "projects/portfolio"}, env); len(ps) != 0 {
		t.Errorf("the fold refused: %v", ps)
	}
	// An owner's quote decides for the owner (verified by the caller).
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active", OwnerQuote: "wt-portfolio-rework is its own site", OwnerQuoteFrom: "claude:x"}, env); len(ps) != 0 {
		t.Errorf("the owner's decision refused: %v", ps)
	}
	// With its own commits it may stand alone, citing them.
	c.Independence = []Independence{{Key: "commits", Line: "12 own commits"}, {Key: "remote:github.com/o/wt", Line: "its own remote"}}
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 1 || !strings.Contains(ps[0], "code's keys: commits, remote:github.com/o/wt") {
		t.Errorf("uncited: %v", ps)
	}
	for _, k := range []Keys{{"commits"}, {"remote:github.com/o/wt"}, {"other", "commits"}} {
		if ps := Enforce(c, Decision{Kind: "repo", Status: "active", Independent: k}, env); len(ps) != 0 {
			t.Errorf("cited %v: %v", k, ps)
		}
	}
	if ps := Enforce(c, Decision{Kind: "repo", Status: "active", Independent: Keys{"made-up"}}, env); len(ps) != 1 {
		t.Errorf("a key code did not list: %v", ps)
	}
	// A folder with no ancestor is not asked for any.
	if ps := Enforce(Candidate{Folder: "projects/solo", Name: repoCheck("solo", check.Holds, "")}, Decision{Kind: "repo", Status: "active"}, env); len(ps) != 0 {
		t.Errorf("no ancestors: %v", ps)
	}
}

func TestAncestorsRenderAndFingerprint(t *testing.T) {
	c := Candidate{Folder: "projects/kvstore", Ancestors: []Ancestor{{Name: "agent-lab", Folder: "projects/agent-lab", Sources: []string{SourcePath}, Why: []string{"2 fact(s) name paths with a kvstore directory inside agent-lab's tree"}}}}
	out := Render([]Candidate{c})
	for _, want := range []string{"ancestor candidates (code's, best first; this folder is presumed part of one of them): 1. projects/agent-lab [path]", "independence: none found"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	d := c
	d.Independence = []Independence{{Key: "work", Line: "3 sessions worked in it"}}
	if Fingerprint(c) == Fingerprint(d) {
		t.Error("independence evidence is no part of the fingerprint")
	}
	if !strings.Contains(Render([]Candidate{d}), "independence [work]: 3 sessions worked in it") {
		t.Error("independence not rendered")
	}
	if p := c.Proposed(); len(p) != 1 || p[0].Name != "agent-lab" {
		t.Errorf("ancestors are proposed: %+v", p)
	}
	var k struct {
		A Keys `json:"a"`
		B Keys `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"commits","b":["work","forge:o/x"]}`), &k); err != nil || len(k.A) != 1 || k.A[0] != "commits" || len(k.B) != 2 {
		t.Fatalf("keys: %+v %v", k, err)
	}
}

// The folders of one ancestor sort together, the ancestor first, so it has
// a record before they fold into it.
func TestSortPutsTheAncestorFirstInItsGroup(t *testing.T) {
	absent := repoCheck("x", check.Fails, "")
	cands := []Candidate{
		{Folder: "projects/kv-2-g1", Group: "pikadon", Name: absent},
		{Folder: "projects/ginza", Group: "pikadon"},
		{Folder: "projects/kv-1-g1", Group: "pikadon", Name: absent},
		{Folder: "projects/pikadon", Group: "pikadon"},
		{Folder: "projects/alpha"},
	}
	SortCandidates(cands, func(string) string { return "" })
	var got []string
	for _, c := range cands {
		got = append(got, c.Folder)
	}
	if want := "projects/alpha projects/pikadon projects/ginza projects/kv-1-g1 projects/kv-2-g1"; strings.Join(got, " ") != want {
		t.Fatalf("order %v", got)
	}
}
