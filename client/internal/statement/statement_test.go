package statement

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/check"
)

func item(name, from, day string) Item {
	return Item{Name: name, Scope: "projects/storefront", Kind: "statement", Words: "words of " + name, From: from, Day: day, Context: "the name", Sessions: []string{from}}
}

func judged(name, says string, same ...Relation) Entry {
	return Entry{Kind: LogJudged, At: "2026-10-22", Name: name, Says: says, Same: same}
}

func kinds(acts []Action) []string {
	var out []string
	for _, a := range acts {
		s := a.Kind
		if a.Level != "" {
			s += ":" + a.Level + ":" + a.By
		}
		if a.Name != "" {
			s += ":" + a.Name
		}
		if a.Into != "" {
			s += "->" + a.Into
		}
		out = append(out, s)
	}
	return out
}

// The same choice said in two separate sessions becomes a decision; said
// twice in one session it stays a statement.
func TestPromoteAChoiceSaidInTwoSessions(t *testing.T) {
	st := State{Statements: map[string]Item{
		"a": item("a", "claude:s1", "2026-10-15"),
		"b": item("b", "claude:s2", "2026-10-21"),
	}, Log: []Entry{judged("a", Choice), judged("b", Choice, Relation{"a", Same})}}
	acts := Plan(st)
	if len(acts) != 1 || acts[0].Kind != ActPromote || acts[0].Level != LevelDecision || acts[0].By != BySessions || acts[0].Words.Name != "b" || len(acts[0].Sessions) != 2 {
		t.Fatalf("%v", kinds(acts))
	}
	if strings.Join(acts[0].Replaces, ",") != "a,b" {
		t.Fatalf("replaces %v", acts[0].Replaces)
	}
	st.Statements["b"] = item("b", "claude:s1", "2026-10-21")
	if acts := Plan(st); len(acts) != 0 {
		t.Fatalf("one session promoted: %v", kinds(acts))
	}
}

// A standing instruction climbs the one-off ladder: two sessions make a
// preference, a third a rule.
func TestInstructionLadder(t *testing.T) {
	st := State{Statements: map[string]Item{
		"a": item("a", "claude:s1", "2026-10-10"),
		"b": item("b", "codex:s2", "2026-10-11"),
	}, Log: []Entry{judged("a", Instruction), judged("b", Instruction, Relation{"a", Same})}}
	acts := Plan(st)
	if len(acts) != 1 || acts[0].Level != LevelPreference {
		t.Fatalf("%v", kinds(acts))
	}
	// The preference is written; a third session says it again.
	st = State{Statements: map[string]Item{"c": item("c", "claude:s3", "2026-10-12")},
		Promoted: map[string]Item{"pref": {Name: "pref", Kind: LevelPreference, Day: "2026-10-11", Sessions: []string{"claude:s1", "codex:s2"}}},
		Log: []Entry{judged("a", Instruction), judged("b", Instruction, Relation{"a", Same}),
			{Kind: LogPromoted, Fact: "pref", Level: LevelPreference, From: []string{"a", "b"}, Sessions: []string{"claude:s1", "codex:s2"}},
			judged("c", Instruction, Relation{"pref", Same})}}
	acts = Plan(st)
	if len(acts) != 1 || acts[0].Level != LevelRule || !contains(acts[0].Replaces, "pref") || len(acts[0].Sessions) != 3 {
		t.Fatalf("%v %+v", kinds(acts), acts)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Said once and a check agrees: a decision by the check. A check that
// disagrees flags it, once for one check output; the statement stays.
func TestCheckAgreesOrDisagrees(t *testing.T) {
	holds := []check.Result{{Kind: check.KindRepo, Claim: "storefront is the real name", Verdict: check.Holds, Output: "the main checkout's directory"}}
	fails := []check.Result{{Kind: check.KindRepo, Claim: "shop is the real name", Verdict: check.Fails, Output: "the main checkout's directory is storefront"}}
	st := State{Statements: map[string]Item{"a": item("a", "claude:s1", "2026-10-15")},
		Log: []Entry{judged("a", Choice)}, Checks: map[string][]check.Result{"a": holds}}
	acts := Plan(st)
	if len(acts) != 1 || acts[0].Level != LevelDecision || acts[0].By != ByCheck || len(acts[0].Checks) != 1 {
		t.Fatalf("%v", kinds(acts))
	}
	st.Checks["a"] = fails
	acts = Plan(st)
	if len(acts) != 1 || acts[0].Kind != ActFlag || acts[0].Hash == "" {
		t.Fatalf("%v", kinds(acts))
	}
	st.Log = append(st.Log, Entry{Kind: LogFlagged, Name: "a", Hash: acts[0].Hash})
	if acts := Plan(st); len(acts) != 0 {
		t.Fatalf("flagged again on the same check: %v", kinds(acts))
	}
	// A check with no verdict (unknown) neither promotes nor flags.
	st.Checks["a"] = []check.Result{{Verdict: check.Unknown}}
	if acts := Plan(st); len(acts) != 0 {
		t.Fatalf("%v", kinds(acts))
	}
}

// A newer statement that changes an older one's point supersedes it; one
// that changes a decision that stands is an optional item, the decision
// stays; a statement that makes a ruling's point again stands for it.
func TestSupersessionAndDecisionsThatStand(t *testing.T) {
	st := State{Statements: map[string]Item{
		"old": item("old", "claude:s1", "2026-10-01"),
		"new": item("new", "claude:s2", "2026-10-20"),
	}, Log: []Entry{judged("old", Choice), judged("new", Choice, Relation{"old", Changes})}}
	acts := Plan(st)
	if len(acts) != 1 || acts[0].Kind != ActSupersede || acts[0].Name != "old" || acts[0].Into != "new" {
		t.Fatalf("%v", kinds(acts))
	}
	st = State{Statements: map[string]Item{"s": item("s", "claude:s3", "2026-10-21")},
		Promoted: map[string]Item{"dec": {Name: "dec", Kind: LevelDecision, Day: "2026-10-10"}},
		Log:      []Entry{{Kind: LogPromoted, Fact: "dec", Level: LevelDecision, From: []string{"x"}}, judged("s", Choice, Relation{"dec", Changes})}}
	acts = Plan(st)
	if len(acts) != 1 || acts[0].Kind != ActDiffers || acts[0].Into != "dec" {
		t.Fatalf("%v", kinds(acts))
	}
	st = State{Statements: map[string]Item{"s": item("s", "claude:s3", "2026-10-21")},
		Promoted: map[string]Item{"rule": {Name: "rule", Kind: KindRuling, Day: "2026-10-15"}},
		Log:      []Entry{judged("rule", Choice), judged("s", Choice, Relation{"rule", Same})}}
	acts = Plan(st)
	if len(acts) != 1 || acts[0].Kind != ActSupersede || acts[0].Name != "s" || acts[0].Into != "rule" {
		t.Fatalf("%v", kinds(acts))
	}
}

// Parse keeps only judgements of the statements asked, each relation to an
// item listed under it, each claim well formed; the rest are problems.
func TestParseChecksTheAnswer(t *testing.T) {
	asks := []Ask{{S: item("a", "claude:s1", "2026-10-15"), Earlier: []Item{item("e", "claude:s0", "2026-10-01")}}, {S: Item{Name: "r", Kind: KindRuling}}}
	js, problems, err := Parse("```json\n["+
		`{"name":"a","says":"choice","same":[{"name":"e","relation":"same"},{"name":"zz","relation":"same"}]},`+
		`{"name":"r","says":"choice","claims":[{"kind":"canonical","name":"storefront"}]},`+
		`{"name":"x","says":"choice"}]`+"\n```", asks)
	if err != nil {
		t.Fatal(err)
	}
	if len(js) != 1 || js[0].Name != "r" || len(js[0].Claims) != 1 {
		t.Fatalf("kept %+v", js)
	}
	if p := strings.Join(problems["a"], " "); !strings.Contains(p, "zz is not an earlier item") {
		t.Fatalf("problems %v", problems)
	}
	if _, _, err := Parse("not json", asks); err == nil {
		t.Fatal("no error for prose")
	}
	out := Render(asks)
	if !strings.Contains(out, "### statement=a") || !strings.Contains(out, "you said once (2026-10-15, claude:s1)") || !strings.Contains(out, "### ruling=r") || !strings.Contains(out, "earlier e (statement") {
		t.Fatalf("render:\n%s", out)
	}
}

// Candidates are the earlier items sharing words or a folder name, most
// shared first.
func TestCandidatesShareWordsOrAFolder(t *testing.T) {
	s := Item{Name: "s", Words: "the real name is storefront, shop is a name for now", Context: "the storefront repository's name"}
	earlier := []Item{
		{Name: "far", Words: "use bun for scripts", Context: "tooling"},
		{Name: "near", Words: "keep storefront as the checkout name", Context: "renaming the checkout"},
	}
	got := Candidates(s, earlier, map[string]bool{"storefront": true, "shop": true})
	if len(got) != 1 || got[0].Name != "near" {
		t.Fatalf("%+v", got)
	}
}
