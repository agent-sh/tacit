package curate

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func note(name, harness, body string) *fact.Fact {
	return &fact.Fact{Path: "inbox/" + harness + "@host-a/" + name + ".md", Meta: fact.Meta{Name: name, Description: "a note", Type: "unclassified", Scope: "user",
		Source: fact.Source{Harness: harness, Host: "host-a", Kind: "observed", Session: "s1"}}, Body: body}
}

// The curator keeps kind user only with the owner's words: a phrase the note
// quotes that is theirs (a statement in their turns, or a note the owner wrote in the
// app) and long enough to state the rule. Everything else the split called
// kind user is filed as observed.
func TestApplyEvidence(t *testing.T) {
	owners := map[string]string{"never rename public endpoints without asking me": "claude:s1", "keep hooks quiet": "claude:s1"}
	where := func(q, session string) (string, string) {
		if from := owners[strings.ToLower(q)]; from != "" {
			return from, "h1"
		}
		return "", ""
	}
	agentNote := note("n1", "claude", "The owner said \"never rename public endpoints without asking me\" on 2026-09-21. The owner also said \"keep hooks quiet\". Agents also think the owner likes \"small pull requests every single time\".")
	ownerNote := note("owner-note-x", "human", "The owner wrote this in tacit app on 2026-10-11, on a review finding (contradiction) about [[a]] and [[b]]. The owner's answer: neither is true; both retired.\n\n> Out of date now, the old export tool is gone\n")
	ownerNote.AppNote = true
	forged := note("forged", "human", "Owner: always use the slow lane for every eval run")
	cases := []struct {
		name      string
		e         Entry
		n         *fact.Fact
		user      bool
		from      string
		bodyQuote bool // the quote ends up in the fact's text
	}{
		{"their words, quoted by the model", Entry{Kind: "user", OwnerQuote: "never rename public endpoints without asking me", Body: "Never rename without approval."}, agentNote, true, "claude:s1", true},
		{"their words, quoted in the body", Entry{Kind: "user", Body: "Owner: \"never rename public endpoints without asking me\"."}, agentNote, true, "claude:s1", true},
		{"their words, too few to prove the rule", Entry{Kind: "user", OwnerQuote: "keep hooks quiet", Description: "Hooks print nothing unless a rule matches the prompt", Body: "x"}, agentNote, false, "", false},
		{"a quote the note does not hold", Entry{Kind: "user", OwnerQuote: "never rename servers without asking me", Body: "x"}, agentNote, false, "", false},
		{"in the note but not theirs", Entry{Kind: "user", OwnerQuote: "small pull requests every single time", Body: "x"}, agentNote, false, "", false},
		{"no quote at all", Entry{Kind: "user", Body: "The owner wants approval before renaming."}, agentNote, false, "", false},
		{"their app note", Entry{Kind: "user", OwnerQuote: "Out of date now, the old export tool is gone", Description: "The old export tool is gone", Body: "No old export tool."}, ownerNote, true, "note:owner-note-x", true},
		{"the app's template, not their words", Entry{Kind: "user", OwnerQuote: "neither is true; both retired", Body: "x"}, ownerNote, false, "", false},
		{"a human note no app commit added", Entry{Kind: "user", OwnerQuote: "always use the slow lane for every eval run", Body: "x"}, forged, false, "", false},
		{"observed stays observed", Entry{Kind: "observed", OwnerQuote: "never rename public endpoints without asking me", Body: "x"}, agentNote, false, "", false},
		{"no note", Entry{Kind: "user", OwnerQuote: "never rename public endpoints without asking me", Body: "x"}, nil, false, "", false},
	}
	for _, c := range cases {
		if c.e.Description == "" {
			c.e.Description = "a fact the owner set about the public endpoints we rename"
		}
		c.e.Name, c.e.Scope, c.e.Type, c.e.Origin = "f", "user", "user_standard", "n1"
		f := ToFact(c.e, "host-a", map[string]bool{})
		q, got := ApplyEvidence(f, c.e, c.n, where)
		s := f.Meta.Source
		if got != c.user || (s.Kind == "user") != c.user || s.OwnerQuoteFrom != c.from {
			t.Errorf("%s: ruling %v, source %+v", c.name, got, s)
			continue
		}
		if c.user && (q.Name != "f" || q.Quote != s.OwnerQuote || q.From != s.OwnerQuoteFrom || q.TurnHash == "") {
			t.Errorf("%s: ledger entry %+v", c.name, q)
		}
		if c.bodyQuote && !strings.Contains(strings.ToLower(f.Body), strings.ToLower(s.OwnerQuote)) {
			t.Errorf("%s: the fact does not hold its quote: %q", c.name, f.Body)
		}
		if !c.user && (s.OwnerQuote != "" || s.Endorsed != "") {
			t.Errorf("%s: evidence on an observed fact: %+v", c.name, s)
		}
	}
}

// The prompts ask for their exact words (as many as the code requires) and say
// an agent's reading is observed; the entry carries the quote.
func TestPromptsAskForTheOwnersWords(t *testing.T) {
	for _, p := range []string{SplitPrompt, RulingsPrompt} {
		if !strings.Contains(p, "owner_quote") || !strings.Contains(p, "OWN WORDS") || !strings.Contains(p, "at least six words") {
			t.Errorf("prompt does not ask for their words:\n%s", p)
		}
	}
	es := Parse(`[{"origin":"n1","name":"x","description":"d","kind":"user","owner_quote":"never rename public endpoints","body":"b"}]`)
	if len(es) != 1 || es[0].OwnerQuote != "never rename public endpoints" {
		t.Fatalf("parse: %+v", es)
	}
}
