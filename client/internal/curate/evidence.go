package curate

import (
	"fmt"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// Where says where the owner said a phrase as a statement of theirs (not their
// question, not negated), as an owner_quote_from ref (claude:<session>,
// codex:<session>), preferring the given session, with the hash
// of that turn for the quote ledger; "" when the owner did not say it anywhere it can
// see.
type Where func(quote, session string) (from, turnHash string)

// OwnerWordsOf is what the owner wrote themself in a note of harness human: the
// blockquoted lines of an app note (the lines around them are the app's
// template), else the whole note.
func OwnerWordsOf(note *fact.Fact) string {
	if q := ownerwords.BlockQuoted(note.Body); q != "" {
		return q
	}
	return note.Meta.Description + "\n" + note.Body
}

// ApplyEvidence settles a curated fact the split marked kind user: it stays a
// ruling only when a phrase of the owner's, quoted by the entry (owner_quote)
// or in its text, is in the note it came from, is theirs (the note is one the owner
// wrote in the app, or where finds it in their turns as a statement), and is
// long enough to state the rule (ownerwords.ProvesRuling). Otherwise the fact
// becomes kind observed. The quote goes into the fact's text when the model
// left it out, so the fact shows what it rests on. It returns the quote
// ledger entry of a ruling.
func ApplyEvidence(f *fact.Fact, e Entry, note *fact.Fact, where Where) (ledger.Quote, bool) {
	src := &f.Meta.Source
	src.OwnerQuote, src.OwnerQuoteFrom, src.Endorsed = "", "", ""
	if src.Kind != "user" {
		return ledger.Quote{}, false
	}
	src.Kind = "observed"
	if note == nil {
		return ledger.Quote{}, false
	}
	noteText := note.Meta.Description + "\n" + note.Body
	cands := []string{}
	if q := strings.TrimSpace(e.OwnerQuote); q != "" {
		cands = append(cands, q)
	}
	cands = append(cands, ownerwords.Quotes(f.Meta.Description+"\n"+f.Body)...)
	for _, q := range cands {
		// The phrase must be in the note (a quote the model wrote itself is not
		// the owner's) and say the rule, not a fragment of it.
		if !ownerwords.In(noteText, q) || !ownerwords.ProvesRuling(q, f.Meta.Description, f.Meta.Name) {
			continue
		}
		from, hash := "", ""
		switch {
		case fact.OwnerWrote(note):
			if w := OwnerWordsOf(note); ownerwords.In(w, q) {
				from, hash = "note:"+note.Meta.Name, ledger.TurnHash(ownerwords.Normalize(w))
			}
		case where != nil:
			from, hash = where(q, note.Meta.Source.Session)
		}
		if from == "" {
			continue
		}
		if !ownerwords.In(f.Meta.Description+"\n"+f.Body, q) {
			f.Body = strings.TrimRight(f.Body, "\n") + fmt.Sprintf(" In the owner's words: \"%s\"\n", strings.Join(strings.Fields(q), " "))
		}
		quote := strings.Join(strings.Fields(q), " ")
		src.Kind, src.OwnerQuote, src.OwnerQuoteFrom = "user", quote, from
		return ledger.Quote{Name: f.Meta.Name, Quote: quote, From: from, TurnHash: hash, At: f.Meta.Created}, true
	}
	return ledger.Quote{}, false
}
