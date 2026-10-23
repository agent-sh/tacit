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
	// Said is a statement's day (when the owner said it); Description and
	// Body are the fact's text, which an owner_status item's words must be in.
	Said, Description, Body string
	// Projects are the other project folders code finds its text names, each
	// with where its name leads now ("gishra (renamed: tower-crane)").
	Projects []string
}

// OwnerTier reports a note whose words may be the owner's word on a
// folder's status: a ruling, a statement, words of theirs, a decision.
func (n Note) OwnerTier() bool { return n.Tier <= TierDecision }

func (n Note) tierLabel() string {
	switch n.Tier {
	case TierRuling:
		return "the owner's ruling"
	case TierStatement:
		return "the owner's statement, said once " + n.Said
	case TierWords:
		return "holds the owner's words"
	case TierDecision:
		return "a decision"
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

// CheckOwnerWords checks d's owner_status items against the notes code
// showed for c: each from a note of the owner's tiers among them, its words
// verbatim in that note, says dead, shipped or dormant, a successor (dead
// only) named in that note. It returns the verified items, newest first
// (the newest supersedes the older), and the problems of the others.
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
		case !n.OwnerTier():
			ps = append(ps, fmt.Sprintf("%s.from: note:%s is %s, not the owner's ruling, statement, words or decision: cite it in why as evidence instead", label, n.Name, n.tierLabel()))
			continue
		}
		w := norm(it.Words)
		if len(strings.Fields(w)) < 2 || !strings.Contains(norm(n.Description+"\n"+n.Body), w) {
			ps = append(ps, fmt.Sprintf("%s.words: copy the words exactly from note:%s (two words or more)", label, n.Name))
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
	if ch.Changed() && cs != "" {
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
		w += "; " + ch.Summary(4)
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
	return d, !ch.Changed() || cs == ""
}

// NoteDay is the day a note shown for c that why cites (note:<name>) was
// said, for the first such note; ok is false when why cites none of c's
// notes.
func NoteDay(c Candidate, why string) (Note, string, bool) {
	for _, cite := range record.Cites(why) {
		n, ok := strings.CutPrefix(cite, "note:")
		if !ok {
			continue
		}
		for _, x := range c.Notes {
			if x.Name == n {
				return x, noteDay(x, ""), true
			}
		}
	}
	return Note{}, "", false
}
