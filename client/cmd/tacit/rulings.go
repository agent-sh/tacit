package main

// tacit rulings audit: a ruling is the owner's own words, provably (SPEC.md,
// Rulings). The audit reads every curated fact the old definition treated as
// a ruling (source.kind user, or type user_standard) and keeps it a ruling
// only with evidence: the owner wrote it in the app, a quoted phrase of the fact is
// their own (found in one of their turns or notes, long enough to state the rule,
// not their question and not negated), or the owner kept the fact themself in a review
// decision. The evidence goes into the frontmatter and the quote ledger;
// every other kind user fact becomes source.kind observed, its type unchanged.
// The audit puts nothing to the owner. A fact whose words of theirs are too few
// to prove the rule becomes observed with those words recorded as evidence
// (source.owner_words and a quote ledger entry marked words), which the
// verifier may not retire it against; a fact with no words of theirs found is a
// plain observation. The marker, written in the audit's own commit, switches
// the store to this definition. No model is called.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Audit outcomes.
const (
	ruledWritten  = "written"  // the owner wrote it in tacit app
	ruledQuote    = "quote"    // words of theirs, enough to state the rule
	ruledEndorsed = "endorsed" // the owner kept it in a review decision
	ruledShort    = "short"    // words of theirs, too few to prove the rule: observed, their words recorded
	ruledDisowned = "disowned" // their note says it is not theirs
	ruledAgent    = "agent"    // no words of theirs: an agent's statement
)

// rulingVerdict is the audit's answer for one fact.
type rulingVerdict struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Type       string   `json:"type"`
	KindBefore string   `json:"kind_before"`
	Live       bool     `json:"live"`
	Outcome    string   `json:"outcome"`
	Quote      string   `json:"quote,omitempty"`
	From       string   `json:"from,omitempty"`
	Endorsed   string   `json:"endorsed,omitempty"`
	DisownedBy string   `json:"disowned_by,omitempty"`
	Near       bool     `json:"near,omitempty"`    // their words with a typo the fact corrected
	Tried      []string `json:"tried,omitempty"`   // the quoted phrases checked, when none proved it
	Unclean    []string `json:"unclean,omitempty"` // phrases of theirs found only in a question or after a negation
	src        fact.Source
	turnHash   string
}

// Ruling reports whether the fact stays (or becomes) a ruling.
func (v rulingVerdict) Ruling() bool {
	return v.Outcome == ruledWritten || v.Outcome == ruledQuote || v.Outcome == ruledEndorsed
}

// changed reports whether the file's source block must change.
func (v rulingVerdict) changed(f *fact.Fact) bool { return v.src != f.Meta.Source }

// ownerText is text the owner wrote themself outside their transcripts: the
// words of their app notes (the blockquoted lines; the lines around them are
// the app's template) and the bodies of the rules the owner wrote in the app.
type ownerText struct {
	ref, words string
}

func ownerTexts(facts []*fact.Fact) []ownerText {
	var out []ownerText
	for _, f := range facts {
		if !fact.OwnerWrote(f) {
			continue
		}
		out = append(out, ownerText{ref: "note:" + f.Meta.Name, words: curate.OwnerWordsOf(f)})
	}
	return out
}

// loadStore walks the store and marks the inbox notes the app added in an
// owner commit (fact.Fact.AppNote), returning when each was added. A note in
// inbox/human@ that an agent wrote (its commit is not the app's) is not theirs.
func loadStore(ctx context.Context, dir string) ([]*fact.Fact, map[string]string, error) {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return nil, nil, err
	}
	added := appNoteCommits(ctx, dir)
	when := map[string]string{}
	for _, f := range facts {
		if c, ok := added[f.Path]; ok && f.InInbox() {
			f.AppNote = strings.HasPrefix(c.subject, ownerCommit)
			when[f.Meta.Name] = c.at
		}
	}
	return facts, when, nil
}

type addCommit struct{ subject, at string }

// appNoteCommits reads, for every note under inbox/human@*, the commit that
// added it: its subject and time.
func appNoteCommits(ctx context.Context, dir string) map[string]addCommit {
	out := map[string]addCommit{}
	log, err := store.Git(ctx, dir, "log", "--diff-filter=A", "--format=%x01%s%x02%cI", "--name-only", "--", ":(glob)inbox/human@*/**")
	if err != nil {
		return out
	}
	var cur addCommit
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, "\x01") {
			subj, at, _ := strings.Cut(l[1:], "\x02")
			cur = addCommit{subject: subj, at: at}
			continue
		}
		if l = strings.TrimSpace(l); l != "" {
			if _, seen := out[l]; !seen { // newest first: the first is the latest add
				out[l] = cur
			}
		}
	}
	return out
}

// disowningNote is a note of the owner's (written in the app) whose own words
// say facts are not theirs: "not owner wording", "interpretation of agents".
func disowningNote(f *fact.Fact) bool {
	return f.InInbox() && fact.OwnerWrote(f) && ownerwords.Disowns(curate.OwnerWordsOf(f))
}

// appNoteRx reads the app's template around their words: the finding's facts and
// their answer (owner_note.go noteFile). Notes written before 2026-10-11 say
// "His answer:", so both spellings are read.
var (
	appNoteAbout  = regexp.MustCompile(`on a review finding \(([a-z-]+)\) about (.+?)\. (?:His|The owner's) answer: ([^\n]*?)\.\n`)
	appNoteKeep   = regexp.MustCompile(`keep \[\[([^\]]+)\]\], retire \[\[([^\]]+)\]\]`)
	appNoteMarked = regexp.MustCompile(`neither is true|they conflict|both are true|outdated; retired|still true`)
)

// disownedTargets are the facts a disowning note is about: those their own words
// name; else the one fact of a single finding; else, on a pair, the side their
// answer marks (keep A, retire B: B; neither, conflict or both: both). A note
// with no answer on a pair names no side.
func disownedTargets(note *fact.Fact, exists func(string) bool) []string {
	words := curate.OwnerWordsOf(note)
	var named []string
	for _, l := range (&fact.Fact{Body: words}).Links() {
		named = append(named, l)
	}
	for _, w := range strings.Fields(strings.ToLower(words)) {
		w = strings.Trim(w, ".,;:!?()'\"")
		if fact.IsSlug(w) && strings.Contains(w, "-") && exists(w) {
			named = append(named, w)
		}
	}
	if len(named) > 0 {
		return dedupe(named)
	}
	links := note.Links()
	m := appNoteAbout.FindStringSubmatch(note.Body)
	if m == nil {
		return links // not the app's template: what it links
	}
	about := (&fact.Fact{Body: m[2]}).Links()
	switch {
	case len(about) == 1:
		return about
	case appNoteKeep.MatchString(m[3]):
		return []string{appNoteKeep.FindStringSubmatch(m[3])[2]}
	case appNoteMarked.MatchString(m[3]):
		return about
	}
	return nil
}

// disowning is one owner note that says a fact is not theirs, and when the owner wrote it.
type disowning struct{ note, at string }

// disownedFacts maps each fact their notes say is not theirs to the newest such note.
func disownedFacts(facts []*fact.Fact, when map[string]string) map[string]disowning {
	names := map[string]bool{}
	for _, f := range facts {
		if !f.InInbox() {
			names[f.Meta.Name] = true
		}
	}
	exists := func(n string) bool { return names[n] }
	out := map[string]disowning{}
	for _, f := range facts {
		if !disowningNote(f) {
			continue
		}
		at := when[f.Meta.Name]
		for _, t := range disownedTargets(f, exists) {
			if prev, ok := out[t]; !ok || at > prev.at {
				out[t] = disowning{note: f.Meta.Name, at: at}
			}
		}
	}
	return out
}

// later reports whether RFC 3339 time a is after b (unparsable times compare
// as text).
func later(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	if ea != nil || eb != nil {
		return a > b
	}
	return ta.After(tb)
}

// rulingAuditor holds what the audit checks a fact against.
type rulingAuditor struct {
	corpus   *ownerwords.Corpus
	notes    []ownerText
	disowned map[string]disowning
	endorsed map[string]ledger.Endorsement
	quotes   map[string]ledger.Quote
	strict   bool // exact words only: no corrected typos
	// recheck verifies a recorded quote again. Without it a quote the ledger
	// holds (verified when it was written) stands: transcripts are deleted
	// after a few weeks, and a ruling must not lapse with them.
	recheck bool
	// legacy is the pending list an older audit wrote into the marker
	// (ledger.LegacyPending): its facts are candidates again, and a short
	// quote it recorded stands as their words when no other words are found
	// (its transcript may be gone).
	legacy map[string]ledger.LegacyPending
}

func newRulingAuditor(facts []*fact.Fact, when map[string]string, decisionsJSONL, quotesJSONL []byte, corpus *ownerwords.Corpus) *rulingAuditor {
	return &rulingAuditor{corpus: corpus, notes: ownerTexts(facts), disowned: disownedFacts(facts, when),
		endorsed: ledger.Endorsements(decisionsJSONL), quotes: ledger.Quotes(quotesJSONL), legacy: map[string]ledger.LegacyPending{}}
}

// auditorFor loads what an auditor needs from the store at dir.
func auditorFor(ctx context.Context, dir string, corpus *ownerwords.Corpus) ([]*fact.Fact, *rulingAuditor, error) {
	facts, when, err := loadStore(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	dl, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.DecisionsPath)))
	ql, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
	a := newRulingAuditor(facts, when, dl, ql, corpus)
	if m, ok := ledger.ReadMarker(dir); ok {
		for _, p := range m.Pending {
			a.legacy[p.Name] = p
		}
	}
	return facts, a, nil
}

// candidate reports a fact the audit decides on: a curated fact the old
// definition treated as a ruling, one that is kind user or carries owner
// evidence (their words included) now, or one an older audit put to them (it
// took the fact off kind user, whatever its type).
func (a *rulingAuditor) candidate(f *fact.Fact) bool {
	if f.InInbox() || strings.HasPrefix(f.Meta.Scope, "derived/") {
		return false
	}
	_, legacy := a.legacy[f.Meta.Name]
	return f.Meta.Source.Kind == "user" || f.Meta.Type == "user_standard" || fact.ClaimsEvidence(f) || legacy
}

// found is where a phrase of theirs was found.
type found struct {
	from, turnHash string
	near, clean    bool
}

// where looks a phrase up: one of their notes or app rules, the given session,
// or any of their sessions; exact before near unless strict. ok is false when the owner
// never wrote it.
func (a *rulingAuditor) where(q, session string) (found, bool) {
	if !ownerwords.Usable(q) {
		return found{}, false
	}
	for _, n := range a.notes {
		if ownerwords.In(n.words, q) || (!a.strict && ownerwords.NearIn(n.words, q)) {
			return found{from: n.ref, turnHash: ledger.TurnHash(ownerwords.Normalize(n.words)), clean: true}, true
		}
	}
	if a.corpus == nil {
		return found{}, false
	}
	m, ok := a.corpus.Find(q, session, a.strict)
	if !ok {
		return found{}, false
	}
	return found{from: m.Ref, turnHash: m.TurnHash, near: m.Near, clean: m.Clean()}, true
}

// proof is the outcome of looking at a fact's quoted phrases.
type proof struct {
	quote          string
	at             found
	short          bool // their words, but too few to prove the rule
	tried, unclean []string
}

// findQuote looks for the owner's words among a fact's quoted phrases: a
// quote the ledger already holds for it first, then every phrase longest
// first. A phrase proves the rule when it is theirs, not their question nor
// negated, and long enough (ownerwords.ProvesRuling); a shorter clean one is
// kept as their words (short). Words of theirs the ledger already holds for the
// fact stand as its short words, like a recorded quote, unless recheck.
func (a *rulingAuditor) findQuote(f *fact.Fact) proof {
	p := a.scanQuotes(f)
	s := f.Meta.Source
	text := f.Meta.Description + "\n" + f.Body
	if !a.recheck && (p.quote == "" || p.short) {
		if s.OwnerWords != "" && ownerwords.In(text, s.OwnerWords) {
			if q, ok := a.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerWords, From: s.OwnerWordsFrom, Words: true}.Key()]; ok {
				p.short, p.quote, p.at = true, q.Quote, found{from: q.From, turnHash: q.TurnHash, clean: true}
			}
		}
	}
	// A short quote an older audit found and put to them stands as their words
	// when nothing else is found: it is the only record once the transcript
	// is gone.
	if l, ok := a.legacy[f.Meta.Name]; ok && p.quote == "" && l.Reason == "short-quote" && l.Quote != "" && l.From != "" && ownerwords.In(text, l.Quote) {
		p.short, p.quote, p.at = true, l.Quote, found{from: l.From, turnHash: legacyTurnHash, clean: true}
	}
	return p
}

// legacyTurnHash marks a words entry carried from an older audit's pending
// list, which recorded no hash of their turn.
const legacyTurnHash = "pending-list"

// scanQuotes is findQuote without the words the ledger holds.
func (a *rulingAuditor) scanQuotes(f *fact.Fact) proof {
	text := f.Meta.Description + "\n" + f.Body
	s := f.Meta.Source
	// A quote the ledger holds is theirs (verified when it was written). It
	// proves the rule only if it still passes ownerwords.ProvesRuling; else it
	// stands as their words, also once their transcript is gone.
	var recorded *proof
	if !a.recheck && s.OwnerQuote != "" && ownerwords.In(text, s.OwnerQuote) {
		if q, ok := a.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerQuote, From: s.OwnerQuoteFrom}.Key()]; ok {
			at := found{from: q.From, turnHash: q.TurnHash, clean: true}
			if ownerwords.ProvesRuling(q.Quote, f.Meta.Description, f.Meta.Name) {
				return proof{quote: q.Quote, at: at}
			}
			recorded = &proof{quote: q.Quote, at: at, short: true}
		}
	}
	qs := append(ownerwords.Quotes(text), ownerwords.Attributed(text)...)
	var p proof
	for _, q := range qs {
		at, ok := a.where(q, s.Session)
		switch {
		case !ok:
			p.tried = append(p.tried, q)
		case !at.clean:
			p.unclean = append(p.unclean, q)
		case !ownerwords.ProvesRuling(q, f.Meta.Description, f.Meta.Name):
			if !p.short {
				p.short, p.quote, p.at = true, q, at
			}
		default:
			return proof{quote: q, at: at}
		}
	}
	// A quote whose tail an agent reworded ("the same query run again" for
	// their "the same qurey run once more") still holds a run of their words:
	// six of them in a row, as a statement of theirs, are their words in the
	// fact, not proof that the reworded rule is theirs.
	if !p.short {
		for _, q := range p.tried {
			ws := strings.Fields(ownerwords.Normalize(q))
			for i := 0; i+ownerwords.ProofWords <= len(ws) && !p.short; i++ {
				run := strings.Join(ws[i:i+ownerwords.ProofWords], " ")
				if at, ok := a.where(run, s.Session); ok && at.clean {
					p.short, p.quote, p.at = true, run, at
				}
			}
		}
	}
	if recorded != nil && (p.quote == "" || len(ownerwords.Normalize(recorded.quote)) > len(ownerwords.Normalize(p.quote))) {
		p.short, p.quote, p.at = true, recorded.quote, recorded.at
	}
	return p
}

// decide is the verdict for one candidate.
func (a *rulingAuditor) decide(f *fact.Fact) rulingVerdict {
	v := rulingVerdict{Name: f.Meta.Name, Path: f.Path, Type: f.Meta.Type, KindBefore: f.Meta.Source.Kind,
		Live: f.Meta.Retired == "" && f.Meta.SupersededBy == ""}
	src := f.Meta.Source
	src.OwnerQuote, src.OwnerQuoteFrom, src.Endorsed, src.OwnerWords, src.OwnerWordsFrom = "", "", "", "", ""
	keep := func(outcome string) rulingVerdict {
		v.Outcome = outcome
		src.Kind = "user"
		v.src = src
		return v
	}
	if fact.OwnerWrote(f) {
		return keep(ruledWritten)
	}
	end := a.endorsed[f.Meta.Name]
	// The later of their words wins: a note that says the fact is not theirs over
	// an older keep, a keep (or This is mine) over an older note.
	dis, disowned := a.disowned[f.Meta.Name]
	if disowned && end.ID != "" && later(end.At, dis.at) {
		disowned = false
	}
	if disowned {
		v.Outcome, v.DisownedBy = ruledDisowned, dis.note
	} else if p := a.findQuote(f); p.quote != "" && !p.short {
		v.Quote, v.From, v.Near, v.turnHash = p.quote, p.at.from, p.at.near, p.at.turnHash
		src.OwnerQuote, src.OwnerQuoteFrom = strings.Join(strings.Fields(p.quote), " "), p.at.from
		src.Endorsed = end.ID
		return keep(ruledQuote)
	} else if end.ID != "" {
		v.Endorsed = end.ID
		src.Endorsed = end.ID
		return keep(ruledEndorsed)
	} else if p.short {
		// Their words, too few to prove the rule: an observation that carries
		// them as evidence (ledger-backed), asked about only on a real conflict.
		v.Outcome, v.Quote, v.From, v.Near, v.Tried, v.Unclean, v.turnHash = ruledShort, p.quote, p.at.from, p.at.near, p.tried, p.unclean, p.at.turnHash
		src.OwnerWords, src.OwnerWordsFrom = strings.Join(strings.Fields(p.quote), " "), p.at.from
	} else {
		v.Outcome, v.Tried, v.Unclean = ruledAgent, p.tried, p.unclean
	}
	if src.Kind == "user" {
		src.Kind = "observed"
	}
	v.src = src
	return v
}

// rulingsReport is one audit.
type rulingsReport struct {
	Corpus   ownerwords.Stats `json:"corpus"`
	Verdicts []rulingVerdict  `json:"verdicts"`
	Changed  []string         `json:"changed"` // paths whose source block changes
	Pushed   bool             `json:"pushed"`
}

// audit decides every candidate of facts.
func (a *rulingAuditor) audit(facts []*fact.Fact) *rulingsReport {
	rep := &rulingsReport{}
	if a.corpus != nil {
		rep.Corpus = a.corpus.Stats
	}
	for _, f := range facts {
		if !a.candidate(f) {
			continue
		}
		v := a.decide(f)
		rep.Verdicts = append(rep.Verdicts, v)
		if v.changed(f) {
			rep.Changed = append(rep.Changed, f.Path)
		}
	}
	return rep
}

// writes are the files an audit writes: every fact whose source block
// changes, the quote ledger with an entry for each quote that proves a rule
// and for each fact's words of theirs (marked words), and the marker. Nothing
// else.
func (rep *rulingsReport) writes(dir, head string, now time.Time) (map[string][]byte, error) {
	out := map[string][]byte{}
	byPath := map[string]rulingVerdict{}
	for _, v := range rep.Verdicts {
		byPath[v.Path] = v
	}
	for _, rel := range rep.Changed {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		f, err := fact.Parse(rel, data)
		if err != nil {
			return nil, err
		}
		f.Meta.Source = byPath[rel].src
		nb, err := fact.PatchSource(data, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		out[rel] = nb
	}
	ql, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
	have := ledger.Quotes(ql)
	add := ql
	for _, v := range rep.Verdicts {
		var q ledger.Quote
		switch v.Outcome {
		case ruledQuote:
			q = ledger.Quote{Name: v.Name, Quote: v.src.OwnerQuote, From: v.src.OwnerQuoteFrom, TurnHash: v.turnHash, At: now.Format("2006-01-02")}
		case ruledShort:
			q = ledger.Quote{Name: v.Name, Quote: v.src.OwnerWords, From: v.src.OwnerWordsFrom, TurnHash: v.turnHash, At: now.Format("2006-01-02"), Words: true}
		default:
			continue
		}
		if _, ok := have[q.Key()]; ok {
			continue
		}
		have[q.Key()] = q
		add = append(add, q.Line()...)
	}
	if len(add) != len(ql) {
		out[ledger.QuotesPath] = add
	}
	// The marker is the switch and nothing else: no list of facts put to them.
	// A marker written by an older audit (with such a list) is rewritten.
	old, hadMarker := ledger.ReadMarker(dir)
	m := ledger.Marker{Version: 1, AuditedHead: old.AuditedHead, At: old.At}
	if !hadMarker {
		m.AuditedHead, m.At = head, now.Format(time.RFC3339)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.MarkerPath)))
	if enc := m.Encode(); !hadMarker || string(enc) != string(raw) {
		out[ledger.MarkerPath] = enc
	}
	return out, nil
}

// errNoOwnerWords: their words could not be read, so nothing may be decided.
var errNoOwnerWords = errors.New("none of the owner's turns could be read on this machine; nothing is decided on an empty corpus (run where their transcripts are)")

// loadOwnerCorpus reads the owner's turns: every source on this machine
// (ownerwords.LoadAll), or only the session digests under digests when
// digestsOnly. An empty corpus is an error: deciding on it would demote every
// ruling (fail closed).
func loadOwnerCorpus(home, digests string, digestsOnly bool) (*ownerwords.Corpus, error) {
	var c *ownerwords.Corpus
	var err error
	if !digestsOnly {
		c, err = ownerwords.LoadAll(home, digests)
	} else {
		b := ownerwords.NewBuilder()
		if digests != "" {
			err = b.AddDigests(digests)
		}
		c = b.Build()
	}
	if err != nil {
		return nil, err
	}
	if c.Empty() {
		return nil, errNoOwnerWords
	}
	return c, nil
}

// whereFor is the curator's and the importer's check (curate.Where): a phrase
// proves their words only when it is theirs, not their question nor negated.
func whereFor(a *rulingAuditor) curate.Where {
	return func(q, session string) (string, string) {
		at, ok := a.where(q, session)
		if !ok || !at.clean {
			return "", ""
		}
		return at.from, at.turnHash
	}
}

func runRulings(args []string) int {
	if len(args) == 0 || args[0] != "audit" {
		fmt.Fprintln(os.Stderr, "usage: tacit rulings audit [--dry-run] [--strict] [--recheck] [--digests DIR] [--digests-only] [--sample N] [--seed N] [--json FILE]")
		return 2
	}
	fs := flag.NewFlagSet("rulings audit", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "decide and report, write nothing, do not sync")
	home, _ := os.UserHomeDir()
	digests := fs.String("digests", ownerwords.DefaultDigests(home), "session digests (eval/mine/digest.py output); empty for none")
	digestsOnly := fs.Bool("digests-only", false, "read the owner's words only from --digests, not from this machine's transcripts and prompt histories")
	sample := fs.Int("sample", 0, "print N random rulings kept and 2N random facts that are no longer rulings, for a hand check")
	seed := fs.Int64("seed", 1, "seed of the sample")
	jsonOut := fs.String("json", "", "write every verdict to this file")
	strict := fs.Bool("strict", false, "exact words only: a quote whose typo the fact corrected does not count")
	recheck := fs.Bool("recheck", false, "verify quotes the ledger already holds again (by default a recorded quote stands)")
	fs.Parse(args[1:])

	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "rulings: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && refuseWriter(context.Background(), s, "curator", "rulings") {
		return 2
	}
	corpus, err := loadOwnerCorpus(home, *digests, *digestsOnly)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rulings: reading the owner's turns:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var rep *rulingsReport
	if *dry {
		facts, a, err := auditorFor(ctx, s.Dir, corpus)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings:", err)
			return 2
		}
		a.strict, a.recheck = *strict, *recheck
		rep = a.audit(facts)
	} else {
		rep, err = rulingsApply(ctx, s, corpus, *strict, *recheck)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings:", err)
			return 1
		}
	}
	writeRulingsReport(os.Stdout, rep, *dry)
	if *sample > 0 {
		writeRulingsSample(os.Stdout, rep, *sample, *seed)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", " ")
		if err := os.WriteFile(*jsonOut, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "rulings:", err)
			return 2
		}
	}
	return 0
}

// rulingsApply runs the audit on a freshly synced clone under the store lock
// and writes the changed source blocks, the quote ledger and the marker in one
// named commit as curator (curatorWrite: lint gate, put back on any failure).
func rulingsApply(ctx context.Context, s *store.Store, corpus *ownerwords.Corpus, strict, recheck bool) (*rulingsReport, error) {
	if corpus.Empty() {
		return nil, errNoOwnerWords
	}
	var rep *rulingsReport
	pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		facts, a, err := auditorFor(ctx, s.Dir, corpus)
		if err != nil {
			return nil, "", err
		}
		a.strict, a.recheck = strict, recheck
		rep = a.audit(facts)
		head, _ := s.Head(ctx)
		files, err := rep.writes(s.Dir, head, time.Now())
		if err != nil {
			return nil, "", err
		}
		return files, rulingsMessage(rep), nil
	})
	if rep != nil {
		rep.Pushed = pushed
	}
	return rep, err
}

func rulingsMessage(rep *rulingsReport) string {
	c := rep.counts()
	return fmt.Sprintf("curator: rulings audit: %d kept as the owner's words (%d quoted, %d kept by them, %d written by them), %d kind user facts set to observed, %d observed with their words recorded, 0 put to them\n\n"+
		"A ruling is the owner's own words, provably (SPEC.md, Rulings). This commit holds the audit marker\n"+
		"(review/rulings-audit.json) that switches the store to that definition. No model was called.",
		c.kept(), c.byOutcome[ruledQuote], c.byOutcome[ruledEndorsed], c.byOutcome[ruledWritten], c.demotedKindUser, c.byOutcome[ruledShort])
}

type rulingCounts struct {
	byOutcome       map[string]int
	live            map[string]int
	demotedKindUser int // kind user before, observed now
	promoted        int // observed before, kind user now
	candidates      int
	liveCandidates  int
}

func (c rulingCounts) kept() int {
	return c.byOutcome[ruledQuote] + c.byOutcome[ruledEndorsed] + c.byOutcome[ruledWritten]
}

func (rep *rulingsReport) counts() rulingCounts {
	c := rulingCounts{byOutcome: map[string]int{}, live: map[string]int{}}
	for _, v := range rep.Verdicts {
		c.candidates++
		c.byOutcome[v.Outcome]++
		if v.Live {
			c.liveCandidates++
			c.live[v.Outcome]++
		}
		switch {
		case v.KindBefore == "user" && !v.Ruling():
			c.demotedKindUser++
		case v.KindBefore != "user" && v.Ruling():
			c.promoted++
		}
	}
	return c
}

func writeRulingsReport(w io.Writer, rep *rulingsReport, dry bool) {
	c := rep.counts()
	st := rep.Corpus
	var src []string
	for _, k := range []string{"transcript", "pi", "hermes", "digest", "claude-history", "codex-history"} {
		if st.Turns[k] > 0 {
			src = append(src, fmt.Sprintf("%s %d", k, st.Turns[k]))
		}
	}
	fmt.Fprintf(w, "owner's words: %d turns in %d sessions (%s); skipped %d agent sessions, %d of unknown origin and %d lines of agent text relayed in their turns\n",
		st.OwnerTurns, st.OwnerSessions, strings.Join(src, ", "), st.AgentSessions, st.UnknownSessions, st.RelayedLines)
	fmt.Fprintf(w, "facts treated as rulings before (kind user or type user_standard, curated): %d, live %d\n", c.candidates, c.liveCandidates)
	row := func(label, k string) {
		fmt.Fprintf(w, "  %-44s %4d  (live %d)\n", label, c.byOutcome[k], c.live[k])
	}
	row("kept: a quoted phrase is theirs, verbatim", ruledQuote)
	row("kept: the owner kept it in a review decision", ruledEndorsed)
	row("kept: the owner wrote it in tacit app", ruledWritten)
	row("observed, their words recorded as evidence", ruledShort)
	row("not a ruling: no owner words found", ruledAgent)
	row("not a ruling: their note says it is not theirs", ruledDisowned)
	fmt.Fprintf(w, "kind user set to observed: %d; observed set to kind user (evidence found): %d; files changed: %d\n", c.demotedKindUser, c.promoted, len(rep.Changed))
	fmt.Fprintf(w, "put to them: 0; pending: 0 (the audit asks them nothing; their words on %d observed fact(s) are evidence the verifier may not retire them against, and it asks them only on a real conflict)\n", c.byOutcome[ruledShort])
	switch {
	case dry:
		fmt.Fprintln(w, "dry run: nothing written")
	case rep.Pushed:
		fmt.Fprintln(w, "pushed as curator")
	default:
		fmt.Fprintln(w, "nothing to write")
	}
}

// writeRulingsSample prints n random kept rulings and 2n random facts that
// are no longer rulings, with the evidence, for a hand check against the
// transcripts.
func writeRulingsSample(w io.Writer, rep *rulingsReport, n int, seed int64) {
	var kept, not []rulingVerdict
	for _, v := range rep.Verdicts {
		if !v.Live {
			continue
		}
		if v.Ruling() {
			kept = append(kept, v)
		} else {
			not = append(not, v)
		}
	}
	r := rand.New(rand.NewSource(seed))
	pick := func(xs []rulingVerdict, k int) []rulingVerdict {
		r.Shuffle(len(xs), func(i, j int) { xs[i], xs[j] = xs[j], xs[i] })
		if len(xs) > k {
			xs = xs[:k]
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].Name < xs[j].Name })
		return xs
	}
	fmt.Fprintf(w, "\nsample: %d kept\n", n)
	for _, v := range pick(kept, n) {
		fmt.Fprintf(w, "- %s [%s] %s %s\n", v.Name, v.Outcome, v.From+v.Endorsed, oneLine(v.Quote))
	}
	fmt.Fprintf(w, "\nsample: %d no longer rulings\n", 2*n)
	for _, v := range pick(not, 2*n) {
		fmt.Fprintf(w, "- %s [%s, was %s%s] tried %d phrase(s)%s %s\n", v.Name, v.Outcome, v.KindBefore, map[bool]string{true: ", their words recorded", false: ""}[v.Outcome == ruledShort], len(v.Tried), map[bool]string{true: " " + v.DisownedBy, false: ""}[v.DisownedBy != ""], oneLine(v.Quote))
	}
}

func oneLine(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%q", strings.Join(strings.Fields(s), " "))
}

// curatorWrite is the curator's locked write for a step whose change is a
// function of the store: under the store lock it syncs, plans on the fresh
// clone, lints, writes, regenerates MEMORY.md, lints again and pushes once as
// curator, committing only the files it names. A lint finding the store did
// not have, a refusal or a failure puts every file back; a lost push race
// plans again (up to four times). It reports whether a commit was pushed.
func curatorWrite(ctx context.Context, s *store.Store, plan func() (map[string][]byte, string, error)) (bool, error) {
	pushed := false
	err := s.WithLock(ctx, func() error {
		for attempt := 0; attempt < 4; attempt++ {
			if err := s.SyncLocked(ctx); err != nil {
				var st *store.Stranded
				if !errors.As(err, &st) {
					return fmt.Errorf("sync: %w", err)
				}
			}
			files, msg, err := plan()
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return nil
			}
			// A file with changes nobody committed is not this step's to push.
			for rel := range files {
				if out, _ := store.Git(ctx, s.Dir, "status", "--porcelain", "--", rel); strings.TrimSpace(out) != "" {
					return fmt.Errorf("%s has uncommitted changes in the clone; nothing written", rel)
				}
			}
			rels := make([]string, 0, len(files)+1)
			for rel := range files {
				if !safeRel(rel) && rel != ledger.QuotesPath && rel != ledger.MarkerPath {
					return fmt.Errorf("refusing path %s", rel)
				}
				rels = append(rels, rel)
			}
			sort.Strings(rels)
			rels = append(rels, "MEMORY.md")
			head, _ := s.Head(ctx)
			before, err := lint.Run(s.Dir)
			if err != nil {
				return fmt.Errorf("lint could not run, nothing written: %w", err)
			}
			snap := takeSnapshot(s.Dir, rels)
			fail := func(err error) error {
				snap.restore(s)
				return err
			}
			for _, rel := range rels[:len(rels)-1] {
				if err := writeFileAtomic(s.Dir, rel, files[rel]); err != nil {
					return fail(err)
				}
			}
			all, _, err := fact.Walk(s.Dir)
			if err != nil {
				return fail(err)
			}
			// MEMORY.md names only facts the commit's tree holds.
			untracked := untrackedFacts(ctx, s)
			var tracked []*fact.Fact
			for _, f := range all {
				if !untracked[f.Path] {
					tracked = append(tracked, f)
				}
			}
			if err := os.WriteFile(filepath.Join(s.Dir, "MEMORY.md"), []byte(index.Generate(tracked)), 0o644); err != nil {
				return fail(err)
			}
			after, err := lint.Run(s.Dir)
			if err != nil {
				return fail(fmt.Errorf("lint could not run, nothing written: %w", err))
			}
			if fresh := newFindings(before, after); len(fresh) > 0 {
				return fail(fmt.Errorf("lint rejects the change, nothing written: %s", strings.Join(fresh, "; ")))
			}
			perr := s.CommitPushNamedOnceLocked(ctx, append(rels, ".gitattributes"), msg, "curator")
			var rej *store.Rejected
			switch {
			case perr == nil:
				pushed = true
				return nil
			case errors.Is(perr, store.ErrBehind):
				snap.restore(s)
				continue
			case errors.As(perr, &rej):
				search.Invalidate(head)
				return fail(fmt.Errorf("the server refused the change: %s", rej.Reason))
			default:
				undoKept(s, head, rels)
				return fail(perr)
			}
		}
		return errors.New("the store kept changing on the server; nothing written")
	})
	return pushed, err
}
