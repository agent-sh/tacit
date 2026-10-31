package main

// The rotation (SPEC.md, "Rotation"), a step of the daily `tacit maintain`
// after curate and before verify, and `tacit rotation` alone: the live
// curated facts nobody has checked for the longest go back to the curator's
// model, in batches, each with the proof code gathered for it
// (internal/rotation). Code checks every verdict, one retry with the
// problems, and writes a batch as one commit: a stale fact retired, a
// replaced one pointing at its successor, a changed detail as an in-place
// note, a compress as one new fact the old ones point at, a question for
// the owner instead of any change to their own facts, and last_checked
// today on every fact the model answered, whatever the answer. The cost
// of every batch is logged per fact.

import (
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

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/rotation"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// rotationRun is the rotation's model call; tests substitute it.
var rotationRun curate.Runner = curate.RunCLI

const (
	// rotationBatch is how many facts one model call judges: each carries
	// its proof (a few KB), so a batch stays well inside one answer.
	rotationBatch = 12
	// rotationAgeDays is how long a checked fact rests before it is due
	// again. The owner set no limit on the rotation's size (phase 2, answer
	// 8); a fact checked yesterday is not due today. TACIT_ROTATION_AGE_DAYS.
	rotationAgeDays = 30
	// rotationLog is the per-batch cost log in the state directory.
	rotationLog = "rotation.jsonl"
)

type rotationOpts struct {
	model string
	max   int // facts per run; 0: every fact that is due
	age   int // days a checked fact rests
	batch int
	dry   bool
	only  map[string]bool // tests, a re-run: these facts alone, due or not
}

type rotationStats struct {
	Due, Checked, Still, Stale, Superseded, Fixed, Compressed, Unknown, Asked, Refused, Unanswered, Batches int
	// Claims: the due facts that claim the owner's authority without their
	// words; Reworded: of those, replaced by a fact in an agent's words.
	Claims, Reworded int
	// Gone: the due facts of this machine whose named path is gone (taken
	// first).
	Gone int
	// Folders: the folders left for review the rotation found due (asked
	// again with extra evidence), Settled: of those, decided otherwise.
	Folders, Settled int
	Cost             float64
}

func (st rotationStats) String() string {
	per := 0.0
	if st.Checked > 0 {
		per = st.Cost / float64(st.Checked)
	}
	return fmt.Sprintf("%d due (%d of this machine naming a path that is gone, first; %d claiming the owner's authority without their words, next), %d checked (%d still, %d stale, %d superseded, %d fixed, %d compressed, %d reworded as an agent's note, %d unknown, %d asked the owner, %d refused), %d unanswered, %d folder(s) left for review asked again (%d settled), $%.3f ($%.4f a fact)",
		st.Due, st.Gone, st.Claims, st.Checked, st.Still, st.Stale, st.Superseded, st.Fixed, st.Compressed, st.Reworded, st.Unknown, st.Asked, st.Refused, st.Unanswered, st.Folders, st.Settled, st.Cost, per)
}

func envInt(key string, def int) int {
	var v int
	if _, err := fmt.Sscan(strings.TrimSpace(os.Getenv(key)), &v); err == nil && v >= 0 {
		return v
	}
	return def
}

// runRotation implements `tacit rotation [--max N] [--age D] [--model M] [--dry-run]`.
func runRotation(args []string) int {
	fs := flag.NewFlagSet("rotation", flag.ExitOnError)
	model := fs.String("model", curate.DefaultModel, "the judging model")
	max := fs.Int("max", envInt("TACIT_ROTATION_MAX", 0), "facts per run (0: every fact that is due)")
	age := fs.Int("age", envInt("TACIT_ROTATION_AGE_DAYS", rotationAgeDays), "days a checked fact rests before it is due again")
	only := fs.String("fact", "", "check these facts alone (comma separated), due or not")
	dry := fs.Bool("dry-run", false, "gather, ask and check; write nothing")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables")
	fs.Parse(args)
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "rotation:", err)
			return 2
		}
		refreshModels()
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "rotation: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	o := rotationOpts{model: *model, max: *max, age: *age, dry: *dry}
	if *only != "" {
		o.only = map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			o.only[strings.TrimSpace(n)] = true
		}
	}
	rep := &curate.Report{At: time.Now().Format(time.RFC3339), ModelCalls: &modelrun.Trace{}}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	st, errs := rotationStep(ctx, s, o, rep)
	fmt.Println("rotation:", st)
	if errs > 0 {
		return 1
	}
	return 0
}

// maintainRotation is maintain's rotation step; tests substitute it.
var maintainRotation = func(ctx context.Context, s *store.Store) int {
	rep := &curate.Report{At: time.Now().Format(time.RFC3339), ModelCalls: &modelrun.Trace{}}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	st, errs := rotationStep(ctx, s, rotationOpts{model: curate.DefaultModel, max: envInt("TACIT_ROTATION_MAX", 0), age: envInt("TACIT_ROTATION_AGE_DAYS", rotationAgeDays)}, rep)
	fmt.Println("rotation:", st)
	return errs
}

// rotator holds what one run knows.
type rotator struct {
	s           *store.Store
	local       bool
	host, today string
	home        string
	facts       []*fact.Fact
	byName      map[string]*fact.Fact
	taken       map[string]bool
	recs        map[string]*record.Record
	ix          *search.Index
	scopes      []string
	tl          *gitev.Builder
	rs          *reviewServer
	ch          *check.Checker
	open        map[string]bool     // facts with an open finding: the verifier's first
	findings    map[string][]string // earlier stale findings of a fact, as lines
	gone        *goneProbe          // this machine's facts whose named paths are gone
}

// rotationStep runs the rotation on the store, or on this machine's local
// layer (its own curated facts, written into the overlay). It returns the
// counts and the number of errors.
func rotationStep(ctx context.Context, s *store.Store, o rotationOpts, rep *curate.Report) (rotationStats, int) {
	var st rotationStats
	if ok, why := fact.FormatWrites(s.Dir); !ok {
		fmt.Fprintln(os.Stderr, "rotation: nothing done:", why)
		return st, 0
	}
	local := localLayer(ctx, s)
	if local && !overlay.Outside(overlay.Dir(), s.Dir) {
		fmt.Fprintln(os.Stderr, "rotation:", overlay.ErrInside)
		return st, 1
	}
	if !local && !o.dry {
		if err := writerCheck(ctx, s, "curator"); err != nil {
			fmt.Fprintln(os.Stderr, "rotation: nothing done:", err)
			return st, 1
		}
		if err := s.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "rotation: sync:", err)
			return st, 1
		}
	}
	r, err := newRotator(ctx, s, local)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotation:", err)
		return st, 1
	}
	defer r.ix.Close()
	defer func() { _ = r.ch.Forge.Save() }()
	due := r.due(o)
	st.Due = len(due)
	for _, f := range due {
		if r.goneFirst(f) {
			st.Gone++
		}
	}
	size := o.batch
	if size <= 0 {
		size = rotationBatch
	}
	errs := 0
	for i := 0; i < len(due); i += size {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "rotation:", ctx.Err())
			errs++
			break
		}
		batch := due[i:min(i+size, len(due))]
		items := make([]rotation.Item, 0, len(batch))
		var judged []*fact.Fact
		for _, f := range batch {
			it, err := r.item(ctx, f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "rotation: %s: no proof: %v\n", f.Meta.Name, err)
				continue
			}
			items = append(items, it)
			judged = append(judged, f)
		}
		if len(items) == 0 {
			continue
		}
		vs, refused, cost, err := r.judge(ctx, o.model, items)
		st.Cost += cost
		rep.CostUSD += cost
		st.Batches++
		if err != nil {
			fmt.Fprintln(os.Stderr, "rotation: model:", err)
			errs++
			continue
		}
		b := r.outcomes(items, vs, refused)
		stamp := stamped(items, vs, refused)
		gone := 0
		for _, f := range judged {
			if r.goneFirst(f) {
				gone++
			}
		}
		r.logBatch(len(items), gone, b, cost, o.model)
		if o.dry {
			for _, it := range items {
				if v, ok := vs[it.Name]; ok {
					fmt.Printf("- %s: %s %s\n", it.Name, v.Outcome, strings.Join(v.Cites, ","))
				} else {
					fmt.Printf("- %s: no verdict\n", it.Name)
				}
			}
			st.add(b)
			continue
		}
		var werr error
		if local {
			werr = r.writeLocal(judged, items, vs, stamp)
		} else {
			werr = r.write(ctx, judged, items, vs, b, stamp)
		}
		if werr != nil {
			fmt.Fprintln(os.Stderr, "rotation: write:", werr)
			errs++
			continue
		}
		st.add(b)
		fmt.Fprintf(os.Stderr, "rotation: batch %d: %d fact(s), $%.3f\n", st.Batches, len(items), cost)
	}
	if o.only == nil && ctx.Err() == nil {
		errs += rotateReviewFolders(ctx, s, local, o, &st, rep)
	}
	return st, errs
}

// rotationAssessStep is the rotation's assessment of the folders left for
// review it found due; tests substitute it.
var rotationAssessStep = assessStep

// rotateReviewFolders puts the folders left for review that are due (never
// asked with extra evidence, or last decided a week or more ago) back to
// the assessment, which gathers more evidence for each: needs-review is in
// the rotation's due set, as a fact checked long ago is.
func rotateReviewFolders(ctx context.Context, s *store.Store, local bool, o rotationOpts, st *rotationStats, rep *curate.Report) int {
	if ok, _ := fact.FormatWrites(s.Dir); !ok {
		return 0
	}
	home, _ := os.UserHomeDir()
	held := loadHeld(filepath.Join(platform.StateDir(home), assessHeldFile))
	recs, _ := record.Walk(s.Dir)
	if local {
		recs = localRecords()
	}
	due := reviewFoldersDue(recs, held, time.Now().Format("2006-01-02"))
	if len(due) == 0 {
		return 0
	}
	only := map[string]bool{}
	for _, f := range due {
		only[f] = true
	}
	st.Folders = len(due)
	arep := &curate.Report{ModelCalls: rep.ModelCalls}
	errs := rotationAssessStep(ctx, s, assessOpts{model: o.model, max: len(due), dry: o.dry, only: only, retry: true}, arep)
	st.Settled = arep.ReviewSettled
	st.Cost += arep.CostUSD
	rep.CostUSD += arep.CostUSD
	fmt.Fprintf(os.Stderr, "rotation: %d folder(s) left for review asked again with extra evidence: %d settled, $%.3f\n", len(due), arep.ReviewSettled, arep.CostUSD)
	return errs
}

// reviewFoldersDue are the live project folders left for review that the
// rotation asks again: never asked with extra evidence, or last decided
// reviewRetryDays or more before today.
func reviewFoldersDue(recs map[string]*record.Record, held *heldState, today string) []string {
	var out []string
	for _, f := range sortedRecordFolders(recs) {
		r := recs[f]
		if !r.Live() || r.Status != record.StatusNeedsReview || (r.Kind != record.KindRepo && r.Kind != record.KindFolder) {
			continue
		}
		if st, ok := held.Review[f]; ok && !retryDue(st.At, today) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (st *rotationStats) add(b rotationStats) {
	st.Checked += b.Checked
	st.Still += b.Still
	st.Stale += b.Stale
	st.Superseded += b.Superseded
	st.Fixed += b.Fixed
	st.Compressed += b.Compressed
	st.Reworded += b.Reworded
	st.Claims += b.Claims
	st.Unknown += b.Unknown
	st.Asked += b.Asked
	st.Refused += b.Refused
	st.Unanswered += b.Unanswered
}

func newRotator(ctx context.Context, s *store.Store, local bool) (*rotator, error) {
	home, _ := os.UserHomeDir()
	r := &rotator{s: s, local: local, host: store.ShortHost(), today: time.Now().Format("2006-01-02"), home: home,
		byName: map[string]*fact.Fact{}, taken: map[string]bool{}, open: map[string]bool{}, findings: map[string][]string{}}
	all, _, err := fact.Walk(s.Dir)
	if err != nil {
		return nil, err
	}
	if local {
		lf, _ := overlay.Load(overlay.Dir())
		all = append(all, lf...)
	}
	r.gone = newGoneProbe(r.host, home, machineNames(all))
	for _, f := range all {
		r.taken[f.Meta.Name] = true
		if f.InInbox() || overlay.IsInbox(f.Path) {
			continue
		}
		r.facts = append(r.facts, f)
		if old := r.byName[f.Meta.Name]; old == nil || (local && overlay.IsCurated(f.Path)) || curatedRank(f) < curatedRank(old) {
			r.byName[f.Meta.Name] = f
		}
	}
	r.recs = storeRecords(s.Dir)
	r.ch = storeChecker(ctx, s.Dir, local)
	head, err := s.Head(ctx)
	if err != nil {
		return nil, err
	}
	if r.ix, err = search.Open(ctx, s.Dir, head); err != nil {
		return nil, err
	}
	r.scopes, _ = r.ix.Scopes(ctx)
	r.tl = newTimelineBuilder(s, time.Time{})
	r.rs = &reviewServer{s: s}
	r.rs.loadFacts()
	for _, it := range r.rs.items() {
		if it.Decision == nil && it.Ask == nil {
			r.open[it.A.Name], r.open[it.B.Name] = true, true
		}
	}
	for _, it := range r.rs.staleItems() {
		if it.NeedsOwner || (it.Decision == nil && it.Ask == nil) {
			r.open[it.A.Name] = true
			continue
		}
		if d := it.Decision; d != nil {
			r.findings[it.A.Name] = append(r.findings[it.A.Name], fmt.Sprintf("a git staleness check of %s answered %q by %s: %s", it.Flag.Since, d.Action, orDefaultStr(d.By, "the owner"), d.Evidence))
		}
	}
	for _, it := range r.rs.checkItems() {
		if it.NeedsOwner {
			r.open[it.A.Name] = true
		}
	}
	// A name two files hold waits for its own answer: the rotation would
	// read one of the files by the name.
	for _, it := range r.rs.nameItems() {
		if it.NeedsOwner {
			r.open[it.A.Name] = true
		}
	}
	return r, nil
}

func orDefaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// rotatable reports a live curated fact the rotation checks: not generated,
// not in a folder suppressed into another (the migration moves it), not one
// with an open finding (the verifier and the owner have it), and on the
// local layer only this machine's own.
func (r *rotator) rotatable(f *fact.Fact) bool {
	if f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Meta.Scope, "derived/") || r.open[f.Meta.Name] {
		return false
	}
	if d, ok := fact.TypeDocOf(f.Meta.Type); ok && d.Generated {
		return false
	}
	if r.local != overlay.IsCurated(f.Path) {
		return false
	}
	if w, chain, err := record.Resolve(r.recs, f.Meta.Scope); err == nil && len(chain) > 0 && w != f.Meta.Scope {
		return false
	}
	return true
}

// claimsOwner reports a fact whose text claims the owner's authority
// without their verified words: the write-time attribution check fails on
// it (search.ClaimsWithoutEvidence), and no hook delivers it.
func claimsOwner(f *fact.Fact) bool {
	ruling, words := owners(f)
	return search.ClaimsWithoutEvidence(f.Meta.Description, f.Body, ruling, words != "" || f.Meta.Type == fact.StatementType)
}

// due are the facts this run checks: the facts that claim the owner's
// authority without their words first (claimsOwner: no hook delivers them
// until they are reworded as an agent's note or their quote is found), then
// those made from an agent's note the reviewer could not confirm (uncertain,
// nosource: the silent notes no question is asked about, SPEC.md
// "Questions"), then never checked, then the oldest check, then the oldest
// created, then by name; a checked fact is due again after o.age days; at
// most o.max when it is set.
// Facts of this machine whose description names a path that is gone
// (goneFirst) come before all of these.
func (r *rotator) due(o rotationOpts) []*fact.Fact {
	cut := time.Now().AddDate(0, 0, -o.age).Format("2006-01-02")
	var out []*fact.Fact
	for _, f := range r.facts {
		if !r.rotatable(f) {
			continue
		}
		if o.only != nil {
			if o.only[f.Meta.Name] {
				out = append(out, f)
			}
			continue
		}
		if f.Meta.LastChecked == "" || f.Meta.LastChecked <= cut {
			out = append(out, f)
		}
	}
	claims, gone := map[*fact.Fact]bool{}, map[*fact.Fact]bool{}
	for _, f := range out {
		claims[f] = claimsOwner(f)
		gone[f] = r.goneFirst(f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Meta, out[j].Meta
		// A fact of this machine whose named path is gone is delivered
		// meanwhile, and most such facts are stale: it goes first.
		if gi, gj := gone[out[i]], gone[out[j]]; gi != gj {
			return gi
		}
		if ci, cj := claims[out[i]], claims[out[j]]; ci != cj {
			return ci
		}
		if ui, uj := unconfirmed(out[i]), unconfirmed(out[j]); ui != uj {
			return ui
		}
		if a.LastChecked != b.LastChecked {
			return a.LastChecked < b.LastChecked // "" (never) first
		}
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.Name < b.Name
	})
	if o.max > 0 && len(out) > o.max {
		out = out[:o.max]
	}
	return out
}

// goneFirst reports a fact of this machine whose description names a path
// that is gone and that was not checked since it last changed: once the
// model has seen the missing paths and answered, the fact takes its turn
// like any other.
func (r *rotator) goneFirst(f *fact.Fact) bool {
	if len(r.gone.paths(f)) == 0 {
		return false
	}
	changed := f.Meta.Updated
	if changed == "" {
		changed = f.Meta.Created
	}
	return f.Meta.LastChecked == "" || f.Meta.LastChecked < changed
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// machineRoots are the system roots a path must be under, or ~, to name a
// place on a machine. Both of the rotation's path readers take them, the
// place reader (machinePathRx) and the gone-path reader (gonePathRx), so
// the paths the pick finds gone and the paths its proof stats are under the
// same roots. Each reader keeps its own boundaries and characters.
const machineRoots = `Users|home|data|opt|etc|var|usr|srv|mnt|Library`

// machinePathRx finds the paths a fact names as places on a machine: under
// ~, or under one of machineRoots. Rotation stats them and the ancestor
// index reads folder names from them, so a route of an API (/v1/messages)
// or of a forge (/repos/<owner>/<name>/pulls) must not count. Where a path
// only names a repository, repoPaths reads every one, these places first.
var machinePathRx = regexp.MustCompile(`(?:^|[\s(\x60"'])((?:~|/(?:` + machineRoots + `))/[A-Za-z0-9._\-/]+)`)

// repoPathRx finds every ~ and absolute path in a text (repoPaths).
var repoPathRx = regexp.MustCompile(`(?:^|[\s(\x60"'])((?:~/+|/)[A-Za-z0-9._\-][A-Za-z0-9._\-/]*)`)

// pseudoPathRx is a path in a kernel's view (/proc, /sys, /dev), whose
// links resolve to what the reading process has open (/proc/self/cwd,
// /dev/fd/3), or under /net, where a lookup mounts another host's export.
var pseudoPathRx = regexp.MustCompile(`^/(?:proc|sys|dev|net)(?:/|$)`)

// repoPaths are the ~ and absolute paths text names, at most n (n < 0: all),
// each for the repository repoid makes of it (SPEC.md, "Orgs"). Only a real
// git root on this machine makes one, so a route names nothing, and a
// checkout outside machinePathRx's roots (/tmp, /root, /workspace) is
// found. The places machinePathRx reads come first and each path counts
// once, so routes never crowd out a path the place reader takes. Paths in
// pseudoPathRx are left out: what they resolve to is the reading process's
// or another host's, not the fact's.
func repoPaths(text string, n int) []string {
	var places, others []string
	seen := map[string]bool{}
	for _, m := range repoPathRx.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(m[1], ".,;:)")
		if seen[p] || pseudoPathRx.MatchString(p) {
			continue
		}
		seen[p] = true
		if machinePathRx.MatchString(p) {
			places = append(places, p)
		} else {
			others = append(others, p)
		}
	}
	out := append(places, others...)
	if n >= 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// owners reports a fact that is the owner's: their ruling, or one holding
// words of theirs. The rotation never changes either; it asks them.
func owners(f *fact.Fact) (ruling bool, words string) {
	if overlay.IsCurated(f.Path) {
		return overlay.IsRuling(f), ""
	}
	return isRuling(f), fact.OwnerWordsOf(f)
}

// item is f with the proof code gathers for it. A failed redaction fails
// it: nothing unredacted reaches the model.
func (r *rotator) item(ctx context.Context, f *fact.Fact) (rotation.Item, error) {
	ruling, words := owners(f)
	it := rotation.Item{Name: f.Meta.Name, Scope: f.Meta.Scope, Type: f.Meta.Type, Description: f.Meta.Description, Body: clip(f.Body, 2400),
		Created: f.Meta.Created, Updated: f.Meta.Updated, LastChecked: f.Meta.LastChecked, Host: f.Meta.Source.Host, Ruling: ruling, OwnerWords: words,
		ClaimsOwner: claimsOwner(f)}
	var ps []rotation.Proof
	add := func(p rotation.Proof) {
		p.ID = fmt.Sprintf("p%d", len(ps)+1)
		p.Text = clip(p.Text, 400)
		ps = append(ps, p)
	}
	if it.ClaimsOwner {
		add(rotation.Proof{Kind: "attribution", Text: "code: its text says the owner ruled, said, decided or wants it, and it holds no verified words of theirs (no quote the ledger backs; the rulings audit looks for one in the owner's history), so no hook delivers it: reword it as an agent's note, never with words of theirs it does not quote"})
	}
	// Git: what each repository holds now of the paths and names it cites.
	gctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	c := *f
	ref := gitev.FactRef{Fact: &c}
	if o := r.rs.inbox[f.Meta.Origin]; o != nil {
		oc := *o
		ref.Origin = &oc
	}
	eff := r.tl.Effective(gctx, ref)
	sub, evs, err := r.tl.ReadSince(gctx, ref, eff.At)
	cancel()
	if err == nil {
		for _, ev := range evs {
			head := fmt.Sprintf("%s at its default branch (%s, %s)", ev.Name, ev.Head, ev.HeadAt.Format("2006-01-02"))
			for _, t := range ev.State {
				switch {
				case t.Generic:
					continue
				case t.Unknown:
					add(rotation.Proof{Kind: "git", Text: fmt.Sprintf("%s: the %s %q could not be searched", head, t.Kind, t.Term)})
				case t.Present:
					add(rotation.Proof{Kind: "git", Text: fmt.Sprintf("%s: the %s %q is there (%d match(es): %s)", head, t.Kind, t.Term, t.Count, strings.Join(t.Where, ", "))})
				default:
					add(rotation.Proof{Kind: "git", Text: fmt.Sprintf("%s: the %s %q is gone", head, t.Kind, t.Term)})
				}
			}
			for _, g := range ev.Deleted {
				add(rotation.Proof{Kind: "git", Text: fmt.Sprintf("%s: %q was last touched by %s on %s (%q)", ev.Name, g.Term, g.Commit.Short, g.Commit.At.Format("2006-01-02"), g.Commit.Subject)})
			}
			for i, cm := range ev.Changed {
				if i == 3 {
					break
				}
				add(rotation.Proof{Kind: "git", Text: fmt.Sprintf("%s: %s on %s rewrote lines with %s (%q)", ev.Name, cm.Short, cm.At.Format("2006-01-02"), strings.Join(cm.Dropped, ", "), cm.Subject)})
			}
			if ev.Incomplete {
				add(rotation.Proof{Kind: "git", Text: ev.Name + ": the read was incomplete: " + strings.Join(ev.Notes, "; ")})
			}
		}
	}
	// This machine's paths, when the fact is about this machine: first the
	// ones its description names that are gone (what put it first), then
	// up to 6 more it names.
	if f.Meta.Source.Host == r.host || thisMachine(f, r.host) {
		seen := map[string]bool{}
		for _, p := range r.gone.paths(f) {
			if !seen[p] {
				seen[p] = true
				add(rotation.Proof{Kind: "path", Text: fmt.Sprintf("%s does not exist on %s", p, r.host)})
			}
		}
		n := 0
		for _, m := range machinePathRx.FindAllStringSubmatch(f.Meta.Description+"\n"+f.Body, -1) {
			p := strings.TrimRight(m[1], ".,;:)")
			if seen[p] || n == 6 {
				continue
			}
			seen[p] = true
			n++
			full := p
			if strings.HasPrefix(p, "~/") {
				full = filepath.Join(r.home, p[2:])
			}
			if fi, err := os.Stat(full); err == nil {
				kind := "file"
				if fi.IsDir() {
					kind = "directory"
				}
				add(rotation.Proof{Kind: "path", Text: fmt.Sprintf("%s exists on %s (a %s, changed %s)", p, r.host, kind, fi.ModTime().Format("2006-01-02"))})
			} else if errors.Is(err, os.ErrNotExist) {
				add(rotation.Proof{Kind: "path", Text: fmt.Sprintf("%s does not exist on %s", p, r.host)})
			}
		}
	}
	// Newer facts on the same subject, in its folder and its repositories'.
	scopes := []string{f.Meta.Scope}
	repos := append([]string(nil), f.Meta.AppliesTo.Repos...)
	if rest, ok := strings.CutPrefix(f.Meta.Scope, "projects/"); ok {
		repos = append(repos, rest)
	}
	for _, rp := range suppressionNames(r.recs, repos) {
		scopes = append(scopes, "projects/"+strings.ToLower(rp))
	}
	var idents []string
	for _, id := range sub.Idents {
		if gitev.Specific(id) {
			idents = append(idents, id)
		}
	}
	shown := map[string]bool{f.Meta.Name: true}
	q := f.Meta.Description + " " + strings.Join(idents, " ")
	if hits, err := r.ix.Query(ctx, search.Situation{Query: q, Scopes: dedupeNames(scopes), Derived: true, ShortTerms: true}, 12, false); err == nil {
		n := 0
		for _, h := range hits {
			if shown[h.Name] || n == 6 || h.Inbox {
				continue
			}
			newer := h.Created > f.Meta.Created
			if !newer && h.Scope != f.Meta.Scope {
				continue
			}
			shown[h.Name] = true
			n++
			add(rotation.Proof{Kind: "newer", Fact: h.Name, Newer: newer, Folder: h.Scope == f.Meta.Scope, Text: fmt.Sprintf("[[%s]] (%s, %s, written %s): %s", h.Name, h.Scope, h.Type, h.Created, h.Description)})
		}
	}
	// Its distinctive names anywhere in the store.
	for i, id := range idents {
		if i == 3 {
			break
		}
		hits, err := r.ix.Query(ctx, search.Situation{Query: id, Scopes: r.scopes, Derived: true, ShortTerms: true}, 4, false)
		if err != nil {
			continue
		}
		for _, h := range hits {
			if shown[h.Name] || h.Inbox {
				continue
			}
			shown[h.Name] = true
			add(rotation.Proof{Kind: "mention", Fact: h.Name, Newer: h.Created > f.Meta.Created, Folder: h.Scope == f.Meta.Scope, Text: fmt.Sprintf("%q also in [[%s]] (%s, written %s): %s", id, h.Name, h.Scope, h.Created, h.Description)})
		}
	}
	// Its folder's record.
	if rec := r.recs[f.Meta.Scope]; rec != nil && (rec.Status != record.StatusActive || rec.SuppressedBy != "") {
		why := ""
		if len(rec.Was) > 0 {
			why = ": " + rec.Was[0].Why
		}
		add(rotation.Proof{Kind: "folder", Text: fmt.Sprintf("its folder %s is %s (%s)%s", rec.Folder, rec.Status, rec.Kind, why)})
	}
	for _, l := range r.findings[f.Meta.Name] {
		add(rotation.Proof{Kind: "finding", Text: l})
	}
	// Code's checks of its dry references: the source of truth, not a
	// judgement (a fix whose new value a check refutes is refused).
	for _, cr := range factChecks(ctx, r.ch, f) {
		add(rotation.Proof{Kind: "check", Text: "checked: " + cr.Line()})
	}
	texts := []string{it.Description, it.Body}
	for _, p := range ps {
		texts = append(texts, p.Text)
	}
	red, err := digest.RedactAllContext(ctx, texts)
	if err != nil {
		return it, fmt.Errorf("redaction: %w", err)
	}
	it.Description, it.Body = red[0], red[1]
	for i := range ps {
		ps[i].Text = red[i+2]
	}
	it.Proof = ps
	return it, nil
}

// checkValue is code's check of a fix's new value, when it is a dry value
// code can check: a forge repository URL, a version (v1.2.3) or a path of
// the fact's repository, or a repository name replacing one (was names a
// repository the catalogs or the forge know). ok is false for any other.
func (r *rotator) checkValue(it rotation.Item, was, now string) (check.Result, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	now = strings.TrimSpace(now)
	if refs := check.Refs(now); len(refs) == 1 {
		f := r.byName[it.Name]
		repo, root := "", ""
		if f != nil {
			repo, root = factRepo(r.ch.Catalog, f)
		}
		switch ref := refs[0]; {
		case ref.Kind == check.KindRepo:
			return r.ch.ForgeRepo(ctx, ref.Owner, ref.Value), true
		case ref.Kind == check.KindVersion && root != "" && strings.Trim(now, "`") == ref.Value:
			return r.ch.Version(ctx, repo, root, ref.Value), true
		}
	}
	p := strings.Trim(now, "`")
	if f := r.byName[it.Name]; f != nil && strings.Contains(p, "/") && !strings.ContainsAny(p, " \t") && !strings.Contains(p, "://") {
		if repo, root := factRepo(r.ch.Catalog, f); root != "" && len(check.Refs("`"+p+"`")) == 1 {
			return r.ch.Path(ctx, repo, root, p), true
		}
	}
	if fact.IsSlug(strings.ToLower(was)) && fact.IsSlug(strings.ToLower(now)) && r.ch.Repo(ctx, strings.ToLower(was)).Verdict != check.Unknown && (r.ch.Catalog.Has(strings.ToLower(was)) || record.RepoOwner(r.recs, strings.ToLower(was)) != "") {
		return r.ch.Repo(ctx, strings.ToLower(now)).Summary(), true
	}
	return check.Result{}, false
}

// env is what rotation.Check reads of the store.
func (r *rotator) env() rotation.Env {
	return rotation.Env{
		Today:      r.today,
		CheckValue: r.checkValue,
		Taken:      func(n string) bool { return r.taken[n] },
		Fact: func(n string) (rotation.Live, bool) {
			f := r.byName[n]
			if f == nil || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
				return rotation.Live{}, false
			}
			ruling, words := owners(f)
			return rotation.Live{Scope: f.Meta.Scope, Created: f.Meta.Created, Ruling: ruling || words != ""}, true
		},
	}
}

// judge asks the model about items, checks every verdict, and sends the
// refused ones back once. It returns the verdicts that passed, the facts
// refused twice (with why), and the cost.
func (r *rotator) judge(ctx context.Context, model string, items []rotation.Item) (map[string]rotation.Verdict, map[string][]string, float64, error) {
	prompt := fmt.Sprintf(rotation.Prompt, rotation.Render(items))
	res, cost, err := rotationRun(ctx, model, prompt)
	if err != nil {
		return nil, nil, cost, err
	}
	env := r.env()
	byName := map[string]rotation.Item{}
	var order []string
	for _, it := range items {
		byName[it.Name] = it
		order = append(order, it.Name)
	}
	ok := map[string]rotation.Verdict{}
	refused := map[string][]string{}
	take := func(text string) {
		vs, err := rotation.Parse(text)
		if err != nil {
			for _, n := range order {
				if _, done := ok[n]; !done {
					refused[n] = []string{err.Error()}
				}
			}
			return
		}
		seen := map[string]bool{}
		for _, v := range vs {
			it, in := byName[v.Name]
			if !in || seen[v.Name] {
				continue
			}
			seen[v.Name] = true
			if _, done := ok[v.Name]; done {
				continue
			}
			if ps := rotation.Check(v, it, env); len(ps) > 0 {
				refused[v.Name] = ps
				continue
			}
			ok[v.Name] = v
			delete(refused, v.Name)
		}
		for _, n := range order {
			if _, done := ok[n]; !done && !seen[n] && refused[n] == nil {
				refused[n] = []string{rotation.ErrNoVerdict.Error()}
			}
		}
	}
	take(res)
	if len(refused) > 0 {
		res2, c2, err := rotationRun(ctx, model, rotation.RetryPrompt(prompt, res, refused, order))
		cost += c2
		if err == nil {
			take(res2)
		}
	}
	return ok, refused, cost, nil
}

// outcomes counts a batch. A verdict refused twice counts as unknown (the
// fact was looked at; code could not take the answer); a fact with no
// verdict at all is unanswered and not stamped.
func (r *rotator) outcomes(items []rotation.Item, vs map[string]rotation.Verdict, refused map[string][]string) rotationStats {
	var b rotationStats
	for _, it := range items {
		v, ok := vs[it.Name]
		if !ok {
			if noVerdict(refused[it.Name]) {
				b.Unanswered++
				continue
			}
			b.Refused++
			b.Checked++
			b.Unknown++
			continue
		}
		b.Checked++
		if v.Outcome != rotation.Still && v.Outcome != rotation.Unknown && (it.Ruling || it.OwnerWords != "") && !(v.Outcome == rotation.Fix && !it.Ruling) {
			b.Asked++
			continue
		}
		switch v.Outcome {
		case rotation.Still:
			b.Still++
		case rotation.Unknown:
			b.Unknown++
		case rotation.Stale:
			b.Stale++
		case rotation.Superseded:
			b.Superseded++
		case rotation.Fix:
			b.Fixed++
		case rotation.Compress:
			b.Compressed++
		case rotation.Reword:
			b.Reworded++
		}
	}
	for _, it := range items {
		if it.ClaimsOwner {
			b.Claims++
		}
	}
	return b
}

// noVerdict reports a fact the answer left out, as judge records it.
func noVerdict(ps []string) bool { return len(ps) == 1 && ps[0] == rotation.ErrNoVerdict.Error() }

// stamped is the set of facts a batch stamps: every fact with a verdict,
// or refused twice (it was looked at); not one the model left out.
func stamped(items []rotation.Item, vs map[string]rotation.Verdict, refused map[string][]string) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if _, ok := vs[it.Name]; ok || (refused[it.Name] != nil && !noVerdict(refused[it.Name])) {
			out[it.Name] = true
		}
	}
	return out
}

// plan is a batch's changes to the files as they are now under dir (the
// clone, or the overlay's curated folder), keyed by path relative to dir.
// Facts that changed since they were read are left alone. Paths of facts
// are f.Path less prefix.
func (r *rotator) plan(dir, prefix string, batch []*fact.Fact, items []rotation.Item, vs map[string]rotation.Verdict, stamp map[string]bool) (files map[string][]byte, decisions []decision, asks []string, body []string, err error) {
	files = map[string][]byte{}
	itemOf := map[string]rotation.Item{}
	for _, it := range items {
		itemOf[it.Name] = it
	}
	read := func(f *fact.Fact) (*fact.Fact, []byte, error) {
		rel := strings.TrimPrefix(f.Path, prefix)
		data, ok := files[rel]
		if !ok {
			var err error
			if data, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				return nil, nil, err
			}
		}
		cur, err := fact.Parse(f.Path, data)
		if err != nil {
			return nil, nil, err
		}
		return cur, data, nil
	}
	patch := func(f *fact.Fact, set func(m *fact.Meta)) error {
		cur, data, err := read(f)
		if errors.Is(err, os.ErrNotExist) {
			return nil // gone since (deduped or superseded): nothing to change
		}
		if err != nil {
			return err
		}
		c := *cur
		set(&c.Meta)
		nb, err := fact.PatchRelations(data, &c)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		files[strings.TrimPrefix(f.Path, prefix)] = nb
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	for _, f := range batch {
		it := itemOf[f.Meta.Name]
		if !stamp[f.Meta.Name] {
			continue
		}
		cur, _, err := read(f)
		if err != nil || cur.Meta.SupersededBy != "" || cur.Meta.Retired != "" || cur.Meta.Updated != f.Meta.Updated {
			continue // changed since it was read: the next run looks again
		}
		v, has := vs[f.Meta.Name]
		owner := it.Ruling || it.OwnerWords != ""
		ask := has && owner && v.Outcome != rotation.Still && v.Outcome != rotation.Unknown && !(v.Outcome == rotation.Fix && !it.Ruling)
		cites := citeText(it, v)
		switch {
		case !has || v.Outcome == rotation.Still || v.Outcome == rotation.Unknown:
		case ask:
			lines := []string{checkLine(f.Meta.Name, f.Meta.Scope, v.Outcome, r.today), "  The curator reads it as " + outcomeWords(v) + ": " + clip(v.Evidence, 400)}
			for _, c := range cites {
				lines = append(lines, "  Proof: "+c)
			}
			asks = append(asks, strings.Join(lines, "\n"))
			body = append(body, fmt.Sprintf("%s: %s, asked the owner", f.Meta.Name, v.Outcome))
		case v.Outcome == rotation.Stale:
			if err := patch(f, func(m *fact.Meta) { m.Retired, m.Updated = r.today, r.today }); err != nil {
				return nil, nil, nil, nil, err
			}
			body = append(body, f.Meta.Name+": stale, retired")
		case v.Outcome == rotation.Superseded:
			x := v.SupersededBy
			if err := patch(f, func(m *fact.Meta) { m.SupersededBy, m.Updated = x, r.today }); err != nil {
				return nil, nil, nil, nil, err
			}
			body = append(body, fmt.Sprintf("%s: superseded by %s", f.Meta.Name, x))
		case v.Outcome == rotation.Fix:
			_, data, err := read(f)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			line, err := fact.NoteLine(r.today, v.Was, v.Now)
			if err != nil {
				continue
			}
			nb, err := fact.AppendNote(data, line)
			if err != nil {
				continue
			}
			files[strings.TrimPrefix(f.Path, prefix)] = nb
			if err := patch(f, func(m *fact.Meta) { m.Updated = r.today }); err != nil {
				return nil, nil, nil, nil, err
			}
			body = append(body, fmt.Sprintf("%s: fixed, %s", f.Meta.Name, strings.TrimPrefix(line, fact.NotePrefix)))
		case v.Outcome == rotation.Reword:
			// One fact in an agent's words replaces the claim: the same
			// folder, type and applies_to, superseding it (never deleted).
			nf := v.New
			newF := &fact.Fact{Path: prefix + f.Meta.Scope + "/" + nf.Name + ".md", Meta: fact.Meta{Name: nf.Name, Description: clip(nf.Description, 200), Type: f.Meta.Type, Scope: f.Meta.Scope,
				AppliesTo: f.Meta.AppliesTo, Source: fact.Source{Harness: "curator", Host: r.host, Kind: "observed"}, Created: r.today}, Body: strings.TrimSpace(nf.Body) + "\n"}
			nb, err := newF.Encode()
			if err != nil {
				continue
			}
			files[strings.TrimPrefix(newF.Path, prefix)] = nb
			if err := patch(f, func(m *fact.Meta) { m.SupersededBy, m.Updated = nf.Name, r.today }); err != nil {
				return nil, nil, nil, nil, err
			}
			r.taken[nf.Name] = true
			body = append(body, fmt.Sprintf("%s: reworded as an agent's note, %s", f.Meta.Name, nf.Name))
		case v.Outcome == rotation.Compress:
			nf := v.New
			members := []*fact.Fact{f}
			for _, n := range v.Compress {
				// Only facts of the layer being written: on this machine's layer
				// an agent's note or a store fact is never patched.
				if m := r.byName[n]; m != nil && m != f && strings.HasPrefix(m.Path, prefix) {
					members = append(members, m)
				}
			}
			newF := &fact.Fact{Path: prefix + f.Meta.Scope + "/" + nf.Name + ".md", Meta: fact.Meta{Name: nf.Name, Description: clip(nf.Description, 200), Type: nf.Type, Scope: f.Meta.Scope,
				Source: fact.Source{Harness: "curator", Host: r.host, Kind: "observed"}, Created: r.today}, Body: strings.TrimSpace(nf.Body) + "\n"}
			for _, m := range members {
				newF.Meta.AppliesTo.Repos = union(newF.Meta.AppliesTo.Repos, m.Meta.AppliesTo.Repos)
			}
			nb, err := newF.Encode()
			if err != nil {
				continue
			}
			files[strings.TrimPrefix(newF.Path, prefix)] = nb
			for _, m := range members {
				if err := patch(m, func(mm *fact.Meta) { mm.SupersededBy, mm.Updated = nf.Name, r.today }); err != nil {
					return nil, nil, nil, nil, err
				}
			}
			r.taken[nf.Name] = true
			var names []string
			for _, m := range members {
				names = append(names, m.Meta.Name)
			}
			body = append(body, fmt.Sprintf("%s: compressed into %s (%s)", f.Meta.Name, nf.Name, strings.Join(names, ", ")))
		}
		if has && v.Outcome != rotation.Still && v.Outcome != rotation.Unknown {
			act := v.Outcome
			if ask {
				act = "ask"
			}
			decisions = append(decisions, decision{ID: pairID("rotation", f.Meta.Name, r.today), Action: act, A: f.Meta.Name, B: firstNonEmpty(v.SupersededBy, newName(v)), At: now, By: "curator",
				Evidence: capText(strings.TrimSpace(v.Evidence+" Proof: "+strings.Join(cites, " | ")), 1500)})
		}
		if err := patch(f, func(m *fact.Meta) { m.LastChecked = r.today }); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return files, decisions, asks, body, nil
}

func newName(v rotation.Verdict) string {
	if v.New != nil {
		return v.New.Name
	}
	return ""
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string(nil), a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// citeText is the proof items a verdict cites, as text.
func citeText(it rotation.Item, v rotation.Verdict) []string {
	var out []string
	for _, c := range v.Cites {
		for _, p := range it.Proof {
			if p.ID == c {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

// outcomeWords says a verdict in the page's words.
func outcomeWords(v rotation.Verdict) string {
	switch v.Outcome {
	case rotation.Stale:
		return "outdated"
	case rotation.Superseded:
		return "replaced by [[" + v.SupersededBy + "]]"
	case rotation.Fix:
		return fmt.Sprintf("true, with one detail changed (%q is now %q)", v.Was, v.Now)
	case rotation.Compress:
		return "one of several facts that say one thing"
	case rotation.Reword:
		return "an agent's note that claims your authority"
	}
	return v.Outcome
}

// write puts a batch in the store as one curator commit.
func (r *rotator) write(ctx context.Context, batch []*fact.Fact, items []rotation.Item, vs map[string]rotation.Verdict, b rotationStats, stamp map[string]bool) error {
	_, err := curatorWrite(ctx, r.s, func() (map[string][]byte, string, error) {
		r.rs.loadFacts()
		files, ds, asks, body, err := r.plan(r.s.Dir, "", batch, items, vs, stamp)
		if err != nil || len(files) == 0 {
			return nil, "", err
		}
		for _, d := range ds {
			r.rs.appendDecision(files, d)
		}
		if len(asks) > 0 {
			rel := "review/" + r.today + "-rotation.md"
			cur, ok := files[rel]
			if !ok {
				cur, _ = os.ReadFile(filepath.Join(r.s.Dir, rel))
			}
			if len(cur) == 0 {
				cur = []byte("# The curator's questions from the rotation, " + r.today + "\n\n")
			}
			files[rel] = append(cur, []byte(strings.Join(asks, "\n")+"\n")...)
		}
		msg := fmt.Sprintf("%s %d checked (%d still, %d stale, %d superseded, %d fixed, %d compressed, %d reworded, %d unknown, %d asked)", gitev.RotationPrefix, b.Checked, b.Still, b.Stale, b.Superseded, b.Fixed, b.Compressed, b.Reworded, b.Unknown, b.Asked)
		if len(body) > 0 {
			msg += "\n\n" + strings.Join(body, "\n")
		}
		return files, msg, nil
	})
	return err
}

// writeLocal puts a batch into this machine's overlay: its own curated
// facts only, never the clone.
func (r *rotator) writeLocal(batch []*fact.Fact, items []rotation.Item, vs map[string]rotation.Verdict, stamp map[string]bool) error {
	dir := overlay.Dir()
	curated := filepath.Join(dir, filepath.FromSlash(overlay.CuratedDir))
	files, ds, asks, _, err := r.plan(curated, overlay.Prefix+overlay.CuratedDir+"/", batch, items, vs, stamp)
	if err != nil {
		return err
	}
	for _, rel := range sortedFileKeys(files) {
		if err := overlay.Write(dir, r.s.Dir, overlay.CuratedDir+"/"+rel, files[rel]); err != nil {
			return err
		}
	}
	var log []byte
	for _, d := range ds {
		line, _ := json.Marshal(d)
		log = append(append(log, line...), '\n')
	}
	if len(log) > 0 {
		if err := overlay.Append(dir, r.s.Dir, overlay.CuratedDir+"/"+ledger.DecisionsPath, log); err != nil {
			return err
		}
	}
	if len(asks) > 0 {
		if err := overlay.Append(dir, r.s.Dir, overlay.CuratedDir+"/review/"+r.today+"-rotation.md", []byte(strings.Join(asks, "\n")+"\n")); err != nil {
			return err
		}
	}
	local, _ := overlay.Load(dir)
	return regenLocalIndexes(dir, r.s.Dir, local)
}

// logBatch appends the batch's cost to the state directory's rotation log:
// what a fact costs to check is measured, not assumed.
func (r *rotator) logBatch(n, gone int, b rotationStats, cost float64, model string) {
	p := filepath.Join(platform.StateDir(r.home), rotationLog)
	os.MkdirAll(filepath.Dir(p), 0o700)
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer fh.Close()
	per := 0.0
	if n > 0 {
		per = cost / float64(n)
	}
	line, _ := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339), "model": model, "facts": n, "gone": gone, "cost_usd": cost, "cost_per_fact": per, "local": r.local,
		"still": b.Still, "stale": b.Stale, "superseded": b.Superseded, "fixed": b.Fixed, "compressed": b.Compressed, "reworded": b.Reworded, "claims": b.Claims, "unknown": b.Unknown, "asked": b.Asked, "refused": b.Refused, "unanswered": b.Unanswered})
	fh.Write(append(line, '\n'))
}

// unconfirmed reports a fact made from an agent's note the reviewer could
// not confirm: uncertain, or no session to judge it against.
func unconfirmed(f *fact.Fact) bool {
	return f.Review == ledger.VerdictUncertain || f.Review == ledger.VerdictNoSource
}
