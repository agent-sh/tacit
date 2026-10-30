package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/store"
)

// A stale finding is about one fact, not a pair: the staleness check
// (stale.go) found that something the fact names was deleted in git after the
// day the fact was last current. It is a line in review/<date>.md:
//
//   - stale? [[name]] (scope) @abc1234 n=3 since=2026-10-01
//     2026-10-09 abc1234 repo: subject - FOO (gone at HEAD)
//
// The sha is the newest triggering commit, so a later commit makes a new
// finding and the same commit never makes two. The indented lines say why, for
// a person reading the file; the page and the verifier read git themselves.
//
// Marks, in review/decisions.jsonl like every other finding:
//
//	current  still true (owner or agent): the fact is re-dated to the mark
//	retire   outdated per git: the fact gets `retired: <date>` and stops
//	         reaching agents; reopen undoes it. An agent never retires an
//	         owner ruling: that becomes a question
//	fix      an agent: true, but a detail changed; says what. Stays with the owner
//	ask      an agent's question for the owner (a ruling, or no git trace)
const kindStale = "stale"

var reStaleLine = regexp.MustCompile(`^- stale\? \[\[([^\]]+)\]\] \(([^)]*)\) @([0-9a-f]{4,40}) n=(\d+) since=(\d{4}-\d{2}-\d{2})`)

type staleRef struct {
	name, scope, sha, since, source string
	n                               int
	detail                          []string
	outcome                         string // a rotation question's (check_review.go)
}

// parseStaleFiles reads the stale findings of review/*.md in file order.
func parseStaleFiles(dir string) []staleRef {
	files, _ := filepath.Glob(filepath.Join(dir, "review", "*.md"))
	sort.Strings(files)
	var out []staleRef
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		src := filepath.Base(f)
		cur := -1
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if m := reStaleLine.FindStringSubmatch(l); m != nil {
				n, _ := strconv.Atoi(m[4])
				out = append(out, staleRef{name: m[1], scope: m[2], sha: m[3], n: n, since: m[5], source: src})
				cur = len(out) - 1
				continue
			}
			if cur >= 0 && strings.HasPrefix(l, "  ") && strings.TrimSpace(l) != "" {
				out[cur].detail = append(out[cur].detail, strings.TrimSpace(l))
				continue
			}
			cur = -1
		}
		fh.Close()
	}
	return out
}

// staleFlag is what the finding line says.
type staleFlag struct {
	Sha    string   `json:"sha"`
	N      int      `json:"n"`
	Since  string   `json:"since"`
	Detail []string `json:"detail,omitempty"`
}

// staleItem is a stale finding as the page and the verifier see it. It has
// the fields of a pair finding that the page reads, with one fact in A.
type staleItem struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	A          *factView `json:"a"`
	Single     bool      `json:"single"`
	Flag       staleFlag `json:"flag"`
	State      string    `json:"state"` // live | retired | gone (superseded or missing)
	Allowed    []string  `json:"allowed"`
	Mark       string    `json:"mark,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
	Decision   *decision `json:"decision,omitempty"`
	Ask        *decision `json:"ask,omitempty"`
	NeedsOwner bool      `json:"needs_owner"`
	// Applied: an agent's fix was written into the fact (its file was committed
	// after the verdict), so the finding no longer needs the owner.
	Applied   bool   `json:"applied,omitempty"`
	AppliedAt string `json:"applied_at,omitempty"`
	// Prior is the agent's fix on an older finding of this fact that was never
	// applied, still worth the owner's eye when a newer finding replaced it.
	Prior *decision `json:"prior,omitempty"`
	// Notes are what the owner wrote on this finding without answering it.
	Notes []ownerNote `json:"notes,omitempty"`
}

// staleItems lists the latest finding of every flagged fact. An older
// finding of the same fact is history: its decision stays in the log, and the
// newer commit made a new question.
func (rs *reviewServer) staleItems() []staleItem {
	if rs.labelling() {
		return nil // a label sample is pairs
	}
	refs := parseStaleFiles(rs.s.Dir)
	last := map[string]int{}
	for i, r := range refs {
		last[r.name] = i
	}
	ds, asks := rs.decisions()
	notes := rs.ownerNotes()
	prior := map[string]*decision{}
	for i, r := range refs {
		if last[r.name] != i {
			if d := ds[pairID(kindStale, r.name, r.sha)]; d != nil && d.By == "agent" && d.Action == "fix" {
				prior[r.name] = d
			}
		}
	}
	var out []staleItem
	for i, r := range refs {
		if last[r.name] != i {
			continue
		}
		id := pairID(kindStale, r.name, r.sha)
		it := staleItem{ID: id, Kind: kindStale, Source: r.source, A: rs.view(r.name), Single: true,
			Flag: staleFlag{Sha: r.sha, N: r.n, Since: r.since, Detail: r.detail}, Decision: ds[id], Ask: asks[id], Notes: notes.For(id, r.name)}
		switch {
		case it.A.Missing || it.A.SupersededBy != "":
			it.State = "gone"
		case it.A.Retired != "":
			it.State = "retired"
		default:
			it.State = "live"
		}
		if d := it.Decision; d != nil {
			it.Mark = d.Action
			it.Stale = d.Action == "retire" && it.State != "retired"
			if d.By == "agent" && d.Action == "fix" && d.Note != "" {
				it.Applied, it.AppliedAt = true, d.At // written as an in-place note with the mark
			} else if d.By == "agent" && d.Action == "fix" {
				// A fix is closed when the fact is edited after the verdict: the
				// owner (or the curator) applied it.
				if at, err := time.Parse(time.RFC3339, d.At); err == nil {
					if when := rs.factChangedAfter(it.A.Path, at); !when.IsZero() {
						it.Applied, it.AppliedAt = true, when.Format(time.RFC3339)
					}
				}
			}
		}
		if p := prior[r.name]; p != nil && it.Decision == nil {
			it.Prior = p
		}
		it.Allowed = rs.allowedStale(it)
		it.NeedsOwner = it.State == "live" && (it.Decision == nil || (it.Decision.By == "agent" && it.Decision.Action == "fix" && !it.Applied))
		out = append(out, it)
	}
	return out
}

func (rs *reviewServer) staleByID(id string) *staleItem {
	for _, it := range append(rs.staleItems(), rs.checkItems()...) {
		if it.ID == id {
			c := it
			return &c
		}
	}
	return nil
}

// allowedStale lists the marks the server accepts now.
func (rs *reviewServer) allowedStale(it staleItem) []string {
	if rs.viewOnly != "" && !rs.labelling() {
		return []string{}
	}
	d := it.Decision
	if it.State != "live" {
		if d != nil {
			return []string{"reopen"}
		}
		return nil
	}
	if d != nil && !(d.By == "agent" && d.Action == "fix") {
		return []string{"reopen"}
	}
	return []string{"current", "retire"}
}

// allItems is what the page lists: the pair findings, then the stale ones.
func (rs *reviewServer) allItems() []any {
	var out []any
	for _, it := range rs.items() {
		out = append(out, it)
	}
	for _, it := range rs.staleItems() {
		out = append(out, it)
	}
	for _, it := range rs.checkItems() {
		out = append(out, it)
	}
	return out
}

// staleFactRef is the fact with the inbox note it was split from.
func (rs *reviewServer) staleFactRef(name string) (gitev.FactRef, bool) {
	f := rs.facts[name]
	if f == nil {
		return gitev.FactRef{}, false
	}
	c := *f // a mark made while the timeline is read changes the stored fact
	r := gitev.FactRef{Fact: &c}
	if o := rs.inbox[f.Meta.Origin]; o != nil {
		oc := *o
		r.Origin = &oc
	}
	return r, true
}

// factChangedAfter is the commit time of the newest store commit of a fact's
// file when it is after t, else the zero time.
func (rs *reviewServer) factChangedAfter(path string, t time.Time) time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := store.Git(ctx, rs.s.Dir, "log", "-1", "--format=%ct", "--", path)
	if err != nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || !time.Unix(n, 0).After(t) {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// staleQuestion is what an agent's verdict on a ruling is turned into.
func staleQuestion(name, mark string, it *staleItem) string {
	what := map[string]string{"retire": "look outdated", "fix": "need a detail fixed", "current": "still hold"}[mark]
	if mark == "ask" {
		return ""
	}
	return fmt.Sprintf("Does your ruling %s still hold? Something it names was deleted after %s (%d commit(s), newest %s); an agent read it as: it would %s.", name, it.Flag.Since, it.Flag.N, it.Flag.Sha, what)
}

// applyStaleOp checks one mark against a stale finding and writes its files.
// Everything is prepared in memory first, so a refused mark writes nothing.
func (rs *reviewServer) applyStaleOp(o *op, it *staleItem) (string, []string, error) {
	name := it.A.Name
	if o.Mark == mootMark {
		return rs.applyStaleMoot(o, it) // an agent's question whose fact went moot
	}
	if o.By == "agent" && o.Proof != "" {
		return rs.applyStaleClosure(o, it) // an agent closes the question it can prove
	}
	if o.By == "agent" && it.Kind == kindCheck {
		return "", nil, errors.New("the rotation's question is the owner's to answer, or an agent's closure with its proof")
	}
	if o.By == "agent" {
		// An agent says one of four things. The guard below does not depend on a
		// caller having checked: no other mark, reopen included, gets past here.
		switch o.Mark {
		case "current", "retire", "fix", "ask":
		default:
			return "", nil, fmt.Errorf("an agent may not make the mark %q on a stale finding", o.Mark)
		}
		if o.A != "" && o.A != name {
			return "", nil, fmt.Errorf("finding %s is about %s, the verdict says %s", o.ID, name, o.A)
		}
		o.A, o.B = name, it.Flag.Sha
		if it.Decision != nil || it.Ask != nil {
			return "", nil, errors.New("an agent may not mark a finding that already carries a mark or a question")
		}
		// The owner's word on the finding, or on the fact (keeping one fact over
		// another in a pair is a word on both), is theirs.
		if owned := rs.ownerTouched(); owned[o.ID] || owned[name] {
			return "", nil, errors.New("the owner has marked or reopened this finding, or kept this fact in a pair; it is theirs")
		}
		if it.State != "live" {
			return "", nil, errors.New("this fact is already retired or gone")
		}
		if o.Mark != "ask" && strings.TrimSpace(o.Evidence) == "" {
			return "", nil, errors.New("an agent mark needs evidence")
		}
		// Never retire, or rewrite the standing of, an owner ruling on an
		// agent's word, nor retire a fact the owner left a note on: those stay
		// a question for the owner.
		if o.Comment != "" {
			return "", nil, errors.New("only the owner may leave a note")
		}
		if o.Mark == "retire" && len(it.Notes) > 0 {
			if o.Question == "" {
				o.Question = "You left a note on this. An agent reads " + name + " as outdated; is it?"
			}
			o.Mark = "ask"
		}
		if o.Mark != "ask" && isRuling(rs.facts[name]) {
			if o.Question == "" {
				o.Question = staleQuestion(name, o.Mark, it)
			}
			o.Mark = "ask"
		}
		// Words of theirs in the fact: retiring it would go against them.
		if w := fact.OwnerWordsOf(rs.facts[name]); o.Mark == "retire" && w != "" {
			if o.Question == "" {
				o.Question = "You said \"" + w + "\" (in " + name + "). An agent reads it as outdated; do your words still hold?"
			}
			o.Mark = "ask"
		}
	} else {
		o.A, o.B = name, it.Flag.Sha
		if !contains(it.Allowed, o.Mark) {
			if it.Decision != nil {
				return "", nil, errors.New("this finding is already marked; reopen it first")
			}
			return "", nil, errors.New("that mark is not possible for this finding now")
		}
	}
	mark := o.Mark
	switch mark {
	case "current", "retire", "fix", "ask", "reopen":
	default:
		return "", nil, fmt.Errorf("unknown mark %q for a stale finding", mark)
	}
	if mark == "ask" && it.Decision != nil {
		return "", nil, errors.New("this finding is already marked")
	}
	today := time.Now().Format("2006-01-02")
	d := decision{ID: o.ID, Action: mark, A: name, B: it.Flag.Sha, At: time.Now().Format(time.RFC3339), By: o.By, Evidence: o.Evidence, Question: o.Question, Model: o.Model}
	var note string
	var touched []*fact.Fact
	files := map[string][]byte{}
	switch mark {
	case "retire":
		f := *rs.facts[name]
		d.Before = map[string]snap{name: snapOf(&f)}
		f.Meta.Retired, f.Meta.Updated = today, today
		touched = append(touched, &f)
	case "fix":
		// An agent's fix lands as an in-place note when the fact says the
		// replaced detail word for word; otherwise it waits for the owner.
		if o.By == "agent" && o.FixWas != "" && o.FixNow != "" {
			if line, data, ok := rs.fixNote(name, o.FixWas, o.FixNow, today); ok {
				files[rs.facts[name].Path] = data
				d.Note = line
			}
		}
	case "reopen":
		prev := it.Decision
		if prev == nil {
			return "", nil, errors.New("there is no mark to reopen")
		}
		if prev.Action == "retire" {
			f := rs.facts[name]
			switch {
			case prev.Before == nil:
				note = "This mark is older than the review page's records, so the fact was left as it is."
			case f == nil:
				note = "This fact is gone from the store."
			case f.Meta.Retired == "":
				note = "The store changed since this mark, so the fact was left as it is."
			default:
				c := *f
				b := prev.Before[name]
				c.Meta.Retired, c.Meta.Updated = b.Retired, b.Updated
				touched = append(touched, &c)
			}
		}
	}
	for _, f := range touched {
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
		if err != nil {
			return "", nil, err
		}
		nb, err := fact.PatchRelations(old, f)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if string(nb) != string(old) {
			files[f.Path] = nb
		}
	}
	if o.Comment != "" && mark != "reopen" && mark != "ask" {
		ref := &findingRef{id: it.ID, kind: kindStale, a: name, b: it.Flag.Sha, scope: it.A.Scope, names: []string{name}}
		c, err := rs.noteWrites(files, ref, o.Comment, mark)
		if err != nil {
			return "", nil, err
		}
		d.Comment = c
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, note, files)
}

// fixNote is an agent's fix as the fact's in-place note: the line and the
// file with it appended and updated set to today, when the fact is live,
// not a ruling (an agent writes into none), holds was word for word in its
// body and the note is well formed.
func (rs *reviewServer) fixNote(name, was, now, today string) (string, []byte, bool) {
	f := rs.facts[name]
	if f == nil || isRuling(f) || f.Meta.Retired != "" || f.Meta.SupersededBy != "" || !strings.Contains(f.Body, was) {
		return "", nil, false
	}
	line, err := fact.NoteLine(today, was, now)
	if err != nil {
		return "", nil, false
	}
	old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
	if err != nil {
		return "", nil, false
	}
	data, err := fact.AppendNote(old, line)
	if err != nil {
		return "", nil, false
	}
	c := *f
	c.Meta.Updated = today
	if data, err = fact.PatchRelations(data, &c); err != nil {
		return "", nil, false
	}
	return line, data, true
}
