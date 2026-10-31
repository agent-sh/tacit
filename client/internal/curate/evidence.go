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

// SaidOn is the day (YYYY-MM-DD) the owner said quote in the turn a quote
// ledger line names (from, turnHash): the time their transcript records for
// that turn (ownerwords.Corpus.SaidOn); "" when no turn records one. A
// statement is dated by it, never by the day a step read the session.
type SaidOn func(quote, from, turnHash string) string

// SaidDay is the day the owner said quote (said), "" when no turn of theirs
// records its time. A day after notAfter (the day the fact or line is
// written; "" for no bound) is no day they said it, and is "" too.
func SaidDay(said SaidOn, quote, from, turnHash, notAfter string) string {
	if said == nil || from == "" || strings.HasPrefix(from, "note:") {
		return ""
	}
	if d := said(quote, from, turnHash); fact.IsDate(d) && (notAfter == "" || d <= notAfter) {
		return d
	}
	return ""
}

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
// the day they said it (their turn's time, said; the note's day, the day the
// dreamer read the session, only when no turn records one), and the body code
// writes: their words verbatim and dated, the context, the model's reading
// labelled as its own (fact.StatementBody). With statements false (a store
// whose format marker predates statements) the words stay on an observed
// fact of the entry's type, as owner_words, which every binary reads.
// Otherwise f stays an observed fact with no words of theirs. It returns the
// quote ledger entry (a words entry) and whether the words are theirs.
func ApplyStatement(f *fact.Fact, e Entry, note *fact.Fact, where Where, said SaidOn, statements bool) (ledger.Quote, bool) {
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
	written := f.Meta.Created
	day := f.Meta.Created
	if fact.IsDate(note.Meta.Created) && note.Meta.Created < day {
		day = note.Meta.Created
	}
	turn := SaidDay(said, q, from, hash, written)
	if turn != "" {
		day = turn
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
	return ledger.Quote{Name: f.Meta.Name, Quote: q, From: from, TurnHash: hash, At: written, Words: true, Said: turn}, true
}

// OneOff settles a ruling ApplyEvidence kept (SPEC.md, "One-offs"): words
// the owner said in one session are a statement scoped to their moment,
// never a standing rule. sessions is every session code finds the quote in, as a
// statement of theirs (ownerwords.Corpus.SaidIn). Said in two or more, it
// stays a ruling. Said in one (or none code can count), f becomes their
// statement as ApplyStatement writes one: type statement, their words in
// owner_words, the day they said it (the ruling's line's said, else their
// turn's time, else the day the note was read), the entry's context, the model's
// reading as the gloss; in a store whose format predates statements, an
// observed fact of the entry's type holding their words. Only the
// statements step's ladder (said again in a separate session, or a check
// that agrees) makes a decision or a rule of it. q is the ruling's quote
// ledger line; it returns the words line and whether f was changed.
func OneOff(f *fact.Fact, e Entry, q ledger.Quote, sessions []string, said SaidOn, statements bool) (ledger.Quote, bool) {
	if len(sessions) >= 2 || q.Quote == "" || q.From == "" || strings.HasPrefix(q.From, "note:") {
		return q, false
	}
	src := &f.Meta.Source
	words := strings.Join(strings.Fields(q.Quote), " ")
	src.Kind, src.OwnerQuote, src.OwnerQuoteFrom, src.Endorsed = "observed", "", "", ""
	src.OwnerWords, src.OwnerWordsFrom = words, q.From
	// The day they said it: their turn's (the ruling's ledger line names it),
	// never the day the note was read.
	written := f.Meta.Created
	saidOn := q.Said
	if !fact.IsDate(saidOn) || (written != "" && saidOn > written) {
		saidOn = SaidDay(said, words, q.From, q.TurnHash, written)
	}
	day := f.Meta.Created
	if saidOn != "" {
		day = saidOn
	}
	context := strings.Join(strings.Fields(e.Context), " ")
	if context == "" {
		context = strings.Join(strings.Fields(f.Meta.Description), " ")
	}
	if statements {
		gloss := strings.Join(strings.Fields(strings.ReplaceAll(f.Body, fmt.Sprintf(" In the owner's words: \"%s\"", words), "")), " ")
		f.Meta.Type = fact.StatementType
		f.Meta.Created = day
		f.Meta.Description = fact.ClipDescription(context, 200)
		f.Body = fact.StatementBody(day, words, context, gloss)
	} else if !ownerwords.In(f.Meta.Description+"\n"+f.Body, words) {
		f.Body = strings.TrimRight(f.Body, "\n") + fmt.Sprintf(" In the owner's words, said once (%s): \"%s\"\n", day, words)
	}
	return ledger.Quote{Name: f.Meta.Name, Quote: words, From: q.From, TurnHash: q.TurnHash, At: written, Words: true, Said: saidOn}, true
}
