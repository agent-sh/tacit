package main

// A name two files hold (SPEC.md, "Near-duplicates"). Names are unique
// store-wide, but a tool outside tacit can leave one name in two curated
// files: a copy in a second folder. Every lookup by that name has to pick one
// of them, and every finding, mark and question is keyed by names, so `tacit
// dedupe` raises such a pair as a finding about the two files:
//
//	- name held twice: [[name]] in `projects/a/name.md` and `user/name.md`
//
// The earlier file (created, then path) comes first. The finding takes a
// pair's marks, recorded in review/decisions.jsonl under its id with the
// files' state before the mark keyed by path: left keeps the first file and
// gives the second `superseded_by` the name, which then reads as the first
// (Search and the tree); right keeps the second; both keeps the two; neither
// (wrong) retires both. An agent marks it through `tacit review apply`,
// naming the two paths, under the guards of every agent mark, or relays the
// owner's own answer with `tacit questions close --proof words`
// (applyNameRelay); the owner answers it on the review page and reopens an
// answer there. The verifier does not take these findings.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

const kindName = "name"

var reNameTwice = regexp.MustCompile("^- name held twice: \\[\\[([a-z0-9][a-z0-9-]*)\\]\\] in `([^`]+)` and `([^`]+)`")

// nameRef is one name finding: the name and its two files, earlier first.
type nameRef struct{ name, a, b, source string }

// parseNameFiles reads the name findings of review/*.md in file order.
func parseNameFiles(dir string) []nameRef {
	files, _ := filepath.Glob(filepath.Join(dir, "review", "*.md"))
	sort.Strings(files)
	var out []nameRef
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if m := reNameTwice.FindStringSubmatch(l); m != nil && m[2] != m[3] {
				out = append(out, nameRef{name: m[1], a: m[2], b: m[3], source: filepath.Base(f)})
			}
		}
	}
	return out
}

// nameLine is the finding's line in a review file.
func nameLine(name, a, b string) string {
	return "- name held twice: [[" + name + "]] in `" + a + "` and `" + b + "`"
}

// heldTwice is every pair of live curated files that hold one name: the
// earliest file (created, then path) with each later one. Generated derived
// facts are left out: their next run writes them again.
func heldTwice(facts []*fact.Fact) [][2]*fact.Fact {
	by := map[string][]*fact.Fact{}
	for _, f := range facts {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Meta.Scope, "derived/") {
			continue
		}
		by[f.Meta.Name] = append(by[f.Meta.Name], f)
	}
	var names []string
	for n, fs := range by {
		if len(fs) > 1 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out [][2]*fact.Fact
	for _, n := range names {
		fs := by[n]
		sort.SliceStable(fs, func(i, j int) bool {
			if fs[i].Meta.Created != fs[j].Meta.Created {
				return fs[i].Meta.Created < fs[j].Meta.Created
			}
			return fs[i].Path < fs[j].Path
		})
		for _, f := range fs[1:] {
			out = append(out, [2]*fact.Fact{fs[0], f})
		}
	}
	return out
}

// namesHeldTwice reads the clone: every pair of live curated files that hold
// one name, and the ones no review file raises yet.
func namesHeldTwice(dir string) (all, fresh [][2]*fact.Fact, err error) {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return nil, nil, err
	}
	raised := map[[2]string]bool{}
	for _, r := range parseNameFiles(dir) {
		raised[[2]string{r.a, r.b}] = true
	}
	for _, p := range heldTwice(facts) {
		all = append(all, p)
		if !raised[[2]string{p[0].Path, p[1].Path}] {
			fresh = append(fresh, p)
		}
	}
	return all, fresh, nil
}

// nameFindings adds a finding for each fresh pair to today's review file in
// files, and returns the commit message's lines for them.
func nameFindings(dir string, fresh [][2]*fact.Fact, now time.Time, files map[string][]byte) []string {
	if len(fresh) == 0 {
		return nil
	}
	// Its own file: the curator appends to the day's file outside the lock
	// and commits it later, and a write naming it would wait on that.
	rel := "review/" + now.Format("2006-01-02") + "-names.md"
	cur, ok := files[rel]
	if !ok {
		cur, _ = os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	}
	var b bytes.Buffer
	b.Write(cur)
	if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
		b.WriteString("\n")
	}
	if len(cur) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## dedupe %s, %d name(s) two files hold\n\n", now.Format(time.RFC3339), len(fresh))
	var msg []string
	for _, p := range fresh {
		b.WriteString(nameLine(p[0].Meta.Name, p[0].Path, p[1].Path) + "\n")
		msg = append(msg, p[0].Meta.Name+": "+p[0].Path+" and "+p[1].Path)
	}
	files[rel] = b.Bytes()
	return msg
}

// nameAnswerWords is an answer to a name finding in words: the files it
// keeps and retires.
func nameAnswerWords(mark, a, b string) string {
	switch mark {
	case "left":
		return "keep the file " + a + ", retire " + b
	case "right":
		return "keep the file " + b + ", retire " + a
	case "both":
		return "both files are true"
	case "wrong":
		return "neither file stands, retire " + a + " and " + b
	}
	return answerWords(mark, a, b)
}

// fileView is the curated file at rel as the page shows it, when it still
// holds name.
func (rs *reviewServer) fileView(name, rel string) *factView {
	f := rs.byPath[rel]
	if f == nil || f.Meta.Name != name {
		return &factView{Name: name, Path: rel, Missing: true}
	}
	return viewOf(f)
}

// nameItems is every name finding as the page shows it: the two files, read
// by path.
func (rs *reviewServer) nameItems() []reviewItem {
	if rs.labelling() {
		return nil // a label sample is pairs of names
	}
	ds, asks := rs.decisions()
	notes := rs.ownerNotes()
	// The files each name takes part in: live, or retired into the name
	// itself. With more than two, `superseded_by` the name no longer says
	// which file replaced a copy.
	held := map[string][]string{}
	for _, f := range rs.byPath {
		if (f.Meta.SupersededBy == "" && f.Meta.Retired == "") || f.Meta.SupersededBy == f.Meta.Name {
			held[f.Meta.Name] = append(held[f.Meta.Name], f.Path)
		}
	}
	seen := map[string]bool{}
	var out []reviewItem
	for _, r := range parseNameFiles(rs.s.Dir) {
		id := pairID(kindName, r.a, r.b)
		if seen[id] {
			continue
		}
		seen[id] = true
		it := reviewItem{ID: id, Kind: kindName, Source: r.source, A: rs.fileView(r.name, r.a), B: rs.fileView(r.name, r.b), Decision: ds[id], Ask: asks[id], Notes: notes.For(id, r.name)}
		// A file another fact replaced, or one retired or gone, ends the
		// finding: the name is no longer held twice. Both files retired by
		// the standing answer neither is that answer's state: it reopens.
		live := func(v *factView) bool {
			return !v.Missing && v.Retired == "" && (v.SupersededBy == "" || v.SupersededBy == r.name)
		}
		retired := func(v *factView) bool {
			return !v.Missing && v.Retired != "" && (v.SupersededBy == "" || v.SupersededBy == r.name)
		}
		switch {
		case live(it.A) && live(it.B):
			it.State = pairState(it.A, it.B)
		case retired(it.A) && retired(it.B) && it.Decision != nil && markOf(it.Decision.Action) == "wrong":
			it.State = "wrong"
		default:
			it.State = "gone"
		}
		if d := it.Decision; d != nil {
			it.Mark = markOf(d.Action)
			it.Stale = it.Mark != "" && it.State != it.Mark
		}
		if ps := held[r.name]; len(ps) > 2 {
			it.Crowded = true
			for _, p := range ps {
				if p != r.a && p != r.b {
					it.Also = append(it.Also, p)
				}
			}
			sort.Strings(it.Also)
		}
		it.Allowed = rs.allowedName(it)
		it.NeedsOwner = it.State != "gone" && (it.Decision == nil || (it.Decision.By == "agent" && it.Stale))
		out = append(out, it)
	}
	return out
}

func (rs *reviewServer) nameItemByID(id string) *reviewItem {
	for _, it := range rs.nameItems() {
		if it.ID == id {
			c := it
			return &c
		}
	}
	return nil
}

// allowedName lists the marks the server accepts on a name finding now. No
// conflict mark: a fact cannot conflict with its own name (lint L13).
// Neither (wrong) retires both files: the owner's answer on the page or in
// their own words, relayed; an agent never sends it on its own.
func (rs *reviewServer) allowedName(it reviewItem) []string {
	if rs.viewOnly != "" {
		return []string{}
	}
	if it.Decision != nil && !(it.Decision.By == "agent" && it.Stale) {
		return []string{"reopen"}
	}
	if it.State == "gone" || it.State == "wrong" || it.Crowded {
		return []string{} // a crowded name is the owner's to sort out, file by file, in tacit app
	}
	out := []string{"both", "wrong"}
	if it.B.SupersededBy == "" || it.B.SupersededBy == it.A.Name {
		out = append(out, "left")
	}
	if it.A.SupersededBy == "" || it.A.SupersededBy == it.B.Name {
		out = append(out, "right")
	}
	return withoutGenerated(out, it)
}

// applyNameOp applies one mark to a name finding: the files are the
// finding's, read by path, and the record keeps their state by path so the
// owner's reopen puts back exactly what the mark changed.
func (rs *reviewServer) applyNameOp(o *op, it *reviewItem) (string, []string, error) {
	if o.By == "agent" && (o.A == "" || o.B == "") {
		return "", nil, errors.New("an agent verdict must name both files")
	}
	if o.A != "" && !(o.A == it.A.Path && o.B == it.B.Path) {
		return "", nil, fmt.Errorf("finding %s is about %s then %s, the verdict says %s then %s", o.ID, it.A.Path, it.B.Path, o.A, o.B)
	}
	o.A, o.B = it.A.Path, it.B.Path
	if o.By == "agent" && o.Proof != "" {
		return rs.applyNameRelay(o, it) // the owner's own answer, relayed
	}
	if o.Mark == mootMark || o.Proof != "" {
		return "", nil, errors.New("a name two files hold takes a mark, not a closure")
	}
	mark := o.Mark
	switch mark {
	case "left", "right", "both", "ask", "reopen":
	default:
		return "", nil, fmt.Errorf("a name two files hold takes left, right, both, an agent's question or the owner's reopen, not %q", mark)
	}
	if o.By == "agent" {
		if mark == "both" || mark == "reopen" {
			return "", nil, errors.New("an agent keeps one file or asks the owner")
		}
		if it.Decision != nil || it.Ask != nil {
			return "", nil, errors.New("an agent may not mark a finding that already carries a mark or a question")
		}
		if owned := rs.ownerTouched(); owned[o.ID] || owned[it.A.Name] {
			return "", nil, errors.New("the owner has marked or reopened this finding or a fact of this name; it is theirs")
		}
		if mark != "ask" && strings.TrimSpace(o.Evidence) == "" {
			return "", nil, errors.New("an agent mark needs evidence")
		}
		// What every agent mark keeps from: an owner's note on the finding,
		// their ruling, their words. Each makes the mark a question.
		if retired, kept := map[string]*factView{"left": it.B, "right": it.A}[mark], map[string]*factView{"left": it.A, "right": it.B}[mark]; retired != nil {
			q := ""
			switch {
			case len(it.Notes) > 0:
				q = "You left a note on this name. An agent reads " + kept.Path + " as the file that stands; does " + retired.Path + " still hold?"
			case retired.guarded():
				q = "Two files hold your ruling's name, " + it.A.Name + ". An agent reads " + kept.Path + " as the one that stands; does your ruling in " + retired.Path + " still hold?"
			case retired.hisWords():
				q = "You said \"" + retired.OwnerWords + "\" (in " + retired.Path + "). An agent reads " + kept.Path + " as the file that stands; do your words still hold?"
			}
			if q != "" {
				if o.Question == "" {
					o.Question = q
				}
				o.proposed = kept.Path
				mark, o.Mark = "ask", "ask"
			}
		}
		if mark == "ask" && strings.TrimSpace(o.Question) == "" {
			return "", nil, errors.New("an agent's question for the owner needs its text")
		}
	}
	switch {
	case mark == "ask":
		if it.Decision != nil {
			return "", nil, errors.New("this finding is already marked")
		}
	case !contains(it.Allowed, mark):
		if it.Decision != nil {
			return "", nil, errors.New("this finding is already marked; reopen it first")
		}
		return "", nil, errors.New("that mark is not possible for this finding now")
	}
	today := time.Now().Format("2006-01-02")
	d := decision{ID: o.ID, Action: mark, A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339), By: o.By}
	d.Evidence, d.Question, d.Proposed, d.Model = o.Evidence, o.Question, o.proposed, o.Model
	var touched []*fact.Fact
	var note string
	switch mark {
	case "ask":
	case "reopen":
		prev := it.Decision
		x, y := rs.byPath[it.A.Path], rs.byPath[it.B.Path]
		switch {
		case prev.Before == nil:
			note = "This mark changed no file, so nothing was put back."
		case it.Stale:
			note = "The store changed since this mark, so the files were left as they are."
		case x == nil || y == nil:
			note = "One of these files is gone from the store."
		default:
			reopenPair(x, y, prev.Before[x.Path])
			reopenPair(y, x, prev.Before[y.Path])
			if markOf(prev.Action) == "wrong" {
				// Neither retired both: put back what each was.
				x.Meta.Retired, y.Meta.Retired = prev.Before[x.Path].Retired, prev.Before[y.Path].Retired
			}
			for _, f := range []*fact.Fact{x, y} {
				b := prev.Before[f.Path]
				if sameSnap(f, b) {
					f.Meta.Updated = b.Updated
				} else {
					f.Meta.Updated = today
				}
				touched = append(touched, f)
			}
		}
	default:
		a, b := rs.byPath[it.A.Path], rs.byPath[it.B.Path]
		if a == nil || b == nil {
			return "", nil, errors.New("one of these files is gone from the store")
		}
		d.Before = map[string]snap{a.Path: snapOf(a), b.Path: snapOf(b)}
		if err := setState(a, b, mark); err != nil {
			return "", nil, err
		}
		for _, f := range []*fact.Fact{a, b} {
			if !sameSnap(f, d.Before[f.Path]) {
				f.Meta.Updated = today
				touched = append(touched, f)
			}
		}
	}
	files := map[string][]byte{}
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
		ref := &findingRef{id: it.ID, kind: it.Kind, a: it.A.Name, b: it.B.Name, scope: it.A.Scope, names: []string{it.A.Name, it.A.Path, it.B.Path}}
		c, err := rs.noteWrites(files, ref, o.Comment, mark)
		if err != nil {
			return "", nil, err
		}
		d.Comment = c
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, note, files)
}

// applyNameRelay closes a name finding with the owner's own answer, which an
// agent relays (SPEC.md, "Closing questions"; questions_relay.go): their
// words, found by code in their turns, said on or after the day the name was
// raised or an agent asked about it, after their own last word on it, in a
// turn on its subject. It keeps one file, both, or neither (both retired),
// read by path; its record keeps their words with the files as they were, so
// their reopen puts back what it changed. Without such words no closure takes
// a name: an agent keeps one file with tacit review apply, under the guards
// of every agent mark.
func (rs *reviewServer) applyNameRelay(o *op, it *reviewItem) (string, []string, error) {
	if o.Proof != "words" {
		return "", nil, errors.New("a name two files hold closes only on the owner's own answer (--proof words); without their words an agent keeps one file with tacit review apply")
	}
	if err := closureText(o); err != nil {
		return "", nil, err
	}
	switch {
	case it.Decision != nil:
		return "", nil, errors.New("this name is answered already; the owner reopens that answer first")
	case it.State == "gone":
		return "", nil, errors.New("a file of this name is retired, replaced or not in the store: the name is no longer held twice")
	case it.Crowded:
		return "", nil, errors.New("more than two files hold this name; the owner retires the copies, file by file, in tacit app")
	}
	closes, question, asked := kindName, nameQuestion(*it), strings.TrimSuffix(it.Source, ".md")
	if it.Ask != nil {
		closes, question, asked = closesAsk, it.Ask.Question, it.Ask.At
	}
	why := o.relays(relayQuestion{asked: asked, question: question, facts: []*factView{it.A, it.B}, last: rs.ownerLast(it.ID, it.A.Name)})
	if why != nil {
		return "", nil, fmt.Errorf("a name two files hold closes only on the owner's own answer, and these words are not their answer to it: %v", why)
	}
	o.Relayed = true
	if !contains(it.Allowed, o.Mark) {
		return "", nil, errors.New("that answer is not possible for this name now")
	}
	a, b := rs.byPath[it.A.Path], rs.byPath[it.B.Path]
	if a == nil || b == nil {
		return "", nil, errors.New("one of these files is gone from the store")
	}
	before := map[string]snap{a.Path: snapOf(a), b.Path: snapOf(b)}
	ca, cb := *a, *b
	ca.Meta.ConflictsWith = append([]string(nil), a.Meta.ConflictsWith...)
	cb.Meta.ConflictsWith = append([]string(nil), b.Meta.ConflictsWith...)
	day := time.Now().Format("2006-01-02")
	if o.Mark == "wrong" {
		for _, c := range []*fact.Fact{&ca, &cb} {
			if c.Meta.Retired == "" {
				c.Meta.Retired = day
			}
		}
	} else if err := setState(&ca, &cb, o.Mark); err != nil {
		return "", nil, err
	}
	files := map[string][]byte{}
	for _, c := range []*fact.Fact{&ca, &cb} {
		was := before[c.Path]
		if sameSnap(c, was) {
			continue
		}
		f := rs.byPath[c.Path]
		// Their ruling changes, and a file with their words is retired,
		// only on their own later words on it.
		gone := (c.Meta.Retired != "" && was.Retired == "") || (c.Meta.SupersededBy != "" && was.SupersededBy == "")
		if isRuling(f) && !o.wordsLetRetire(f) {
			return "", nil, fmt.Errorf("the answer would change your ruling in %s; a relayed answer does that only on your own later words on it", c.Path)
		}
		if w := fact.OwnerWordsOf(f); w != "" && gone && !o.wordsLetRetire(f) {
			return "", nil, fmt.Errorf("the answer retires %s, which holds the owner's words %q; only their own later words on it let a relayed answer do that", c.Path, w)
		}
		c.Meta.Updated = day
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
		if err != nil {
			return "", nil, err
		}
		nb, err := fact.PatchRelations(old, c)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if string(nb) != string(old) {
			files[f.Path] = nb
		}
	}
	d := closureDecision(o, closes, it.A.Name, it.B.Name)
	d.Before, d.Question = before, question
	if o.dry {
		return "the closure would land", nil, nil
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, "", files)
}
