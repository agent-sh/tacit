package importer

import (
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// A memory its system files as the owner's is their ruling only with their words
// in it: a feedback memory or a Codex preference whose quoted phrase is in one
// of their turns becomes kind user with the evidence; one whose quote is not
// theirs, or that has none, is observed.
func TestImportKeepsKindUserOnlyWithHisWords(t *testing.T) {
	home := t.TempDir()
	mem := filepath.Join(home, ".claude/projects/-home-u/memory")
	write(t, filepath.Join(mem, "feedback_rename.md"), "---\nname: feedback_rename\ndescription: Renaming public endpoints needs approval\nmetadata:\n  type: feedback\n  originSessionId: s1\n---\n\nHe said \"never rename public endpoints without asking me first\". Ask before renaming.\n")
	write(t, filepath.Join(mem, "feedback_guess.md"), "---\nname: feedback_guess\ndescription: The owner prefers small PRs\nmetadata:\n  type: feedback\n---\n\nThe agent concluded the owner likes \"small focused pull requests always\".\n")
	write(t, filepath.Join(mem, "feedback_plain.md"), "---\nname: feedback_plain\ndescription: Keep answers short\nmetadata:\n  type: feedback\n---\n\nKeep answers short.\n")
	cdir := filepath.Join(home, "codex")
	write(t, filepath.Join(cdir, "memory_summary.md"), "## User preferences\n\n- The owner wants \"dont reset the shared cache on friday\" respected.\n- Prefers tabs.\n")

	b := ownerwords.NewBuilder()
	b.Add("transcript", "claude:s1", "please, never rename public endpoints without asking me first. thanks")
	b.Add("transcript", "codex:c1", "Dont reset the shared cache on friday, those are mine")
	c := b.Build()
	where := func(q, session string) string {
		if m, ok := c.Find(q, session, true); ok && m.Clean() {
			return m.Ref
		}
		return ""
	}
	store := t.TempDir()
	o := Options{Store: store, Host: "host-a", Home: home, OwnerWords: where}
	if r := Claude(o, mem); len(r.Errors) != 0 || r.Written != 3 {
		t.Fatalf("claude import %v %v", r, r.Errors)
	}
	if r := Codex(o, cdir); len(r.Errors) != 0 || r.Written != 2 {
		t.Fatalf("codex import %v %v", r, r.Errors)
	}
	facts, _, _ := fact.Walk(store)
	got := map[string]fact.Source{}
	for _, f := range facts {
		got[f.Meta.Description] = f.Meta.Source
	}
	rent := got["Renaming public endpoints needs approval"]
	if rent.Kind != "user" || rent.OwnerQuote != "never rename public endpoints without asking me first" || rent.OwnerQuoteFrom != "claude:s1" {
		t.Fatalf("a feedback memory with their words: %+v", rent)
	}
	for _, d := range []string{"The owner prefers small PRs", "Keep answers short", "Prefers tabs."} {
		if s := got[d]; s.Kind != "observed" || s.OwnerQuote != "" {
			t.Errorf("%q has no words of theirs but is %+v", d, s)
		}
	}
	var pref fact.Source
	for d, s := range got {
		if s.OriginalType == "user-preferences" && d != "Prefers tabs." {
			pref = s
		}
	}
	if pref.Kind != "user" || pref.OwnerQuoteFrom != "codex:c1" {
		t.Fatalf("a Codex preference quoting them: %+v", pref)
	}
	if fs, _ := lint.Run(store); len(fs) != 0 {
		t.Fatalf("lint: %v", fs)
	}
}
