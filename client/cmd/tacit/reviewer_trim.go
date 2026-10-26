package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Trims (SPEC.md, "Trims"): an uncertain note whose core holds is cut down
// by the reviewer to the claims its session supports, each cited; code
// checks the cut only removes or narrows (agentreview.CheckTrim). The
// trimmed note stands in the note's place as a trusted agent's note
// (verdict trimmed); the note stays where it is, its verdict replaced, its
// removed claims and their reasons in the ledger. When what is kept is a
// rule or a choice resting on one remark of the owner's, the result is their
// statement (their words verbatim, the day, the session, a line of context),
// never a rule: only the statements step's ladder promotes it.

// newTrimmedNote reports a path the curator may write in the inbox: a
// trimmed note (its name ends in -trimmed) that does not exist yet. Every
// other inbox note stays read-only: an agent's note is never edited.
func newTrimmedNote(dir, rel string) bool {
	if !strings.HasPrefix(rel, "inbox/") || !strings.HasSuffix(rel, "-trimmed.md") || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return errors.Is(err, os.ErrNotExist)
}

// trimmedName is the trimmed note's name.
func trimmedName(name string) string { return importer.Slug(name+"-trimmed", 80) }

// trimmedNote is the trimmed note: the note's own metadata (its harness,
// host, session, kind), its description and body the kept claims, its scope
// the trim's narrower one when it gives one, in the note's folder.
func trimmedNote(note *fact.Fact, t *agentreview.Trim) *fact.Fact {
	desc, body := t.Text()
	m := note.Meta
	m.Name = trimmedName(note.Meta.Name)
	m.Description, m.Scope = desc, note.Meta.Scope
	if t.Scope != "" {
		m.Scope = t.Scope
	}
	m.SupersededBy, m.Retired, m.ConflictsWith = "", "", nil
	f := &fact.Fact{Meta: m, Body: body}
	f.Path = path.Join(path.Dir(note.Path), m.Name+".md")
	return f
}

// ownerClaims are the kept claims that cite a message the owner typed
// (owner true), or those that cite none (owner false).
func ownerClaims(t *agentreview.Trim, msgs []agentreview.Msg, owner bool) []ledger.Claim {
	var out []ledger.Claim
	for _, c := range t.Claims {
		theirs := false
		for _, k := range c.Cites {
			if k >= 1 && k <= len(msgs) && msgs[k-1].Role == "user" && msgs[k-1].Owner {
				theirs = true
			}
		}
		if theirs == owner {
			out = append(out, c)
		}
	}
	return out
}

// trimStatement is the owner's remark a trimmed note rested on, as their
// statement (SPEC.md, "Statements"): their words verbatim, dated by the
// message they typed (else the note's day), the session, the reviewer's line
// of context, and the kept claims that cite the owner's messages as the
// labelled gloss; in the trim's scope or the note's. On this machine's layer
// it is the local curator's fact.
func trimStatement(note *fact.Fact, t *agentreview.Trim, msgs []agentreview.Msg, conv agentreview.Conversation, local bool) (*fact.Fact, ledger.Quote) {
	o := t.OneOff
	m := msgs[o.Line-1]
	day := note.Meta.Created
	if len(m.At) >= 10 && fact.IsDate(m.At[:10]) {
		day = m.At[:10]
	}
	scope := note.Meta.Scope
	if t.Scope != "" {
		scope = t.Scope
	}
	if scope == "" || !fact.ValidScope(scope) {
		scope = "user"
	}
	words := strings.Join(strings.Fields(o.Words), " ")
	// A curated fact's links must name facts (lint L6): the note's [[links]]
	// are plain names in the context and the gloss.
	context := unlink(strings.Join(strings.Fields(o.Context), " "))
	var gloss []string
	for _, c := range ownerClaims(t, msgs, true) {
		gloss = append(gloss, unlink(strings.Join(strings.Fields(c.Text), " ")))
	}
	from := conv.Ref()
	name := importer.Slug("owner-said-once-"+note.Meta.Name, 80)
	f := &fact.Fact{Meta: fact.Meta{
		Name: name, Description: fact.ClipDescription(context, 200), Type: fact.StatementType, Scope: scope, AppliesTo: note.Meta.AppliesTo,
		Source:  fact.Source{Harness: "curator", Host: store.ShortHost(), Kind: "observed", OwnerWords: words, OwnerWordsFrom: from},
		Created: day, // no origin: it is the owner's words, not a split of the note
	}, Body: fact.StatementBody(day, words, context, strings.Join(gloss, " "))}
	f.Path = scope + "/" + name + ".md"
	if local {
		f.Path = overlay.Prefix + overlay.CuratedDir + "/" + f.Path
	}
	q := ledger.Quote{Name: name, Quote: words, From: from, TurnHash: ledger.TurnHash(ownerwords.Normalize(m.Text)), At: day, Words: true}
	return f, q
}

// applyTrim turns a review whose trim passed code's check into the step's
// work: the note's line becomes replaced (its kept claims with their
// citations, its removed claims with their reasons, what stands in its
// place), the trimmed note (or the owner's statement) is written with its
// own line, and the note's curated facts point at it. ok is false when the
// trim cannot be applied (a statement in a store whose format predates
// statements, redaction unavailable): the note keeps its uncertain verdict.
func applyTrim(w *reviewWork, note *fact.Fact, res agentreview.Result, curated []*fact.Fact, local, statements bool) (into string, as string, ok bool) {
	t := *res.Trim
	line := res.Line
	if t.OneOff.Dropped() {
		// Not every sentence of the owner's is a statement (SPEC.md,
		// "Relevance"): their remark makes none, and the kept claims that
		// rest on it are removed with why; what the session shows otherwise
		// stays a trimmed note. With nothing left the note keeps its
		// uncertain verdict, the reason on its line.
		why := "rests on the owner's remark, which does not matter to later work (" + t.OneOff.Relevance.Category + ")"
		t.Removed = append([]ledger.Removed(nil), t.Removed...)
		for _, c := range ownerClaims(&t, res.Msgs, true) {
			t.Removed = append(t.Removed, ledger.Removed{Text: c.Text, Reason: why, Basis: ledger.BasisDropped})
		}
		t.Claims, t.OneOff = ownerClaims(&t, res.Msgs, false), nil
		if len(t.Claims) == 0 {
			return "", trimDropped, false
		}
	}
	if t.OneOff != nil && !local && !statements {
		return "", "", false
	}
	// The trim is the model's text: masked like any text written from a
	// conversation (fail closed).
	texts := []string{}
	for _, c := range t.Claims {
		texts = append(texts, c.Text)
	}
	if t.OneOff != nil {
		texts = append(texts, t.OneOff.Context)
	}
	red, err := digest.RedactAll(texts)
	if err != nil || len(red) != len(texts) {
		return "", "", false
	}
	for i := range t.Claims {
		t.Claims[i].Text = red[i]
	}
	if t.OneOff != nil {
		o := *t.OneOff
		o.Context = red[len(red)-1]
		t.OneOff = &o
	}
	line.Verdict, line.As, line.Kept, line.Removed = ledger.VerdictReplaced, t.As(), t.Claims, t.Removed
	if t.OneOff != nil {
		st, q := trimStatement(note, &t, res.Msgs, res.Conv, local)
		// Their words are the conversation's, verbatim: masked too.
		rw, err := digest.RedactAll([]string{st.Meta.Source.OwnerWords})
		if err != nil || rw[0] != st.Meta.Source.OwnerWords {
			return "", "", false // their words held something scrump masks: not written
		}
		line.Into = st.Meta.Name
		w.statements = append(w.statements, st)
		w.quotes = append(w.quotes, q)
		into = st.Meta.Name
		// The kept claims the owner's words do not hold (what the agent
		// observed and the session shows) stay a trimmed agent's note.
		if rest := ownerClaims(&t, res.Msgs, false); len(rest) > 0 {
			nt := t
			nt.Claims, nt.OneOff = rest, nil
			tn := trimmedNote(note, &nt)
			line.Also = tn.Path
			w.trimmed = append(w.trimmed, tn)
			w.lines = append(w.lines, trimmedLine(note, tn, line, len(rest), len(t.Removed)))
		}
	} else {
		tn := trimmedNote(note, &t)
		line.Into = tn.Path
		w.trimmed = append(w.trimmed, tn)
		w.lines = append(w.lines, trimmedLine(note, tn, line, len(t.Claims), len(t.Removed)))
		into = tn.Meta.Name
	}
	w.lines = append(w.lines, line)
	if !local {
		for _, f := range curated {
			w.supersede[f.Path] = into
		}
	}
	return into, line.As, true
}

// trimDropped is applyTrim's answer for a trim that rested only on a remark
// of the owner's the reviewer judged not to matter: nothing is written.
const trimDropped = "dropped"

// projectActive reports a repository whose project record (the winner when
// its folder is suppressed) is live and says active.
func projectActive(recs map[string]*record.Record) func(repo string) bool {
	return func(repo string) bool {
		if repo == "" {
			return false
		}
		folder := "projects/" + repo
		if w, _, err := record.Resolve(recs, folder); err == nil && w != "" {
			folder = w
		}
		r := recs[folder]
		return r.Live() && r.Status == record.StatusActive
	}
}

// ownerSettles is the owner's words that settle a note: a live statement or
// ruling in the note's own scope whose words of theirs (owner_words,
// owner_quote) the note holds, or whose content words are three fifths in
// the note's description (four content words at least: a phrase too short
// says too little). It returns why, "" for none.
func ownerSettles(facts []*fact.Fact) func(note *fact.Fact) string {
	return func(note *fact.Fact) string {
		text := note.Meta.Description + "\n" + note.Body
		for _, f := range facts {
			if f == note || f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || f.Meta.Scope != note.Meta.Scope {
				continue
			}
			words := ""
			switch {
			case fact.IsRuling(f):
				words = f.Meta.Source.OwnerQuote
			case fact.IsStatement(f):
				words = f.Meta.Source.OwnerWords
			}
			if strings.TrimSpace(words) == "" {
				continue
			}
			n := len(ownerwords.ContentStems(words))
			if ownerwords.In(text, words) || (n >= 4 && ownerwords.SharedStems(words, note.Meta.Description)*5 >= n*3) {
				return "your words on it are in the store: " + f.Meta.Name
			}
		}
		return ""
	}
}

// reviewChecks is the checks of a note's dry references (factChecks) with
// the store's checker; a variable, so tests need no checkouts.
var reviewChecks = func(ctx context.Context, ch *check.Checker, f *fact.Fact) []check.Result {
	return factChecks(ctx, ch, f)
}

// checkSettles is a check that settles a note: one of its dry references
// (a repository, a path, a version) that a check refutes. It returns why,
// "" for none. The checker is made once, at the first note that needs it.
func checkSettles(ctx context.Context, dir string, local bool) func(note *fact.Fact) string {
	var ch *check.Checker
	return func(note *fact.Fact) string {
		if ch == nil {
			ch = storeChecker(ctx, dir, local)
		}
		for _, r := range reviewChecks(ctx, ch, note) {
			if r.Disagrees() {
				return "a check disagrees: " + r.Line()
			}
		}
		return ""
	}
}

var wikiLink = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// unlink writes a text's [[links]] as plain names.
func unlink(s string) string { return wikiLink.ReplaceAllString(s, "$1") }

// trimmedLine is the trimmed note's own ledger line: trimmed, from the note.
func trimmedLine(note, tn *fact.Fact, of ledger.Review, kept, removed int) ledger.Review {
	return ledger.Review{Note: tn.Path, Name: tn.Meta.Name, Hash: ledger.NoteHash(tn.Meta.Description, tn.Body), Verdict: ledger.VerdictTrimmed,
		Reason: fmt.Sprintf("trimmed from %s to what its session supports: %d claim(s) kept, each cited; %d removed", note.Meta.Name, kept, removed),
		By:     ledger.ByTacit, Model: of.Model, Session: of.Session, At: of.At, Of: note.Path}
}
