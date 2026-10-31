package main

// Closing questions (SPEC.md, "Closing questions"). The owner, 2026-10-30:
// "tacit questions - im probably not going to check it at all, so where agent
// can prove and close, it should." A question is what tacit leaves waiting
// for the owner's answer: an agent's question on a finding (an ask), an
// agent's query on a pair (a conflict it left to them), the rotation's check?
// on one of their facts, and the reviewer's question about an agent's note
// (questions.json). An agent closes one when it can prove the answer
// (`tacit questions close`), and code closes one that went moot (a fact it is
// about was retired, replaced or is gone) before the verifier sends
// anything. Each closure is a record in review/decisions.jsonl with its
// evidence and its reason; tacit app and the review page show it, and the
// owner reopens it, which makes the question theirs again for good.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// unreadName is the name of the fact file a parse error of fact.Walk is
// about: its file name without .md.
func unreadName(err error) string {
	rel, _, ok := strings.Cut(err.Error(), ": ")
	if !ok || !strings.HasSuffix(rel, ".md") {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(rel), ".md")
}

// What a question is, as a closure's Closes records it.
const (
	closesAsk      = "ask"      // an agent's question on a finding
	closesQuery    = "query"    // an agent's query on a pair: a conflict it left to the owner
	closesCheck    = "check"    // the rotation's question about one of the owner's facts
	closesQuestion = "question" // the reviewer's question about an agent's note
)

// How a closure is proved. Code's own is moot; an agent gives one of the
// others with its evidence.
var closureProofs = map[string]string{
	"git":    "git history: a commit, a file as a commit left it",
	"file":   "the current files",
	"config": "a configuration: a unit, a settings file, an installed program",
	"words":  "the owner's own words, which code finds as a statement of theirs in their turns",
	"gone":   "its subject is gone: a path, a repository, a service no longer exists",
}

// kindQuestion is a reviewer question as an item of the owner's page, once an
// agent closed it.
const kindQuestion = "question"

// questionOf is what open question an agent left on a pair finding: its ask
// with no mark after it, or its standing query; "" for none.
func questionOf(it reviewItem) string {
	switch {
	case it.Decision == nil && it.Ask != nil && it.Ask.By == "agent":
		return closesAsk
	case it.Decision != nil && it.Decision.By == "agent" && it.Decision.Action == "query":
		return closesQuery
	}
	return ""
}

// staleQuestionOf is the open question on a one-fact finding: an agent's ask
// on a stale finding with no mark after it, or the rotation's check? with no
// answer.
func staleQuestionOf(it staleItem) string {
	switch {
	case it.Kind == kindCheck && it.NeedsOwner: // a statement's optional item is never owed
		return closesCheck
	case it.Kind == kindStale && it.Decision == nil && it.Ask != nil && it.Ask.By == "agent":
		return closesAsk
	}
	return ""
}

// closureText checks what every agent closure carries: a proof it names, the
// evidence and the reason.
func closureText(o *op) error {
	if _, ok := closureProofs[o.Proof]; !ok {
		return fmt.Errorf("unknown proof %q: give git, file, config, words or gone", o.Proof)
	}
	if strings.TrimSpace(o.Evidence) == "" || strings.TrimSpace(o.Reason) == "" {
		return errors.New("a closure needs its evidence and its reason")
	}
	if o.Proof == "words" && (o.Words == "" || o.WordsFrom == "") {
		return errors.New("a words proof needs the owner's words, found in their turns")
	}
	return nil
}

// applyClosure checks an agent's closure of the question a pair finding
// carries and writes it. A closure never changes a ruling: an answer that
// would retire one, replace one or touch its relations is refused and the
// question stays the owner's. Nor does it retire a fact that holds the
// owner's words, but on their own later words (a words proof). The owner's
// own answer, relayed (questions_relay.go), answers their question too: one
// they marked, reopened or left a note on, or kept a fact of, and their
// ruling on their later words on its subject.
func (rs *reviewServer) applyClosure(o *op, it *reviewItem) (string, []string, error) {
	closes := questionOf(*it)
	if closes == "" {
		return "", nil, errors.New("a closure answers a question an agent left on the finding; this one has none open")
	}
	why := o.relays(relayQuestion{asked: pairAsked(*it), question: questionText(*it), facts: []*factView{it.A, it.B}, last: rs.ownerLast(it.ID, it.A.Name, it.B.Name)})
	o.Relayed = why == nil
	if owned := rs.ownerTouched(); (owned[o.ID] || owned[it.A.Name] || owned[it.B.Name]) && !o.Relayed {
		return "", nil, notTheirAnswer("the owner has marked or reopened this finding or one of its facts; it is theirs", o, why)
	}
	if len(it.Notes) > 0 && !o.Relayed {
		return "", nil, notTheirAnswer("the owner left a note on this finding or its facts; it stays their question", o, why)
	}
	if err := closureText(o); err != nil {
		return "", nil, err
	}
	mark := o.Mark
	switch mark {
	case "left", "right", "both", "wrong":
	default:
		return "", nil, fmt.Errorf("a pair's question closes by keeping one fact, both or neither, not %q", mark)
	}
	if it.A.Missing || it.B.Missing || it.A.Retired != "" || it.B.Retired != "" || replacedSide(*it) {
		return "", nil, errors.New("a fact of this finding is retired, replaced or not in the store: the question is moot, and `tacit questions moot` closes it")
	}
	if !contains(it.Allowed, mark) {
		return "", nil, errors.New("that answer is not possible for this finding now")
	}
	a, b := rs.facts[it.A.Name], rs.facts[it.B.Name]
	before := map[string]snap{a.Meta.Name: snapOf(a), b.Meta.Name: snapOf(b)}
	day := time.Now().Format("2006-01-02")
	ca, cb := *a, *b
	ca.Meta.ConflictsWith = append([]string(nil), a.Meta.ConflictsWith...)
	cb.Meta.ConflictsWith = append([]string(nil), b.Meta.ConflictsWith...)
	if mark == "wrong" {
		for _, c := range []*fact.Fact{&ca, &cb} {
			if c.Meta.Retired == "" {
				c.Meta.Retired = day
			}
		}
	} else if err := setState(&ca, &cb, mark); err != nil {
		return "", nil, err
	}
	var touched []*fact.Fact
	for _, c := range []*fact.Fact{&ca, &cb} {
		was := before[c.Meta.Name]
		if sameSnap(c, was) {
			continue
		}
		f := rs.facts[c.Meta.Name]
		if isRuling(f) && !(o.Relayed && o.wordsLetRetire(f)) {
			return "", nil, rulingRefusal(fmt.Sprintf("the answer would change your ruling %s; a closure never does but on your own later words on it, so the question stays yours", c.Meta.Name), o, why)
		}
		gone := (c.Meta.Retired != "" && was.Retired == "") || (c.Meta.SupersededBy != "" && was.SupersededBy == "")
		if w := fact.OwnerWordsOf(f); w != "" && gone && !o.wordsLetRetire(f) {
			return "", nil, fmt.Errorf("the answer retires %s, which holds the owner's words %q; only their own later words (a words proof) let a closure do that", c.Meta.Name, w)
		}
		c.Meta.Updated = day
		touched = append(touched, c)
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
	d := closureDecision(o, closes, it.A.Name, it.B.Name)
	d.Before, d.Question = before, questionText(*it)
	if o.dry {
		return "the closure would land", nil, nil
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, "", files)
}

// questionText is what an agent's question on a pair asked: its ask, or for a
// query the conflict it left to the owner, kept with the closure so the
// owner's page shows what was closed.
func questionText(it reviewItem) string {
	switch {
	case it.Decision == nil && it.Ask != nil:
		return it.Ask.Question
	case it.Decision != nil && it.Decision.Action == "query":
		return "An agent marked these as conflicting and left the call to you: " + it.Decision.Evidence
	}
	return ""
}

// closureDecision is the record an agent's closure leaves.
func closureDecision(o *op, closes, a, b string) decision {
	return decision{ID: o.ID, Action: o.Mark, A: a, B: b, At: time.Now().Format(time.RFC3339), By: "agent",
		Evidence: o.Evidence, Reason: o.Reason, Proof: o.Proof, Closes: closes, Session: o.Session, Model: o.Model,
		Words: o.Words, WordsFrom: o.WordsFrom, WordsOn: o.WordsOn, Relayed: o.Relayed}
}

// applyStaleClosure checks an agent's closure of the question a one-fact
// finding carries: its own ask on a stale finding (still true or outdated),
// or the rotation's check? (still true only: only the owner retires their
// fact). A ruling is never retired. The owner's own answer, relayed
// (questions_relay.go), retires their fact, and their ruling on their later
// words on it.
func (rs *reviewServer) applyStaleClosure(o *op, it *staleItem) (string, []string, error) {
	name := it.A.Name
	closes := staleQuestionOf(*it)
	if closes == "" {
		return "", nil, errors.New("a closure answers a question left on the finding; this one has none open")
	}
	asked, question := it.Flag.Since, "The rotation read this fact against what tacit knows now: "+strings.Join(it.Flag.Detail, " ")
	if closes == closesAsk {
		asked, question = it.Ask.At, it.Ask.Question
	}
	why := o.relays(relayQuestion{asked: asked, question: question, facts: []*factView{it.A}, last: rs.ownerLast(it.ID, name)})
	o.Relayed = why == nil
	if owned := rs.ownerTouched(); (owned[o.ID] || owned[name]) && !o.Relayed {
		return "", nil, notTheirAnswer("the owner has marked or reopened this finding, or kept this fact in a pair; it is theirs", o, why)
	}
	if len(it.Notes) > 0 && !o.Relayed {
		return "", nil, notTheirAnswer("the owner left a note on this finding or its fact; it stays their question", o, why)
	}
	if err := closureText(o); err != nil {
		return "", nil, err
	}
	if it.State != "live" {
		return "", nil, errors.New("this fact is already retired or gone: the question is moot, and `tacit questions moot` closes it")
	}
	f := rs.facts[name]
	switch {
	case o.Mark == "current":
	case o.Mark == "retire" && closes == closesCheck && !o.Relayed:
		return "", nil, notTheirAnswer("the rotation's question is about the owner's own fact: a closure may say it still holds, never retire it but on their own answer", o, why)
	case o.Mark == "retire" && isRuling(f) && !(o.Relayed && o.wordsLetRetire(f)):
		return "", nil, rulingRefusal(fmt.Sprintf("%s is your ruling; a closure never retires one but on your own later words on it, so the question stays yours", name), o, why)
	case o.Mark == "retire" && fact.OwnerWordsOf(f) != "" && !o.wordsLetRetire(f):
		return "", nil, fmt.Errorf("%s holds the owner's words; only their own later words (a words proof) let a closure retire it", name)
	case o.Mark == "retire":
	default:
		return "", nil, fmt.Errorf("a one-fact question closes as still true (current) or outdated (retire), not %q", o.Mark)
	}
	o.A, o.B = name, it.Flag.Sha
	d := closureDecision(o, closes, name, it.Flag.Sha)
	if it.Ask != nil {
		d.Question = it.Ask.Question
	}
	if o.dry {
		return "the closure would land", nil, nil
	}
	files := map[string][]byte{}
	if o.Mark == "retire" {
		c := *f
		d.Before = map[string]snap{name: snapOf(&c)}
		day := time.Now().Format("2006-01-02")
		c.Meta.Retired, c.Meta.Updated = day, day
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
		if err != nil {
			return "", nil, err
		}
		nb, err := fact.PatchRelations(old, &c)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		files[f.Path] = nb
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, "", files)
}

// questionMoot is why the question a pair finding carries is moot, "" when it
// is not, and the move to the live facts when a fact of it has a live
// successor (planRepoint): a fact of it is gone from the store, was replaced
// by a third fact, or was retired.
func (rs *reviewServer) questionMoot(it reviewItem, ix findingIndex) (string, repointPlan) {
	if rs.walkErr != nil || (it.A.Missing && rs.unread[it.A.Name]) || (it.B.Missing && rs.unread[it.B.Name]) {
		return "", repointPlan{} // a fact that did not read is not gone
	}
	switch {
	case it.A.Missing && it.B.Missing:
		return fmt.Sprintf("moot: neither %s nor %s is in the store", it.A.Name, it.B.Name), repointPlan{}
	case it.A.Missing || it.B.Missing:
		gone := it.A.Name
		if !it.A.Missing {
			gone = it.B.Name
		}
		return fmt.Sprintf("moot: %s is not in the store", gone), repointPlan{}
	case replacedSide(it):
		p := rs.planRepoint(it, ix)
		return p.Why, p
	case it.A.Retired != "" && it.B.Retired != "":
		return fmt.Sprintf("moot: both facts are retired (%s on %s, %s on %s)", it.A.Name, it.A.Retired, it.B.Name, it.B.Retired), repointPlan{}
	case it.A.Retired != "":
		return fmt.Sprintf("moot: %s is retired (%s) and no fact replaces it; %s stands", it.A.Name, it.A.Retired, it.B.Name), repointPlan{}
	case it.B.Retired != "":
		return fmt.Sprintf("moot: %s is retired (%s) and no fact replaces it; %s stands", it.B.Name, it.B.Retired, it.A.Name), repointPlan{}
	}
	return "", repointPlan{}
}

// mootQuestion reports a pair finding whose agent's question went moot and
// that the curator may close: not the owner's, no note of theirs on it.
func (rs *reviewServer) mootQuestion(it reviewItem, owned map[string]bool, ix findingIndex) (string, repointPlan, bool) {
	if questionOf(it) == "" || len(it.Notes) > 0 || owned[it.ID] || owned[it.A.Name] || owned[it.B.Name] {
		return "", repointPlan{}, false
	}
	why, p := rs.questionMoot(it, ix)
	return why, p, why != ""
}

// applyQuestionMoot closes, as the curator, the moot question a pair finding
// carries, and raises the finding on the live facts when its facts lead to
// them, planned again on the store as it is under the lock.
func (rs *reviewServer) applyQuestionMoot(o *op, it *reviewItem) (string, []string, error) {
	if o.By != "curator" {
		return "", nil, errors.New("only the curator closes a question as moot")
	}
	closes := questionOf(*it)
	why, p, ok := rs.mootQuestion(*it, rs.ownerTouched(), indexFindings(rs.items()))
	if !ok {
		return "", nil, errors.New("this finding carries no moot question of an agent's")
	}
	files := map[string][]byte{}
	if p.Line != "" {
		rel := repointedFile(time.Now())
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, rel))
		if err != nil && !os.IsNotExist(err) {
			return "", nil, err
		}
		if len(old) > 0 && old[len(old)-1] != '\n' {
			old = append(old, '\n')
		}
		files[rel] = append(old, p.Line...)
	}
	d := decision{ID: o.ID, Action: mootMark, A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339), By: "curator", Evidence: why, Closes: closes, Proof: mootMark, Question: questionText(*it)}
	if p.NewA != "" {
		d.Repointed = p.To
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, why, files)
}

// applyStaleMoot closes, as the curator, an agent's question on a one-fact
// finding whose fact was retired, replaced or is gone.
func (rs *reviewServer) applyStaleMoot(o *op, it *staleItem) (string, []string, error) {
	if o.By != "curator" {
		return "", nil, errors.New("only the curator closes a question as moot")
	}
	why, ok := rs.staleMoot(*it, rs.ownerTouched())
	if !ok {
		return "", nil, errors.New("this finding carries no moot question")
	}
	files := map[string][]byte{}
	rs.appendDecision(files, decision{ID: o.ID, Action: mootMark, A: it.A.Name, B: it.Flag.Sha, At: time.Now().Format(time.RFC3339), By: "curator", Evidence: why, Closes: closesAsk, Proof: mootMark, Question: it.Ask.Question})
	return writeFiles(rs.s.Dir, why, files)
}

// staleMoot is why an agent's question on a one-fact finding is moot: its
// fact is not live any more. The owner's findings stay theirs.
func (rs *reviewServer) staleMoot(it staleItem, owned map[string]bool) (string, bool) {
	if it.Kind != kindStale || it.Decision != nil || it.Ask == nil || it.Ask.By != "agent" || owned[it.ID] || owned[it.A.Name] || len(it.Notes) > 0 {
		return "", false
	}
	if rs.walkErr != nil || (it.A.Missing && rs.unread[it.A.Name]) {
		return "", false // a fact that did not read is not gone
	}
	switch {
	case it.A.Missing:
		return "moot: " + it.A.Name + " is not in the store", true
	case it.A.SupersededBy != "":
		return "moot: " + it.A.Name + " is replaced by " + it.A.SupersededBy, true
	case it.A.Retired != "":
		return "moot: " + it.A.Name + " is retired (" + it.A.Retired + ")", true
	}
	return "", false
}

// closeMootQuestions closes every question that went moot: on a pair, a
// fact replaced, retired or gone (moved to the live facts when both lead to
// one, as Replaced facts moves a finding); on a stale finding, its fact not
// live. Commits of at most repointChunk; a dry run prints and writes nothing.
// It returns how many it closed.
func closeMootQuestions(ctx context.Context, rs *reviewServer, owned map[string]bool, only map[string]bool, dry bool, out func(string)) (int, error) {
	if rs.walkErr != nil {
		// Every fact would look gone: nothing is closed on a store not read.
		return 0, fmt.Errorf("the store's facts could not be read, so no question was closed: %w", rs.walkErr)
	}
	items := rs.items()
	ix := indexFindings(items)
	var ops []op
	// A stale ask the curator closed as moot whose fact is live again (its
	// retirement or replacement undone) asks again.
	for _, it := range rs.staleItems() {
		d := it.Decision
		if (len(only) > 0 && !only[it.ID]) || d == nil || d.Action != mootMark || d.By != "curator" || d.Closes == "" || it.State != "live" || owned[it.ID] || owned[it.A.Name] {
			continue
		}
		ops = append(ops, op{ID: it.ID, Mark: "reopen", By: "curator"})
		if dry {
			out(fmt.Sprintf("would reopen %s (%s, its ask): its fact is live again", it.ID, it.A.Name))
		}
	}
	for _, it := range items {
		if len(only) > 0 && !only[it.ID] {
			continue
		}
		if why, _, ok := rs.mootQuestion(it, owned, ix); ok {
			ops = append(ops, op{ID: it.ID, Mark: mootMark, By: "curator", A: it.A.Name, B: it.B.Name})
			if dry {
				out(fmt.Sprintf("would close %s (%s / %s, its %s): %s", it.ID, it.A.Name, it.B.Name, questionOf(it), why))
			}
		}
	}
	for _, it := range rs.staleItems() {
		if len(only) > 0 && !only[it.ID] {
			continue
		}
		if why, ok := rs.staleMoot(it, owned); ok {
			ops = append(ops, op{ID: it.ID, Mark: mootMark, By: "curator"})
			if dry {
				out(fmt.Sprintf("would close %s (%s, its ask): %s", it.ID, it.A.Name, why))
			}
		}
	}
	if dry || len(ops) == 0 {
		return 0, nil
	}
	var lastErr error
	for start := 0; start < len(ops); start += repointChunk {
		chunk := ops[start:min(start+repointChunk, len(ops))]
		actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		_, _, err := rs.applyOps(actx, chunk)
		cancel()
		if err != nil {
			lastErr = err
		}
	}
	ds, _ := rs.decisions()
	n := 0
	for _, o := range ops {
		d := ds[o.ID]
		if o.Mark == "reopen" {
			if d == nil {
				out(fmt.Sprintf("reopened %s: its fact is live again", o.ID))
			}
			continue
		}
		if d == nil || d.Action != mootMark || d.Closes == "" {
			out(fmt.Sprintf("%s not closed", o.ID))
			continue
		}
		n++
		out(fmt.Sprintf("closed %s (its %s): %s", o.ID, d.Closes, d.Evidence))
	}
	return n, lastErr
}

// --- the reviewer's questions (questions.json) ---

// questionAnswers are what an agent may prove of an agent's note the
// reviewer asked the owner about: it holds, it is wrong (the facts made from
// it are retired), or the question went moot (its subject is gone).
var questionAnswers = map[string]bool{"holds": true, "wrong": true, "moot": true}

// questionRecords is the standing record of every reviewer question in the
// decision log (an agent's closure; a reopen clears it) and whether the owner
// ever reopened it.
func (rs *reviewServer) questionRecords() (standing map[string]*decision, reopened map[string]bool) {
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	return questionRecordsOf(b)
}

// questionRecordsOf is questionRecords of a decision log's bytes.
func questionRecordsOf(b []byte) (standing map[string]*decision, reopened map[string]bool) {
	standing, reopened = map[string]*decision{}, map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) != nil || !strings.HasPrefix(d.ID, "q-") {
			continue
		}
		dd := d
		switch {
		case d.Action == "reopen":
			delete(standing, d.ID)
			if d.By == "" {
				reopened[d.ID] = true
			}
		case d.By == "agent" && d.Closes == closesQuestion:
			standing[d.ID] = &dd
		}
	}
	return
}

// questionClosures is, from the store's decision log at dir, what each
// closure of a reviewer question said (its answer, proof and reason) and the
// questions the owner reopened, for the reviewer step's queue.
func questionClosures(dir string) (closed map[string]string, reopened map[string]bool) {
	// A log that does not read whole (missing, empty, or a line cut short:
	// read while a write rewrites it) says nothing: the queue is left as it
	// is. The bytes checked are the bytes read.
	b, err := os.ReadFile(filepath.Join(dir, "review", "decisions.jsonl"))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" && !json.Valid([]byte(l)) {
			return nil, nil
		}
	}
	standing, reopened := questionRecordsOf(b)
	closed = map[string]string{}
	for id, d := range standing {
		closed[id] = d.Action + " (" + d.Proof + "): " + oneLineNote(d.Reason)
	}
	return closed, reopened
}

// questionItem is a reviewer question an agent closed, as the owner's page
// shows it: one note (and the fact made from it), the question, the closure.
type questionItem struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	A          *factView `json:"a"`
	Single     bool      `json:"single"`
	Question   string    `json:"question"`
	State      string    `json:"state"`
	Allowed    []string  `json:"allowed"`
	Mark       string    `json:"mark,omitempty"`
	Decision   *decision `json:"decision,omitempty"`
	NeedsOwner bool      `json:"needs_owner"`
}

// questionItems is every reviewer question an agent closed, newest first: the
// owner sees each with its proof and reopens it there.
func (rs *reviewServer) questionItems() []questionItem {
	if rs.labelling() {
		return nil
	}
	standing, _ := rs.questionRecords()
	var out []questionItem
	for id, d := range standing {
		v := rs.view(d.B)
		if d.B == "" || v.Missing {
			v = &factView{Name: filepath.Base(strings.TrimSuffix(d.A, ".md")), Path: d.A, Missing: true}
			if in := rs.inboxByPath(d.A); in != nil {
				v = &factView{Name: in.Meta.Name, Path: in.Path, Scope: in.Meta.Scope, Type: in.Meta.Type, Kind: in.Meta.Source.Kind, Description: in.Meta.Description, Body: strings.TrimSpace(in.Body), Created: in.Meta.Created, Harness: in.Meta.Source.Harness}
			}
		}
		it := questionItem{ID: id, Kind: kindQuestion, Source: "questions", A: v, Single: true, Question: d.Question, State: "closed", Mark: d.Action, Decision: d, Allowed: []string{}}
		if rs.viewOnly == "" {
			it.Allowed = []string{"reopen"}
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Decision.At > out[j].Decision.At })
	return out
}

// inboxByPath is the inbox note at a store path.
func (rs *reviewServer) inboxByPath(rel string) *fact.Fact {
	for _, f := range rs.inbox {
		if f.Path == rel {
			return f
		}
	}
	return nil
}

// applyQuestionOp writes the owner's reopen of a reviewer question an agent
// closed: what the closure retired comes back, and the question waits for
// them at their prompt again (the reviewer step reads the reopen; this
// machine's queue is set at once). Only the owner reopens; agents close
// through closeReviewerQuestion.
func (rs *reviewServer) applyQuestionOp(o *op) (string, []string, error) {
	if o.Mark != "reopen" || o.By != "" {
		return "", nil, errors.New("a reviewer question takes only the owner's reopen here")
	}
	standing, _ := rs.questionRecords()
	prev := standing[o.ID]
	if prev == nil {
		return "", nil, errors.New("there is no closure to reopen")
	}
	files := map[string][]byte{}
	note := ""
	day := prev.At
	if len(day) > 10 {
		day = day[:10] // the closure retired its facts on the day it was made
	}
	for name, b := range prev.Before {
		f := rs.facts[name]
		if f == nil {
			note = "A fact the closure retired is gone from the store."
			continue
		}
		if f.Meta.Retired == "" {
			continue
		}
		if f.Meta.Retired != day {
			// Retired again since, by someone else: that retirement stands.
			note = name + " was retired again after the closure; it stays as it is."
			continue
		}
		c := *f
		c.Meta.Retired, c.Meta.Updated = b.Retired, b.Updated // as it was before the closure
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
		if err != nil {
			return "", nil, err
		}
		nb, err := fact.PatchRelations(old, &c)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		files[f.Path] = nb
	}
	rs.appendDecision(files, decision{ID: o.ID, Action: "reopen", A: prev.A, B: prev.B, At: time.Now().Format(time.RFC3339)})
	n, w, err := writeFiles(rs.s.Dir, note, files)
	if err == nil {
		home, _ := os.UserHomeDir()
		_ = agentreview.Locked(agentreview.Path(platform.StateDir(home)), 5*time.Second, func(q *agentreview.Queue) bool {
			return reopenQuestion(q, o.ID)
		})
	}
	return n, w, err
}

// syncQuestionClosures brings this machine's queue in step with the store's
// decision log: an open question an agent closed (here or on another run) is
// closed with what the closure said, and a closed one the owner reopened
// waits for them again. closed nil: nothing read, nothing changes. It returns
// how many it closed and reopened.
func syncQuestionClosures(q *agentreview.Queue, closed map[string]string, reopened map[string]bool, now time.Time) (int, int) {
	if closed == nil {
		return 0, 0
	}
	c, r := 0, 0
	for i := range q.Items {
		it := &q.Items[i]
		why, isClosed := closed[it.ID]
		switch {
		case it.State == agentreview.StateOpen && isClosed:
			it.Close(agentreview.StateClosed, why, now)
			c++
		case it.State == agentreview.StateClosed && !isClosed && reopened[it.ID]:
			if reopenQuestion(q, it.ID) {
				r++
			}
		case it.State == agentreview.StateClosed && !isClosed && closingLong(it.Closed, now):
			// Closed here by a closure the store's log never got (a run
			// stopped before its commit), long past any closure's deadline:
			// it waits for the owner again, as it was.
			it.State, it.Closed, it.Why = agentreview.StateOpen, "", ""
			r++
		}
	}
	return c, r
}

// closingGrace is how long a question this machine closed may wait for its
// closure in the store's log before it waits for the owner again: past the
// closure command's 3 minutes, so a closure still on its way is never
// undone.
const closingGrace = 10 * time.Minute

// closingLong reports a queue close older than closingGrace.
func closingLong(closed string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, closed)
	return err == nil && now.Sub(at) > closingGrace
}

// reopenQuestion puts a closed question back in the queue, asked afresh: a
// new day queued and no sessions asked, so the queue's own limits (30 days,
// 3 sessions) count from the owner's reopen. It reports a change.
func reopenQuestion(q *agentreview.Queue, id string) bool {
	it := q.ByID(id)
	if it == nil || it.State != agentreview.StateClosed {
		return false
	}
	it.State, it.Closed, it.Why, it.Asks = agentreview.StateOpen, "", "", nil
	it.Queued = time.Now().UTC().Format(time.RFC3339)
	return true
}

// closeReviewerQuestion writes an agent's closure of a reviewer question: a
// record in the decision log (the store keeps it, the owner's page shows it)
// and, for wrong, the retirement of the facts made from the note. The
// question is closed in this machine's queue first, in the lock section that
// reads the owner's answers, and put back when the store write fails. A fact
// made from the note that is a ruling, holds the owner's words, carries the
// owner's verdict on the note, or is theirs is never retired by it. A dry
// run checks every guard on the clone as it is and writes nothing.
func closeReviewerQuestion(ctx context.Context, rs *reviewServer, o *op, q agentreview.Item) (string, error) {
	if !questionAnswers[o.Mark] {
		return "", fmt.Errorf("a reviewer question closes as holds, wrong or moot, not %q", o.Mark)
	}
	if o.Mark == "moot" {
		if o.Proof != "gone" {
			return "", errors.New("a reviewer question goes moot when its subject is gone: give --proof gone with the evidence")
		}
	} else if o.Proof == "gone" {
		return "", errors.New("a subject that is gone makes the question moot: --answer moot")
	}
	if err := closureText(o); err != nil {
		return "", err
	}
	if !o.dry {
		if err := rs.writeRefusal(ctx); err != nil {
			return "", err
		}
	}
	// The owner's own answer comes first: any answer of theirs to it, read
	// now from the sessions it was put in or read before, keeps every agent
	// from closing it.
	restore, err := claimQuestion(ctx, q.ID, o.Mark+" ("+o.Proof+"): "+oneLineNote(o.Reason), o.dry)
	if err != nil {
		return "", err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if o.dry {
		rs.loadFacts() // the clone as it is: no lock, no sync
		if err := rs.mayCloseQuestion(q); err != nil {
			return "", err
		}
		if _, err := rs.wrongFacts(o, q); err != nil {
			return "", err
		}
		return "the closure would land", nil
	}
	kept := false // a local commit kept after a failed push: it goes up with the next push
	err = rs.s.WithLock(ctx, func() error {
		for attempt := 0; attempt < 4; attempt++ {
			if err := rs.s.SyncLocked(ctx); err != nil {
				var st *store.Stranded
				if !errors.As(err, &st) {
					return fmt.Errorf("could not sync the store, nothing was closed (%s)", shortErr(err))
				}
			}
			rs.loadFacts()
			if err := rs.mayCloseQuestion(q); err != nil {
				return err
			}
			made, err := rs.wrongFacts(o, q)
			if err != nil {
				return err
			}
			d := closureDecision(o, closesQuestion, q.Note, q.Fact)
			d.Question = q.Text()
			files := map[string][]byte{}
			if o.Mark == "wrong" {
				d.Before = map[string]snap{}
				day := time.Now().Format("2006-01-02")
				for _, f := range made {
					c := *f
					d.Before[f.Meta.Name] = snapOf(&c)
					c.Meta.Retired, c.Meta.Updated = day, day
					old, err := os.ReadFile(filepath.Join(rs.s.Dir, f.Path))
					if err != nil {
						return err
					}
					nb, err := fact.PatchRelations(old, &c)
					if err != nil {
						return fmt.Errorf("%s: %w", f.Path, err)
					}
					files[f.Path] = nb
				}
			}
			rs.appendDecision(files, d)
			head, _ := rs.s.Head(ctx)
			_, written, err := writeFiles(rs.s.Dir, "", files)
			if err != nil {
				rs.s.Restore(written)
				return err
			}
			rels := written
			all, _, err := fact.Walk(rs.s.Dir)
			if err == nil {
				var ix []string
				ix, err = regenIndexes(ctx, rs.s.Dir, all, nil)
				rels = append(rels, ix...)
			}
			if err != nil {
				rs.s.Restore(rels)
				return err
			}
			msg := fmt.Sprintf("review: close %s (%s, %s proof) by agent", q.ID, o.Mark, o.Proof)
			err = rs.s.CommitPushOnceLocked(ctx, existing(rs.s.Dir, append(dedupe(rels), ".gitattributes")), msg, "curator")
			if errors.Is(err, store.ErrBehind) {
				continue
			}
			if err != nil {
				// HEAD read on its own deadline: a closure whose own ran out
				// before its commit made none, and keeps nothing.
				hctx, hcancel := context.WithTimeout(context.Background(), 15*time.Second)
				now, herr := rs.s.Head(hctx)
				hcancel()
				if herr == nil && now != head {
					kept = true
				} else {
					rs.s.Restore(rels) // no commit was made: nothing stays written
				}
			}
			return err
		}
		return errors.New("the store kept changing on the server; nothing was closed, try again")
	})
	rs.loadFacts()
	if err != nil {
		if kept {
			return "", fmt.Errorf("closed in this clone, but the push failed; the commit goes up with the next push: %w", err)
		}
		restore() // the question waits in this machine's queue again, as it was
		return "", err
	}
	return "", nil
}

// mayCloseQuestion checks the decision log under the lock: a reviewer
// question an agent closed already, or one the owner reopened, takes no
// closure.
func (rs *reviewServer) mayCloseQuestion(q agentreview.Item) error {
	if rs.walkErr != nil {
		return fmt.Errorf("the store's facts could not be read, so nothing was closed: %w", rs.walkErr)
	}
	standing, reopened := rs.questionRecords()
	if standing[q.ID] != nil {
		return errors.New("this question is closed already")
	}
	if reopened[q.ID] {
		return errors.New("the owner reopened this question; it is theirs")
	}
	return nil
}

// wrongFacts is, for a wrong closure, the facts made from the question's
// note that it retires, checked on the store as it is: none may be the
// owner's (kept or noted by them), a ruling, hold their words but on their
// own later words, or carry their verdict on the note (source.review), and
// no fact file that names the note as its origin may fail to parse.
func (rs *reviewServer) wrongFacts(o *op, q agentreview.Item) ([]*fact.Fact, error) {
	if o.Mark != "wrong" {
		return nil, nil
	}
	origin := regexp.MustCompile(`(?m)^origin:\s*["']?` + regexp.QuoteMeta(q.Name) + `["']?\s*$`)
	for _, rel := range rs.unreadPaths {
		if b, err := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(rel))); err == nil && origin.Match(b) {
			return nil, fmt.Errorf("%s, made from this note, does not parse here; the question stays the owner's while it does not", rel)
		}
	}
	made := rs.madeFrom(q)
	owned, notes := rs.ownerTouched(), rs.ownerNotes()
	for _, f := range made {
		if owned[f.Meta.Name] || len(notes.For("", f.Meta.Name)) > 0 {
			return nil, fmt.Errorf("%s, made from this note, is one the owner kept or left a note on; it stays their question", f.Meta.Name)
		}
		if isRuling(f) {
			return nil, fmt.Errorf("%s, made from this note, is your ruling; a closure never retires one, so the question stays yours", f.Meta.Name)
		}
		if w := fact.OwnerWordsOf(f); w != "" && !o.wordsLetRetire(f) {
			return nil, fmt.Errorf("%s, made from this note, holds the owner's words %q; only their own later words let a closure retire it", f.Meta.Name, w)
		}
		if v := f.Meta.Source.Review; ledger.OwnerVerdict(v) {
			return nil, fmt.Errorf("%s, made from this note, carries the owner's answer on it (source.review: %s); it stays theirs", f.Meta.Name, v)
		}
	}
	return made, nil
}

// claimQuestion reads the owner's answer to one question of this machine's
// queue from the sessions it was put in (agentreview.ReadAnswers, for this
// question only) and refuses a closure unless it is still open: an answer of
// theirs, read now or before, comes first. For a closure it then closes the
// question in the same queue-lock section, so no answer read later lands on
// top of the agent's, and returns how to put it back as it was when the
// store write fails. A dry run reads a copy and writes nothing.
func claimQuestion(ctx context.Context, id, why string, dry bool) (func(), error) {
	home, _ := os.UserHomeDir()
	src := agentreview.DefaultSources(home)
	if v := os.Getenv("TACIT_HERMES_DB"); strings.EqualFold(v, "off") {
		src.HermesDB = ""
	}
	qpath := agentreview.Path(platform.StateDir(home))
	read := func(it *agentreview.Item) bool {
		if it.State != agentreview.StateOpen {
			return false
		}
		one := &agentreview.Queue{Items: []agentreview.Item{*it}}
		one.Items[0].Asks = append([]agentreview.Ask(nil), it.Asks...)
		changed := agentreview.ReadAnswers(ctx, one, src, nil, time.Now())
		// And in the rulebook pane.
		if agentreview.TakeRulebookAnswers(one, agentreview.RulebookAnswers(agentreview.ReadRulebook(agentreview.RulebookLog(home)))) {
			changed = true
		}
		if !changed {
			return false
		}
		*it = one.Items[0]
		return true
	}
	answered := func(it *agentreview.Item) error {
		if it.State == agentreview.StateOpen {
			return nil
		}
		return fmt.Errorf("the owner answered this question, or it is no longer asked (%s); no agent closes it", it.State)
	}
	if dry {
		q, err := agentreview.Load(qpath)
		if err != nil {
			return nil, fmt.Errorf("the owner's answers could not be read: %w", err)
		}
		it := q.ByID(id)
		if it == nil {
			return nil, fmt.Errorf("%s is no question in this machine's queue", id)
		}
		read(it)
		return func() {}, answered(it)
	}
	var refused error
	var prev agentreview.Item
	err := agentreview.Locked(qpath, 10*time.Second, func(q *agentreview.Queue) bool {
		it := q.ByID(id)
		if it == nil {
			refused = fmt.Errorf("%s is no question in this machine's queue", id)
			return false
		}
		changed := read(it)
		if refused = answered(it); refused != nil {
			return changed
		}
		prev = *it
		prev.Asks = append([]agentreview.Ask(nil), it.Asks...)
		it.Close(agentreview.StateClosed, why, time.Now())
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("the owner's answers could not be read, so nothing was closed: %w", err)
	}
	if refused != nil {
		return nil, refused
	}
	return func() {
		_ = agentreview.Locked(qpath, 10*time.Second, func(q *agentreview.Queue) bool {
			it := q.ByID(id)
			if it == nil || it.State != agentreview.StateClosed || it.Why != why {
				return false
			}
			*it = prev
			return true
		})
	}, nil
}

// madeFrom is the live curated facts made from a question's note.
func (rs *reviewServer) madeFrom(q agentreview.Item) []*fact.Fact {
	var out []*fact.Fact
	for _, f := range rs.facts {
		if f.Meta.Origin == q.Name && f.Meta.Retired == "" && f.Meta.SupersededBy == "" {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// --- tacit questions ---

// questionRow is one question as `tacit questions` lists it.
type questionRow struct {
	ID       string   `json:"id"`
	Closes   string   `json:"kind"`              // ask, query, check, question
	Finding  string   `json:"finding,omitempty"` // the finding's kind
	Asked    string   `json:"asked,omitempty"`   // when it was asked
	Question string   `json:"question"`
	Facts    []string `json:"facts"`
	Answers  []string `json:"answers"`
	// Owners is why an agent may not close it on its own proof (the owner
	// touched it, left a note on it or reopened it, an agent asked them);
	// empty when it may.
	Owners string `json:"owners,omitempty"`
	// WordsOnly: only the owner's own answer closes it, which an agent relays
	// (tacit questions close --proof words): a question of theirs, or a name
	// two files hold.
	WordsOnly bool `json:"words_only,omitempty"`
	// Moot is why the question went moot: code closes it (tacit questions
	// moot), no agent does.
	Moot   string    `json:"moot,omitempty"`
	Closed *decision `json:"closed,omitempty"`
}

// openQuestions is every question waiting for the owner on this machine.
func openQuestions(rs *reviewServer, q *agentreview.Queue) []questionRow {
	var out []questionRow
	owned := rs.ownerTouched()
	items := rs.items()
	ix := indexFindings(items)
	for _, it := range items {
		k := questionOf(it)
		if k == "" {
			continue
		}
		r := questionRow{ID: it.ID, Closes: k, Finding: it.Kind, Facts: []string{it.A.Name, it.B.Name}, Answers: []string{"keep:" + it.A.Name, "keep:" + it.B.Name, "both", "neither"}}
		switch {
		case owned[it.ID] || owned[it.A.Name] || owned[it.B.Name]:
			r.Owners, r.WordsOnly = "the owner marked or reopened it, or kept one of its facts", true
		case len(it.Notes) > 0:
			r.Owners, r.WordsOnly = "the owner left a note on it", true
		default:
			r.Moot, _ = rs.questionMoot(it, ix)
		}
		if k == closesAsk {
			r.Asked, r.Question = it.Ask.At, it.Ask.Question
		} else {
			r.Asked, r.Question = it.Decision.At, "An agent read these as in conflict and left the call to the owner: "+it.Decision.Evidence
		}
		out = append(out, r)
	}
	for _, it := range append(rs.staleItems(), rs.checkItems()...) {
		k := staleQuestionOf(it)
		if k == "" || it.State != "live" {
			continue
		}
		r := questionRow{ID: it.ID, Closes: k, Finding: it.Kind, Facts: []string{it.A.Name}, Answers: []string{"current", "retire"}}
		switch {
		case owned[it.ID] || owned[it.A.Name]:
			r.Owners, r.WordsOnly = "the owner marked or reopened it, or kept its fact", true
		case len(it.Notes) > 0:
			r.Owners, r.WordsOnly = "the owner left a note on it", true
		}
		if k == closesAsk {
			r.Asked, r.Question = it.Ask.At, it.Ask.Question
		} else {
			r.Asked, r.Question, r.Answers = it.Flag.Since, "The rotation read this fact of the owner's against what tacit knows now: "+strings.Join(it.Flag.Detail, " "), []string{"current"}
			if r.WordsOnly {
				r.Answers = []string{"current", "retire"} // their own answer may retire it
			}
		}
		out = append(out, r)
	}
	// A name two files hold waits for an agent's mark or the owner's answer:
	// the verifier does not take it (names_review.go). A closure takes it
	// only as the owner's own answer, relayed; without their words an agent
	// keeps one file with tacit review apply.
	for _, it := range rs.nameItems() {
		if it.State == "gone" || it.Decision != nil {
			continue
		}
		r := questionRow{ID: it.ID, Closes: kindName, Finding: kindName, Asked: strings.TrimSuffix(it.Source, ".md"), Facts: []string{it.A.Path, it.B.Path},
			Question: nameQuestion(it), Answers: []string{"keep:" + it.A.Path, "keep:" + it.B.Path, "both", "neither"}, WordsOnly: true}
		if it.Ask != nil {
			r.Closes, r.Asked, r.Question = closesAsk, it.Ask.At, it.Ask.Question
		}
		switch {
		case it.Crowded:
			r.Owners, r.Answers, r.WordsOnly = "more than two files hold this name (also "+strings.Join(it.Also, ", ")+"); the owner retires the copies, file by file, in tacit app", []string{}, false
		case it.Ask != nil:
			r.Owners = "an agent asked the owner"
		case owned[it.ID] || owned[it.A.Name]:
			r.Owners = "the owner marked or reopened it, or kept a fact of this name"
		case len(it.Notes) > 0:
			r.Owners = "the owner left a note on it"
		}
		out = append(out, r)
	}
	if q != nil {
		_, reopened := rs.questionRecords()
		for _, it := range q.Open() {
			r := questionRow{ID: it.ID, Closes: closesQuestion, Asked: it.Queued, Question: it.Text(), Facts: nonEmpty(it.Fact, it.Note), Answers: []string{"holds", "wrong", "moot"}}
			if reopened[it.ID] {
				r.Owners = "the owner reopened it"
			}
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Asked < out[j].Asked })
	return out
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// runQuestions implements `tacit questions [--json]`, `tacit questions close
// ID ...` and `tacit questions moot [--dry-run]`.
func runQuestions(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "close":
			return runQuestionsClose(args[1:])
		case "moot":
			return runQuestionsMoot(args[1:])
		}
	}
	fs := flag.NewFlagSet("questions", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print them as JSON")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "questions: no store")
		return 2
	}
	rs := &reviewServer{s: s}
	rs.loadFacts()
	home, _ := os.UserHomeDir()
	q, err := agentreview.Load(agentreview.Path(platform.StateDir(home)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "questions:", err)
		return 1
	}
	rows := openQuestions(rs, q)
	if *asJSON {
		if rows == nil {
			rows = []questionRow{}
		}
		b, _ := json.MarshalIndent(rows, "", " ")
		fmt.Println(string(b))
		return 0
	}
	if len(rows) == 0 {
		fmt.Println("no questions waiting")
		return 0
	}
	for _, r := range rows {
		day := r.Asked
		if len(day) > 10 {
			day = day[:10]
		}
		answers := "answers: " + strings.Join(r.Answers, ", ")
		switch {
		case r.Moot != "":
			answers = "moot, `tacit questions moot` closes it: " + strings.TrimPrefix(r.Moot, "moot: ")
		case r.Owners != "" && r.WordsOnly:
			answers = "the owner's (" + r.Owners + "): an agent closes it only with their own answer (--proof words); answers: " + strings.Join(r.Answers, ", ")
		case r.Owners != "":
			answers = "the owner's, no agent closes it: " + r.Owners
		case r.WordsOnly:
			answers = "answers, on the owner's own words (--proof words): " + strings.Join(r.Answers, ", ") + "; without them an agent keeps one file with tacit review apply"
		}
		fmt.Printf("%s  %s (%s), asked %s, about %s\n  %s\n  %s\n", r.ID, r.Closes, firstNonEmpty(r.Finding, "note"), day, strings.Join(r.Facts, ", "), oneLineNote(r.Question), answers)
	}
	return 0
}

// idLast moves a leading id after the flags (`close ID --answer ...`): the
// flag package stops at the first word that is not a flag.
func idLast(args []string) []string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return append(append([]string(nil), args[1:]...), args[0])
	}
	return args
}

// runQuestionsClose implements `tacit questions close ID --harness H
// --answer A --proof P --evidence E --reason R [--words W [--from REF]]
// [--dry-run]`: an agent's closure of one question it can prove.
func runQuestionsClose(args []string) int {
	fs := flag.NewFlagSet("questions close", flag.ExitOnError)
	harness := fs.String("harness", "", "the harness of the agent that closes it (claude, codex, pi, hermes, grok, muse, agy); required")
	answer := fs.String("answer", "", "the answer proved: keep:<fact>, both or neither for a pair; keep:<path>, both or neither for a name two files hold (the owner's own words only); current or retire for one fact; holds, wrong or moot for a reviewer question")
	proof := fs.String("proof", "", "how it is proved: git, file, config, words or gone (words: the owner's own answer, relayed when they said it on or after the day it was asked, on its subject)")
	evidence := fs.String("evidence", "", "what was checked: commits, file:line, the probe and what it showed")
	reason := fs.String("reason", "", "why that evidence answers the question")
	words := fs.String("words", "", "the owner's own words a words proof rests on, as they wrote them")
	from := fs.String("from", "", "with --words: the session they are in (<harness>:<id>), when known")
	dry := fs.Bool("dry-run", false, "check the closure and print it; write nothing")
	fs.Parse(idLast(args))
	if fs.NArg() != 1 || *answer == "" || *proof == "" || *harness == "" {
		fmt.Fprintln(os.Stderr, "usage: tacit questions close ID --harness H --answer A --proof P --evidence E --reason R [--words W [--from REF]] [--dry-run]")
		return 2
	}
	if !agentHarness(*harness) {
		fmt.Fprintf(os.Stderr, "questions close: %q is no agent's harness; the owner answers in tacit app\n", *harness)
		return 2
	}
	id := fs.Arg(0)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "questions close: no store")
		return 2
	}
	if !*dry && refuseWriter(context.Background(), s, "curator", "questions close") {
		return 2
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "questions close: redaction needs scrump with the text profile; nothing closed:", err)
		return 2
	}
	// The record keeps the owner's words masked, as the statements step keeps
	// them; code finds them in their turns as they wrote them.
	red, err := digest.RedactAll([]string{strings.TrimSpace(*evidence), strings.TrimSpace(*reason), strings.TrimSpace(*words)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "questions close: redaction needs scrump with the text profile; nothing closed:", err)
		return 2
	}
	o := op{ID: id, By: "agent", Proof: *proof, Evidence: capText(red[0], 1500), Reason: capText(red[1], 1000)}
	if sess := harnessSession(*harness); sess != "" {
		o.Session = *harness + ":" + sess
	} else {
		o.Session = *harness
	}
	rs := &reviewServer{s: s}
	if !*dry { // a dry run reads the clone as it is
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := s.Sync(sctx); err != nil {
			var st *store.Stranded
			if !errors.As(err, &st) {
				cancel()
				fmt.Fprintln(os.Stderr, "questions close: sync:", err)
				return 1
			}
		}
		cancel()
	}
	rs.loadFacts()
	home, _ := os.UserHomeDir()
	queue, err := agentreview.Load(agentreview.Path(platform.StateDir(home)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "questions close:", err)
		return 1
	}
	// The owner's words: found by code in their own turns, or no proof.
	if *words != "" || *proof == "words" {
		if *proof != "words" || *words == "" {
			fmt.Fprintln(os.Stderr, "questions close: --words goes with --proof words, and --proof words needs --words")
			return 2
		}
		corpus, err := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "questions close: the owner's words cannot be read here, so nothing rests on them:", err)
			return 1
		}
		session := *from
		if _, id, ok := strings.Cut(session, ":"); ok {
			session = id
		}
		m, ok := corpus.Find(*words, session, true)
		if !ok || !m.Clean() {
			fmt.Fprintln(os.Stderr, "questions close: these words are no statement of the owner's in their turns (not found, a question of theirs, or negated); nothing closed")
			return 1
		}
		at := corpus.SaidAt(*words, m.Ref, m.TurnHash)
		o.Words, o.WordsFrom, o.WordsOn = red[2], m.Ref, corpus.SaidOn(*words, m.Ref, m.TurnHash)
		// Their whole turn is what the same-subject rule reads: an answer of
		// theirs is often a terse line under the question's own words, which
		// they pasted.
		turn := corpus.TurnAt(m.Ref, m.TurnHash)
		if turn == "" {
			turn = *words
		}
		o.answer = &ownerAnswer{turn: turn, at: at, on: o.WordsOn}
		// Their later words on the same subject let a closure retire a fact
		// that holds older words of theirs, and let their relayed answer
		// change their ruling: said after the fact's words (or the ruling's
		// quote, or the day the ruling was written), in a turn that shares
		// content words with them or with the fact's description. The
		// closure asks it of each fact as it finds it, under the store's lock.
		on := o.WordsOn
		o.wordsRetire = func(f *fact.Fact) bool {
			ruling := isRuling(f)
			w := fact.OwnerWordsOf(f)
			if w == "" && ruling {
				w = f.Meta.Source.OwnerQuote // "" for a rule they wrote or kept
			}
			if on == "" || (w == "" && !ruling) {
				return false
			}
			// The last day they said the fact's words, in any session: words
			// said again after the proof's still stand.
			was := ""
			if w != "" {
				if t := corpus.LastSaidAt(w); !t.IsZero() {
					was = t.Local().Format("2006-01-02")
				}
			}
			if was == "" {
				was = f.Meta.Created
			}
			if ruling && f.Meta.Updated > was {
				was = f.Meta.Updated // a rule they edited speaks for that day
			}
			same := (w != "" && agentreview.SameSubject(w, turn)) || agentreview.SameSubject(f.Meta.Description, turn)
			return same && was != "" && on > was
		}
	}
	if strings.HasPrefix(id, "q-") {
		it := queue.ByID(id)
		if it == nil || it.State != agentreview.StateOpen {
			fmt.Fprintf(os.Stderr, "questions close: %s is no open reviewer question on this machine\n", id)
			return 1
		}
		o.Mark, o.dry = *answer, *dry
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		note, err := closeReviewerQuestion(ctx, rs, &o, *it)
		if err != nil {
			fmt.Fprintln(os.Stderr, "questions close:", err)
			return 1
		}
		if *dry {
			b, _ := json.MarshalIndent(closureDecision(&o, closesQuestion, it.Note, it.Fact), "", " ")
			fmt.Println(string(b))
			fmt.Fprintln(os.Stderr, "questions close: dry run: "+note)
			return 0
		}
		fmt.Printf("closed %s: %s (%s)\n", id, o.Mark, o.Proof)
		if note != "" {
			fmt.Fprintln(os.Stderr, "questions close:", note)
		}
		return 0
	}
	// A finding: map the answer to its mark.
	var a, b string
	pair := false
	for _, it := range rs.items() {
		if it.ID == id {
			a, b, pair = it.A.Name, it.B.Name, true
		}
	}
	ni := rs.nameItemByID(id)
	switch {
	case ni != nil && *proof != "words":
		fmt.Fprintf(os.Stderr, "questions close: %s is a name two files hold; it closes only on the owner's own answer (--proof words). Without their words an agent marks it with tacit review apply (keep one path, with its evidence)\n", id)
		return 2
	case ni != nil:
		// The two files are the answer, by path: the name is both.
		a, b = ni.A.Path, ni.B.Path
		switch *answer {
		case "keep:" + a:
			o.Mark = "left"
		case "keep:" + b:
			o.Mark = "right"
		case "both":
			o.Mark = "both"
		case "neither":
			o.Mark = "wrong"
		default:
			fmt.Fprintf(os.Stderr, "questions close: a name two files hold takes keep:%s, keep:%s, both or neither\n", a, b)
			return 2
		}
	case pair && strings.HasPrefix(*answer, "keep:"):
		switch strings.TrimPrefix(*answer, "keep:") {
		case a:
			o.Mark = "left"
		case b:
			o.Mark = "right"
		default:
			fmt.Fprintf(os.Stderr, "questions close: keep names %s or %s\n", a, b)
			return 2
		}
	case pair && *answer == "both":
		o.Mark = "both"
	case pair && *answer == "neither":
		o.Mark = "wrong"
	case !pair && (*answer == "current" || *answer == "retire"):
		o.Mark = *answer
		if st := rs.staleByID(id); st != nil {
			a, b = st.A.Name, st.Flag.Sha
		}
	default:
		fmt.Fprintf(os.Stderr, "questions close: %q is no answer to %s (see tacit questions)\n", *answer, id)
		return 2
	}
	o.A, o.B = a, b
	if *dry {
		// Every guard runs, on the clone as it is; nothing is written.
		o.dry = true
		rs.mu.Lock()
		note, _, err := rs.applyOp(&o)
		rs.mu.Unlock()
		if err != nil {
			fmt.Fprintln(os.Stderr, "questions close: dry run:", err)
			return 1
		}
		b, _ := json.MarshalIndent(closureDecision(&o, "", o.A, o.B), "", " ")
		fmt.Println(string(b))
		fmt.Fprintln(os.Stderr, "questions close: dry run: "+note)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	notes, errs, err := rs.applyOps(ctx, []op{o})
	if err == nil && errs[0] != nil {
		err = errs[0]
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "questions close:", err)
		return 1
	}
	fmt.Printf("closed %s: %s (%s)\n", id, *answer, *proof)
	if notes[0] != "" {
		fmt.Fprintln(os.Stderr, "questions close:", notes[0])
	}
	return 0
}

// runQuestionsMoot implements `tacit questions moot [--dry-run] [ID...]`:
// the curator closes every question that went moot (tacit verify does it
// before it sends anything).
func runQuestionsMoot(args []string) int {
	fs := flag.NewFlagSet("questions moot", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print what it would close; write nothing")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "questions moot: no store")
		return 2
	}
	if !*dry && refuseWriter(context.Background(), s, "curator", "questions moot") {
		return 2
	}
	rs := &reviewServer{s: s}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if !*dry { // a dry run reads the clone as it is
		if err := s.Sync(ctx); err != nil {
			var st *store.Stranded
			if !errors.As(err, &st) {
				fmt.Fprintln(os.Stderr, "questions moot: sync:", err)
				return 1
			}
		}
	}
	rs.loadFacts()
	only := map[string]bool{}
	for _, id := range fs.Args() {
		only[id] = true
	}
	n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), only, *dry, func(l string) { fmt.Println("questions moot: " + l) })
	if !*dry {
		fmt.Printf("questions moot: %d question(s) closed\n", n)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "questions moot:", err)
		return 1
	}
	return 0
}

// idSet is ids as a set; empty for none.
func idSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}
