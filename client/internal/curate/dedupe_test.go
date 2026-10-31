package curate

import (
	"reflect"
	"sort"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// cf is a curated fact with its description as its body, created on day.
func cf(scope, name, day, desc string) *fact.Fact {
	return &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Scope: scope, Description: desc, Created: day, Source: fact.Source{Kind: "observed"}}, Body: desc + "\n"}
}

// ruling is a fact the store backs as the owner's ruling.
func ruling(f *fact.Fact, quote string) *fact.Fact {
	f.Audited, f.QuoteBacked = true, true
	f.Meta.Source = fact.Source{Kind: "user", OwnerQuote: quote, OwnerQuoteFrom: "claude:s1"}
	return f
}

func copiesOf(cs []Copies) map[string][]string {
	out := map[string][]string{}
	for _, c := range cs {
		for _, x := range c.Copies {
			out[c.Kept.Meta.Name] = append(out[c.Kept.Meta.Name], x.Meta.Name)
		}
		sort.Strings(out[c.Kept.Meta.Name])
	}
	return out
}

func TestDuplicatesKeepsTheBestCopyAndRetiresTheRest(t *testing.T) {
	const ws = "machines/ws"
	facts := []*fact.Fact{
		// Three wordings of one fact; the newest is kept.
		cf(ws, "cwd-resets-a", "2026-10-01", "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd"),
		cf(ws, "cwd-resets-b", "2026-10-03", "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd"),
		cf(ws, "cwd-resets-c", "2026-10-02", "The Bash tool resets cwd to ~/work after every call; start each command with cd"),
		// Same words, another port: an update, never a copy.
		cf(ws, "lantern-port", "2026-10-01", "The lantern proxy for the quokka scheduler listens on port 7781 on host-a"),
		cf(ws, "lantern-port-new", "2026-10-05", "The lantern proxy for the quokka scheduler listens on port 7790 on host-a"),
		// Same words, another plugin: never a copy.
		cf("user", "plugin-a-off", "2026-10-01", "The remember@plugins-official plugin is disabled in settings.json enabledPlugins; do not re-enable it"),
		cf("user", "plugin-b-off", "2026-10-02", "The security-guidance@plugins-official plugin is disabled in settings.json enabledPlugins; do not re-enable it"),
		// Same words, another scope: both stay.
		cf("projects/webapi", "zebra-receipts", "2026-10-01", "zebra crossing tool writes its receipts under receipts/zebra in the repo root"),
		cf("projects/other", "zebra-receipts-other", "2026-10-02", "zebra crossing tool writes its receipts under receipts/zebra in the repo root"),
		// A negation in one description: both stay.
		cf(ws, "warm-cache", "2026-10-01", "the api server needs the warm cache flag on host-a for the large index"),
		cf(ws, "warm-cache-not", "2026-10-02", "the api server does not need the warm cache flag on host-a for the large index"),
	}
	got := copiesOf(Duplicates(facts, DedupeGuard{}))
	want := map[string][]string{"cwd-resets-b": {"cwd-resets-a", "cwd-resets-c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Duplicates = %v, want %v", got, want)
	}
}

func TestDuplicatesNeverRetiresWhatTheOwnerOwns(t *testing.T) {
	const ws = "machines/ws"
	desc := "Never retry a failed deploy by hand in a loop; rerun it with the deploy tool and a timeout"
	newer := func(name string) *fact.Fact { return cf("user", name, "2026-10-09", desc) }
	words := cf("user", "deploy-words", "2026-10-01", desc)
	words.Meta.Source.OwnerWords, words.Meta.Source.OwnerWordsFrom = "rerun it with the deploy tool", "claude:s2"
	kept := cf(ws, "kept-by-owner", "2026-10-01", "The quokka build cache lives in ~/.cache/quokka and is safe to delete")
	answered := cf(ws, "answered-pair", "2026-10-01", "The quokka build cache lives in ~/.cache/quokka and is safe to delete")
	conflicted := cf(ws, "conflicted", "2026-10-01", "The quokka build cache lives in ~/.cache/quokka and is safe to delete")
	conflicted.Meta.ConflictsWith = []string{"other"}
	facts := []*fact.Fact{
		// Two rulings with other quotes: their words are never folded into each other.
		ruling(cf("user", "deploy-rule-1", "2026-10-01", desc), "never retry a failed deploy by hand in a loop"),
		ruling(cf("user", "deploy-rule-2", "2026-10-02", desc), "rerun it with the deploy tool and a timeout"),
		// An agent's copy of a ruling goes into the newest ruling, though the copy is newer still.
		newer("deploy-copy"),
		// A fact with words of theirs stays.
		words,
		cf("system", "deploy-system", "2026-10-01", desc),
		cf(ws, "quokka-cache", "2026-10-09", "The quokka build cache lives in ~/.cache/quokka and is safe to delete"),
		kept, answered, conflicted,
	}
	g := DedupeGuard{Kept: map[string]bool{"kept-by-owner": true}, Answered: func(a, b string) bool {
		return a == "answered-pair" && b == "quokka-cache" || a == "quokka-cache" && b == "answered-pair"
	}}
	got := copiesOf(Duplicates(facts, g))
	want := map[string][]string{"deploy-rule-2": {"deploy-copy"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Duplicates = %v, want %v", got, want)
	}
}

func TestDuplicatesReadsPathsSpelledTwoWays(t *testing.T) {
	const ws = "machines/ws"
	facts := []*fact.Fact{
		cf(ws, "notes-full", "2026-10-02", "Routing notes for the quokka provider are in ~/.claude/memory/routing.md; read them before changing the provider"),
		cf(ws, "notes-home", "2026-10-01", "Routing notes for the quokka provider are in /Users/u/.claude/memory/routing.md; read before changing the provider"),
		// A path the kept fact does not hold is new.
		cf(ws, "notes-other", "2026-10-01", "Routing notes for the quokka provider are in ~/.claude/memory/other.md; read them before changing the provider"),
	}
	got := copiesOf(Duplicates(facts, DedupeGuard{}))
	want := map[string][]string{"notes-full": {"notes-home"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Duplicates = %v, want %v", got, want)
	}
}

func TestAbsorberAbsorbsARestatedFactNotARuling(t *testing.T) {
	obs := cf("machines/ws", "cwd-resets", "2026-10-01", "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd")
	rule := ruling(cf("user", "deploy-rule", "2026-10-01", "Never retry a failed deploy by hand in a loop; rerun it with the deploy tool"), "never retry a failed deploy by hand in a loop")
	a := NewAbsorber([]*fact.Fact{obs, rule})
	split := func(scope, desc string) *fact.Fact { return cf(scope, "new", "2026-10-09", desc) }
	cases := []struct {
		f      *fact.Fact
		ruling bool
		want   string
	}{
		{split("machines/ws", "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd"), false, "cwd-resets"},
		// another scope
		{split("projects/webapi", "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd"), false, ""},
		// a new path
		{split("machines/ws", "Bash tool resets the cwd to /Users/u/other after each call, so start every command with cd"), false, ""},
		// an agent's restatement of a ruling is the ruling's
		{split("user", "Never retry a failed deploy by hand in a loop, rerun it with the deploy tool"), false, "deploy-rule"},
		// a ruling restating an observation stays
		{ruling(split("machines/ws", "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd"), "start every command with cd please"), true, ""},
		// a ruling with another quote stays; with the same quote it is the same ruling
		{ruling(split("user", "Never retry a failed deploy by hand in a loop, rerun it with the deploy tool"), "rerun it with the deploy tool every time"), true, ""},
		{ruling(split("user", "Never retry a failed deploy by hand in a loop, rerun it with the deploy tool"), "Never retry a failed deploy by hand in a loop"), true, "deploy-rule"},
	}
	for i, c := range cases {
		if got := a.Restates(c.f, c.ruling); got != c.want {
			t.Errorf("case %d: Restates = %q, want %q", i, got, c.want)
		}
	}
	// A fact the run retired absorbs nothing.
	a.Drop(obs.Path)
	if got := a.Restates(cases[0].f, false); got != "" {
		t.Fatalf("absorbed by a retired fact: %q", got)
	}
}

// Copies of one statement that only an owner could retire stay: system
// facts, rulings of a store the audit has not run on, a fact the owner
// endorsed. A command one copy names in code is a specific the other lacks.
func TestDuplicatesKeepsSystemPreAuditEndorsedAndCode(t *testing.T) {
	const ws = "machines/ws"
	desc := "The quokka build cache lives in ~/.cache/quokka and is safe to delete"
	pre := func(name, day string) *fact.Fact {
		f := cf(ws, name, day, desc)
		f.Meta.Source.Kind = "user" // unaudited store: kind user is a ruling
		return f
	}
	endorsed := cf("user", "endorsed-copy", "2026-10-01", desc)
	endorsed.Meta.Source.Endorsed, endorsed.EndorsedBacked = "0123456789ab", true
	// Two facts the owner endorsed: neither is ever a copy.
	en := func(name, day string) *fact.Fact {
		f := cf("projects/quokka", name, day, desc)
		f.Meta.Source.Endorsed, f.EndorsedBacked = "ab"+name, true
		return f
	}
	withCode := cf("user", "with-code", "2026-10-01", desc)
	withCode.Body = desc + "; clear it with `quokka cache purge --all`.\n"
	facts := []*fact.Fact{
		cf("system", "sys-a", "2026-10-01", desc), cf("system", "sys-b", "2026-10-02", desc),
		pre("pre-a", "2026-10-01"), pre("pre-b", "2026-10-02"),
		endorsed, cf("user", "plain-newer", "2026-10-05", desc),
		withCode,
		en("endorsed-a", "2026-10-01"), en("endorsed-b", "2026-10-02"),
	}
	// The endorsed fact is never the one retired: the newer plain copy goes
	// into it.
	got := copiesOf(Duplicates(facts, DedupeGuard{}))
	want := map[string][]string{"endorsed-copy": {"plain-newer"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Duplicates = %v, want %v", got, want)
	}
}

// A copy whose checked references hold is never retired into a fact whose
// references the checks refute (DedupeGuard.Refuted); the absorber the same.
func TestDuplicatesAndAbsorberHonourTheChecks(t *testing.T) {
	const ws = "machines/ws"
	a := cf(ws, "cwd-resets-a", "2026-10-01", "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd")
	b := cf(ws, "cwd-resets-b", "2026-10-03", "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd")
	if got := copiesOf(Duplicates([]*fact.Fact{a, b}, DedupeGuard{})); len(got) != 1 {
		t.Fatalf("without checks: %v", got)
	}
	refuted := func(kept, copy *fact.Fact) bool { return kept.Meta.Name == "cwd-resets-b" }
	if got := copiesOf(Duplicates([]*fact.Fact{a, b}, DedupeGuard{Refuted: refuted})); len(got) != 0 {
		t.Fatalf("a copy retired into a refuted fact: %v", got)
	}
	ab := NewAbsorber([]*fact.Fact{b})
	c := cf(ws, "", "2026-10-04", "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd")
	if ab.Restates(c, false) != "cwd-resets-b" {
		t.Fatal("without checks the absorber absorbs")
	}
	ab.Refuted = refuted
	if by := ab.Restates(c, false); by != "" {
		t.Fatalf("absorbed into a refuted fact: %s", by)
	}
}
