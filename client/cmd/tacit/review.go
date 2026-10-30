package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/store"
)

//go:embed reviewui
var reviewUI embed.FS

// Review item kinds. A is always the older or existing fact and B the newer
// or candidate one, so "B replaces A" reads the same on every card.
const (
	kindContradiction = "contradiction"
	kindSupersedeAsk  = "supersede-proposed"
	kindSupersedeDone = "supersede-applied"
	kindRulingChange  = "ruling-change"
	kindDuplicate     = "duplicate"
	// kindResultSup: a saved fact quotes a measured value that a newer commit
	// replaced; B is the result fact read from git (tacit results).
	kindResultSup = "result-superseded"
)

// A ruling is an owner statement an agent may not retire: source.kind user
// with the evidence that the words are theirs (fact.IsRuling, SPEC.md Rulings).
// A user_standard an agent filed is its reading of their preference, not their
// rule.
func isRuling(f *fact.Fact) bool { return fact.IsRuling(f) }

// guarded reports a fact no agent may retire or rewrite: their ruling.
func (v *factView) guarded() bool { return v != nil && v.Ruling }

// hisWords reports a fact that holds words of theirs (fact.OwnerWordsOf): an
// agent may not retire it against them; that mark becomes a question.
func (v *factView) hisWords() bool { return v != nil && v.OwnerWords != "" }

type factView struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	Scope         string   `json:"scope"`
	Type          string   `json:"type"`
	Kind          string   `json:"kind"`
	Description   string   `json:"description"`
	Body          string   `json:"body"`
	Created       string   `json:"created"`
	Updated       string   `json:"updated,omitempty"`
	Harness       string   `json:"harness"`
	Origin        string   `json:"origin,omitempty"`
	SupersededBy  string   `json:"superseded_by,omitempty"`
	ConflictsWith []string `json:"conflicts_with,omitempty"`
	Retired       string   `json:"retired,omitempty"`
	Missing       bool     `json:"missing,omitempty"`
	// Ruling: the owner's own words (isRuling). OwnerQuote is the phrase of their
	// the fact holds, shown as "In your words".
	Ruling     bool   `json:"ruling,omitempty"`
	OwnerQuote string `json:"owner_quote,omitempty"`
	// OwnerWords: a phrase of theirs the fact holds, too short to make it their
	// rule (fact.OwnerWordsOf, backed by the quote ledger). The verifier reads
	// it as strong evidence; an agent may not retire the fact against it.
	OwnerWords string `json:"owner_words,omitempty"`
}

// snap is the part of a fact a verdict may change, saved so reopen can put
// it back exactly.
type snap struct {
	SupersededBy  string   `json:"superseded_by,omitempty"`
	ConflictsWith []string `json:"conflicts_with,omitempty"`
	Updated       string   `json:"updated,omitempty"`
	Retired       string   `json:"retired,omitempty"` // stale findings only
}

type decision struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	A      string          `json:"a"`
	B      string          `json:"b"`
	At     string          `json:"at"`
	Before map[string]snap `json:"before,omitempty"`
	// By is who marked: empty for the owner on this page, "agent" for a
	// verifier that checked the facts against their sources. Evidence is
	// what it checked; Question is what an "ask" record wants the owner to
	// answer.
	By       string `json:"by,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Question string `json:"question,omitempty"`
	Proposed string `json:"proposed,omitempty"` // the fact an agent would have kept, when its mark became a question
	// Repointed is the finding on the live facts that took a moot one's place
	// (mootMark: a fact of the pair was replaced by a third fact).
	Repointed string `json:"repointed,omitempty"`
	Model     string `json:"model,omitempty"` // the model that produced an agent's verdict
	// OwnerOnly, label mode only: the owner says no source settles this
	// pair and only the owner knows the answer, so a verifier is right to ask.
	OwnerOnly bool `json:"owner_only,omitempty"`
	// Comment is the owner's note in their own words (redacted), sent with an
	// answer, or alone in a "note" record that leaves the finding open.
	Comment string `json:"comment,omitempty"`
	// Note is the in-place note an agent's fix wrote into the fact
	// (fact.AppendNote); a fix with a note is applied.
	Note string `json:"note,omitempty"`
}

type reviewItem struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Source   string    `json:"source"`
	A        *factView `json:"a"`
	B        *factView `json:"b"`
	State    string    `json:"state"`           // what the store says now about this pair: left, right, both, query, tangled
	Allowed  []string  `json:"allowed"`         // marks the server will accept now
	Mark     string    `json:"mark,omitempty"`  // the recorded mark, in this item's left/right orientation
	Stale    bool      `json:"stale,omitempty"` // the store no longer says what the mark set
	Decision *decision `json:"decision,omitempty"`
	Ask      *decision `json:"ask,omitempty"` // an agent checked and left this question for the owner
	// NeedsOwner: nobody has settled this, or an agent could only flag a
	// conflict, which is the owner's call.
	NeedsOwner bool `json:"needs_owner"`
	// Notes are what the owner wrote on this finding without answering it.
	Notes []ownerNote `json:"notes,omitempty"`
}

var (
	reLink     = regexp.MustCompile(`\[\[([a-z0-9][a-z0-9-]*)\]\]`)
	reHeading  = regexp.MustCompile(`^###\s+(contradicts|supersedes)\b`)
	reSupDone  = regexp.MustCompile(`^- supersedes: \[\[([^\]]+)\]\] replaces \[\[([^\]]+)\]\]`)
	reSupAsk   = regexp.MustCompile(`^- supersedes\? \[\[([^\]]+)\]\] may replace \[\[([^\]]+)\]\]`)
	reResultUp = regexp.MustCompile(`^- result-superseded\? \[\[([^\]]+)\]\] may replace \[\[([^\]]+)\]\]`)
	reRuling   = regexp.MustCompile(`^- RULING CHANGE\? new \[\[([^\]]+)\]\] may supersede owner ruling \[\[([^\]]+)\]\]`)
	reContra   = regexp.MustCompile(`^- CONTRADICTION\??: \[\[([^\]]+)\]\].*? vs \[\[([^\]]+)\]\]`)
	reDup      = regexp.MustCompile(`^- duplicate in one run: \[\[([^\]]+)\]\] and \[\[([^\]]+)\]\]`)
	rePairLine = regexp.MustCompile(`^- \[\[([^\]]+)\]\] \([^)]*\) vs \[\[([^\]]+)\]\]`)
)

type pairRef struct{ kind, a, b, source string }

// parseReviewFiles reads review/*.md (not decisions) into pairs.
func parseReviewFiles(dir string) []pairRef {
	files, _ := filepath.Glob(filepath.Join(dir, "review", "*.md"))
	sort.Strings(files)
	var out []pairRef
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		src := filepath.Base(f)
		section := ""
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if m := reHeading.FindStringSubmatch(l); m != nil {
				section = m[1]
				continue
			}
			switch {
			case reSupDone.MatchString(l):
				m := reSupDone.FindStringSubmatch(l)
				out = append(out, pairRef{kindSupersedeDone, m[2], m[1], src})
			case reResultUp.MatchString(l):
				m := reResultUp.FindStringSubmatch(l)
				out = append(out, pairRef{kindResultSup, m[2], m[1], src})
			case reSupAsk.MatchString(l):
				m := reSupAsk.FindStringSubmatch(l)
				out = append(out, pairRef{kindSupersedeAsk, m[2], m[1], src})
			case reRuling.MatchString(l):
				m := reRuling.FindStringSubmatch(l)
				out = append(out, pairRef{kindRulingChange, m[2], m[1], src})
			case reContra.MatchString(l):
				m := reContra.FindStringSubmatch(l)
				out = append(out, pairRef{kindContradiction, m[2], m[1], src})
			case reDup.MatchString(l):
				m := reDup.FindStringSubmatch(l)
				out = append(out, pairRef{kindDuplicate, m[1], m[2], src})
			case section != "" && rePairLine.MatchString(l):
				m := rePairLine.FindStringSubmatch(l)
				if section == "contradicts" {
					out = append(out, pairRef{kindContradiction, m[1], m[2], src})
				} else {
					// recheck: "B supersedes A", B is the current state.
					out = append(out, pairRef{kindSupersedeAsk, m[1], m[2], src})
				}
			}
		}
		fh.Close()
	}
	return out
}

func pairID(kind, a, b string) string {
	family := kind
	if kind == kindContradiction || kind == kindDuplicate {
		family = "pair"
		if b < a {
			a, b = b, a
		}
	}
	h := sha256.Sum256([]byte(family + "\x00" + a + "\x00" + b))
	return hex.EncodeToString(h[:6])
}

type reviewServer struct {
	mu    sync.Mutex
	s     *store.Store
	token string
	facts map[string]*fact.Fact // name -> curated fact
	inbox map[string]*fact.Fact // name -> inbox note, the source a curated fact's origin names

	// tl reads the git timeline under a proof; nil when a server has none.
	tl        *gitev.Builder
	tlMemo    map[string]tlMemo
	tlFlights map[string]*tlFlight
	tlSem     chan struct{}
	tlTimeout time.Duration // how long one /api/timeline request waits; 90 s when zero

	// Label mode: the page shows only sampleIDs, hides every stored mark, and
	// writes the owner's marks to labelFile instead of the store. The file
	// lives outside the store and outside every probe root, so a verifier
	// replaying the same findings can neither see nor reach the answers.
	labelFile string
	sampleIDs map[string]bool

	// viewOnly is set at start on a machine that cannot write as the curator
	// (a read-only clone, or no curator key): the page shows no marks and
	// every write refuses first (writeRefusal).
	viewOnly string

	// snap is every fact at one HEAD, for tacit app's list, search and
	// links (owner.go).
	snapMu sync.Mutex
	snap   *factSnap

	// beforePush, in tests only, runs once before an owner write's first push.
	beforePush func()
	// freshNotes are the owner's inbox notes this write created on disk; a
	// write that does not land removes them, so no note is left untracked.
	freshNotes []string
}

// ownerQuoteOf is the phrase of the owner's a ruling shows as "In your
// words": its recorded quote, or nothing when the owner wrote the whole fact or kept
// it in a review.
func ownerQuoteOf(f *fact.Fact) string {
	if !isRuling(f) {
		return ""
	}
	return f.Meta.Source.OwnerQuote
}

func (rs *reviewServer) labelling() bool { return rs.labelFile != "" }

func (rs *reviewServer) loadFacts() {
	all, _, _ := fact.Walk(rs.s.Dir)
	m, in := map[string]*fact.Fact{}, map[string]*fact.Fact{}
	for _, f := range all {
		if !f.InInbox() {
			m[f.Meta.Name] = f
		} else if old := in[f.Meta.Name]; old == nil || f.Meta.Created < old.Meta.Created {
			in[f.Meta.Name] = f
		}
	}
	rs.facts, rs.inbox = m, in
}

func (rs *reviewServer) view(name string) *factView {
	f, ok := rs.facts[name]
	if !ok {
		return &factView{Name: name, Missing: true}
	}
	return &factView{
		Name: f.Meta.Name, Path: f.Path, Scope: f.Meta.Scope, Type: f.Meta.Type, Kind: f.Meta.Source.Kind,
		Description: f.Meta.Description, Body: strings.TrimSpace(f.Body), Created: f.Meta.Created, Updated: f.Meta.Updated,
		Harness: f.Meta.Source.Harness, Origin: f.Meta.Origin, SupersededBy: f.Meta.SupersededBy, ConflictsWith: f.Meta.ConflictsWith, Retired: f.Meta.Retired,
		Ruling: isRuling(f), OwnerQuote: ownerQuoteOf(f), OwnerWords: fact.OwnerWordsOf(f),
	}
}

// decisions reads the log: the standing mark per finding (a reopen clears
// it) and the latest question an agent left for the owner.
func (rs *reviewServer) decisions() (marks, asks map[string]*decision) {
	marks, asks = map[string]*decision{}, map[string]*decision{}
	src := filepath.Join(rs.s.Dir, "review", "decisions.jsonl")
	if rs.labelling() {
		src = rs.labelFile
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) == nil && d.ID != "" {
			dd := d
			switch d.Action {
			case "reopen":
				delete(marks, d.ID)
			case "note":
				// The owner's note leaves the finding as it is (ownerNotes).
			case "ask":
				asks[d.ID] = &dd
			default:
				marks[d.ID] = &dd
			}
		}
	}
	return
}

// ownerTouched lists findings with any record the owner made themself (a
// mark from the page, a reopen, or a record from before marks carried By),
// and the facts of every pair where the owner kept one over the other. A verifier
// never touches those findings or facts.
func (rs *reviewServer) ownerTouched() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) == nil && d.ID != "" && d.By == "" && d.Action != "note" {
			// A note alone answers nothing: the finding stays open to the
			// verifier, which reads the note as the owner's word on it.
			out[d.ID] = true
			// Keeping one fact over another, or neither, is a word on both
			// facts; "both stand" or a flag on one pair says nothing about
			// other pairs.
			switch markOf(d.Action) {
			case "left", "right", "wrong":
				out[d.A], out[d.B] = true, true
			}
		}
	}
	return out
}

func (rs *reviewServer) items() []reviewItem {
	ds, asks := rs.decisions()
	notes := rs.ownerNotes()
	seen := map[string]bool{}
	var out []reviewItem
	for _, p := range parseReviewFiles(rs.s.Dir) {
		if p.a == p.b {
			continue
		}
		id := pairID(p.kind, p.a, p.b)
		if seen[id] || (rs.labelling() && !rs.sampleIDs[id]) {
			continue
		}
		seen[id] = true
		it := reviewItem{ID: id, Kind: p.kind, Source: p.source, A: rs.view(p.a), B: rs.view(p.b), Decision: ds[id], Ask: asks[id], Notes: notes.For(id, p.a, p.b)}
		it.State = pairState(it.A, it.B)
		if d := it.Decision; d != nil {
			it.Mark = markOf(d.Action)
			if d.A == it.B.Name && d.B == it.A.Name {
				// Recorded the other way round: left and right swap.
				switch it.Mark {
				case "left":
					it.Mark = "right"
				case "right":
					it.Mark = "left"
				}
			}
			// A label is the owner's truth, not a store state, so it is never stale.
			it.Stale = it.Mark != "" && it.State != it.Mark && !rs.labelling()
		}
		it.Allowed = rs.allowed(it)
		if rs.labelling() && it.Decision == nil {
			// Not sure: counts as answered, stays out of the comparison.
			// Both wrong: neither fact is true; the store has no mark for it yet.
			it.Allowed = addUnique(addUnique(it.Allowed, "wrong"), "skip")
		}
		it.NeedsOwner = it.Decision == nil || (it.Decision.By == "agent" && (it.Decision.Action == "query" || it.Stale))
		out = append(out, it)
	}
	return out
}

func without(xs []string, x string) []string {
	var out []string
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}

// pairState reads what the store says about this pair right now.
func pairState(a, b *factView) string {
	if a.Retired != "" && b.Retired != "" && a.SupersededBy != b.Name && b.SupersededBy != a.Name {
		return "wrong" // both retired: neither is true
	}
	aRetired := a.SupersededBy == b.Name
	bRetired := b.SupersededBy == a.Name
	conflict := contains(a.ConflictsWith, b.Name) || contains(b.ConflictsWith, a.Name)
	switch {
	case aRetired && bRetired:
		return "tangled"
	case aRetired:
		return "right"
	case bRetired:
		return "left"
	case conflict:
		return "query"
	}
	return "both"
}

// legacyMarks maps the action names of the first review page to marks.
var legacyMarks = map[string]string{"a-replaces-b": "left", "b-replaces-a": "right", "keep": "right", "dismiss": "both", "undo": "both", "conflict": "query"}

func markOf(action string) string {
	switch action {
	case "left", "right", "both", "query", "skip", "wrong":
		return action
	}
	return legacyMarks[action]
}

// allowed lists the marks the server accepts for an item now. A decided
// item accepts only reopen; a pair with a missing fact only "both"; a mark
// that would retire a fact already retired by a third fact, or close a loop
// of retirements, is refused.
func (rs *reviewServer) allowed(it reviewItem) []string {
	if rs.viewOnly != "" && !rs.labelling() { // label mode writes only its label file
		return []string{}
	}
	if it.Decision != nil && !(it.Decision.By == "agent" && (it.Decision.Action == "query" || it.Stale)) {
		return []string{"reopen"}
	}
	if it.A.Missing || it.B.Missing {
		return []string{"both"}
	}
	out := []string{"both", "query"}
	// Neither is true: both retired, while at least one still reaches agents.
	if (it.A.Retired == "" || it.B.Retired == "") && it.A.SupersededBy == "" && it.B.SupersededBy == "" {
		out = append(out, "wrong")
	}
	if (it.B.SupersededBy == "" || it.B.SupersededBy == it.A.Name) && !rs.reaches(it.A.Name, it.B.Name) {
		out = append(out, "left")
	}
	if (it.A.SupersededBy == "" || it.A.SupersededBy == it.B.Name) && !rs.reaches(it.B.Name, it.A.Name) {
		out = append(out, "right")
	}
	return withoutGenerated(out, it)
}

// generatedView: a fact a generator writes (the results step, derive). The
// next run rewrites it from git, so a relation patched into it by a mark would
// be erased while the decision log still says the question was answered.
func generatedView(v *factView) bool {
	return v != nil && v.Kind == "derived" && strings.HasPrefix(v.Scope, "derived/")
}

// withoutGenerated drops the marks that would patch a generated fact: "left"
// retires B, "right" retires A, "query" flags both. Marking "both" or retiring
// the saved side leaves the generated file as it is.
func withoutGenerated(marks []string, it reviewItem) []string {
	var out []string
	for _, m := range marks {
		switch {
		case m == "left" && generatedView(it.B), m == "right" && generatedView(it.A), (m == "query" || m == "wrong") && (generatedView(it.A) || generatedView(it.B)):
			continue
		}
		out = append(out, m)
	}
	return out
}

// reaches tells whether following superseded_by from `from` arrives at
// target, ignoring the direct edge from -> target (a mark on the pair
// replaces it). Retiring target by from would then close a loop.
func (rs *reviewServer) reaches(from, target string) bool {
	seen := map[string]bool{}
	for cur := from; cur != "" && !seen[cur]; {
		seen[cur] = true
		f := rs.facts[cur]
		if f == nil {
			return false
		}
		next := f.Meta.SupersededBy
		if cur == from && next == target {
			return false
		}
		if next == target {
			return true
		}
		cur = next
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func snapOf(f *fact.Fact) snap {
	return snap{SupersededBy: f.Meta.SupersededBy, ConflictsWith: append([]string(nil), f.Meta.ConflictsWith...), Updated: f.Meta.Updated, Retired: f.Meta.Retired}
}

// setState makes the store say `mark` about the pair a, b, touching only the
// relations between these two facts. Marks set a state rather than apply a
// delta, so no sequence of marks can leave the two retiring each other.
func setState(a, b *fact.Fact, mark string) error {
	clearPair := func() {
		if a.Meta.SupersededBy == b.Meta.Name {
			a.Meta.SupersededBy = ""
		}
		if b.Meta.SupersededBy == a.Meta.Name {
			b.Meta.SupersededBy = ""
		}
		a.Meta.ConflictsWith = without(a.Meta.ConflictsWith, b.Meta.Name)
		b.Meta.ConflictsWith = without(b.Meta.ConflictsWith, a.Meta.Name)
	}
	switch mark {
	case "both":
		clearPair()
	case "query":
		clearPair()
		a.Meta.ConflictsWith = addUnique(a.Meta.ConflictsWith, b.Meta.Name)
		b.Meta.ConflictsWith = addUnique(b.Meta.ConflictsWith, a.Meta.Name)
	case "left":
		if b.Meta.SupersededBy != "" && b.Meta.SupersededBy != a.Meta.Name {
			return fmt.Errorf("%s is already retired by %s; reopen that first", b.Meta.Name, b.Meta.SupersededBy)
		}
		clearPair()
		b.Meta.SupersededBy = a.Meta.Name
	case "right":
		if a.Meta.SupersededBy != "" && a.Meta.SupersededBy != b.Meta.Name {
			return fmt.Errorf("%s is already retired by %s; reopen that first", a.Meta.Name, a.Meta.SupersededBy)
		}
		clearPair()
		a.Meta.SupersededBy = b.Meta.Name
	default:
		return fmt.Errorf("unknown mark %q", mark)
	}
	return nil
}

func sameSnap(f *fact.Fact, s snap) bool {
	return f.Meta.SupersededBy == s.SupersededBy && strings.Join(f.Meta.ConflictsWith, ",") == strings.Join(s.ConflictsWith, ",") && f.Meta.Retired == s.Retired
}

// reopenPair undoes what a mark did to the relations between x and y, and
// nothing else: other proofs' marks and later edits on these facts stay.
func reopenPair(x, y *fact.Fact, before snap) {
	if x.Meta.SupersededBy == y.Meta.Name && before.SupersededBy != y.Meta.Name {
		x.Meta.SupersededBy = before.SupersededBy
	} else if x.Meta.SupersededBy == "" && before.SupersededBy == y.Meta.Name {
		x.Meta.SupersededBy = y.Meta.Name
	}
	if contains(before.ConflictsWith, y.Meta.Name) {
		x.Meta.ConflictsWith = addUnique(x.Meta.ConflictsWith, y.Meta.Name)
	} else {
		x.Meta.ConflictsWith = without(x.Meta.ConflictsWith, y.Meta.Name)
	}
}

// op is one mark to apply: the owner's from the page, or an agent's with
// its evidence. Mark "ask" records a question for the owner and changes no
// fact.
type op struct {
	ID, Mark, By, Evidence, Question string
	Comment                          string // the owner's note sent with their mark
	Model                            string // the verifier model that produced the verdict, recorded with it
	A, B                             string // when set, the finding must still name these facts
	// FixWas and FixNow: an agent's fix, the detail the fact says and the
	// one the sources say; applyStaleOp writes them as an in-place note when
	// the fact holds FixWas word for word.
	FixWas, FixNow string
	proposed       string
}

// decide applies one mark for the page. See applyOps. In label mode the mark
// is appended to the label file and nothing in the store changes.
func (rs *reviewServer) decide(ctx context.Context, id, mark string, ownerOnly bool) (string, error) {
	return rs.answer(ctx, id, mark, "", ownerOnly)
}

// applyOps applies marks in one commit. It syncs first and computes every
// change on the store as it is now; if the server moved during the push, it
// drops its commit, syncs and applies the marks again (a mark sets a state,
// so this is safe). Per-op problems come back in errs; err is for the batch.
func (rs *reviewServer) applyOps(ctx context.Context, ops []op) (notes []string, errs []error, err error) {
	if err := rs.writeRefusal(ctx); err != nil {
		notes, errs = make([]string, len(ops)), make([]error, len(ops))
		for i := range errs {
			errs[i] = err
		}
		return notes, errs, err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.freshNotes = nil
	err = rs.s.WithLock(ctx, func() error {
		for attempt := 0; attempt < 4; attempt++ {
			var syncNote string
			if err := rs.s.SyncLocked(ctx); err != nil {
				var st *store.Stranded
				if !errors.As(err, &st) {
					return fmt.Errorf("could not sync the store, nothing was marked (%s)", shortErr(err))
				}
				syncNote = st.Error()
			}
			var err error
			notes, errs, err = rs.applyOnce(ctx, ops)
			if errors.Is(err, store.ErrBehind) {
				continue
			}
			if syncNote != "" {
				notes[0] = strings.TrimSpace(syncNote + " " + notes[0])
			}
			return err
		}
		return errors.New("the store kept changing on the server; nothing was marked, try again")
	})
	if notes == nil {
		notes, errs = make([]string, len(ops)), make([]error, len(ops))
	}
	if err != nil {
		rs.dropFreshNotes()
		// Nothing landed: every op in the batch failed with the batch.
		for i := range errs {
			if errs[i] == nil {
				errs[i] = err
			}
		}
	}
	rs.loadFacts()
	return notes, errs, err
}

func (rs *reviewServer) applyOnce(ctx context.Context, ops []op) ([]string, []error, error) {
	notes, errs := make([]string, len(ops)), make([]error, len(ops))
	var written []string
	applied := 0
	for i := range ops {
		n, w, err := rs.applyOp(&ops[i])
		written = append(written, w...)
		if err != nil {
			var io *ioFailure
			if errors.As(err, &io) {
				rs.s.Restore(written)
				return notes, errs, err
			}
			errs[i] = err
			continue
		}
		notes[i] = n
		applied++
	}
	if applied == 0 {
		return notes, errs, nil
	}
	// Keep the index agents read in step with the retirements.
	if all, _, err := fact.Walk(rs.s.Dir); err == nil {
		rels, err := regenIndexes(ctx, rs.s.Dir, all, nil)
		written = append(written, rels...)
		if err != nil {
			rs.s.Restore(written)
			return notes, errs, err
		}
	}
	rels := append(dedupe(written), ".gitattributes")
	var by, first string
	for i, o := range ops {
		if errs[i] == nil && first == "" {
			first, by = fmt.Sprintf("%s %s / %s", o.Mark, o.A, o.B), o.By
		}
	}
	msg := "review: " + first
	if applied > 1 {
		msg = fmt.Sprintf("review: %d mark(s), first %s", applied, first)
	}
	if by != "" {
		msg += " by " + by
	}
	err := rs.s.CommitPushOnceLocked(ctx, existing(rs.s.Dir, rels), msg, "curator")
	var rej *store.Rejected
	switch {
	case err == nil:
		return notes, errs, nil
	case errors.Is(err, store.ErrBehind):
		return notes, errs, err
	case errors.As(err, &rej):
		return notes, errs, fmt.Errorf("the server refused this mark: %s", rej.Reason)
	case errors.Is(err, store.ErrReadOnly):
		// No commit was made: put every file back, so the clone still syncs.
		rs.s.Restore(written)
		return notes, errs, errViewOnly
	}
	// The commit stays here and goes up with the next push.
	return notes, errs, fmt.Errorf("saved on this machine, not on the server yet: %s", shortErr(err))
}

// ioFailure is a write that failed part way; the whole batch is put back.
type ioFailure struct{ err error }

func (e *ioFailure) Error() string { return e.err.Error() }
func (e *ioFailure) Unwrap() error { return e.err }

// applyOp checks one mark against the store as it is on disk now and
// writes its files. Everything is prepared in memory first, so a refused
// mark writes nothing.
func (rs *reviewServer) applyOp(o *op) (string, []string, error) {
	rs.loadFacts()
	if st := rs.staleByID(o.ID); st != nil {
		return rs.applyStaleOp(o, st)
	}
	var it *reviewItem
	for _, x := range rs.items() {
		if x.ID == o.ID {
			xx := x
			it = &xx
		}
	}
	if it == nil {
		return "", nil, errors.New("this finding is no longer in the review files")
	}
	// A verdict must name the finding's facts in the finding's own order;
	// a swapped pair is refused rather than guessed at, since a wrong guess
	// retires the fact the verdict meant to keep.
	if o.By == "agent" && (o.A == "" || o.B == "") {
		return "", nil, errors.New("an agent verdict must name both facts")
	}
	if o.A != "" && !(o.A == it.A.Name && o.B == it.B.Name) {
		return "", nil, fmt.Errorf("finding %s is about %s then %s, the verdict says %s then %s", o.ID, it.A.Name, it.B.Name, o.A, o.B)
	}
	o.A, o.B = it.A.Name, it.B.Name
	if o.Mark == mootMark {
		return rs.applyMoot(o, it)
	}
	if o.Mark == "reopen" && o.By == "curator" {
		if err := rs.curatorMayReopen(it); err != nil {
			return "", nil, err
		}
	}
	mark := o.Mark
	if o.By == "agent" && (mark == "wrong" || mark == "note" || o.Comment != "") {
		return "", nil, errors.New("only the owner may say neither is true or leave a note")
	}
	if o.By != "agent" && mark == "wrong" {
		return "", nil, errors.New("neither is true goes through the owner's write path")
	}
	if o.By == "agent" {
		// An agent never overrides a mark, a question, or anything the
		// owner ever touched, and it must say what it checked.
		if it.Decision != nil || it.Ask != nil {
			return "", nil, errors.New("an agent may not mark a finding that already carries a mark or a question")
		}
		if owned := rs.ownerTouched(); owned[o.ID] || owned[it.A.Name] || owned[it.B.Name] {
			return "", nil, errors.New("the owner has marked or reopened this finding or one of its facts; it is theirs")
		}
		if mark != "ask" && strings.TrimSpace(o.Evidence) == "" {
			return "", nil, errors.New("an agent mark needs evidence")
		}
		// Never retire an owner ruling on an agent's word: that stays a
		// question for the owner.
		retired, kept := map[string]*factView{"left": it.B, "right": it.A}[mark], map[string]*factView{"left": it.A, "right": it.B}[mark]
		if mark != "ask" && !it.A.Missing && !it.B.Missing && (it.A.guarded() || it.B.guarded()) {
			// Only the owner writes into a ruling file. Try the mark on copies;
			// if the ruling's relations would change, it becomes a question.
			a, b := *rs.facts[it.A.Name], *rs.facts[it.B.Name]
			a.Meta.ConflictsWith = append([]string(nil), a.Meta.ConflictsWith...)
			b.Meta.ConflictsWith = append([]string(nil), b.Meta.ConflictsWith...)
			before := map[string]snap{a.Meta.Name: snapOf(&a), b.Meta.Name: snapOf(&b)}
			if setState(&a, &b, mark) == nil {
				for _, f := range []*fact.Fact{&a, &b} {
					if isRuling(f) && !sameSnap(f, before[f.Meta.Name]) {
						if o.Question == "" {
							o.Question = "An agent would change the relations of your ruling " + f.Meta.Name + " (" + mark + "). What stands?"
						}
						mark, o.Mark = "ask", "ask"
					}
				}
			}
		}
		// The owner's note on an open finding outranks an agent's reading: an
		// agent never retires a fact of it, it asks them.
		if retired != nil && len(it.Notes) > 0 && mark != "ask" {
			if o.Question == "" {
				o.Question = "You left a note on this. An agent reads " + kept.Name + " as the current one; does " + retired.Name + " still hold?"
			}
			o.proposed = kept.Name
			mark, o.Mark = "ask", "ask"
		}
		if retired != nil && retired.guarded() {
			if o.Question == "" {
				o.Question = "Does your ruling " + retired.Name + " still hold? An agent reads " + kept.Name + " as the current one."
			}
			o.proposed = kept.Name
			mark, o.Mark = "ask", "ask"
		}
		// Words of theirs in a fact outrank an agent's reading: retiring that fact
		// would go against them, so it is a question, and only then.
		if retired != nil && retired.hisWords() && mark != "ask" {
			if o.Question == "" {
				o.Question = "You said \"" + retired.OwnerWords + "\" (in " + retired.Name + "). An agent reads " + kept.Name + " as the current one; do your words still hold?"
			}
			o.proposed = kept.Name
			mark, o.Mark = "ask", "ask"
		}
		// A conflict with their words is theirs to settle: marked conflicting,
		// delivery shows one side only and may drop them.
		if mark == "query" && (it.A.hisWords() || it.B.hisWords()) {
			if o.Question == "" {
				w := it.A
				if !w.hisWords() {
					w = it.B
				}
				o.Question = "You said \"" + w.OwnerWords + "\" (in " + w.Name + "). An agent reads " + it.A.Name + " and " + it.B.Name + " as in conflict; which holds?"
			}
			mark, o.Mark = "ask", "ask"
		}
	}
	if mark == "ask" {
		if it.Decision != nil {
			return "", nil, errors.New("this finding is already marked")
		}
	} else if mark != "reopen" && it.Decision != nil && contains(it.Allowed, mark) {
		// The owner overrules an agent's query: record the agent's before-state
		// as this mark's own, so reopen returns to what the agent left.
	} else if !contains(it.Allowed, mark) {
		if it.Decision != nil {
			return "", nil, errors.New("this finding is already marked; reopen it first")
		}
		return "", nil, errors.New("that mark is not possible for this finding now")
	}
	var note string
	var touched []*fact.Fact
	d := decision{ID: o.ID, Action: mark, A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339), By: o.By}
	d.Evidence, d.Question, d.Proposed, d.Model = o.Evidence, o.Question, o.proposed, o.Model
	if mark == "reopen" {
		prev := it.Decision
		x, y := rs.facts[prev.A], rs.facts[prev.B]
		switch {
		case prev.Action == mootMark:
			note = "Closing it changed no fact, so nothing was put back."
		case prev.Before == nil:
			note = "This mark is older than the review page's records, so the facts were left as they are."
		case it.Stale:
			note = "The store changed since this mark, so the facts were left as they are."
		case x == nil || y == nil:
			note = "One of these facts is gone from the store."
		default:
			reopenPair(x, y, prev.Before[x.Meta.Name])
			reopenPair(y, x, prev.Before[y.Meta.Name])
			if markOf(prev.Action) == "wrong" {
				// Neither is true retired both: put back what each was.
				x.Meta.Retired, y.Meta.Retired = prev.Before[x.Meta.Name].Retired, prev.Before[y.Meta.Name].Retired
			}
			for _, f := range []*fact.Fact{x, y} {
				b := prev.Before[f.Meta.Name]
				if sameSnap(f, b) {
					f.Meta.Updated = b.Updated
				} else {
					f.Meta.Updated = time.Now().Format("2006-01-02")
				}
				touched = append(touched, f)
			}
		}
	} else if mark != "ask" && !it.A.Missing && !it.B.Missing {
		a, b := rs.facts[it.A.Name], rs.facts[it.B.Name]
		d.Before = map[string]snap{a.Meta.Name: snapOf(a), b.Meta.Name: snapOf(b)}
		if err := setState(a, b, mark); err != nil {
			return "", nil, err
		}
		for _, f := range []*fact.Fact{a, b} {
			if !sameSnap(f, d.Before[f.Meta.Name]) {
				f.Meta.Updated = time.Now().Format("2006-01-02")
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
		ref := &findingRef{id: it.ID, kind: it.Kind, a: it.A.Name, b: it.B.Name, scope: it.A.Scope, names: []string{it.A.Name, it.B.Name}}
		c, err := rs.noteWrites(files, ref, o.Comment, mark)
		if err != nil {
			return "", nil, err
		}
		d.Comment = c
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, note, files)
}

// writeFiles writes a mark's files; a write that fails part way is put back
// by the caller (ioFailure).
func writeFiles(dir, note string, files map[string][]byte) (string, []string, error) {
	var written []string
	for rel, b := range files {
		written = append(written, rel)
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", written, &ioFailure{err}
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return "", written, &ioFailure{err}
		}
	}
	return note, written, nil
}

// label records the owner's ground truth for one sample finding.
func (rs *reviewServer) label(id, mark string, ownerOnly bool) (string, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.loadFacts()
	var it *reviewItem
	for _, x := range rs.items() {
		if x.ID == id {
			xx := x
			it = &xx
		}
	}
	if it == nil {
		return "", errors.New("this finding is not in the sample")
	}
	if mark == "reopen" {
		if it.Decision == nil {
			return "", errors.New("nothing to reopen")
		}
	} else if it.Decision != nil {
		return "", errors.New("this finding is already labelled; reopen it first")
	} else if !contains([]string{"left", "right", "both", "query", "wrong", "skip"}, mark) {
		return "", errors.New("that mark is not possible for this finding now")
	}
	d := decision{ID: id, Action: mark, A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339), By: "owner-label", OwnerOnly: ownerOnly && mark != "reopen" && mark != "skip"}
	line, _ := json.Marshal(d)
	os.MkdirAll(filepath.Dir(rs.labelFile), 0o700)
	fh, err := os.OpenFile(rs.labelFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	fh.Write(append(line, '\n'))
	fh.Close()
	n := 0
	marks, _ := rs.decisions()
	for id := range rs.sampleIDs {
		if marks[id] != nil {
			n++
		}
	}
	return fmt.Sprintf("Labelled %d of %d.", n, len(rs.sampleIDs)), nil
}

func existing(dir string, rels []string) []string {
	var out []string
	for _, r := range rels {
		if _, err := os.Stat(filepath.Join(dir, r)); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// verdict is one line of a verifier's output.
type verdict struct {
	ID, A, B, Mark, Keep, Evidence, Fix, Question string
}

// runReviewApply implements `tacit review apply FILE...`: marks from a
// verifier that checked each finding against its sources, applied in one
// commit with the evidence recorded. "owner" verdicts leave the question
// on the page instead of a mark.
func runReviewApply(files []string) int {
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "review apply: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if refuseWriter(context.Background(), s, "curator", "review apply") {
		return 2
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "review apply: redaction needs scrump with the text profile:", err)
		return 2
	}
	var ops []op
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "review apply:", err)
			return 2
		}
		var vs []verdict
		if err := json.Unmarshal(b, &vs); err != nil {
			fmt.Fprintf(os.Stderr, "review apply: %s: %v\n", f, err)
			return 2
		}
		var texts []string
		for _, v := range vs {
			texts = append(texts, strings.TrimSpace(v.Evidence), strings.TrimSpace(v.Fix), strings.TrimSpace(v.Question))
		}
		red, err := digest.RedactAll(texts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "review apply: %s: redaction: %v; nothing applied\n", f, err)
			return 1
		}
		for i, v := range vs {
			v.Evidence, v.Fix, v.Question = red[3*i], red[3*i+1], red[3*i+2]
			o := op{ID: v.ID, A: v.A, B: v.B, By: "agent", Mark: v.Mark, Evidence: v.Evidence}
			if v.Fix != "" {
				o.Evidence = strings.TrimSpace(o.Evidence + " Fix needed: " + v.Fix)
			}
			if v.Mark == "keep" {
				switch v.Keep {
				case v.A:
					o.Mark = "left"
				case v.B:
					o.Mark = "right"
				default:
					fmt.Fprintf(os.Stderr, "review apply: %s: keep names neither fact, skipped\n", v.ID)
					continue
				}
			}
			switch o.Mark {
			case "left", "right", "both", "query":
			case "owner":
				o.Mark, o.Question = "ask", v.Question
				if o.Question == "" {
					fmt.Fprintf(os.Stderr, "review apply: %s: owner verdict without a question, skipped\n", v.ID)
					continue
				}
			default:
				fmt.Fprintf(os.Stderr, "review apply: %s: unknown mark %q, skipped\n", v.ID, v.Mark)
				continue
			}
			ops = append(ops, o)
		}
	}
	if len(ops) == 0 {
		fmt.Fprintln(os.Stderr, "review apply: nothing to apply")
		return 1
	}
	rs := &reviewServer{s: s}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, errs, err := rs.applyOps(ctx, ops)
	counts := map[string]int{}
	failed := 0
	for i, o := range ops {
		if errs[i] != nil {
			failed++
			fmt.Fprintf(os.Stderr, "review apply: %s (%s / %s): %v\n", o.ID, o.A, o.B, errs[i])
			continue
		}
		counts[o.Mark]++
	}
	fmt.Printf("applied: left %d, right %d, both %d, query %d, asked the owner %d; failed %d\n", counts["left"], counts["right"], counts["both"], counts["query"], counts["ask"], failed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review apply:", err)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// runReview implements `tacit review [--addr 127.0.0.1:7448] [--no-open] [--fact NAME]`
// and `tacit review apply FILE...`.
func runReview(args []string) int {
	if len(args) > 0 && args[0] == "apply" {
		return runReviewApply(args[1:])
	}
	fsf := flag.NewFlagSet("review", flag.ExitOnError)
	addr := fsf.String("addr", "127.0.0.1:7448", "listen address (loopback only)")
	noOpen := fsf.Bool("no-open", false, "do not open a browser")
	sample := fsf.String("label", "", "label mode: JSON array of {id} samples; marks go to --labels, never to the store")
	labels := fsf.String("labels", "", "label mode: JSONL file that receives the owner's marks (default ~/.local/state/tacit/labels/<sample>.jsonl)")
	openFact := fsf.String("fact", "", "open the page on the fact of this name")
	fsf.Parse(args)
	if *openFact != "" && *sample != "" {
		fmt.Fprintln(os.Stderr, "review: --fact opens the owner's page, which label mode does not serve")
		return 2
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		fmt.Fprintln(os.Stderr, "review: --addr must be a loopback address")
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "review: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "review: sync failed, showing local state:", err)
	}
	cancel()
	rs := &reviewServer{s: s, token: newToken(), tl: newTimelineBuilder(s, time.Time{})}
	rs.viewOnly = viewOnlyReason(context.Background(), s)
	if *sample != "" {
		b, err := os.ReadFile(*sample)
		if err != nil {
			fmt.Fprintln(os.Stderr, "review:", err)
			return 2
		}
		var items []struct{ ID string }
		if err := json.Unmarshal(b, &items); err != nil || len(items) == 0 {
			fmt.Fprintln(os.Stderr, "review: --label needs a JSON array of {id}")
			return 2
		}
		rs.sampleIDs = map[string]bool{}
		for _, it := range items {
			rs.sampleIDs[it.ID] = true
		}
		rs.labelFile = *labels
		if rs.labelFile == "" {
			home, _ := os.UserHomeDir()
			base := strings.TrimSuffix(filepath.Base(*sample), filepath.Ext(*sample))
			rs.labelFile = filepath.Join(platform.StateDir(home), "labels", base+".jsonl")
		}
		fmt.Println("tacit review: label mode,", len(rs.sampleIDs), "samples, marks go to", rs.labelFile)
	}
	rs.loadFacts()

	// Pull what other machines decided, every two minutes, under the store lock.
	go func() {
		if rs.labelling() {
			return
		}
		for range time.Tick(2 * time.Minute) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rs.mu.Lock()
			if err := s.Sync(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "review: sync:", err)
			}
			rs.mu.Unlock()
			cancel()
		}
	}()
	if note := openFactNote(rs, *openFact); note != "" {
		fmt.Fprintln(os.Stderr, "review:", note)
	}
	return serveOwner("review", *addr, rs.token, *openFact, *noOpen, rs.handler())
}

// handler is what tacit review serves. Outside label mode it is the owner's
// page, the one the desktop window shows (App, uiHandler): what waits on
// them, every fact with edit, retire, restore and undo, new rules and the
// Folders page, all through the methods the window binds, so every release
// carries the owner's channel without the desktop build. In label mode it
// is the review page alone, whose marks go to the label file and never to
// the store. Either way the page needs the run's token on its URL and every
// call needs it in X-Tacit-Token.
func (rs *reviewServer) handler() http.Handler {
	if !rs.labelling() {
		return newApp(rs).uiHandler(rs.token)
	}
	return pageHandler("index.html", rs.token, []string{"/fonts/", "/app.css", "/app.js", "/theme.js"}, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
			if !tokenOK(r.Header.Get("X-Tacit-Token"), rs.token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			rs.mu.Lock()
			rs.loadFacts() // another tool or machine may have changed the store
			items := rs.allItems()
			rs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"items": items, "host": store.ShortHost(), "labelling": rs.labelling(), "view_only": rs.viewOnly})
		})
		mux.HandleFunc("/api/decide", rs.handleDecide)
		mux.HandleFunc("/api/timeline", rs.handleTimeline)
	})
}

// handleDecide serves POST /api/decide: one mark from the page. OwnerOnly
// matters only in label mode.
func (rs *reviewServer) handleDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !tokenOK(r.Header.Get("X-Tacit-Token"), rs.token) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		ID, Action, Comment string
		OwnerOnly           bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A closed tab must not kill git mid-rebase: the mark runs to its own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	note, err := rs.answer(ctx, req.ID, req.Action, req.Comment, req.OwnerOnly)
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": note})
}
