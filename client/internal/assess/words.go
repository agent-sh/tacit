package assess

// The owner's words beat dates (the owner, 2026-10-23: "quote of me is
// stronger than dates, beside if its seems like i changed my mind or so").
// Code finds the store's notes in other folders that name a folder, the
// owner's rulings, statements and decisions first, and shows them before
// the first ask. When the model reads one as the owner's word on the
// folder's status (dead, shipped, paused, replaced by another), it cites
// the words verbatim (owner_status), and code checks them like a
// statement: the words are in that note, and the note is one code showed
// for this folder. The newest such words set the status against the dates,
// unless the owner's real work on the folder after the day they said them
// shows a changed mind (Changes: sessions that worked there, commits that
// change more than maintenance files); then the dates stand, and the record
// says so. Activity before the words never overrides them.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/record"
)

// Tiers of a note that names a folder: the owner's own words first.
const (
	TierRuling    = 0 // an owner ruling (their words, proven)
	TierStatement = 1 // a statement of theirs (their words, said once)
	TierWords     = 2 // a fact holding words of theirs the ledger backs
	TierDecision  = 3 // a decision recorded for them (type design_decision)
	TierOther     = 4 // any other note: evidence, not their word
)

// Note is a live fact outside a folder that names it (its name or an alias,
// as a word, a path or in applies_to.repos), gathered by code before the
// first ask.
type Note struct {
	Name, Scope, Type, Created, Session string
	Tier                                int
	// Said is the day the owner said the words a statement, a ruling or a
	// fact holding their words rests on (their turn's day, from the quote
	// ledger or their transcript; "" when neither records it); Description and
	// Body are the fact's text, which an owner_status item's words must be in.
	Said, Description, Body string
	// Projects are the other project folders code finds its text names, each
	// with where its name leads now ("gishra (renamed: tower-crane)").
	Projects []string
	// Claim: an agent's note whose text claims the owner's authority ("Owner
	// decided ...") without their words, by delivery's check
	// (search.FactClaims). It is TierOther, never the owner's word on a
	// status or a successor, and no dead or shipped answer rests on it
	// against the dates.
	Claim bool
}

// OwnerTier reports a note whose words may be the owner's word on a
// folder's status: a ruling, a statement, words of theirs, a decision.
func (n Note) OwnerTier() bool { return n.Tier <= TierDecision }

func (n Note) tierLabel() string {
	switch n.Tier {
	case TierRuling:
		if n.Said != "" {
			return "the owner's ruling, said " + n.Said
		}
		return "the owner's ruling"
	case TierStatement:
		return "the owner's statement, said once " + n.Said
	case TierWords:
		if n.Said != "" {
			return "holds the owner's words, said " + n.Said
		}
		return "holds the owner's words"
	case TierDecision:
		return "a decision"
	}
	if n.Claim {
		return "an agent's note that claims the owner's authority without their words: evidence only, not their word"
	}
	return "a note"
}

// SortNotes orders notes as the prompt shows them: the owner's tiers first,
// then newest first, then by name (one order every run).
func SortNotes(ns []Note) {
	sort.SliceStable(ns, func(i, j int) bool {
		if ns[i].Tier != ns[j].Tier {
			return ns[i].Tier < ns[j].Tier
		}
		if ns[i].Created != ns[j].Created {
			return ns[i].Created > ns[j].Created
		}
		return ns[i].Name < ns[j].Name
	})
}

// NotesShown bounds the notes a prompt lists for one folder.
const NotesShown = 8

// noteLines are the prompt's lines of c's notes.
func (c Candidate) noteLines() []string {
	if len(c.Notes) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("notes in other folders that name it (code's store search, the owner's rulings, statements and decisions first; %d shown of %d):", min(len(c.Notes), NotesShown), c.NotesTotal)}
	for i, n := range c.Notes {
		if i == NotesShown {
			break
		}
		// The owner's notes with their text (their words are quoted from it);
		// the others, evidence only, by their description.
		text := oneLine(n.Description, 120)
		if n.OwnerTier() {
			text = oneLine(n.Description, 220)
			if b := oneLine(n.Body, 220); b != "" {
				text += " | " + b
			}
		}
		if len(n.Projects) > 0 {
			text += " [names projects: " + strings.Join(n.Projects, ", ") + "]"
		}
		out = append(out, fmt.Sprintf("  - [[%s]] (%s, %s, %s; %s): %s", n.Name, n.Scope, n.Type, n.Created, n.tierLabel(), text))
	}
	return out
}

// OwnerWords is one item of a decision's owner_status: the owner's word on
// the folder's status, as the model reads a note code showed.
type OwnerWords struct {
	// Says is the status the words give: dead (obsolete, removed, to be
	// deleted, replaced), shipped (done and delivered) or dormant (paused).
	Says string `json:"says"`
	// Words are copied exactly from the note; From is note:<name>.
	Words string `json:"words"`
	From  string `json:"from"`
	// Successor: the project the words say replaces this one, if any.
	Successor string `json:"successor,omitempty"`
	// Temporal: the words of Words that limit them in time ("for now"), as
	// the model reads them; code refuses dead on them.
	Temporal string `json:"temporal,omitempty"`
}

// OwnerWordsList reads a JSON object or an array of them.
type OwnerWordsList []OwnerWords

// UnmarshalJSON reads one item or a list.
func (l *OwnerWordsList) UnmarshalJSON(b []byte) error {
	if strings.TrimSpace(string(b)) == "null" {
		*l = nil
		return nil
	}
	var one OwnerWords
	if err := json.Unmarshal(b, &one); err == nil {
		if one.Words == "" && one.From == "" && one.Says == "" {
			*l = nil
		} else {
			*l = OwnerWordsList{one}
		}
		return nil
	}
	var many []OwnerWords
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// Said is a verified item of owner_status: its note, its day (code's) and
// the successor's name as the words give it.
type Said struct {
	OwnerWords
	Note Note
	Day  string
}

var dayRx = regexp.MustCompile(`\b(20[0-9]{2}-[01][0-9]-[0-3][0-9])\b`)

// norm is text compared for verbatim words: lower case, one space.
func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// noteDay is the day the owner said what a note records: a statement's own
// day; else the first day the words name, then the note's description, then
// its body (none later than the note); else the day the note was made.
func noteDay(n Note, words string) string {
	if n.Said != "" {
		return n.Said
	}
	for _, t := range []string{words, n.Description, n.Body} {
		for _, m := range dayRx.FindAllString(t, -1) {
			if n.Created == "" || m <= n.Created {
				return m
			}
		}
	}
	return n.Created
}

// NameIn reports a project name in text as words, its dashes as dashes or
// spaces.
func NameIn(text, name string) bool { return nameIn(text, name) }

// nameIn reports a project name in text as words, its dashes as dashes or
// spaces ("tower-crane" in "Tower Crane replaces them").
func nameIn(text, name string) bool {
	t, n := strings.ToLower(text), strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	if record.WholeWord(t, n) {
		return true
	}
	if strings.Contains(n, "-") {
		return record.WholeWord(strings.ReplaceAll(t, "-", " "), strings.ReplaceAll(n, "-", " "))
	}
	return false
}

// listSplitRx splits words into clauses: at sentence and clause marks.
var listSplitRx = regexp.MustCompile(`[.;:()!?\n]+`)

// listItemRx splits a clause into the items of an enumeration: at commas,
// "and", "or", "&".
var listItemRx = regexp.MustCompile(`(?i),|\s+(?:and|or|&)\s+`)

// coListed reports words that list a successor's name and this folder's as
// items of one enumeration ("deslop, sync-docs, drift-detect ... fold into
// one tool"): the words retire both, so the one is no successor of the
// other. An item is short (at most 4 words), as an enumeration's are.
func coListed(words, folder, succ string) bool {
	for _, clause := range listSplitRx.Split(words, -1) {
		self, other := false, false
		for _, it := range listItemRx.Split(clause, -1) {
			if len(strings.Fields(it)) > 4 {
				continue
			}
			s, o := nameIn(it, folder), nameIn(it, succ)
			if s && o {
				continue // one item naming both ("tower replaces oldtool") is no list of them
			}
			self = self || s
			other = other || o
		}
		if self && other {
			return true
		}
	}
	return false
}

// names are the folder's names a cited sentence may name it by: its folder
// name and the other names code found for it (Candidate.Names).
func (c Candidate) names() []string {
	out := []string{strings.ToLower(strings.TrimPrefix(c.Folder, "projects/"))}
	for _, n := range c.Names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && n != out[0] {
			out = append(out, n)
		}
	}
	return out
}

// namedIn reports words that name the folder by one of its names (a forge
// name owner/name by its last part too).
func (c Candidate) namedIn(words string) bool {
	for _, n := range c.names() {
		if nameIn(words, n) {
			return true
		}
		if i := strings.LastIndex(n, "/"); i >= 0 && nameIn(words, n[i+1:]) {
			return true
		}
	}
	return false
}

// CheckOwnerWords checks d's owner_status items against the notes code
// showed for c: each from a note of the owner's tiers among them, its words
// verbatim in that note, says dead, shipped or dormant, a successor (dead
// only) named in the cited words themselves together with this folder (its
// name or another name of it: the same words name both, whichever sentence
// of the note is cited; not elsewhere in the note, and not listed with this
// folder as one more thing the words retire: then it is no successor, and
// dead with no successor is what they say). It returns the verified items,
// newest first (the newest supersedes the older), and the problems of the
// others.
func CheckOwnerWords(c Candidate, d Decision) ([]Said, []string) {
	var ok []Said
	var ps []string
	byName := map[string]Note{}
	for i, n := range c.Notes {
		if i == NotesShown {
			break
		}
		byName[n.Name] = n
	}
	for i, it := range d.OwnerStatus {
		label := fmt.Sprintf("owner_status[%d]", i)
		from := strings.TrimSpace(it.From)
		name, isNote := strings.CutPrefix(from, "note:")
		n, shown := byName[strings.TrimPrefix(strings.TrimSuffix(name, "]]"), "[[")]
		switch {
		case it.Says != record.StatusDead && it.Says != record.StatusShipped && it.Says != record.StatusDormant:
			ps = append(ps, fmt.Sprintf("%s.says: dead, shipped or dormant (got %q)", label, it.Says))
			continue
		case !isNote || !shown:
			ps = append(ps, fmt.Sprintf("%s.from: name one of the notes code showed for this folder as note:<name> (got %q)", label, from))
			continue
		case n.Claim:
			ps = append(ps, fmt.Sprintf("%s.from: note:%s claims the owner's authority without their words (no quote of theirs found): an agent's note, not their word; cite it in why as evidence instead", label, n.Name))
			continue
		case !n.OwnerTier():
			ps = append(ps, fmt.Sprintf("%s.from: note:%s is %s, not the owner's ruling, statement, words or decision: cite it in why as evidence instead", label, n.Name, n.tierLabel()))
			continue
		}
		w := norm(it.Words)
		if len(strings.Fields(w)) < 2 || !strings.Contains(norm(n.Description+"\n"+n.Body), w) {
			ps = append(ps, fmt.Sprintf("%s.words: copy the words exactly from note:%s (two words or more)", label, n.Name))
			continue
		}
		if p := temporalProblem(label+".temporal", it.Temporal, it.Words, it.Says); p != "" {
			ps = append(ps, p)
			continue
		}
		succ := strings.TrimPrefix(strings.TrimSpace(it.Successor), "projects/")
		if succ != "" && it.Says != record.StatusDead {
			ps = append(ps, fmt.Sprintf("%s.successor: only a dead folder has a successor (says %s)", label, it.Says))
			continue
		}
		if succ != "" && !nameIn(n.Description+"\n"+n.Body, succ) {
			ps = append(ps, fmt.Sprintf("%s.successor: note:%s does not name %s", label, n.Name, succ))
			continue
		}
		if succ != "" && !nameIn(it.Words, succ) {
			ps = append(ps, fmt.Sprintf("%s.successor: the words you cite do not name %s; a successor is accepted only where the cited words name it as this folder's replacement (not the rest of the note, not a related plan): cite the words that do, or give no successor (dead with no successor)", label, succ))
			continue
		}
		if succ != "" && !c.namedIn(it.Words) {
			ps = append(ps, fmt.Sprintf("%s.successor: the words you cite do not name this folder (%s); a successor is accepted only where the cited words name both this folder (or another name of it) and the successor: cite the words that say %s replaces it, or give no successor (dead with no successor)", label, strings.Join(c.names(), ", "), succ))
			continue
		}
		if folder := strings.TrimPrefix(c.Folder, "projects/"); succ != "" && coListed(it.Words, folder, succ) {
			ps = append(ps, fmt.Sprintf("%s.successor: the words list %s with %s as things they retire or fold together, so it is no successor: give no successor (dead with no successor) unless other words of the owner's name one", label, succ, folder))
			continue
		}
		it.Successor = succ
		ok = append(ok, Said{OwnerWords: it, Note: n, Day: noteDay(n, it.Words)})
	}
	// The newest first; of one day, one that names the successor (the same
	// word said more fully), then the owner's stronger tier.
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].Day != ok[j].Day {
			return ok[i].Day > ok[j].Day
		}
		if si, sj := ok[i].Successor != "", ok[j].Successor != ""; si != sj {
			return si
		}
		return ok[i].Note.Tier < ok[j].Note.Tier
	})
	return ok, ps
}

// WordsWhy starts the why of a status that rests on the owner's words.
const WordsWhy = "code: "

// WordsOutcome is code's decision for a folder whose decision cites the
// owner's word on its status (s, verified, the newest): the status the words
// give, and successor (a live project folder, "" for none), unless ch holds
// the owner's real work on it after the day they said it; then code's status
// from its dates (cs) stands, with the words and the work in its why. ""
// for cs: the folder has no activity day, and the words stand. It returns
// the decision and whether the words stood.
func WordsOutcome(c Candidate, d Decision, s Said, successor string, ch Changes) (Decision, bool) {
	what := strings.TrimSpace(d.Why)
	quote := fmt.Sprintf("note:%s (%s): %q", s.Note.Name, s.Day, oneLine(s.Words, 300))
	cs := c.CodeStatus()
	if c.WorkOverrides(s.Says, ch) && cs != "" {
		d.Status = cs
		d.Why = c.Activity.StatusWhy() + fmt.Sprintf(" Owner activity after the %s statement (%s), so the dates stand: %s.", s.Day, quote, ch.Summary(4))
		d.Words = nil
		d.Successor = ""
	} else {
		d.Status = s.Says
		w := fmt.Sprintf("%s%s on the owner's words of %s, %s", WordsWhy, s.Says, s.Day, quote)
		if successor != "" && s.Says == record.StatusDead {
			w += "; replaced by " + successor
		}
		w += "; " + ch.Summary(4) + c.goneNote(s.Says, ch)
		if cs != "" && cs != s.Says {
			w += fmt.Sprintf(" (its dates alone say %s; the owner's words beat dates)", cs)
		}
		d.Why = w + "."
		d.Words = &record.Entry{Words: oneLine(s.Words, 500), WordsFrom: "note:" + s.Note.Name, WordsOn: s.Day}
		d.Successor = ""
		if s.Says == record.StatusDead {
			d.Successor = successor
		}
	}
	d.OwnerStatus = nil
	if what != "" {
		d.Why += " What it is: " + what
	}
	return d, !c.WorkOverrides(s.Says, ch) || cs == ""
}

// WorkOverrides reports the owner's real work after the day of words that
// say says (ch): the sign of a changed mind, so the folder's dates stand.
// Not for words that say dead of a repository the forge has deleted or
// archived since (ForgeGone): no commit follows that, so it came after the
// work, and the words stand.
func (c Candidate) WorkOverrides(says string, ch Changes) bool {
	return ch.Changed() && !(says == record.StatusDead && c.ForgeGone())
}

// goneNote is what the why of a status on words says of work after them
// that the forge's deletion or archive overrides.
func (c Candidate) goneNote(says string, ch Changes) string {
	if !ch.Changed() || c.WorkOverrides(says, ch) {
		return ""
	}
	return " (the owner worked on it after that day, but the forge has the repository deleted or archived since, so the words stand)"
}

// NoteDay is the day a note shown for c that why cites (note:<name>) was
// said, for the first such note; ok is false when why cites none of c's
// notes. A note that claims the owner's authority without their words
// (Note.Claim) is none: no status rests on it against the dates.
func NoteDay(c Candidate, why string) (Note, string, bool) {
	for _, cite := range record.Cites(why) {
		n, ok := strings.CutPrefix(cite, "note:")
		if !ok {
			continue
		}
		for _, x := range c.Notes {
			if x.Name == n && !x.Claim {
				return x, noteDay(x, ""), true
			}
		}
	}
	return Note{}, "", false
}

// temporalProblem refuses dead on words whose own text limits them in time,
// as the model flagged it (temporal, copied from words): a pause is dormant.
// Flagged words that are not in words are a problem too.
func temporalProblem(field, temporal, words, says string) string {
	t := norm(temporal)
	switch {
	case t == "":
		return ""
	case !strings.Contains(norm(words), t):
		return fmt.Sprintf("%s: copy the words exactly from the owner's words that limit them in time (got %q)", field, temporal)
	case says == record.StatusDead:
		return fmt.Sprintf("%s: the words limit themselves in time (%q): stopping or pausing it for a while is dormant, not dead; answer dormant, or cite other words of the owner's that end it", field, temporal)
	}
	return ""
}

// ForgeGone reports a folder whose repository the forge has deleted or
// archived: evidence that it ended, besides the owner's words.
func (c Candidate) ForgeGone() bool {
	a := c.Activity
	return a != nil && (a.Deleted || a.Forge != nil && a.Forge.Archived)
}

// CheckQuoteStatus checks what a decision resting on the owner's quote
// (owner_quote) says of the folder's status, as the model reads the words:
// flagged words that limit them in time (owner_quote_temporal) refuse dead,
// and dead needs words of the quote that make an end of the folder
// (owner_quote_final) or the forge's word that the repository is gone
// (ForgeGone). Each is copied from the quote. The words of the quote are
// the owner's; what they say of its status is the model's reading, which
// code holds to what it can check.
func CheckQuoteStatus(c Candidate, d Decision) []string {
	q := strings.TrimSpace(d.OwnerQuote)
	if q == "" {
		return nil
	}
	var ps []string
	if p := temporalProblem("owner_quote_temporal", d.OwnerQuoteTemporal, q, d.Status); p != "" {
		ps = append(ps, p)
	}
	if f := norm(d.OwnerQuoteFinal); f != "" && !strings.Contains(norm(q), f) {
		ps = append(ps, fmt.Sprintf("owner_quote_final: copy the words exactly from owner_quote (got %q)", d.OwnerQuoteFinal))
	}
	if d.Status == record.StatusDead && norm(d.OwnerQuoteFinal) == "" && !c.ForgeGone() {
		ps = append(ps, "status dead on owner_quote: the words must make an end of it (owner_quote_final: words of the quote that drop, kill, delete, remove or replace it), or the forge must show the repository deleted or archived; stopping, banking or pausing work is dormant")
	}
	return ps
}

// QuoteOutcome is code's decision for a folder whose status rests on the
// owner's quote (owner_quote, verified by the caller; CheckQuoteStatus has
// passed), said on day: as for a note's words (WordsOutcome), the dead,
// shipped or dormant status the words give stands against the dates unless
// ch holds the owner's real work on the folder after that day; then code's
// status from its dates stands, and the decision no longer rests on the
// quote (its owner_quote is cleared: the entry is not the owner's). An
// active status on their words stands: work since agrees with it. It
// returns the decision and whether the words stood.
func QuoteOutcome(c Candidate, d Decision, day string, ch Changes) (Decision, bool) {
	what := strings.TrimSpace(d.Why)
	q := strings.TrimSpace(d.OwnerQuote)
	quote := fmt.Sprintf("%s (%s): %q", strings.TrimSpace(d.OwnerQuoteFrom), day, oneLine(q, 300))
	cs := c.CodeStatus()
	switch d.Status {
	case record.StatusDead, record.StatusShipped, record.StatusDormant:
	default:
		return d, true
	}
	if c.WorkOverrides(d.Status, ch) && cs != "" {
		d.Status = cs
		d.Why = c.Activity.StatusWhy() + fmt.Sprintf(" Owner activity after the %s words (%s), so the dates stand: %s.", day, quote, ch.Summary(4))
		d.OwnerQuote, d.OwnerQuoteFrom, d.OwnerQuoteTemporal, d.OwnerQuoteFinal = "", "", "", ""
		if what != "" {
			d.Why += " What it is: " + what
		}
		return d, false
	}
	w := fmt.Sprintf("%s%s on the owner's words of %s, %s; %s", WordsWhy, d.Status, day, quote, ch.Summary(4)+c.goneNote(d.Status, ch))
	if cs != "" && cs != d.Status {
		w += fmt.Sprintf(" (its dates alone say %s; the owner's words beat dates)", cs)
	}
	d.Why = w + "."
	if what != "" {
		d.Why += " What it is: " + what
	}
	return d, true
}
