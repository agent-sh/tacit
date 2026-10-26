package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The local layer (SPEC.md, "Local layer"): on a read-only clone with
// local_layer on, the dreamer and the curator work on this machine's own
// sessions and write only to its overlay (internal/overlay), which never
// leaves the machine. They read the clone (its facts, its HEAD for the
// index) and never write it: no file, no commit, no sync, no push.

// localLayer reports a clone whose dreamer and curator work on this
// machine's overlay: read-only, with the local layer on.
func localLayer(ctx context.Context, s *store.Store) bool {
	return overlay.LayerOn() && s.ReadOnly(ctx)
}

// canDream reports whether a dream started here has somewhere to write: the
// store as the dreamer, or the local layer (an overlay outside the clone).
// The hooks ask it before they start a session dream.
func canDream(ctx context.Context, s *store.Store) bool {
	if writerCheck(ctx, s, "dreamer") == nil {
		return true
	}
	return localLayer(ctx, s) && overlay.Outside(overlay.Dir(), s.Dir)
}

// persistLocalDreamWindow writes a window's notes to this machine's overlay,
// inbox/dreamer@<host>/<name>.md, redacted like an agent's overlay note
// (fail closed: no scrump, nothing written) and atomically. A note already
// there is not written again. It returns the Prefix paths it wrote. catalog,
// when not nil and a note was written, is this machine's repository catalog
// (repoid.CatalogFile), written beside the notes in the overlay when it
// changed: the local layer's checks read it, and it never reaches the store.
func persistLocalDreamWindow(s *store.Store, facts []*fact.Fact, catalog []byte) ([]string, error) {
	dir := overlay.Dir()
	if !overlay.Outside(dir, s.Dir) {
		return nil, overlay.ErrInside
	}
	host := store.ShortHost()
	var written []string
	for _, f := range facts {
		red, err := digest.RedactAll([]string{f.Meta.Description, f.Body})
		if err != nil {
			return written, fmt.Errorf("redaction unavailable, notes not written: %w", err)
		}
		if red[0] != f.Meta.Description {
			f.Meta.Description = red[0]
			f.Meta.Name = importer.Slug(red[0], 60)
		}
		f.Body = red[1]
		rel := filepath.ToSlash(filepath.Join(overlay.InboxDir, "dreamer@"+host, f.Meta.Name+".md"))
		f.Path = overlay.Prefix + rel
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			continue // an earlier window wrote it
		} else if !os.IsNotExist(err) {
			return written, err
		}
		b, err := f.Encode()
		if err != nil {
			return written, err
		}
		if err := overlay.Write(dir, s.Dir, rel, b); err != nil {
			return written, err
		}
		written = append(written, f.Path)
	}
	if len(written) > 0 && catalog != nil {
		rel := filepath.ToSlash(filepath.Join(overlay.InboxDir, "dreamer@"+host, repoid.CatalogFile))
		if old, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err != nil || !bytes.Equal(old, catalog) {
			if err := overlay.Write(dir, s.Dir, rel, catalog); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// localCurate is what runCurate passes to runCurateLocal.
type localCurate struct {
	model         string
	maxBatches    int
	budget        time.Duration
	dry, quick    bool
	minInterval   time.Duration
	retrySetAside bool
}

// localCuratorState is the local curator's own state file: its notes are
// this machine's, never the store's, so their names never mix.
const localCuratorState = "curator-local.json"

// runCurateLocal is `tacit curate` on a clone with the local layer: the
// pending notes of the overlay's inbox (inbox/dreamer@<host>/) through the
// store curator's split, rulings pass, rulings evidence check (against the
// owner's turns on this machine only), dedupe and conflict judge, into
// curated facts under the overlay's curated/<scope>/. The checks see the
// store's facts and the overlay's together; a verdict that would change a
// store fact is a line in the overlay's review file instead. No whole-store
// step runs (derive, results, disowned, links, staleness), nothing is
// synced, and nothing is written to the clone.
func runCurateLocal(s *store.Store, o localCurate) int {
	dir := overlay.Dir()
	if !overlay.Outside(dir, s.Dir) {
		if o.quick {
			return 0
		}
		fmt.Fprintln(os.Stderr, "curate: nothing done:", overlay.ErrInside)
		return 2
	}
	host := store.ShortHost()
	tail := 30 * time.Minute // the last batch's judge; no step follows
	if o.quick {
		tail = 10 * time.Minute
	}
	home, _ := os.UserHomeDir()
	stateDir := platform.StateDir(home)
	if !o.dry {
		var release func()
		var err error
		if o.quick {
			release, err = tryJobLock(stateDir, "curator")
			if errors.Is(err, platform.ErrBusy) {
				fmt.Println("curate: busy (another curate holds " + filepath.Join(stateDir, "curator.lock") + "); nothing done")
				return 0
			}
		} else {
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
		if o.quick {
			qpath := filepath.Join(stateDir, "curate-quick.json")
			if last, ok := lastQuickRun(qpath); ok && time.Since(last) < o.minInterval {
				fmt.Printf("curate: quick run %s ago, within --min-interval %s; nothing done\n", time.Since(last).Round(time.Second), o.minInterval)
				return 0
			}
			saveQuickRun(qpath, time.Now())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.budget+tail)
	defer cancel()
	batchesUntil := time.Now().Add(o.budget)
	statePath := filepath.Join(stateDir, localCuratorState)
	cst := loadCuratorState(statePath)
	if o.retrySetAside {
		cst.Failed = map[string]int{}
	}

	// The store's facts as the clone holds them (the follower keeps it
	// current; this run does not sync), and the overlay's.
	shared, perrs, err := fact.Walk(s.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate:", err)
		return 2
	}
	if len(perrs) > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d unparsable fact file(s) in the clone skipped\n", len(perrs))
	}
	local, lerrs := overlay.Load(dir)
	if len(lerrs) > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d local file(s) in %s did not parse and are skipped\n", len(lerrs), dir)
	}
	// The reviewer judges this machine's agents' notes (SPEC.md, "The
	// reviewer"), into the overlay's ledger: nothing leaves the machine.
	localReview(ctx, s, dir, local, o.quick, o.dry, time.Now().Add(o.budget/3))
	names := map[string]bool{}
	var known []*fact.Fact // what a new fact is checked against: the store's curated facts and the overlay's
	for _, f := range shared {
		names[f.Meta.Name] = true
		if !f.InInbox() {
			known = append(known, f)
		}
	}
	curatedOrigins := map[string]bool{}
	var notes []*fact.Fact
	for _, f := range local {
		names[f.Meta.Name] = true
		switch {
		case overlay.IsInbox(f.Path):
			// Only this machine's dreamer writes the local inbox: any other file
			// there (a note claiming harness human, another host's) is not
			// curated, so nothing but a dreamed note can reach the split.
			if strings.HasPrefix(f.Path, overlay.Prefix+overlay.InboxDir+"/dreamer@"+host+"/") && f.Meta.Source.Harness == "dreamer" && f.Meta.Source.Host == host {
				notes = append(notes, f)
			}
		case overlay.IsCurated(f.Path):
			if f.Meta.Origin != "" {
				curatedOrigins[f.Meta.Origin] = true
			}
			known = append(known, f)
		default:
			known = append(known, f) // an agent's note on this machine (tacit add)
		}
	}
	var pending []*fact.Fact
	heldBack := 0
	env := curateEnv(s.Dir, true) // the metadata checks: this machine, the store and the layer's folders
	noteByName := map[string]*fact.Fact{}
	for _, f := range notes {
		noteByName[f.Meta.Name] = f
		if curatedOrigins[f.Meta.Name] || cst.Empty[f.Meta.Name] != "" || cst.Restated[f.Meta.Name] != "" {
			continue
		}
		if cst.Failed[f.Meta.Name] >= maxNoteFailures {
			heldBack++
			continue
		}
		pending = append(pending, f)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Path < pending[j].Path })
	if o.quick {
		pending = recentLocalNotes(dir, pending, time.Now().Add(-quickWindow))
	}
	var where curate.Where
	var saidIn func(quote string) []string // the sessions the owner said a quote in (one-offs)
	noOwnerCorpus := false
	deferredOwner := 0
	if len(pending) > 0 {
		// The owner's words on this machine only: its transcripts, prompt
		// histories, pi and Hermes sessions. The store's notes are not read,
		// so a local ruling rests on what the owner said here.
		corpus, cerr := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
		if cerr != nil && !errors.Is(cerr, errNoOwnerWords) {
			fmt.Fprintln(os.Stderr, "curate: owner history:", cerr)
			return 1
		}
		noOwnerCorpus = corpus.Empty()
		where = whereFor(newRulingAuditor(nil, nil, nil, nil, corpus))
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
	pending, deferredRepo, deferChanged := deferUnknownRepos(pending, env, cst, time.Now())
	if deferredRepo > 0 {
		fmt.Fprintf(os.Stderr, "curate: %d note(s) name a repository no catalog knows yet; each waits up to %d days for one (%s \"deferred_repo\")\n", deferredRepo, int(maxRepoDefer.Hours()/24), localCuratorState)
	}
	if deferChanged && !o.dry {
		saveCuratorState(statePath, cst)
	}
	restated := curate.Restated(pending, known)
	if len(restated) > 0 {
		kept := pending[:0]
		for _, f := range pending {
			if by, ok := restated[f.Meta.Name]; ok {
				cst.Restated[f.Meta.Name] = by
				if o.dry {
					fmt.Printf("- restated, not split: %s (restates [[%s]])\n", f.Meta.Name, by)
				}
				continue
			}
			kept = append(kept, f)
		}
		pending = kept
	}
	absorber := curate.NewAbsorber(known)
	rep := curate.Report{At: time.Now().Format(time.RFC3339), Notes: len(pending), Restated: len(restated), DeferredOwner: deferredOwner, DeferredRepo: deferredRepo, Quick: o.quick, Local: true}
	rep.ModelCalls = &modelrun.Trace{}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	batches := curate.Batches(pending)
	if len(batches) > o.maxBatches {
		batches = batches[:o.maxBatches]
	}
	rep.Batches = len(batches)
	rep.SetAside = heldBack
	if len(batches) == 0 {
		fmt.Println("curate: local layer: nothing pending")
		if !o.dry && len(restated) > 0 {
			saveCuratorState(statePath, cst)
		}
		if rep.DeferredOwner > 0 && !o.quick {
			return 1
		}
		return 0
	}
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
	if heldBack > 0 && !o.quick {
		fmt.Fprintf(os.Stderr, "curate: %d note(s) set aside after failing alone in %d runs (%s \"failed\"; --retry-set-aside tries them again)\n", heldBack, maxNoteFailures, localCuratorState)
	}
	if cst.Failed == nil {
		cst.Failed = map[string]int{}
	}
	queue := batches
	bi := -1
	valid := 0
	var failedAlone []string
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
		if valid == 0 && rep.BatchSplits >= systemicSplits || rep.BatchSplits >= systemicSplits*max(1, o.maxBatches) {
			return false
		}
		h := len(batch) / 2
		queue = append([][]*fact.Fact{batch[:h], batch[h:]}, queue...)
		rep.BatchSplits++
		fmt.Fprintf(os.Stderr, "curate: %v; splitting the batch of %d note(s) in two\n", err, len(batch))
		return true
	}
	wrote := 0
	for len(queue) > 0 {
		batch := queue[0]
		queue = queue[1:]
		bi++
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "curate: %v, %d batch(es) wait for the next run\n", err, len(queue)+1)
			break
		}
		if time.Now().After(batchesUntil) {
			fmt.Fprintf(os.Stderr, "curate: budget %s spent, %d batch(es) wait for the next run\n", o.budget, len(queue)+1)
			break
		}
		// The model's metadata is checked by code (internal/validate); a refused
		// answer goes back to the model once with the problems.
		check := entryCheck(env, host, batch, noteByName, where, noOwnerCorpus)
		entries, err := splitChecked(ctx, &rep, "split", curate.RunCLI, o.model, splitPrompt(env, host, batch), check)
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
		if !noOwnerCorpus {
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
		inBatch := curate.NewAbsorber(nil)
		absorbed := map[string]string{}
		nAbsorbed := 0
		var ledgerLines []byte
		var proposals []byte // the overlay's review/folders.jsonl lines of area and topic proposals
		produced := map[string]bool{}
		nRulings, notWords := 0, 0
		invalidBatch, unbackedRuling := false, false
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
				if o.dry {
					fmt.Printf("- dropped, the owner's remark in %s: %q: %s %s\n", d.Note, d.Words, d.Category, d.Reason)
				}
				continue
			}
			f := curate.ToFact(e, host, batchNames)
			if env.RedirectFact(f) { // as the check saw it (entryCheck)
				rep.Redirected++
			}
			env.RepoPaths(&f.Meta)
			f.Path = overlay.Prefix + overlay.CuratedDir + "/" + f.Meta.Scope + "/" + f.Meta.Name + ".md"
			// The Mac's statements stay in its overlay, whatever the store's
			// format: no other machine reads it.
			var q ledger.Quote
			ruling, said := false, false
			if e.Kind == curate.KindStatement && wordsMasked(e.OwnerQuote) {
				// Words scrump masks are no longer the owner's turn: no
				// statement, as with a ruling (writeLocalBatch).
				f.Meta.Source.Kind = "observed"
				notWords++
			} else if e.Kind == curate.KindStatement {
				q, said = curate.ApplyStatement(f, e, noteByName[e.Origin], where, true)
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
				// (Words scrump masks stay a ruling here, which the batch's
				// write files as observed.)
				if ruling && saidIn != nil && !wordsMasked(q.Quote) {
					if wq, ok := curate.OneOff(f, e, q, saidIn(q.Quote), true); ok {
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
			by := absorber.Restates(f, ruling)
			if by == "" {
				by = inBatch.Restates(f, ruling)
			}
			if by != "" {
				if absorbed[e.Origin] == "" {
					absorbed[e.Origin] = by
				}
				nAbsorbed++
				if o.dry {
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
			delete(cst.Failed, n.Meta.Name)
		}
		// The closest fact of the store or the overlay for each new fact (the
		// index holds both), and the batch's own facts of one scope.
		pairs, refs, err := conflictPairs(ctx, s, newFacts, host, true, true, nil)
		if err != nil {
			rep.Errors++
			rep.FailedBatches++
			fmt.Fprintf(os.Stderr, "curate: conflict check failed, batch %d not written and its %d note(s) stay pending: %v\n", bi+1, len(batch), err)
			break
		}
		rep.Checked += len(pairs)
		review, supersede, err := judgeLocalPairs(ctx, dir, pairs, refs, newFacts, &rep)
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
		if o.dry {
			for _, f := range newFacts {
				fmt.Printf("- [%s/%s/%s] %s\n", f.Meta.Scope, f.Meta.Type, f.Meta.Source.Kind, f.Meta.Description)
			}
			for _, r := range review {
				fmt.Println(r)
			}
			continue
		}
		n, retired, err := writeLocalBatch(dir, s.Dir, newFacts, newRulings, ledgerLines, review, supersede, rep.At, bi+1)
		wrote += n
		if err == nil && len(proposals) > 0 {
			err = overlay.Append(dir, s.Dir, overlay.CuratedDir+"/"+ledger.FoldersPath, proposals)
		}
		if err != nil {
			rep.Errors++
			fmt.Fprintln(os.Stderr, "curate: local write:", err)
			break
		}
		for _, nn := range batch {
			if !produced[nn.Meta.Name] {
				markDone(cst, nn.Meta.Name, absorbed)
			}
		}
		for i, f := range newFacts {
			absorber.Add(f, newRulings[i])
		}
		for _, p := range retired {
			absorber.Drop(p)
		}
		names = batchNames
		fmt.Printf("curate: batch %d written to this machine: %d fact(s) from %d note(s)\n", bi+1, len(newFacts), len(batch))
	}
	if valid > 0 && !o.quick {
		for _, n := range failedAlone {
			cst.Failed[n]++
		}
	} else if valid == 0 && (len(failedAlone) > 0 || rep.BatchSplits >= systemicSplits) {
		fmt.Fprintf(os.Stderr, "curate: no batch got a valid answer; nothing counted against the notes (check the model and runner)\n")
	}
	if !o.dry {
		saveCuratorState(statePath, cst)
		if f, err := os.OpenFile(filepath.Join(stateDir, "curator.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			json.NewEncoder(f).Encode(rep)
			f.Close()
		}
		if wrote > 0 {
			// The hooks read the index with the overlay: build it now.
			if ix, err := openIndex(ctx, s); err == nil {
				ix.Close()
			}
		}
	}
	if !o.quick {
		// The folders of this machine's layer: records decided on its own
		// evidence, and the facts of suppressed folders moved, in the overlay.
		rep.Errors += curateStatementsStep(ctx, s, o.model, o.dry, &rep)
		rep.Errors += curateAssessStep(ctx, s, o.model, o.dry, &rep)
		rep.Errors += curateMigrateStep(ctx, s, o.dry)
		// Overviews of this machine's own folders, and the rotation of its own
		// facts: the layer's daily job runs curate, not maintain.
		rep.Errors += curateOverviewStep(ctx, s, o.model, o.dry, &rep)
		if !o.dry {
			rep.Errors += maintainRotation(ctx, s)
		}
	}
	fmt.Printf("curate: local layer: %d notes in %d batches (%d split) -> %d facts (%d rulings, %d absorbed), %d pairs in %d judge calls, %d contradictions, %d supersedes, %d unchecked, %d set aside, %s, %d error(s)\n", rep.Notes, rep.Batches, rep.BatchSplits, rep.Facts, rep.Rulings, rep.Absorbed, rep.Checked, rep.JudgeCalls, rep.Contradicts, rep.Supersedes, rep.JudgeUnchecked, rep.SetAside, rep.ModelCalls.CostSummary(rep.CostUSD), rep.Errors)
	if rep.Errors > 0 || rep.DeferredOwner > 0 && !o.quick {
		return 1
	}
	return 0
}

// recentLocalNotes keeps the pending local notes written since since (the
// file's mtime: the overlay has no commits), newest first.
func recentLocalNotes(dir string, pending []*fact.Fact, since time.Time) []*fact.Fact {
	at := map[string]time.Time{}
	var out []*fact.Fact
	for _, n := range pending {
		rel, _ := overlay.Rel(n.Path)
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || fi.ModTime().Before(since) {
			continue
		}
		at[n.Path] = fi.ModTime()
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return at[out[i].Path].After(at[out[j].Path]) })
	return out
}

// judgeLocalPairs asks the judge, as the store's curator does, and routes
// its verdicts for the local layer. A contradiction is a review line. A
// supersession retires an existing fact only when the local curator wrote it
// (never a ruling, the same rule as supersedeRoute), and it is
// returned in supersede (overlay path -> the new fact); one of the store's
// facts is never touched: the verdict is a line for the owner.
func judgeLocalPairs(ctx context.Context, dir string, pairs []curate.Pair, refs []judgedPair, facts []*fact.Fact, rep *curate.Report) (review []string, supersede map[string]string, err error) {
	verdicts, unjudged, err := judgeVerdicts(ctx, pairs, rep)
	if err != nil {
		return nil, nil, err
	}
	supersede = map[string]string{}
	sameOrigin := func(rel, origin string) bool {
		r, ok := overlay.Rel(rel)
		if !ok {
			return false
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(r)))
		if err != nil {
			return false
		}
		f, err := fact.Parse(rel, b)
		return err == nil && f.Meta.Origin != "" && f.Meta.Origin == origin
	}
	for k, v := range verdicts {
		pm := refs[k]
		nf := facts[pm.newIdx]
		if unjudged[k] {
			rep.JudgeUnchecked++
			review = append(review, uncheckedLine(nf, pm, facts))
			continue
		}
		shared := pm.existing != nil && !pm.existing.Local
		switch v {
		case curate.Contradicts:
			rep.Contradicts++
			where := ""
			if shared {
				where = " (the store's)"
			}
			review = append(review, contradictionLine(nf, pm, facts, where))
		case curate.Supersedes:
			rep.Supersedes++
			if pm.existing == nil {
				review = append(review, fmt.Sprintf("- duplicate in one run: [[%s]] and [[%s]]", nf.Meta.Name, facts[pm.otherIdx].Meta.Name))
				continue
			}
			if shared || !overlay.IsCurated(pm.existing.Path) {
				// The store's facts change only on the curator's machine, and an
				// agent's note is never edited (as a store inbox note is not).
				whose := "the store's"
				if !shared {
					whose = "an agent's note on this machine,"
				}
				review = append(review, fmt.Sprintf("- supersedes? [[%s]] (this machine) may replace %s [[%s]]; owner decides\n  new: %s\n  old: %s", nf.Meta.Name, whose, pm.existing.Name, nf.Meta.Description, pm.existing.Description))
				continue
			}
			line, retire := supersedeRouteWith(nf, pm.existing, sameOrigin)
			if !retire {
				review = append(review, line)
				continue
			}
			supersede[pm.existing.Path] = nf.Meta.Name
			review = append(review, fmt.Sprintf("- supersedes: [[%s]] replaces [[%s]]", nf.Meta.Name, pm.existing.Name))
		}
	}
	return review, supersede, nil
}

// writeLocalBatch writes one batch into the overlay: each fact redacted
// (fail closed) and written atomically under curated/<scope>/, the quote
// ledger lines of its rulings, its review lines, and superseded_by on the
// overlay facts it replaces. A ruling whose quote the redaction changed is
// filed as observed: its words no longer match the owner's turn. It returns
// how many facts it wrote and the overlay paths it retired.
func writeLocalBatch(dir, storeDir string, facts []*fact.Fact, rulings []bool, ledgerLines []byte, review []string, supersede map[string]string, at string, batch int) (int, []string, error) {
	n := 0
	for i, f := range facts {
		src := &f.Meta.Source
		red, err := digest.RedactAll([]string{f.Meta.Description, f.Body, src.OwnerQuote})
		if err != nil {
			return n, nil, fmt.Errorf("redaction unavailable, batch not written: %w", err)
		}
		f.Meta.Description, f.Body = red[0], red[1]
		if rulings[i] && red[2] != src.OwnerQuote {
			ledgerLines = dropQuoteLine(ledgerLines, f.Meta.Name)
			src.Kind, src.OwnerQuote, src.OwnerQuoteFrom = "observed", "", ""
		}
		rel, _ := overlay.Rel(f.Path)
		b, err := f.Encode()
		if err != nil {
			return n, nil, err
		}
		if err := overlay.Write(dir, storeDir, rel, b); err != nil {
			return n, nil, err
		}
		n++
	}
	if len(ledgerLines) > 0 {
		if err := overlay.Append(dir, storeDir, overlay.QuotesRel, ledgerLines); err != nil {
			return n, nil, err
		}
	}
	var retired []string
	for path, by := range supersede {
		rel, ok := overlay.Rel(path)
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		f, err := fact.Parse(path, b)
		if err != nil || f.Meta.SupersededBy != "" {
			continue
		}
		f.Meta.SupersededBy, f.Meta.Updated = by, time.Now().Format("2006-01-02")
		out, err := f.Encode()
		if err != nil {
			continue
		}
		if err := overlay.Write(dir, storeDir, rel, out); err != nil {
			return n, retired, err
		}
		retired = append(retired, path)
	}
	if len(review) > 0 {
		rel := overlay.ReviewDir + "/" + time.Now().Format("2006-01-02") + ".md"
		if err := overlay.Append(dir, storeDir, rel, fmt.Appendf(nil, "## local curator run %s, batch %d\n\n%s\n\n", at, batch, strings.Join(review, "\n"))); err != nil {
			return n, retired, err
		}
	}
	// The local layer's folder indexes follow its facts (tacit_tree reads them).
	local, _ := overlay.Load(dir)
	if err := regenLocalIndexes(dir, storeDir, local); err != nil {
		return n, retired, err
	}
	return n, retired, nil
}

// dropQuoteLine removes the ledger line of the fact called name.
func dropQuoteLine(lines []byte, name string) []byte {
	var out []byte
	for _, l := range strings.SplitAfter(string(lines), "\n") {
		var q ledger.Quote
		if json.Unmarshal([]byte(l), &q) == nil && q.Name == name {
			continue
		}
		out = append(out, l...)
	}
	return out
}

// wordsMasked reports the owner's words holding something scrump masks (or
// redaction unavailable: fail closed): the layer writes no statement of them.
func wordsMasked(words string) bool {
	if strings.TrimSpace(words) == "" {
		return false
	}
	red, err := digest.RedactAll([]string{words})
	return err != nil || len(red) != 1 || red[0] != words
}
