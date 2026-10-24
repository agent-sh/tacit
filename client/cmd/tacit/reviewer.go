package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The reviewer (SPEC.md, "The reviewer"): every note an agent writes (tacit
// add, the MCP tacit_add, the Hermes provider's memory writes) and every
// note imported from an agent's own memory is judged by the deciding model
// against the conversation it came from: correct, uncertain or wrong, with a
// reason, in the reviewer's ledger. It is on by default; TACIT_REVIEWER=off
// (or reviewer: off in the settings file) turns it off. It runs as a step of
// every curate, full and quick: on a writer clone before the batches, on the
// stores' inbox notes; on a read-only clone with the local layer on this
// machine's agents' notes in its overlay, and nothing leaves the machine.

// reviewerOn reports the reviewer on (the default).
func reviewerOn() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("TACIT_REVIEWER")), "off")
}

// reviewedHarness is a harness whose notes are agents' saves: the dreamer's
// notes are not reviewed (they are read from the owner's sessions already),
// nor the owner's, the curator's or a reader's.
var reviewedHarness = map[string]bool{"claude": true, "codex": true, "pi": true, "hermes": true, "grok": true, "muse": true, "agy": true, "import": true}

// reviewCandidate reports an agent's note the reviewer judges: a note in the
// store's inbox, or in this machine's overlay with the local layer (an
// agent's note there, never the layer's dreamer notes or curated facts).
func reviewCandidate(f *fact.Fact, local bool) bool {
	if !reviewedHarness[f.Meta.Source.Harness] {
		return false
	}
	if local {
		return strings.HasPrefix(f.Path, overlay.Prefix) && !overlay.IsInbox(f.Path) && !overlay.IsCurated(f.Path)
	}
	return f.InInbox()
}

// reviewNote is the note as the reviewer reads it.
func reviewNote(f *fact.Fact, home string) agentreview.Note {
	from := f.Meta.Source.ImportedFrom
	if strings.HasPrefix(from, "~/") {
		from = filepath.Join(home, from[2:])
	}
	return agentreview.Note{Path: f.Path, Name: f.Meta.Name, Desc: f.Meta.Description, Body: strings.TrimSpace(f.Body), Type: f.Meta.Type, Scope: f.Meta.Scope,
		Harness: f.Meta.Source.Harness, Session: f.Meta.Source.Session, ImportedFrom: from, Created: f.Meta.Created}
}

// noteRepo is the repository a note is about, for its question's relevance:
// its projects/<repo> scope, else its one applies_to.repos name.
func noteRepo(f *fact.Fact) string {
	if r, ok := strings.CutPrefix(f.Meta.Scope, "projects/"); ok {
		return r
	}
	if len(f.Meta.AppliesTo.Repos) == 1 {
		return f.Meta.AppliesTo.Repos[0]
	}
	return ""
}

// reviewRun asks the reviewer's model (role review, Opus by default: every
// deciding call runs on Opus).
var reviewRun agentreview.Runner = func(ctx context.Context, prompt string) (string, float64, string, error) {
	model := modelrun.Model("review", "opus")
	r, err := modelrun.Run(ctx, "review", model, prompt, "")
	if r.Route.Model != "" {
		model = r.Route.Model
	}
	return r.Text, r.CostUSD, model, err
}

// reviewOpts bounds one run of the step.
type reviewOpts struct {
	dry      bool
	max      int             // model reviews at most (0: none, only the free verdicts)
	until    time.Time       // no review starts after this
	backfill bool            // also notes created before the reviewer started
	quick    bool            // a quick curate's step
	only     map[string]bool // review only these notes (measurement), by path
}

// reviewReport is what one run of the step did.
type reviewReport struct {
	At         string         `json:"at"`
	Local      bool           `json:"local,omitempty"`
	Quick      bool           `json:"quick,omitempty"`
	Candidates int            `json:"candidates"`
	Before     int            `json:"before_start,omitempty"` // notes older than the reviewer's start, not reviewed
	Rulebook   int            `json:"rulebook,omitempty"`     // verdicts taken from the rulebook mod's log
	Reviewed   int            `json:"reviewed"`               // judged by the model
	NoSource   int            `json:"nosource,omitempty"`
	Failed     int            `json:"failed,omitempty"` // no verdict (model, redaction): they wait
	Waiting    int            `json:"waiting,omitempty"`
	Verdicts   map[string]int `json:"verdicts,omitempty"`
	CostUSD    float64        `json:"cost_usd"`
	Queued     int            `json:"questions_queued,omitempty"`
	Answered   int            `json:"answers_read,omitempty"`
	Applied    int            `json:"answers_applied,omitempty"`
	Settled    int            `json:"questions_settled,omitempty"`
	Expired    int            `json:"questions_expired,omitempty"`
	Errors     []string       `json:"errors,omitempty"`
}

func (r reviewReport) String() string {
	var vs []string
	for _, k := range []string{ledger.VerdictCorrect, ledger.VerdictUncertain, ledger.VerdictWrong, ledger.VerdictNoSource, ledger.VerdictConfirmed, ledger.VerdictRejected, ledger.VerdictCorrected} {
		if n := r.Verdicts[k]; n > 0 {
			vs = append(vs, fmt.Sprintf("%d %s", n, k))
		}
	}
	v := "none"
	if len(vs) > 0 {
		v = strings.Join(vs, ", ")
	}
	return fmt.Sprintf("reviewer: %d note(s) to judge, %d judged by the model, %d from the rulebook, %d failed, %d waiting; verdicts: %s; $%.3f; questions: %d queued, %d answered, %d applied, %d settled, %d expired",
		r.Candidates, r.Reviewed, r.Rulebook, r.Failed, r.Waiting, v, r.CostUSD, r.Queued, r.Answered, r.Applied, r.Settled, r.Expired)
}

// reviewWork is what the step decided, for one writer to write: the
// ledger's new lines, the owner's words its corrections hold (quote ledger
// lines), the statements that hold them, and the curated facts that change
// (retired, superseded) with what changes.
type reviewWork struct {
	lines      []ledger.Review
	quotes     []ledger.Quote
	statements []*fact.Fact
	retire     map[string]string // curated fact path -> the day
	supersede  map[string]string // curated fact path -> the statement's name
	confirm    map[string]bool   // curated fact path -> confirmed (frontmatter, format 4)
	started    string            // the reviewer's start day, when this run starts it
}

// empty reports work with nothing to write: the start line goes into the
// ledger only with the first verdict.
func (w *reviewWork) empty() bool {
	return len(w.lines) == 0 && len(w.quotes) == 0 && len(w.statements) == 0 && len(w.retire) == 0 && len(w.supersede) == 0 && len(w.confirm) == 0
}

// reviewerStart is the day the reviewer first ran on this machine
// (reviewer-start in the state directory), recorded now when it is not yet.
func reviewerStart(stateDir string, now time.Time, dry bool) string {
	p := filepath.Join(stateDir, "reviewer-start")
	if b, err := os.ReadFile(p); err == nil {
		if d := strings.TrimSpace(string(b)); fact.IsDate(d) {
			return d
		}
	}
	day := now.Format("2006-01-02")
	if !dry && os.MkdirAll(stateDir, 0o700) == nil {
		os.WriteFile(p, []byte(day+"\n"), 0o600)
	}
	return day
}

// reviewStep judges the agents' notes among facts that have no verdict yet,
// reads and applies the owner's answers, and keeps the questions file. It
// decides; write writes (the store's curator commit, or the overlay).
// reviews is the ledger as it stands; facts the store's (and, local, the
// overlay's) facts.
func reviewStep(ctx context.Context, facts []*fact.Fact, reviews *ledger.Reviews, local bool, o reviewOpts, statements bool) (*reviewWork, reviewReport) {
	home, _ := os.UserHomeDir()
	stateDir := platform.StateDir(home)
	now := time.Now()
	rep := reviewReport{At: now.UTC().Format(time.RFC3339), Local: local, Quick: o.quick, Verdicts: map[string]int{}}
	w := &reviewWork{retire: map[string]string{}, supersede: map[string]string{}, confirm: map[string]bool{}}
	started := reviews.Started
	if started == "" {
		// The day the reviewer first ran here (machine-local until its first
		// verdict carries it into the ledger): notes from before it are not
		// judged unless asked.
		started = reviewerStart(stateDir, now, o.dry)
		w.started = started
	}
	rows := agentreview.ReadRulebook(agentreview.RulebookLog(home))
	rv := agentreview.Reviewer{Sources: agentreview.DefaultSources(home), Run: reviewRun, Redact: digest.RedactAll, Now: time.Now}
	if v := os.Getenv("TACIT_HERMES_DB"); strings.EqualFold(v, "off") {
		rv.Sources.HermesDB = ""
	}
	byName := map[string]*fact.Fact{}
	curatedFrom := map[string][]*fact.Fact{} // note name -> its live curated facts
	for _, f := range facts {
		byName[f.Meta.Name] = f
		if !f.InInbox() && !local && f.Meta.Origin != "" && f.Meta.SupersededBy == "" && f.Meta.Retired == "" && f.Meta.Source.Harness == "curator" {
			curatedFrom[f.Meta.Origin] = append(curatedFrom[f.Meta.Origin], f)
		}
	}
	var todo []*fact.Fact
	notes := map[string]*fact.Fact{}
	for _, f := range facts {
		if !reviewCandidate(f, local) {
			continue
		}
		notes[f.Path] = f
		if _, ok := reviews.ForNote(f.Path); ok {
			continue
		}
		if o.only != nil && !o.only[f.Path] {
			continue
		}
		rep.Candidates++
		if !o.backfill && f.Meta.Created != "" && f.Meta.Created < started {
			rep.Before++
			continue
		}
		n := reviewNote(f, home)
		if l, ok := agentreview.RulebookVerdict(rows, n, now.UTC().Format(time.RFC3339)); ok {
			w.lines = append(w.lines, l)
			rep.Rulebook++
			rep.Verdicts[l.Verdict]++
			continue
		}
		todo = append(todo, f)
	}
	// Newest first: the sessions a recent note came from are the likeliest
	// to be on this machine still, and the owner the likeliest to remember.
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].Meta.Created > todo[j].Meta.Created })
	convs := map[string]agentreview.Conversation{}
	called := 0
	for _, f := range todo {
		n := reviewNote(f, home)
		if _, _, ok := agentreview.SessionOf(n); ok {
			if called >= o.max || (!o.until.IsZero() && time.Now().After(o.until)) || ctx.Err() != nil {
				rep.Waiting++
				continue
			}
		}
		if o.dry {
			fmt.Printf("- would review %s (%s)\n", f.Path, f.Meta.Description)
			continue
		}
		res, err := rv.Review(ctx, n)
		if res.Called {
			called++
			rep.CostUSD += res.Line.CostUSD
		}
		if err != nil {
			rep.Failed++
			rep.Errors = append(rep.Errors, f.Path+": "+err.Error())
			continue
		}
		if res.Called {
			rep.Reviewed++
		} else {
			rep.NoSource++
		}
		rep.Verdicts[res.Line.Verdict]++
		w.lines = append(w.lines, res.Line)
		convs[f.Path] = res.Conv
	}
	// The ledger as it will stand, for the questions.
	all := append(append([]ledger.Review(nil), reviews.Lines...), w.lines...)
	merged := mergedReviews(all, started)

	if o.dry {
		return w, rep
	}
	qpath := agentreview.Path(stateDir)
	err := agentreview.Locked(qpath, 10*time.Second, func(q *agentreview.Queue) bool {
		changed := false
		// Uncertain notes become questions (a rulebook uncertain too: the
		// owner may not have seen its pane).
		for _, l := range w.lines {
			if l.Verdict != ledger.VerdictUncertain {
				continue
			}
			f := notes[l.Note]
			if f == nil {
				continue
			}
			it := agentreview.Item{Note: f.Path, Name: f.Meta.Name, Desc: f.Meta.Description, Day: f.Meta.Created, Repo: noteRepo(f), Reason: l.Reason}
			it.Harness, it.Session, _ = agentreview.SessionOf(reviewNote(f, home))
			if c, ok := convs[f.Path]; ok {
				it.Cwd = c.Cwd
			}
			if fs := curatedFrom[f.Meta.Name]; len(fs) > 0 {
				it.Fact = fs[0].Meta.Name
			}
			if q.Add(it, now) {
				rep.Queued++
				changed = true
			}
		}
		// The owner's answers: from their own turns, and from the rulebook pane.
		if agentreview.ReadAnswers(ctx, q, rv.Sources, nil, now) {
			changed = true
		}
		if agentreview.TakeRulebookAnswers(q, agentreview.RulebookAnswers(rows)) {
			changed = true
		}
		for i := range q.Items {
			it := &q.Items[i]
			switch it.State {
			case agentreview.StateAnswered:
				rep.Answered++
				if applyAnswer(it, notes[it.Note], curatedFrom, w, local, statements, now) {
					rep.Applied++
					it.Close(agentreview.StateApplied, "answered "+it.Answer.Kind, now)
				}
				changed = true
			case agentreview.StateOpen:
				if f := notes[it.Note]; f != nil && it.Fact == "" {
					if fs := curatedFrom[f.Meta.Name]; len(fs) > 0 {
						it.Fact, changed = fs[0].Meta.Name, true
					}
				}
				// Later evidence settles it: the note is gone, a verdict other
				// than uncertain now stands on it (the owner's in the rulebook
				// pane, a newer review), or every fact made from it was replaced.
				f := notes[it.Note]
				l, ok := merged.ForNote(it.Note)
				switch {
				case f == nil:
					it.Close(agentreview.StateSettled, "the note is gone", now)
				case ok && l.Verdict != ledger.VerdictUncertain:
					it.Close(agentreview.StateSettled, "a later verdict stands on it: "+l.Verdict+" by "+l.By, now)
				case !local && it.Fact != "" && len(curatedFrom[f.Meta.Name]) == 0:
					it.Close(agentreview.StateSettled, "every fact made from it was replaced or retired", now)
				default:
					continue
				}
				rep.Settled++
				changed = true
			}
		}
		before := countState(q, agentreview.StateExpired)
		if q.Tidy(now) {
			changed = true
		}
		rep.Expired = countState(q, agentreview.StateExpired) - before
		return changed
	})
	if err != nil {
		rep.Errors = append(rep.Errors, "questions: "+err.Error())
	}
	return w, rep
}

func countState(q *agentreview.Queue, state string) int {
	n := 0
	for _, it := range q.Items {
		if it.State == state {
			n++
		}
	}
	return n
}

func mergedReviews(lines []ledger.Review, started string) *ledger.Reviews {
	var b []byte
	if started != "" {
		b = append(b, ledger.Review{Started: started}.Line()...)
	}
	for _, l := range lines {
		b = append(b, l.Line()...)
	}
	return ledger.ParseReviews(b)
}

// applyAnswer turns the owner's answer into the ledger's owner line and its
// effects: yes, the note is theirs to keep (confirmed, ranked with their
// statements); no, it is retired with their reason (its curated facts get
// retired; on this machine the note is held back); a correction, their words
// become a statement that supersedes it. It reports whether it applied.
func applyAnswer(it *agentreview.Item, note *fact.Fact, curatedFrom map[string][]*fact.Fact, w *reviewWork, local, statements bool, now time.Time) bool {
	a := it.Answer
	if a == nil || note == nil {
		return note == nil // a note that is gone has nothing to apply to: closed
	}
	day := now.Format("2006-01-02")
	line := ledger.Review{Note: note.Path, Name: note.Meta.Name, Hash: ledger.NoteHash(note.Meta.Description, note.Body), By: ledger.ByOwner,
		From: a.From, TurnHash: a.TurnHash, At: now.UTC().Format(time.RFC3339)}
	facts := curatedFrom[note.Meta.Name]
	switch a.Kind {
	case agentreview.AnswerYes:
		line.Verdict, line.Reason = ledger.VerdictConfirmed, "the owner said yes"
		for _, f := range facts {
			w.confirm[f.Path] = true
		}
	case agentreview.AnswerNo:
		line.Verdict, line.Reason = ledger.VerdictRejected, "the owner said no"
		if a.Words != "" {
			line.Reason += ": " + a.Words
			line.Words = a.Words
		}
		for _, f := range facts {
			w.retire[f.Path] = day
		}
	case agentreview.AnswerCorrect:
		red, err := digest.RedactAll([]string{a.Words})
		if err != nil || strings.TrimSpace(red[0]) == "" {
			return false // fail closed: their words wait for redaction
		}
		words := strings.Join(strings.Fields(red[0]), " ")
		if !local && !statements {
			// A store whose format predates statements: their words are the
			// reason the note is retired.
			line.Verdict, line.Reason, line.Words = ledger.VerdictRejected, "the owner corrected it: "+words, words
			for _, f := range facts {
				w.retire[f.Path] = day
			}
			break
		}
		st := correctionStatement(note, words, a.From, day, local)
		line.Verdict, line.Reason, line.Words, line.Statement = ledger.VerdictCorrected, "the owner corrected it", words, st.Meta.Name
		w.statements = append(w.statements, st)
		w.quotes = append(w.quotes, ledger.Quote{Name: st.Meta.Name, Quote: words, From: a.From, TurnHash: a.TurnHash, At: day, Words: true})
		for _, f := range facts {
			w.supersede[f.Path] = st.Meta.Name
		}
	default:
		return false
	}
	w.lines = append(w.lines, line)
	return true
}

// correctionStatement is the owner's correction as a statement (SPEC.md,
// "Statements"): their words verbatim and dated, the agent's note as the
// context, in the note's scope. On this machine's layer it is the local
// curator's fact.
func correctionStatement(note *fact.Fact, words, from, day string, local bool) *fact.Fact {
	scope := note.Meta.Scope
	if scope == "" || !fact.ValidScope(scope) {
		scope = "user"
	}
	name := importer.Slug("owner-corrects-"+note.Meta.Name, 80)
	context := "correcting an agent's note: " + strings.Join(strings.Fields(note.Meta.Description), " ")
	f := &fact.Fact{Meta: fact.Meta{
		Name: name, Description: fact.ClipDescription(context, 200), Type: fact.StatementType, Scope: scope, AppliesTo: note.Meta.AppliesTo,
		Source:  fact.Source{Harness: "curator", Host: store.ShortHost(), Kind: "observed", OwnerWords: words, OwnerWordsFrom: from},
		Created: day, Origin: note.Meta.Name,
	}, Body: fact.StatementBody(day, words, context, "")}
	f.Path = scope + "/" + name + ".md"
	if local {
		f.Path = overlay.Prefix + overlay.CuratedDir + "/" + f.Path
	}
	return f
}

// writeReviewStore writes the step's work as one curator commit: the
// ledger's lines (and the start line), the statements and their words'
// ledger lines, and the curated facts it retires, supersedes or confirms.
func writeReviewStore(ctx context.Context, s *store.Store, w *reviewWork) error {
	if w.empty() {
		return nil
	}
	reviewsFM := false
	if f, ok, err := ledger.ReadFormat(s.Dir); err == nil && ok && f.Version >= ledger.ReviewsFormat && f.Writes() {
		reviewsFM = true
	}
	_, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		files := map[string][]byte{}
		var add []byte
		cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.ReviewsPath)))
		if w.started != "" && ledger.ParseReviews(cur).Started == "" {
			add = append(add, ledger.Review{Started: w.started}.Line()...)
		}
		for _, l := range w.lines {
			add = append(add, l.Line()...)
		}
		if len(add) > 0 {
			files[ledger.ReviewsPath] = append(withNewline(cur), add...)
		}
		if len(w.quotes) > 0 {
			q, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
			q = withNewline(q)
			for _, x := range w.quotes {
				q = append(q, x.Line()...)
			}
			files[ledger.QuotesPath] = q
		}
		for _, st := range w.statements {
			if _, err := os.Stat(filepath.Join(s.Dir, filepath.FromSlash(st.Path))); err == nil {
				continue // written before
			}
			b, err := st.Encode()
			if err != nil {
				return nil, "", err
			}
			files[st.Path] = b
		}
		patch := func(rel string, change func(f *fact.Fact)) error {
			data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil // gone since: nothing to change
			}
			f, err := fact.Parse(rel, data)
			if err != nil {
				return err
			}
			change(f)
			out, err := fact.PatchRelations(data, f)
			if err != nil {
				return err
			}
			files[rel] = out
			return nil
		}
		today := time.Now().Format("2006-01-02")
		for rel, day := range w.retire {
			if err := patch(rel, func(f *fact.Fact) { f.Meta.Retired, f.Meta.Updated = day, today }); err != nil {
				return nil, "", err
			}
		}
		for rel, by := range w.supersede {
			if err := patch(rel, func(f *fact.Fact) { f.Meta.SupersededBy, f.Meta.Updated = by, today }); err != nil {
				return nil, "", err
			}
		}
		if reviewsFM {
			for rel := range w.confirm {
				data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
				if err != nil {
					continue
				}
				f, err := fact.Parse(rel, data)
				if err != nil {
					return nil, "", err
				}
				f.Meta.Source.Review, f.Meta.Source.ReviewReason = ledger.VerdictConfirmed, "the owner said yes"
				out, err := fact.PatchSource(data, f)
				if err != nil {
					return nil, "", err
				}
				files[rel] = out
			}
		}
		return files, reviewCommitMessage(w), nil
	})
	return err
}

func withNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}

func reviewCommitMessage(w *reviewWork) string {
	n := map[string]int{}
	for _, l := range w.lines {
		n[l.Verdict]++
	}
	var parts []string
	for _, k := range []string{ledger.VerdictCorrect, ledger.VerdictUncertain, ledger.VerdictWrong, ledger.VerdictNoSource, ledger.VerdictConfirmed, ledger.VerdictRejected, ledger.VerdictCorrected} {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[k], k))
		}
	}
	msg := "curator: review: " + strings.Join(parts, ", ")
	if len(parts) == 0 {
		msg = "curator: review: the reviewer starts"
	}
	var b strings.Builder
	b.WriteString(msg + "\n\n")
	for _, l := range w.lines {
		fmt.Fprintf(&b, "- %s: %s (%s)\n", l.Name, l.Verdict, l.By)
	}
	return b.String()
}

// writeReviewLocal writes the step's work into this machine's overlay: the
// local ledger, the local quote ledger, the statements as the local
// curator's facts. An agent's note is never edited: its verdict holds it
// back or ranks it.
func writeReviewLocal(dir, storeDir string, w *reviewWork) error {
	if w.empty() {
		return nil
	}
	var add []byte
	cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.ReviewsRel)))
	if w.started != "" && ledger.ParseReviews(cur).Started == "" {
		add = append(add, ledger.Review{Started: w.started}.Line()...)
	}
	for _, st := range w.statements {
		rel := strings.TrimPrefix(st.Path, overlay.Prefix)
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			continue
		}
		red, err := digest.RedactAll([]string{st.Meta.Description, st.Body})
		if err != nil {
			return err
		}
		st.Meta.Description, st.Body = red[0], red[1]
		b, err := st.Encode()
		if err != nil {
			return err
		}
		if err := overlay.Write(dir, storeDir, rel, b); err != nil {
			return err
		}
	}
	if len(w.quotes) > 0 {
		var q []byte
		for _, x := range w.quotes {
			q = append(q, x.Line()...)
		}
		if err := overlay.Append(dir, storeDir, overlay.QuotesRel, q); err != nil {
			return err
		}
	}
	for _, l := range w.lines {
		add = append(add, l.Line()...)
	}
	if len(add) == 0 {
		return nil
	}
	return overlay.Append(dir, storeDir, overlay.ReviewsRel, append(withNewline(nil), add...))
}

// logReview appends the step's report line to reviewer.log in the state
// directory.
func logReview(rep reviewReport) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(platform.StateDir(home), "reviewer.log")
	b, _ := json.Marshal(rep)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// reviewLimits are the step's bounds in a curate: a quick run judges a few
// notes in a few minutes, the full run more.
const (
	reviewMaxQuick = 6
	reviewMaxFull  = 40
)

// curateReview is the step as the store's curate runs it, before the
// batches: it judges, writes and returns the ledger as it now stands.
func curateReview(ctx context.Context, s *store.Store, facts []*fact.Fact, quick, dry bool, until time.Time) (*ledger.Reviews, string) {
	cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.ReviewsPath)))
	reviews := ledger.ParseReviews(cur)
	if !reviewerOn() {
		return reviews, ""
	}
	started := reviews.Started
	if started == "" {
		home, _ := os.UserHomeDir()
		started = reviewerStart(platform.StateDir(home), time.Now(), dry)
	}
	max := reviewMaxFull
	if quick {
		max = reviewMaxQuick
	}
	w, rep := reviewStep(ctx, facts, reviews, false, reviewOpts{dry: dry, max: max, until: until, quick: quick}, statementsWrite(s.Dir))
	fmt.Println(rep.String())
	if dry {
		return reviews, started
	}
	if err := writeReviewStore(ctx, s, w); err != nil {
		fmt.Fprintln(os.Stderr, "curate: reviewer:", err)
		rep.Errors = append(rep.Errors, "write: "+err.Error())
	}
	logReview(rep)
	cur, _ = os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.ReviewsPath)))
	return ledger.ParseReviews(cur), started
}

// localReview is the step as the local layer's curate runs it: this
// machine's agents' notes, into the overlay.
func localReview(ctx context.Context, s *store.Store, dir string, local []*fact.Fact, quick, dry bool, until time.Time) {
	if !reviewerOn() {
		return
	}
	cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.ReviewsRel)))
	max := reviewMaxFull
	if quick {
		max = reviewMaxQuick
	}
	w, rep := reviewStep(ctx, local, ledger.ParseReviews(cur), true, reviewOpts{dry: dry, max: max, until: until, quick: quick}, true)
	fmt.Println(rep.String())
	if dry {
		return
	}
	if err := writeReviewLocal(dir, s.Dir, w); err != nil {
		fmt.Fprintln(os.Stderr, "curate: reviewer:", err)
		rep.Errors = append(rep.Errors, "write: "+err.Error())
	}
	logReview(rep)
}

// reviewWait is how long an agent's note with no verdict yet waits for the
// reviewer before the store's curator takes it unreviewed (the model may be
// down for a day; nothing waits forever).
const reviewWait = 3 * 24 * time.Hour

// gateReviewed takes out of pending what the reviewer holds: an agent's note
// judged wrong (or that the owner said no to, or corrected) is not curated
// and stays in the inbox's history with its reason in the ledger; one with
// no verdict yet waits up to reviewWait from the first run that held it
// back (curator.json review_wait). It returns the rest, how many were
// refused and how many wait, and whether cst changed.
func gateReviewed(pending []*fact.Fact, reviews *ledger.Reviews, started string, cst *curatorState, now time.Time) (kept []*fact.Fact, refused, waiting int, changed bool) {
	if !reviewerOn() {
		return pending, 0, 0, false
	}
	today := now.Format("2006-01-02")
	for _, n := range pending {
		if !reviewCandidate(n, false) {
			kept = append(kept, n)
			continue
		}
		l, ok := reviews.ForNote(n.Path)
		switch {
		case ok && (l.Verdict == ledger.VerdictWrong || l.Verdict == ledger.VerdictRejected || l.Verdict == ledger.VerdictCorrected):
			refused++
			continue
		case ok:
			if _, held := cst.ReviewWait[n.Meta.Name]; held {
				delete(cst.ReviewWait, n.Meta.Name)
				changed = true
			}
			kept = append(kept, n)
			continue
		case started == "" || n.Meta.Created < started:
			kept = append(kept, n) // from before the reviewer: not judged unless backfilled
			continue
		}
		first, held := cst.ReviewWait[n.Meta.Name]
		if !held {
			if cst.ReviewWait == nil {
				cst.ReviewWait = map[string]string{}
			}
			cst.ReviewWait[n.Meta.Name], first, changed = today, today, true
		}
		if since, err := time.ParseInLocation("2006-01-02", first, now.Location()); err == nil && now.Sub(since) < reviewWait {
			waiting++
			continue
		}
		kept = append(kept, n)
	}
	return kept, refused, waiting, changed
}

// stampReview writes the reviewer's verdict on the note a curated fact is
// made from into its frontmatter, in a store of format 4.
func stampReview(f *fact.Fact, reviews *ledger.Reviews, frontmatter bool) {
	if !frontmatter || f.Meta.Origin == "" {
		return
	}
	if l, ok := reviews.ForOrigin(f.Meta.Origin); ok && l.Verdict != ledger.VerdictWrong && l.Verdict != ledger.VerdictRejected && l.Verdict != ledger.VerdictCorrected {
		f.Meta.Source.Review, f.Meta.Source.ReviewReason = l.Verdict, fact.ClipDescription(l.Reason, 300)
	}
}

// runReviewer implements `tacit reviewer [--dry-run] [--max N] [--backfill]
// [--note PATH]... [--json FILE] [--env-file F]`, and `tacit reviewer
// questions [--json]`: the step alone (the curate runs it anyway), or the
// questions waiting for the owner.
func runReviewer(args []string) int {
	if len(args) > 0 && args[0] == "questions" {
		return runReviewerQuestions(args[1:])
	}
	fs := flag.NewFlagSet("reviewer", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "list the notes it would judge; write nothing")
	max := fs.Int("max", reviewMaxFull, "model reviews at most")
	backfill := fs.Bool("backfill", false, "also judge notes created before the reviewer started")
	jsonOut := fs.String("json", "", "write the run's report to this file")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables")
	var notes multiFlag
	fs.Var(&notes, "note", "judge only this note (its store or overlay path); repeatable")
	fs.Parse(args)
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "reviewer:", err)
			return 2
		}
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "reviewer: no store")
		return 2
	}
	ctx := context.Background()
	o := reviewOpts{dry: *dry, max: *max, backfill: *backfill}
	if len(notes) > 0 {
		o.only = map[string]bool{}
		for _, n := range notes {
			o.only[n] = true
		}
	}
	var w *reviewWork
	var rep reviewReport
	if localLayer(ctx, s) {
		dir := overlay.Dir()
		if !overlay.Outside(dir, s.Dir) {
			fmt.Fprintln(os.Stderr, "reviewer:", overlay.ErrInside)
			return 2
		}
		local, _ := overlay.Load(dir)
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.ReviewsRel)))
		w, rep = reviewStep(ctx, local, ledger.ParseReviews(cur), true, o, true)
		if !*dry {
			err = writeReviewLocal(dir, s.Dir, w)
		}
	} else {
		if !*dry && refuseWriter(ctx, s, "curator", "reviewer") {
			return 2
		}
		if !*dry {
			if err := s.Sync(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "reviewer: sync:", err)
				return 1
			}
		}
		facts, _, werr := fact.Walk(s.Dir)
		if werr != nil {
			fmt.Fprintln(os.Stderr, "reviewer:", werr)
			return 2
		}
		cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.ReviewsPath)))
		w, rep = reviewStep(ctx, facts, ledger.ParseReviews(cur), false, o, statementsWrite(s.Dir))
		if !*dry {
			err = writeReviewStore(ctx, s, w)
		}
	}
	fmt.Println(rep.String())
	for _, e := range rep.Errors {
		fmt.Fprintln(os.Stderr, "reviewer:", e)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviewer:", err)
		rep.Errors = append(rep.Errors, "write: "+err.Error())
	}
	if *jsonOut != "" {
		type out struct {
			Report reviewReport    `json:"report"`
			Lines  []ledger.Review `json:"lines"`
		}
		b, _ := json.MarshalIndent(out{rep, w.lines}, "", " ")
		os.WriteFile(*jsonOut, append(b, '\n'), 0o600)
	}
	if !*dry {
		logReview(rep)
	}
	if err != nil {
		return 1
	}
	return 0
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runReviewerQuestions prints the questions waiting for the owner.
func runReviewerQuestions(args []string) int {
	fs := flag.NewFlagSet("reviewer questions", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print them as JSON")
	fs.Parse(args)
	home, _ := os.UserHomeDir()
	q, err := agentreview.Load(agentreview.Path(platform.StateDir(home)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "reviewer:", err)
		return 1
	}
	open := q.Open()
	if *asJSON {
		b, _ := json.MarshalIndent(open, "", " ")
		fmt.Println(string(b))
		return 0
	}
	if len(open) == 0 {
		fmt.Println("no questions waiting")
		return 0
	}
	for _, it := range open {
		fmt.Println(it.Text())
	}
	return 0
}
