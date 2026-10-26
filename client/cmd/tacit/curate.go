package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// curatorState remembers inbox notes the split produced nothing for, so
// they are not re-sent every day.
type curatorState struct {
	Empty map[string]string `json:"empty_origins"` // origin -> date
	// Restated holds the notes that only restate a curated fact or an earlier
	// note (curate.Restated, no model), and the notes whose every fact of the
	// split restated a curated fact (curate.Absorber): note -> the fact it
	// restates.
	Restated map[string]string `json:"restated,omitempty"`
	// Failed counts, per note, the runs in which the note failed on its own:
	// the curator's answer for a batch of just that note was malformed or
	// invalid. A note held here maxNoteFailures times is set aside.
	Failed map[string]int `json:"failed,omitempty"`
	// DeferredRepo holds the notes whose scope names a repository no
	// catalog knows yet: note -> the date it was first held back
	// (deferUnknownRepos).
	DeferredRepo map[string]string `json:"deferred_repo,omitempty"`
	// ReviewWait holds the agents' notes with no reviewer's verdict yet:
	// note -> the date a run first held it back (gateReviewed).
	ReviewWait map[string]string `json:"review_wait,omitempty"`
}

// maxRepoDefer is how long a note whose scope names a repository no catalog
// knows waits for one (the machine that has the checkout dreams and pushes
// its catalog) before it is curated like any other note: its entries then
// face the checks, the model's one retry and the failure strikes.
const maxRepoDefer = 14 * 24 * time.Hour

// deferUnknownRepos takes out of pending the notes whose own scope names a
// repository the checks do not know (validate.Env.UnknownRepo), for up to
// maxRepoDefer from the first run that held each one back (recorded in
// cst), and returns the rest and how many wait. A note whose repository is
// known now, or that waited its time, goes on and leaves cst. changed
// reports a change to cst.
func deferUnknownRepos(pending []*fact.Fact, env validate.Env, cst *curatorState, now time.Time) (kept []*fact.Fact, deferred int, changed bool) {
	today := now.Format("2006-01-02")
	for _, n := range pending {
		name := n.Meta.Name
		if !env.UnknownRepo(n.Meta.Scope) {
			if _, ok := cst.DeferredRepo[name]; ok {
				delete(cst.DeferredRepo, name)
				changed = true
			}
			kept = append(kept, n)
			continue
		}
		first, ok := cst.DeferredRepo[name]
		if !ok {
			if cst.DeferredRepo == nil {
				cst.DeferredRepo = map[string]string{}
			}
			cst.DeferredRepo[name], first, changed = today, today, true
		}
		if since, err := time.ParseInLocation("2006-01-02", first, now.Location()); err == nil && now.Sub(since) < maxRepoDefer {
			deferred++
			continue
		}
		kept = append(kept, n) // waited its time: the normal path
	}
	return kept, deferred, changed
}

// maxNoteFailures is how often a note may fail alone before it is set aside.
// Without it, one note the model cannot split fails again in every run and
// costs a bisection each time.
const maxNoteFailures = 3

// systemicSplits is how many splits a run makes before any batch has a valid
// answer: enough to bisect one batch of about 85 notes down to a single
// note (7 levels), and then the run stops splitting, because a model that
// fails every half is failing the run, not a note.
const systemicSplits = 8

// runCurate implements `tacit curate [--model M] [--max-batches N] [--dry-run]
// [--quick [--min-interval D]]`.
//
// Per batch of pending inbox notes (no curated child by origin, not known
// empty): split with the curator model; judge every new fact against its
// closest existing curated fact and against the other new facts of the
// same scope, in one batched judge call; write; regenerate the index;
// commit and push. A batch is pushed before the next starts, so a deadline
// or crash leaves nothing half-done, and nothing is counted as curated
// unless it reached the server.
//
// --quick is the idle job's curate: only the notes added in the last
// quickWindow, newest first, through the same split, rulings check,
// conflict judge, index and push; no derive, results, disowned, links or
// staleness step. A quick run counts no strike against a note that fails
// alone (curator.json "failed"; only the full run does), and owner notes it
// must leave pending for want of owner history are not an error. One curate
// runs at a time (curator.lock): a quick run that finds one running, or that
// comes within --min-interval of the last quick run, exits 0 doing nothing; a
// full run waits for a quick one (up to curateLockWait), and its deadline
// starts once it holds the lock.
func runCurate(args []string) int {
	fs := flag.NewFlagSet("curate", flag.ExitOnError)
	model := fs.String("model", curate.DefaultModel, "split model")
	maxBatches := fs.Int("max-batches", 16, "cap on split batches per run (about 85 dreamer notes and $0.91 each with Opus 5.5, 2026-10-18); the halves of a batch split after an invalid answer do not count")
	budget := fs.Duration("budget", 150*time.Minute, "no split batch starts after this long; the derive, links and staleness steps run after")
	dry := fs.Bool("dry-run", false, "split and check, write nothing")
	recheck := fs.Bool("recheck-conflicts", false, "run only the conflict check again, for the curated facts made from --since on (a run whose check failed)")
	since := fs.String("since", "", "with --recheck-conflicts: first creation day, YYYY-MM-DD")
	until := fs.String("until", "", "with --recheck-conflicts: last creation day, YYYY-MM-DD (default: no end)")
	limit := fs.Int("limit", 0, "with --recheck-conflicts: check only the first N facts (trial runs)")
	retrySetAside := fs.Bool("retry-set-aside", false, "try again the notes set aside after failing alone (curator.json \"failed\")")
	quick := fs.Bool("quick", false, "only the notes added in the last 24 hours: split, rulings check, conflict judge, index, push; no derive, results, links or staleness step")
	minInterval := fs.Duration("min-interval", 30*time.Minute, "with --quick: do nothing within this long of the last quick run")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables, set before any model call (for launchd and Windows, which have no EnvironmentFile)")
	fs.Parse(args)
	if *recheck && *since == "" {
		fmt.Fprintln(os.Stderr, "curate: --recheck-conflicts needs --since YYYY-MM-DD")
		return 2
	}
	if *quick && *recheck {
		fmt.Fprintln(os.Stderr, "curate: --quick and --recheck-conflicts are separate runs")
		return 2
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "curate:", err)
			return 2
		}
		refreshModels()
		if !explicit["model"] {
			*model = curate.DefaultModel
		}
	}
	if *quick {
		// A few minutes, not hours: the backlog is the full run's.
		if !explicit["max-batches"] {
			*maxBatches = quickBatches
		}
		if !explicit["budget"] {
			*budget = quickBudget
		}
	}

	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "curate: no store")
		return 2
	}
	// A read-only clone with the local layer curates this machine's overlay
	// (local_layer.go): no step of it writes the clone.
	if localLayer(context.Background(), s) {
		if *recheck {
			fmt.Fprintln(os.Stderr, "curate: --recheck-conflicts checks the store's facts; the local layer has no such run")
			return 2
		}
		return runCurateLocal(s, localCurate{model: *model, maxBatches: *maxBatches, budget: *budget, dry: *dry, quick: *quick, minInterval: *minInterval, retrySetAside: *retrySetAside})
	}
	// Before any work: a machine that cannot write as curator does nothing.
	// The idle job runs the quick curate every 10 minutes on every machine
	// that dreams; one without the curator key, or a read-only clone, ends
	// there, not as a failure and with no line.
	if !*dry {
		if *quick {
			if writerCheck(context.Background(), s, "curator") != nil {
				return 0
			}
		} else if refuseWriter(context.Background(), s, "curator", "curate") {
			return 2
		}
	}
	host := store.ShortHost()
	// The batches stop at --budget; the steps after them (derive, results,
	// links, staleness: about 25 minutes on the author's workstation) have the rest.
	tail := 75 * time.Minute
	if *quick {
		tail = 10 * time.Minute // the last batch's judge and push
	}
	home, _ := os.UserHomeDir()
	stateDir := platform.StateDir(home)
	if !*dry {
		var release func()
		var err error
		if *quick {
			release, err = tryJobLock(stateDir, "curator")
			if errors.Is(err, platform.ErrBusy) {
				fmt.Println("curate: busy (another curate holds " + filepath.Join(stateDir, "curator.lock") + "); nothing done")
				return 0
			}
		} else {
			// A quick run ends within its budget and tail (half an hour); the
			// full run's own deadline starts once it holds the lock.
			wctx, wcancel := context.WithTimeout(context.Background(), curateLockWait)
			release, err = waitJobLock(wctx, stateDir, "curator", func() {
				fmt.Fprintln(os.Stderr, "curate: waiting for the curate run that holds", filepath.Join(stateDir, "curator.lock"))
			})
			wcancel()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "curate: lock:", err)
			return 2
		}
		defer release()
		if *quick {
			qpath := filepath.Join(stateDir, "curate-quick.json")
			if last, ok := lastQuickRun(qpath); ok && time.Since(last) < *minInterval {
				fmt.Printf("curate: quick run %s ago, within --min-interval %s; nothing done\n", time.Since(last).Round(time.Second), *minInterval)
				return 0
			}
			// Stamped before the run: a run that fails still waits its
			// interval, so a failing model is not asked every 10 minutes.
			saveQuickRun(qpath, time.Now())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *budget+tail)
	defer cancel()
	batchesUntil := time.Now().Add(*budget)
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "curate: sync:", err)
		return 1
	}
	if *recheck {
		return recheckConflicts(ctx, s, host, *since, *until, *limit, *dry)
	}
	cst := loadCuratorState(filepath.Join(stateDir, "curator.json"))
	if *retrySetAside {
		cst.Failed = map[string]int{}
	}

	facts, perrs, err := fact.Walk(s.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate:", err)
		return 2
	}
	if len(perrs) > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d unparsable fact file(s) skipped\n", len(perrs))
	}
	// Their notes count as theirs only when the app added them (an owner commit).
	added := appNoteCommits(ctx, s.Dir)
	for _, f := range facts {
		if c, ok := added[f.Path]; ok && f.InInbox() {
			f.AppNote = strings.HasPrefix(c.subject, ownerCommit)
		}
	}
	// The reviewer judges the agents' notes before they are curated (SPEC.md,
	// "The reviewer"): a wrong one is not curated, and a curated fact carries
	// the verdict on the note it was made from.
	reviews, reviewStarted := curateReview(ctx, s, facts, *quick, *dry, time.Now().Add(*budget/3))
	reviewState := fact.StoreState{Reviews: reviews}
	for _, f := range facts {
		reviewState.ApplyReview(f)
	}
	reviewsFM := false
	if fm, ok, ferr := ledger.ReadFormat(s.Dir); ferr == nil && ok && fm.Writes() && fm.Version >= ledger.ReviewsFormat {
		reviewsFM = true
	}
	curatedOrigins := map[string]bool{}
	names := map[string]bool{}
	for _, f := range facts {
		names[f.Meta.Name] = true
		if !f.InInbox() && f.Meta.Origin != "" {
			curatedOrigins[f.Meta.Origin] = true
		}
	}
	env := curateEnv(s.Dir, false) // the metadata checks: this machine's checkouts, the store's folders and writers
	statementsOK := statementsWrite(s.Dir)
	var pending []*fact.Fact
	heldBack := 0 // notes that failed alone maxNoteFailures times
	noteByName := map[string]*fact.Fact{}
	for _, f := range facts {
		if f.InInbox() {
			noteByName[f.Meta.Name] = f
		}
		// An owner note saying facts are not their words is not split into
		// facts: the disowned step acts on it (curateDisowned).
		if f.InInbox() && !curatedOrigins[f.Meta.Name] && cst.Empty[f.Meta.Name] == "" && cst.Restated[f.Meta.Name] == "" && !disowningNote(f) {
			if cst.Failed[f.Meta.Name] >= maxNoteFailures {
				heldBack++
				continue
			}
			pending = append(pending, f)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Path < pending[j].Path })
	if *quick {
		pending = recentNotes(ctx, s.Dir, pending, time.Now().Add(-quickWindow))
	}
	var where curate.Where
	var saidIn func(quote string) []string // the sessions the owner said a quote in (one-offs)
	noOwnerCorpus := false
	deferredOwner := 0
	if len(pending) > 0 {
		corpus, cerr := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
		if cerr != nil && !errors.Is(cerr, errNoOwnerWords) {
			fmt.Fprintln(os.Stderr, "curate: owner history:", cerr)
			return 1
		}
		noOwnerCorpus = corpus.Empty()
		_, aud, aerr := auditorFor(ctx, s.Dir, corpus)
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "curate:", aerr)
			return 1
		}
		where = whereFor(aud)
		saidIn = corpus.SaidIn
		if noOwnerCorpus {
			var kept []*fact.Fact
			for _, n := range pending {
				if ownerNoteNeedsHistory(n) {
					deferredOwner++
					continue
				}
				kept = append(kept, n)
			}
			pending = kept
			fmt.Fprintf(os.Stderr, "curate: no owner history; observations can be curated, %d owner note(s) stay pending\n", deferredOwner)
		}
	}
	// A note about a repository no catalog knows yet waits for the machine
	// that has it to push its catalog, instead of being refused (after the
	// owner history is read: a run that cannot read it fails closed first).
	pending, deferredRepo, deferChanged := deferUnknownRepos(pending, env, cst, time.Now())
	if deferredRepo > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d note(s) name a repository no catalog knows yet; each waits up to %d days for one (curator.json \"deferred_repo\")\n", deferredRepo, int(maxRepoDefer.Hours()/24))
	}
	// An agent's note waits for the reviewer's verdict; a wrong one is not
	// curated (its reason is in the ledger, the note stays in the inbox).
	pending, reviewRefused, reviewWaiting, reviewChanged := gateReviewed(pending, reviews, reviewStarted, cst, time.Now())
	if reviewWaiting > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d agent note(s) wait for the reviewer's verdict (up to %d days, curator.json \"review_wait\")\n", reviewWaiting, int(reviewWait.Hours()/24))
	}
	if (deferChanged || reviewChanged) && !*dry {
		saveCuratorState(filepath.Join(stateDir, "curator.json"), cst)
	}
	// Notes that only restate a curated fact or an earlier note go before the
	// split (no model): 23 of the 2,108 pending notes (1.1 percent) on 2026-10-11.
	var curated []*fact.Fact
	for _, f := range facts {
		if !f.InInbox() {
			curated = append(curated, f)
		}
	}
	restated := curate.Restated(pending, curated)
	if len(restated) > 0 {
		kept := pending[:0]
		for _, f := range pending {
			if by, ok := restated[f.Meta.Name]; ok {
				cst.Restated[f.Meta.Name] = by
				if *dry {
					fmt.Printf("- restated, not split: %s (restates [[%s]])\n", f.Meta.Name, by)
				}
				continue
			}
			kept = append(kept, f)
		}
		pending = kept
	}
	// A fact of the split that only restates a curated fact (or one an earlier
	// batch of this run wrote) is not written a second time (no model).
	absorber := curate.NewAbsorber(curated)
	rep := curate.Report{At: time.Now().Format(time.RFC3339), Notes: len(pending), Restated: len(restated), DeferredOwner: deferredOwner, DeferredRepo: deferredRepo, Quick: *quick, ReviewRefused: reviewRefused, NotesForReviewer: reviewWaiting}
	rep.ModelCalls = &modelrun.Trace{}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	batches := curate.Batches(pending)
	if len(batches) > *maxBatches {
		batches = batches[:*maxBatches]
	}
	rep.Batches = len(batches)
	if len(batches) == 0 {
		fmt.Println("curate: nothing pending")
	}
	if len(batches) > 0 {
		// An index that cannot be opened at all stops the run before any
		// model call (the check itself opens it again after each split).
		head, herr := s.Head(ctx)
		if herr == nil {
			var ix conflictIndex
			if ix, herr = openConflictIndex(ctx, s.Dir, head); herr == nil {
				ix.Close()
			}
		}
		if herr != nil {
			fmt.Fprintln(os.Stderr, "curate: index:", herr)
			return 2
		}
	}

	rep.SetAside = heldBack
	if heldBack > 0 && !*quick {
		fmt.Fprintf(os.Stderr, "curate: %d note(s) set aside after failing alone in %d runs (curator.json \"failed\"; --retry-set-aside tries them again)\n", heldBack, maxNoteFailures)
	}
	if cst.Failed == nil {
		cst.Failed = map[string]int{}
	}
	// A batch the model answers with invalid output is split in two and both
	// halves go to the front of the queue, so one note it cannot split no
	// longer keeps the rest of its batch pending in every run. A note that
	// fails alone stays pending; it counts against the note (cst.Failed) only
	// when another batch of the run got a valid answer, since a model or
	// runner that fails every batch is not the notes' fault. For the same
	// reason splitting stops after systemicSplits splits with no valid answer
	// yet: a broken model would otherwise cost a full bisection of every
	// batch.
	queue := batches
	bi := -1
	valid := 0               // batches whose answer was valid this run
	var failedAlone []string // notes that failed alone this run, counted at the end
	retry := func(batch []*fact.Fact, err error, counts bool) bool {
		if !errors.Is(err, modelrun.ErrInvalidResult) {
			return false
		}
		if len(batch) == 1 {
			if counts {
				failedAlone = append(failedAlone, batch[0].Meta.Name)
			}
			return false
		}
		// A model that fails every half fails the run, not a note; and a
		// partly failing one may not bisect every batch either.
		if valid == 0 && rep.BatchSplits >= systemicSplits || rep.BatchSplits >= systemicSplits*max(1, *maxBatches) {
			return false
		}
		h := len(batch) / 2
		queue = append([][]*fact.Fact{batch[:h], batch[h:]}, queue...)
		rep.BatchSplits++
		fmt.Fprintf(os.Stderr, "curate: %v; splitting the batch of %d note(s) in two\n", err, len(batch))
		return true
	}
	for len(queue) > 0 {
		batch := queue[0]
		queue = queue[1:]
		bi++
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "curate: %v, %d batch(es) wait for the next run\n", err, len(queue)+1)
			break
		}
		if time.Now().After(batchesUntil) {
			fmt.Fprintf(os.Stderr, "curate: budget %s spent, %d batch(es) wait for the next run\n", *budget, len(queue)+1)
			break
		}
		// The model's metadata is checked by code (internal/validate); a refused
		// answer goes back to the model once with the problems.
		check := entryCheck(env, host, batch, noteByName, where, noOwnerCorpus)
		entries, err := splitChecked(ctx, &rep, "split", curate.RunCLI, *model, splitPrompt(env, host, batch), check)
		if err != nil {
			if errors.Is(err, modelrun.ErrInvalidResult) {
				logInvalid(&rep, "split", err)
				if retry(batch, err, true) {
					continue
				}
			}
			rep.Errors++
			rep.FailedBatches++
			fmt.Fprintln(os.Stderr, "curate:", err)
			continue
		}
		// Second pass: rulings the split may have folded away.
		var rulings []string
		for _, e := range entries {
			if ok, _ := curate.Gate(e); !ok {
				// Judged not to matter: covered, not a ruling to recover.
				rulings = append(rulings, "- ("+e.Origin+") "+e.Description+" [dropped]")
			} else if e.Kind == "user" {
				rulings = append(rulings, "- ("+e.Origin+") "+e.Description)
			}
		}
		if len(rulings) == 0 {
			rulings = []string{"(none)"}
		}
		recovered := 0
		// Recovery needs owner evidence. Ordinary observations do not require
		// a second model call on a machine without owner history.
		recoverRulings := !noOwnerCorpus
		for _, n := range batch {
			recoverRulings = recoverRulings || fact.OwnerWrote(n)
		}
		if recoverRulings {
			// Their entries are kind user (set below): checked as such.
			rcheck := func(es []curate.Entry) []curate.Problem {
				us := make([]curate.Entry, len(es))
				for k, e := range es {
					e.Kind = "user"
					us[k] = e
				}
				return check(us)
			}
			recoveredEntries, rerr := splitChecked(ctx, &rep, "rulings", curate.JudgeCLI, curate.JudgeModel, fmt.Sprintf(curate.RulingsPrompt, strings.Join(rulings, "\n"), curate.Render(batch)), rcheck)
			if rerr != nil && errors.Is(rerr, modelrun.ErrInvalidResult) {
				// Only a malformed or refused answer is split: a runner error
				// says nothing about the notes.
				logInvalid(&rep, "rulings", rerr)
				if retry(batch, rerr, true) {
					continue
				}
			}
			if rerr != nil {
				rep.Errors++
				rep.FailedBatches++
				fmt.Fprintln(os.Stderr, "curate: rulings:", rerr)
				continue
			}
			for _, e := range recoveredEntries {
				e.Kind = "user"
				entries = append(entries, e)
				recovered++
			}
		}
		batchNames := maps.Clone(names)
		var newFacts []*fact.Fact
		var newRulings []bool
		inBatch := curate.NewAbsorber(nil) // the batch's own facts, for a second entry restating a first
		absorbed := map[string]string{}    // note -> the fact one of its entries restates
		nAbsorbed := 0
		var ledgerLines []byte
		var proposals []byte // review/folders.jsonl lines of the batch's area and topic proposals
		produced := map[string]bool{}
		nRulings, notWords := 0, 0
		invalidBatch := false
		unbackedRuling := false // a kind user entry with no owner history on this machine
		batchOrigins := map[string]bool{}
		for _, n := range batch {
			batchOrigins[n.Meta.Name] = true
		}
		for _, e := range entries {
			if !batchOrigins[e.Origin] {
				invalidBatch = true
				break
			}
			// Not every sentence of the owner's is a statement (SPEC.md,
			// "Relevance"): a remark that does not matter to future work is
			// not curated; its note keeps it, the report says why.
			if ok, d := curate.Gate(e); !ok {
				rep.OwnerDropped = append(rep.OwnerDropped, d)
				if *dry {
					fmt.Printf("- dropped, the owner's remark in %s: %q: %s %s\n", d.Note, d.Words, d.Category, d.Reason)
				}
				continue
			}
			f := curate.ToFact(e, host, batchNames)
			if env.RedirectFact(f) { // as the check saw it (entryCheck)
				rep.Redirected++
			}
			env.RepoPaths(&f.Meta)
			stampReview(f, reviews, reviewsFM)
			// A ruling is the owner's own words: kind user stays only with their
			// quote found in the note and in their turns or notes. A statement
			// (their words said once) is kept verbatim on the same evidence.
			var q ledger.Quote
			ruling, said := false, false
			if e.Kind == curate.KindStatement {
				q, said = curate.ApplyStatement(f, e, noteByName[e.Origin], where, statementsOK)
				if !said && noOwnerCorpus {
					invalidBatch, unbackedRuling = true, true
					break
				}
				if !said {
					notWords++
				}
			} else {
				q, ruling = curate.ApplyEvidence(f, e, noteByName[e.Origin], where)
				// A ruling resting on words said in one session is their
				// statement, never a rule (SPEC.md, "One-offs").
				if ruling && saidIn != nil {
					if wq, ok := curate.OneOff(f, e, q, saidIn(q.Quote), statementsOK); ok {
						q, ruling, said = wq, false, true
						rep.OneOffs++
					}
				}
			}
			if !ruling && !said && e.Kind == "user" {
				if noOwnerCorpus {
					invalidBatch, unbackedRuling = true, true
					break
				}
				notWords++
			}
			// The same fact again, in other words: the note is absorbed by the
			// fact it restates, and no second copy is written. A ruling is only
			// absorbed by a ruling with the same quote (curate.Absorber).
			by := absorber.Restates(f, ruling)
			if by == "" {
				by = inBatch.Restates(f, ruling)
			}
			if by != "" {
				if absorbed[e.Origin] == "" {
					absorbed[e.Origin] = by
				}
				nAbsorbed++
				if *dry {
					fmt.Printf("- absorbed: [%s] %s (restates [[%s]])\n", f.Meta.Scope, f.Meta.Description, by)
				}
				continue
			}
			if ruling {
				nRulings++
				ledgerLines = append(ledgerLines, q.Line()...)
			}
			if said {
				rep.Statements++
				ledgerLines = append(ledgerLines, q.Line()...)
			}
			inBatch.Add(f, ruling)
			newFacts = append(newFacts, f)
			newRulings = append(newRulings, ruling)
			produced[e.Origin] = true
			proposals = append(proposals, proposalLines(env, e, f.Meta.Name, noteByName[e.Origin])...)
		}

		if invalidBatch {
			// An unbacked ruling is invalid only because this machine has no
			// owner history: split to isolate it, but never count it against the
			// note, which curates once the history is here.
			if retry(batch, fmt.Errorf("%w: invalid origin or unbacked owner ruling", modelrun.ErrInvalidResult), !unbackedRuling) {
				continue
			}
			rep.Errors++
			rep.FailedBatches++
			fmt.Fprintln(os.Stderr, "curate: invalid origin or unbacked owner ruling; batch stays pending")
			continue
		}

		valid++
		for _, n := range batch {
			delete(cst.Failed, n.Meta.Name) // a note the model answered is no longer failing
		}

		// Conflict pairs: closest existing fact per new fact, and new facts of
		// the same scope that share most of their terms. The index is opened
		// here, after the split, at the HEAD the batch will be written on.
		pairs, refs, err := conflictPairs(ctx, s, newFacts, host, true, false, nil)
		if err != nil {
			// A fact written without its conflict check is what the check is
			// for: the batch is not written, its notes stay pending, and the
			// run stops here and exits non-zero.
			rep.Errors++
			rep.FailedBatches++
			fmt.Fprintf(os.Stderr, "curate: conflict check failed, batch %d not written and its %d note(s) stay pending: %v\n", bi+1, len(batch), err)
			break
		}
		rep.Checked += len(pairs)
		unchecked := rep.JudgeUnchecked
		review, written, err := judgePairs(ctx, s, pairs, refs, newFacts, *dry, &rep)
		if rep.JudgeUnchecked > unchecked {
			// Written with a review line each, but the run must not look clean.
			rep.Errors++
		}
		if err != nil {
			rep.Errors++
			rep.FailedBatches++
			fmt.Fprintln(os.Stderr, "curate: conflict judge; batch stays pending:", err)
			continue
		}
		rep.Facts += len(newFacts)
		rep.Absorbed += nAbsorbed
		rep.Rulings += nRulings
		rep.NotOwnerWords += notWords
		rep.RecoveredRulings += recovered

		if len(newFacts) == 0 {
			if !*dry {
				for _, n := range batch {
					markDone(cst, n.Meta.Name, absorbed)
				}
			}
			continue
		}
		if *dry {
			for _, f := range newFacts {
				fmt.Printf("- [%s/%s/%s] %s\n", f.Meta.Scope, f.Meta.Type, f.Meta.Source.Kind, f.Meta.Description)
			}
			for _, r := range review {
				fmt.Println(r)
			}
			continue
		}
		for _, f := range newFacts {
			b, err := f.Encode()
			if err != nil {
				rep.Errors++
				continue
			}
			full := filepath.Join(s.Dir, f.Path)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, b, 0o644); err != nil {
				rep.Errors++
				continue
			}
			written = append(written, f.Path)
		}
		if len(ledgerLines) > 0 {
			full := filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath))
			os.MkdirAll(filepath.Dir(full), 0o755)
			if fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fh.Write(ledgerLines)
				fh.Close()
				written = append(written, ledger.QuotesPath)
			} else {
				rep.Errors++
			}
		}
		if len(proposals) > 0 {
			full := filepath.Join(s.Dir, filepath.FromSlash(ledger.FoldersPath))
			os.MkdirAll(filepath.Dir(full), 0o755)
			if fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fh.Write(proposals)
				fh.Close()
				written = append(written, ledger.FoldersPath)
			} else {
				rep.Errors++
			}
		}
		if len(review) > 0 {
			rel := "review/" + time.Now().Format("2006-01-02") + ".md"
			full := filepath.Join(s.Dir, rel)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintf(fh, "## curator run %s, batch %d\n\n%s\n\n", rep.At, bi+1, strings.Join(review, "\n"))
				fh.Close()
				written = append(written, rel)
			}
		}
		all, _, _ := fact.Walk(s.Dir)
		if rels, err := regenIndexes(ctx, s.Dir, all, nil); err != nil {
			rep.Errors++
			fmt.Fprintln(os.Stderr, "curate: index:", err)
		} else {
			written = append(written, rels...)
		}
		if findings, err := lint.Run(s.Dir); err == nil && len(findings) > 0 {
			fmt.Fprintf(os.Stderr, "curate: lint: %d finding(s) remain (first: %s)\n", len(findings), findings[0])
		}
		msg := fmt.Sprintf("curator: %d fact(s) from %d note(s), %d contradiction(s), %d supersede(s)", len(newFacts), len(batch), rep.Contradicts, rep.Supersedes)
		if err := s.CommitPush(ctx, dedupe(written), msg, "curator"); err != nil {
			rep.Errors++
			var rej *store.Rejected
			if errors.As(err, &rej) {
				fmt.Fprintln(os.Stderr, "curate: server refused the batch; files set aside:", rej.Files)
			} else {
				fmt.Fprintln(os.Stderr, "curate: push:", err)
			}
			break
		}
		for _, n := range batch {
			if !produced[n.Meta.Name] {
				markDone(cst, n.Meta.Name, absorbed)
			}
		}
		fresh := map[string]bool{}
		for i, f := range newFacts {
			absorber.Add(f, newRulings[i])
			fresh[f.Path] = true
		}
		for _, rel := range written {
			if !fresh[rel] {
				absorber.Drop(rel) // a fact this batch superseded
			}
		}
		names = batchNames
		fmt.Printf("curate: batch %d pushed: %d fact(s) from %d note(s)\n", bi+1, len(newFacts), len(batch))
	}

	if valid > 0 && !*quick {
		// Only the full run counts a strike: the quick one comes every 10
		// minutes and would set a note aside within the hour.
		for _, n := range failedAlone {
			cst.Failed[n]++
		}
	} else if valid == 0 && (len(failedAlone) > 0 || rep.BatchSplits >= systemicSplits) {
		fmt.Fprintf(os.Stderr, "curate: no batch got a valid answer; nothing counted against the notes (check the model and runner)\n")
	}

	if !*quick {
		// The owner's statements: promotions by evidence, before the folders
		// read the decisions (statements.go).
		rep.Errors += curateStatementsStep(ctx, s, *model, *dry, &rep)
		// The folders: records for repositories and project folders, decided
		// by the model on code's evidence (assess.go), before derive.
		rep.Errors += curateAssessStep(ctx, s, *model, *dry, &rep)
		// The suppressed folders' facts move to their winners (migrate.go).
		rep.Errors += curateMigrateStep(ctx, s, *dry)
		// The folders' cited overviews, on a material change (overview.go).
		rep.Errors += curateOverviewStep(ctx, s, *model, *dry, &rep)
		curateUpkeep(ctx, s, host, *dry, &rep)
		if !*dry {
			// The folder steps change what the hooks read (the records, the
			// quiet marks, an area's tools), and the hooks read the closest
			// index while the exact one is missing: build it now, before the
			// staleness step, not in the first session that asks.
			if ix, err := openIndex(ctx, s); err == nil {
				ix.Close()
			}
		}
	} else if !*dry && rep.Facts > 0 {
		// The hooks read the index at the new HEAD: build it now, not in the
		// first session that asks.
		if ix, err := openIndex(ctx, s); err == nil {
			ix.Close()
		}
	}
	if len(batches) == 0 {
		if !*quick {
			rep.Errors += curateStaleStep(ctx, s, *dry)
		}
		if rep.Errors > 0 || rep.DeferredOwner > 0 && !*quick {
			return 1
		}
		return 0
	}

	if !*dry {
		saveCuratorState(filepath.Join(stateDir, "curator.json"), cst)
		if f, err := os.OpenFile(filepath.Join(stateDir, "curator.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			json.NewEncoder(f).Encode(rep)
			f.Close()
		}
	}
	// The staleness step (stale.go) runs last, after this run's state is saved:
	// it can take most of an hour and must not be what a kill loses. It flags
	// curated facts git has overtaken; it never retires.
	if !*quick {
		rep.Errors += curateStaleStep(ctx, s, *dry)
	}
	fmt.Printf("curate: %d notes in %d batches (%d split) -> %d facts (%d rulings, %d absorbed), %d pairs in %d judge calls, %d contradictions, %d supersedes, %d unchecked, %d set aside, %s, %d error(s)\n", rep.Notes, rep.Batches, rep.BatchSplits, rep.Facts, rep.Rulings, rep.Absorbed, rep.Checked, rep.JudgeCalls, rep.Contradicts, rep.Supersedes, rep.JudgeUnchecked, rep.SetAside, rep.ModelCalls.CostSummary(rep.CostUSD), rep.Errors)
	if rep.Errors > 0 || rep.DeferredOwner > 0 && !*quick {
		return 1
	}
	return 0
}

// logInvalid writes why a split or rulings answer failed validation, one
// line per entry and field, to stderr and to the run's report (curator.log):
// the field and the rule, never the entry's text.
func logInvalid(rep *curate.Report, pass string, err error) {
	for _, p := range curate.Problems(err) {
		line := pass + ": " + p.String()
		fmt.Fprintln(os.Stderr, "curate: invalid answer:", line)
		rep.InvalidEntries = append(rep.InvalidEntries, line)
	}
}

// curateLockWait bounds how long the full curate waits for a quick one that
// holds curator.lock (a quick run ends within quickBudget and its tail).
var curateLockWait = time.Hour

// The quick curate's limits (--quick): the notes of the last day, newest
// first, at most two batches (about 170 dreamer notes; the dreamer adds about
// 12 a half hour on the author's workstation) started within 20 minutes.
// Older pending notes are the full run's.
const (
	quickWindow  = 24 * time.Hour
	quickBatches = 2
	quickBudget  = 20 * time.Minute
)

// recentNotes keeps the pending notes a commit added to the clone since
// since, newest first. A note no commit added (still uncommitted) is not
// taken: it has not reached the server yet.
func recentNotes(ctx context.Context, dir string, pending []*fact.Fact, since time.Time) []*fact.Fact {
	log, err := store.Git(ctx, dir, "log", "--diff-filter=A", "--since="+since.Format(time.RFC3339), "--format=%x01%cI", "--name-only", "--", "inbox/")
	if err != nil {
		return nil
	}
	added := map[string]string{}
	at := ""
	for _, l := range strings.Split(log, "\n") {
		if t, ok := strings.CutPrefix(l, "\x01"); ok {
			at = t
			continue
		}
		if l = strings.TrimSpace(l); l != "" && added[l] == "" {
			added[l] = at // newest first: the first is the latest add
		}
	}
	var out []*fact.Fact
	for _, n := range pending {
		if added[n.Path] != "" {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return added[out[i].Path] > added[out[j].Path] })
	return out
}

// lastQuickRun reads when the last quick curate started (curate-quick.json).
func lastQuickRun(path string) (time.Time, bool) {
	var st struct {
		LastRun time.Time `json:"last_run"`
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil || st.LastRun.IsZero() {
		return time.Time{}, false
	}
	return st.LastRun, true
}

func saveQuickRun(path string, at time.Time) {
	b, _ := json.Marshal(map[string]string{"last_run": at.Format(time.RFC3339Nano)})
	os.WriteFile(path, append(b, '\n'), 0o600)
}

// curateStaleStep is the staleness step; tests substitute it.
var curateStaleStep = curateStale

// curateUpkeep is the full curate's no-model steps after the batches: derive,
// results, disowned, links. Tests substitute it.
var curateUpkeep = func(ctx context.Context, s *store.Store, host string, dry bool, rep *curate.Report) {
	// The derived-facts step needs no inbox notes and no model: hot spots,
	// co-change and module maps from git, as curator. It runs even when
	// nothing was pending.
	var drep *derive.Report
	dopts, derr := deriveOptions(s.Dir, host)
	if derr == nil {
		dopts.Projects = defaultProjects()
		drep, derr = deriveStep(ctx, s, dopts, dry)
	}
	derr = errors.Join(derr, curateResults(ctx, s, dopts, dry)) // results from git: result facts and the saved facts they replace
	printDerive(drep, false)
	if drep != nil {
		for _, r := range drep.Repos {
			if r.Status != derive.Failed {
				rep.DerivedFiles += len(r.Paths)
			}
		}
		if drep.Count(derive.Failed) > 0 {
			rep.Errors++
		}
	}
	if derr != nil {
		rep.Errors++
		fmt.Fprintln(os.Stderr, "curate: derive:", derr)
	}
	// Facts the owner's notes say are not their words lose kind user. No model.
	rep.Errors += curateDisowned(ctx, s, dry)
	// Dead [[links]] in curated facts: rewritten to a clear match or made plain
	// text; owner rulings get a review line instead (links.go). No model.
	rep.Errors += curateLinks(ctx, s, dry)
}

// markDone records a note the split wrote no fact for: absorbed, when its
// entries only restated curated facts (note -> the fact, like a restated
// note), else empty.
func markDone(cst *curatorState, note string, absorbed map[string]string) {
	if by := absorbed[note]; by != "" {
		cst.Restated[note] = by
		return
	}
	cst.Empty[note] = time.Now().Format("2006-01-02")
}

// supersedeRoute is what the curator does with a judge's "supersedes" on an
// existing fact: retire it (retire true; the caller writes superseded_by), or
// leave a review line for the owner. Never retire on a judge's word an owner
// ruling (RULING CHANGE?) or a fact that holds words of theirs (supersedes?,
// owner decides). Otherwise retire only when the two facts clearly cover the
// same thing: same origin note, or most of their terms shared. The first live
// run retired a broad fact (several repos) with a narrower one about one of
// them; a judge sees "same subject" too easily.
func supersedeRoute(storeDir string, nf *fact.Fact, ex *search.Hit) (line string, retire bool) {
	return supersedeRouteWith(nf, ex, func(rel, origin string) bool { return sameOrigin(storeDir, rel, origin) })
}

// supersedeRouteWith is supersedeRoute with the same-origin test given: the
// local curator reads the existing fact from the overlay, not the clone.
func supersedeRouteWith(nf *fact.Fact, ex *search.Hit, sameOrigin func(rel, origin string) bool) (line string, retire bool) {
	if ex.Ruling {
		return fmt.Sprintf("- RULING CHANGE? new [[%s]] may supersede owner ruling [[%s]]; owner decides\n  new: %s\n  ruling: %s", nf.Meta.Name, ex.Name, nf.Meta.Description, ex.Description), false
	}
	if ex.Words || nf.Meta.Origin == "" || ex.Name == "" || (overlap(nf.Meta.Description, ex.Description) < 0.6 && !sameOrigin(ex.Path, nf.Meta.Origin)) {
		return fmt.Sprintf("- supersedes? [[%s]] may replace [[%s]]; owner decides\n  new: %s\n  old: %s", nf.Meta.Name, ex.Name, nf.Meta.Description, ex.Description), false
	}
	return "", true
}

// sameOrigin reports whether the existing fact at rel came from the same
// inbox note as origin.
func sameOrigin(storeDir, rel, origin string) bool {
	b, err := os.ReadFile(filepath.Join(storeDir, rel))
	if err != nil {
		return false
	}
	f, err := fact.Parse(rel, b)
	return err == nil && f.Meta.Origin != "" && f.Meta.Origin == origin
}

// overlap is the Jaccard overlap of two descriptions' search terms.
func overlap(a, b string) float64 {
	ta, tb := search.Terms(a), search.Terms(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, t := range ta {
		set[t] = true
	}
	inter := 0
	for _, t := range tb {
		if set[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func loadCuratorState(path string) *curatorState {
	st := &curatorState{Empty: map[string]string{}, Restated: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, st)
		if st.Empty == nil {
			st.Empty = map[string]string{}
		}
		if st.Restated == nil {
			st.Restated = map[string]string{}
		}
	}
	return st
}

func saveCuratorState(path string, st *curatorState) {
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.MarshalIndent(st, "", " ")
	os.WriteFile(path, b, 0o600)
}

// markSuperseded adds superseded_by to an existing curated fact file.
func markSuperseded(storeDir, rel, by string) (string, bool) {
	full := filepath.Join(storeDir, rel)
	b, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	f, err := fact.Parse(rel, b)
	if err != nil || f.Meta.SupersededBy != "" {
		return "", false
	}
	f.Meta.SupersededBy = by
	f.Meta.Updated = time.Now().Format("2006-01-02")
	out, err := f.Encode()
	if err != nil {
		return "", false
	}
	return rel, os.WriteFile(full, out, 0o644) == nil
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Claims of owner authority need transcripts unless the owner wrote the note
// through the trusted app path. Observations remain usable on fresh installs.
func ownerNoteNeedsHistory(n *fact.Fact) bool {
	return !fact.OwnerWrote(n) && (n.Meta.Source.Kind == "user" || fact.ClaimsEvidence(n))
}
