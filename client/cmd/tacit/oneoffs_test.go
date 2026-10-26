package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
)

// `tacit rulings one-offs` on a store curated before one-offs: a ruling
// whose quote the owner said in one session becomes their statement, which
// the ruling points to; one said in two sessions stays a ruling, and so do
// the one they endorsed and the one they wrote. A dry run writes nothing; the
// write lints clean; a second run finds nothing more.
func TestRulingsOneOffsBecomeStatements(t *testing.T) {
	r, c := rulingsRig(t)
	ctx := ctxMinute(t)
	if _, err := rulingsApply(ctx, r.s, c, false, false); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "2026-10-25T00:00:00Z"}.Encode(), 0o644)
	r.commitAs("format")

	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "theirs.jsonl"), []byte(strings.Join([]string{
		`{"type":"user","entrypoint":"cli","message":{"content":"please never rename public endpoints without asking me, ok?"}}`,
		`{"type":"user","entrypoint":"cli","message":{"content":"dont tag a release on fridays without cheking the changelog"}}`,
	}, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "again.jsonl"), []byte(`{"type":"user","entrypoint":"cli","message":{"content":"as before: never rename public endpoints without asking me"}}`+"\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", r.clone)

	head := r.serverHead()
	out := filepath.Join(t.TempDir(), "dry.json")
	if code := runOneOffs([]string{"--dry-run", "--digests", "", "--json", out}); code != 0 {
		t.Fatalf("dry run exit %d", code)
	}
	if r.serverHead() != head {
		t.Fatal("the dry run wrote")
	}
	var rep oneOffsReport
	if json.Unmarshal(readBytes(t, out), &rep) != nil {
		t.Fatal("no report")
	}
	why := map[string]string{}
	for _, it := range rep.Items {
		why[it.Name] = it.Why
	}
	for n, w := range map[string]string{
		"quoted":          "said in two sessions or more",
		"kept-in-review":  "endorsed by them",
		"owners-own-rule": "written by them in the app",
		"typo-fixed":      "said in one session",
		"observed-quoted": "said in one session (the ledger's; no transcript here holds it now)",
	} {
		if why[n] != w {
			t.Errorf("%s: %q, want %q (%+v)", n, why[n], w, why)
		}
	}
	if code := runOneOffs([]string{"--digests", "", "--json", out}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	gitIn(t, r.clone, "pull", "-q", "--ff-only")
	facts, _, err := fact.Walk(r.clone)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*fact.Fact{}
	for _, f := range facts {
		byName[f.Meta.Name] = f
	}
	if f := byName["quoted"]; f == nil || !fact.IsRuling(f) || f.Meta.SupersededBy != "" {
		t.Fatalf("said twice, no longer a ruling: %+v", f)
	}
	for _, n := range []string{"observed-quoted", "typo-fixed"} {
		f, st := byName[n], byName[n+"-said-once"]
		if f == nil || st == nil || f.Meta.SupersededBy != st.Meta.Name || !fact.IsStatement(st) || st.Meta.Source.OwnerWords != f.Meta.Source.OwnerQuote ||
			st.Meta.Source.OwnerWordsFrom != f.Meta.Source.OwnerQuoteFrom || !st.WordsBacked {
			t.Fatalf("%s: %+v -> %+v", n, f, st)
		}
	}
	if findings, err := lint.Run(r.clone); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}
	head = r.serverHead()
	if code := runOneOffs([]string{"--digests", ""}); code != 0 || r.serverHead() != head {
		t.Fatalf("a second run wrote: exit %d", code)
	}
}
