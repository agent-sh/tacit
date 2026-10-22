package curate

import (
	"fmt"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// Where says where the owner said a phrase as a statement of theirs (not their
// question, not negated), as an owner_quote_from ref (claude:, codex:, pi: or
// hermes:<session>), preferring the given session, with the hash
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

// KindStatement is a split entry's kind for a statement of the owner's
// (SPEC.md, "Statements"): their words, said once, not sure to be a
// standing rule.
const KindStatement = "statement"

// ApplyStatement settles a split entry of kind statement. When its quote is
// in the note it came from and is a statement of the owner's (the note is
// one they wrote in the app, or where finds the words in their turns, not
// asked, not negated), f becomes their statement: type statement, kind
// observed, their words in owner_words and owner_words_from, created on
// the note's day (the day the dreamer read the session), and the body code
// writes: their words verbatim and dated, the context, the model's reading
// labelled as its own (fact.StatementBody). With statements false (a store
// whose format marker predates statements) the words stay on an observed
// fact of the entry's type, as owner_words, which every binary reads.
// Otherwise f stays an observed fact with no words of theirs. It returns the
// quote ledger entry (a words entry) and whether the words are theirs.
func ApplyStatement(f *fact.Fact, e Entry, note *fact.Fact, where Where, statements bool) (ledger.Quote, bool) {
	src := &f.Meta.Source
	src.OwnerQuote, src.OwnerQuoteFrom, src.Endorsed, src.OwnerWords, src.OwnerWordsFrom = "", "", "", "", ""
	src.Kind = "observed"
	q := strings.Join(strings.Fields(e.OwnerQuote), " ")
	if note == nil || q == "" {
		return ledger.Quote{}, false
	}
	if !ownerwords.In(note.Meta.Description+"\n"+note.Body, q) {
		return ledger.Quote{}, false
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
		return ledger.Quote{}, false
	}
	day := f.Meta.Created
	if fact.IsDate(note.Meta.Created) && note.Meta.Created < day {
		day = note.Meta.Created
	}
	context := strings.Join(strings.Fields(e.Context), " ")
	if context == "" {
		context = strings.Join(strings.Fields(e.Description), " ")
	}
	if statements {
		f.Meta.Type = fact.StatementType
		f.Meta.Created = day
		f.Meta.Description = fact.ClipDescription(context, 200)
		f.Body = fact.StatementBody(day, q, context, e.Body)
	} else if !ownerwords.In(f.Meta.Description+"\n"+f.Body, q) {
		f.Body = strings.TrimRight(f.Body, "\n") + fmt.Sprintf(" In the owner's words, said once (%s): \"%s\"\n", day, q)
	}
	src.OwnerWords, src.OwnerWordsFrom = q, from
	return ledger.Quote{Name: f.Meta.Name, Quote: q, From: from, TurnHash: hash, At: f.Meta.Created, Words: true}, true
}
