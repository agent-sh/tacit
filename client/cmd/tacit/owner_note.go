package main

// The owner's two extra words on a finding: "neither is true" (both facts of a
// pair are retired, one decision, undone exactly by reopen) and a note in their
// own words, sent with an answer or on its own. A note is kept in the decision
// log (shown under past answers, carried into the verifier's input while the
// finding is open) and written as their inbox note, so the curator reads it like
// any remark the owner makes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/store"
)

// ownerNote is a note the owner left on a finding without answering it.
type ownerNote struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// maxNote bounds a note: a remark, not a document.
const maxNote = 4000

// ownerNotes reads what the owner wrote on findings, oldest first: by finding
// (their notes left without an answer) and by fact (every note and every note
// sent with an answer, keyed by each fact the finding was about). A note
// follows the fact: a later finding on it carries it too.
type notesIndex struct {
	byID, byFact map[string][]ownerNote
}

func (rs *reviewServer) ownerNotes() notesIndex {
	ix := notesIndex{byID: map[string][]ownerNote{}, byFact: map[string][]ownerNote{}}
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if err != nil {
		return ix
	}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) != nil || d.ID == "" || d.By != "" || d.Comment == "" || d.Action == "reopen" {
			continue
		}
		n := ownerNote{At: d.At, Text: d.Comment}
		if d.Action == "note" {
			ix.byID[d.ID] = append(ix.byID[d.ID], n)
		}
		for _, f := range []string{d.A, d.B} {
			if f != "" {
				ix.byFact[f] = append(ix.byFact[f], n)
			}
		}
	}
	return ix
}

// For is every note on a finding: its own and those on any of its facts,
// once each, oldest first.
func (ix notesIndex) For(id string, facts ...string) []ownerNote {
	var out []ownerNote
	seen := map[string]bool{}
	add := func(ns []ownerNote) {
		for _, n := range ns {
			if k := n.At + "\x00" + n.Text; !seen[k] {
				seen[k] = true
				out = append(out, n)
			}
		}
	}
	add(ix.byID[id])
	for _, f := range facts {
		add(ix.byFact[f])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// dropFreshNotes removes the owner notes this write created that no commit
// holds. The caller holds rs.mu.
func (rs *reviewServer) dropFreshNotes() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, rel := range rs.freshNotes {
		if _, err := store.Git(ctx, rs.s.Dir, "cat-file", "-e", "HEAD:"+rel); err != nil {
			os.Remove(filepath.Join(rs.s.Dir, rel))
		}
	}
	rs.freshNotes = nil
}

// ownerNoteLines is the owner's notes as the verifier reads them: "Owner's
// note, <day>: <their words>".
func ownerNoteLines(ns []ownerNote) []string {
	var out []string
	for _, n := range ns {
		day := n.At
		if len(day) >= 10 {
			day = day[:10]
		}
		out = append(out, "Owner's note, "+day+": "+n.Text)
	}
	return out
}

// cleanNote trims a note and redacts it. Fail closed: when the redactor cannot
// run, nothing is written.
func cleanNote(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if len([]rune(text)) > maxNote {
		return "", fmt.Errorf("the note is %d characters; the most is %d", len([]rune(text)), maxNote)
	}
	// The same canary review apply and verify run: a scrump that runs but masks
	// nothing must not pass their words through.
	if err := digest.RedactionReady(); err != nil {
		return "", fmt.Errorf("redaction unavailable, nothing was saved: %w", err)
	}
	red, err := digest.RedactAll([]string{text})
	if err != nil {
		return "", fmt.Errorf("redaction unavailable, nothing was saved: %w", err)
	}
	return red[0], nil
}

// answerWords says an answer the way the inbox note records it.
func answerWords(mark, a, b string) string {
	switch mark {
	case "left":
		return "keep [[" + a + "]], retire [[" + b + "]]"
	case "right":
		return "keep [[" + b + "]], retire [[" + a + "]]"
	case "both":
		return "both are true"
	case "query":
		return "they conflict; both stay, marked as conflicting"
	case "wrong":
		return "neither is true; both retired"
	case "current":
		return "still true"
	case "retire":
		return "outdated; retired"
	case "note":
		return "no answer yet; the finding stays open"
	}
	return mark
}

// noteFile is the owner's inbox note for a note on a finding: their words
// verbatim (already redacted), dated, linking the facts it is about by name.
// links are the facts that exist; others are named in plain text.
func noteFile(dir, comment, mark, kind string, names []string, exists func(string) bool, scope string, now time.Time) (string, []byte, error) {
	day := now.Format("2006-01-02")
	var refs []string
	for _, n := range names {
		if n == "" {
			continue
		}
		if exists(n) {
			refs = append(refs, "[["+n+"]]")
		} else {
			refs = append(refs, n)
		}
	}
	about := strings.Join(refs, " and ")
	answer := answerWords(mark, firstOf(names, 0), firstOf(names, 1))
	if kind == kindName && len(refs) == 3 {
		// Two files of one name: the answer names the files.
		about = refs[0] + ", in two files: " + names[1] + " and " + names[2]
		answer = nameAnswerWords(mark, names[1], names[2])
	}
	body := fmt.Sprintf("The owner wrote this in tacit app on %s, on a review finding (%s) about %s. The owner's answer: %s.\n\n%s\n",
		day, kind, about, answer, quote(comment))
	if !fact.ValidScope(scope) || strings.HasPrefix(scope, "derived/") {
		scope = "user"
	}
	base := "owner-note-" + importer.Slug(firstOf(names, 0), 50)
	name := strings.TrimSuffix(base, "-") + "-" + shortHash(comment+day+strings.Join(names, ","))
	host := store.ShortHost()
	m := fact.Meta{Name: name, Description: fact.ClipDescription("Owner, "+day+": "+oneLineNote(comment), 200), Type: "unclassified", Scope: scope,
		Source: fact.Source{Harness: "human", Host: host, Kind: "user"}, Created: day}
	// A write that lost a push race runs again and finds its own note: the same
	// bytes are the same note. The same words sent again the same day are the
	// same note too. A different note under the name takes the next free one.
	for i := 1; i <= 20; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s-%d", name, i)
		}
		m.Name = n
		rel := filepath.ToSlash(filepath.Join("inbox", "human@"+host, n+".md"))
		f := &fact.Fact{Path: rel, Meta: m, Body: body}
		b, err := f.Encode()
		if err != nil {
			return "", nil, err
		}
		old, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || string(old) == string(b) {
			return rel, b, nil
		}
	}
	return "", nil, errors.New("too many notes under one name today; nothing was saved")
}

func firstOf(xs []string, i int) string {
	if i < len(xs) {
		return xs[i]
	}
	return ""
}

func quote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

func oneLineNote(s string) string { return strings.Join(strings.Fields(s), " ") }

// findingRef is what a note needs to know about any finding: its facts, its
// kind, the scope its note belongs in, and whether it can be answered "wrong".
type findingRef struct {
	id, kind, a, b, scope string
	names                 []string // the facts it is about, for the note's links
	pair                  *reviewItem
}

// findFinding looks an id up among the pairs, the stale checks and the dead
// links. The caller holds rs.mu and has loaded the facts.
func (rs *reviewServer) findFinding(ctx context.Context, id string) *findingRef {
	for _, it := range rs.items() {
		if it.ID == id {
			c := it
			return &findingRef{id: id, kind: it.Kind, a: it.A.Name, b: it.B.Name, scope: it.A.Scope, names: []string{it.A.Name, it.B.Name}, pair: &c}
		}
	}
	if st := rs.staleByID(id); st != nil {
		return &findingRef{id: id, kind: kindStale, a: st.A.Name, b: st.Flag.Sha, scope: st.A.Scope, names: []string{st.A.Name}}
	}
	if it := rs.nameItemByID(id); it != nil {
		return &findingRef{id: id, kind: kindName, a: it.A.Name, b: it.B.Name, scope: it.A.Scope, names: []string{it.A.Name, it.A.Path, it.B.Path}}
	}
	for _, it := range rs.linkItems(ctx) {
		if it.ID == id {
			return &findingRef{id: id, kind: kindLink, a: it.A.Name, b: it.Link.Target, scope: it.A.Scope, names: []string{it.A.Name, it.Link.Target}}
		}
	}
	return nil
}

// noteWrites adds the owner's inbox note for comment to files. It returns the
// redacted comment to record in the decision.
func (rs *reviewServer) noteWrites(files map[string][]byte, f *findingRef, comment, mark string) (string, error) {
	c, err := cleanNote(comment)
	if err != nil || c == "" {
		return c, err
	}
	exists := func(n string) bool { return rs.facts[n] != nil }
	rel, b, err := noteFile(rs.s.Dir, c, mark, f.kind, f.names, exists, f.scope, time.Now())
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(rs.s.Dir, rel)); err != nil {
		rs.freshNotes = append(rs.freshNotes, rel)
	}
	files[rel] = b
	return c, nil
}

// appendDecision adds one record to the decision log in files.
func (rs *reviewServer) appendDecision(files map[string][]byte, d decision) {
	dl := "review/decisions.jsonl"
	old, ok := files[dl]
	if !ok {
		old, _ = os.ReadFile(filepath.Join(rs.s.Dir, dl))
	}
	if len(old) > 0 && old[len(old)-1] != '\n' {
		old = append(old, '\n')
	}
	line, _ := json.Marshal(d)
	files[dl] = append(append(old, line...), '\n')
}

// answer is a mark from the owner with an optional note in their words.
// "wrong" retires both facts of a pair and "note" leaves the finding open with
// only their note; both go through the owner's write path (lock, lint gate,
// named commit). Every other mark is the review page's own (applyOps).
func (rs *reviewServer) answer(ctx context.Context, id, mark, comment string, ownerOnly bool) (string, error) {
	if rs.labelling() {
		return rs.label(id, mark, ownerOnly)
	}
	switch mark {
	case "wrong":
		return rs.saved(rs.neither(ctx, id, comment))
	case "note":
		return rs.saved(rs.noteOnly(ctx, id, comment))
	}
	notes, errs, err := rs.applyOps(ctx, []op{{ID: id, Mark: mark, Comment: comment}})
	if err == nil && errs[0] != nil {
		err = errs[0]
	}
	return notes[0], err
}

// saved turns an owner write's result into the review page's note and error.
func (rs *reviewServer) saved(res *saveResult, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if len(res.Lint) > 0 {
		return "", fmt.Errorf("%s %s", res.Note, strings.Join(res.Lint, "; "))
	}
	if !res.Saved {
		if res.Note != "" {
			return "", errors.New(res.Note)
		}
		return "", errors.New("nothing was saved")
	}
	return res.Note, nil
}

// neither retires both facts of a pair: their answer that neither is true. One
// decision records both facts as they were, so reopen puts both back exactly.
// The owner may retire their own rulings this way; an agent never sends it.
func (rs *reviewServer) neither(ctx context.Context, id, comment string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f := rs.findFinding(ctx, id)
		if f == nil || f.pair == nil {
			return nil, errors.New("neither is true is an answer for two facts; this finding is not a pair")
		}
		it := f.pair
		if !contains(it.Allowed, "wrong") {
			if it.Decision != nil {
				return nil, errors.New("this finding is already answered; change the answer first")
			}
			return nil, errors.New("neither is true is not possible for this finding now")
		}
		a, b := rs.facts[it.A.Name], rs.facts[it.B.Name]
		day := today()
		d := decision{ID: id, Action: "wrong", A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339),
			Before: map[string]snap{a.Meta.Name: snapOf(a), b.Meta.Name: snapOf(b)}}
		files := map[string][]byte{}
		for _, x := range []*fact.Fact{a, b} {
			if x.Meta.Retired != "" {
				continue
			}
			old, err := os.ReadFile(filepath.Join(rs.s.Dir, x.Path))
			if err != nil {
				return nil, err
			}
			c := *x
			c.Meta.Retired, c.Meta.Updated = day, day
			nb, err := fact.PatchRelations(old, &c)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", x.Path, err)
			}
			files[x.Path] = nb
		}
		c, err := rs.noteWrites(files, f, comment, "wrong")
		if err != nil {
			return nil, err
		}
		d.Comment = c
		rs.appendDecision(files, d)
		return &ownerChange{write: files, name: it.A.Name,
			msg: fmt.Sprintf("%sneither is true: retire %s and %s\n\nBy the owner in tacit app, on review finding %s.", ownerCommit, it.A.Name, it.B.Name, id)}, nil
	})
}

// noteOnly records the owner's note on a finding and leaves it open.
func (rs *reviewServer) noteOnly(ctx context.Context, id, comment string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f := rs.findFinding(ctx, id)
		if f == nil {
			return nil, errors.New("this finding is no longer in the review files")
		}
		files := map[string][]byte{}
		c, err := rs.noteWrites(files, f, comment, "note")
		if err != nil {
			return nil, err
		}
		if c == "" {
			return &ownerChange{note: "The note is empty; nothing was saved."}, nil
		}
		rs.appendDecision(files, decision{ID: id, Action: "note", A: f.a, B: f.b, At: time.Now().Format(time.RFC3339), Comment: c})
		return &ownerChange{write: files, name: f.a,
			msg: fmt.Sprintf("%snote on %s\n\nBy the owner in tacit app, on review finding %s; the finding stays open.", ownerCommit, f.a, id)}, nil
	})
}
