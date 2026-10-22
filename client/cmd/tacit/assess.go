package main

// The assess step (SPEC.md, "Folder records"): a project is not a
// repository. Before a folder has a record, code gathers what can be known
// about it (git, the dreamer's registry, the store's facts, the owner's
// words) and the curator's model decides what it is: the owner's project
// (a repository or a folder), or not a project at all, and its status. Code
// checks every decision (record.Validate), sends a refused one back once,
// and writes the records through the curator's locked write; on a
// read-only clone with the local layer, into this machine's overlay.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// assessRun is the model call of the assessment; tests substitute it.
var assessRun curate.Runner = curate.RunCLI

// Bounds of one run: candidates per model call, and in all.
const (
	assessBatch = 10
	assessMax   = 60
)

type assessOpts struct {
	model string
	max   int
	dry   bool
	// only, when set, limits the run to these folders (tests, a re-run).
	only map[string]bool
	// retry: the rotation's run on the folders left for review it found due;
	// they are asked whatever their fingerprint.
	retry bool
}

// runAssess implements `tacit assess [--model M] [--max N] [--dry-run]`:
// the assessment alone (it is also a step of the full `tacit curate`).
func runAssess(args []string) int {
	fs := flag.NewFlagSet("assess", flag.ExitOnError)
	model := fs.String("model", curate.DefaultModel, "the deciding model")
	max := fs.Int("max", assessMax, "candidates per run")
	dry := fs.Bool("dry-run", false, "gather, ask and check; write nothing")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables")
	fs.Parse(args)
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "assess:", err)
			return 2
		}
		refreshModels()
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "assess: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	rep := &curate.Report{At: time.Now().Format(time.RFC3339), ModelCalls: &modelrun.Trace{}}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	if !localLayer(ctx, s) && !*dry && refuseWriter(ctx, s, "curator", "assess") {
		return 2
	}
	if ok, why := fact.FormatWrites(s.Dir); !ok {
		fmt.Fprintln(os.Stderr, "assess: nothing done:", why)
		return 2
	}
	if !localLayer(ctx, s) && !*dry {
		if err := s.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "assess: sync:", err)
			return 1
		}
	}
	errs := assessStep(ctx, s, assessOpts{model: *model, max: *max, dry: *dry}, rep)
	fmt.Printf("assess: %d candidate(s), %d record(s) written, %d refused, %d waiting on unchanged evidence, %d decision(s) refuted by a check, %d left for review asked again (%d settled, %d still for review), %d left for review waiting (their wider search found nothing new), %s, %d error(s)\n", rep.AssessCandidates, rep.AssessRecords, rep.AssessRefused, rep.AssessHeld, rep.CheckRefused, rep.ReviewRetried, rep.ReviewSettled, rep.ReviewRetried-rep.ReviewSettled, rep.ReviewWaiting, rep.ModelCalls.CostSummary(rep.CostUSD), errs)
	if errs > 0 {
		return 1
	}
	return 0
}

// curateAssessStep is the full curate's assessment; tests substitute it.
var curateAssessStep = func(ctx context.Context, s *store.Store, model string, dry bool, rep *curate.Report) int {
	if ok, why := fact.FormatWrites(s.Dir); !ok {
		if !dry {
			fmt.Fprintln(os.Stderr, "curate: assess skipped:", why)
		}
		return 0
	}
	return assessStep(ctx, s, assessOpts{model: model, max: assessMax, dry: dry}, rep)
}

// assessor holds what one run knows.
type assessor struct {
	dir, home, host, today string
	local                  bool // the local layer: records go into the overlay
	catalog                *repoid.Catalog
	facts                  []*fact.Fact
	recs                   map[string]*record.Record // the store's (and the layer's) records
	storeRecs              map[string]*record.Record // the store's alone
	localRecs              map[string]*record.Record // the local layer's alone (local runs)
	reg                    string
	commits                *repoid.CommitCache
	shared                 map[string][]repoid.HostRepo
	corpus                 *ownerwords.Corpus
	known                  map[string]bool // session ids and note names a why may cite
	ledgerBytes            []byte
	tools                  *dream.ToolUsage
	env                    validate.Env
	factFolders            map[string]bool
	sessionRoots           map[string][]string // session id -> the repositories it worked in
	ctx                    context.Context
	checker                *check.Checker
	commitsShown           map[string][]string // folder -> the own commits its evidence lists
	checkRefused           int                 // decisions a check refuted
	held                   *heldState          // refused and review-waiting folders' fingerprints
	refiler                *refiler            // where facts' own evidence points (codeDecision)
	decisions              map[string]ownerDecision
	lowText                map[*fact.Fact]string // facts' lowered text, for the re-attempts' store search
	retry                  bool                  // assessOpts.retry
	attempts               []reviewAttempt       // this run's re-attempts, for the log
	reviewWaiting          int                   // folders left for review that wait this run
}

func newAssessor(ctx context.Context, s *store.Store, local bool) (*assessor, error) {
	home, _ := os.UserHomeDir()
	a := &assessor{dir: s.Dir, home: home, host: store.ShortHost(), today: time.Now().Format("2006-01-02"), local: local, ctx: ctx, commitsShown: map[string][]string{}}
	facts, _, err := fact.Walk(s.Dir)
	if err != nil {
		return nil, err
	}
	a.facts = facts
	if local {
		lf, _ := overlay.Load(overlay.Dir())
		a.facts = append(a.facts, lf...)
	}
	a.catalog = knownCatalog(home, s.Dir, local)
	a.checker = newChecker(ctx, home, s.Dir, a.catalog)
	a.storeRecs, _ = record.Walk(s.Dir)
	a.recs = storeRecords(s.Dir)
	if local {
		a.localRecs = localRecords()
	}
	state := platform.StateDir(home)
	a.reg = filepath.Join(state, repoid.RegistryFile)
	a.commits = repoid.LoadCommitCache(filepath.Join(state, repoid.CommitCacheFile))
	a.shared = repoid.StoreEvidence(s.Dir)
	a.tools = dream.LoadToolUsage(filepath.Join(state, dream.ToolsFile))
	a.ledgerBytes, _ = os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
	if local {
		lq, _ := os.ReadFile(filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.QuotesRel)))
		a.ledgerBytes = append(append([]byte(nil), a.ledgerBytes...), lq...)
	}
	if c, err := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false); err == nil {
		a.corpus = c
	} else if !errors.Is(err, errNoOwnerWords) {
		return nil, fmt.Errorf("owner history: %w", err)
	}
	a.env = curateEnv(s.Dir, local)
	a.known = map[string]bool{}
	for _, f := range a.facts {
		if f.Meta.Source.Session != "" {
			a.known[f.Meta.Source.Session] = true
		}
		a.known["note:"+f.Meta.Name] = true
	}
	for _, q := range ledger.Quotes(a.ledgerBytes) {
		a.known[record.CiteID(q.From)] = true
	}
	for _, ws := range repoid.LoadRegistryWork(a.reg) {
		for _, w := range ws {
			a.known[w.Session] = true
		}
	}
	for id := range a.tools.Sessions {
		a.known[id] = true
	}
	return a, nil
}

// knownCite reports a citation a why may rest on.
func (a *assessor) knownCite(c string) bool {
	if strings.HasPrefix(c, "note:") {
		return a.known[c]
	}
	return a.known[record.CiteID(c)]
}

// knownCommit reports a commit a why cites that the folder's evidence
// listed as one of its own (a prefix of seven or more characters either way).
func (a *assessor) knownCommit(folder, sha string) bool {
	if len(sha) < 7 {
		return false
	}
	sha = strings.ToLower(sha)
	for _, h := range a.commitsShown[folder] {
		if strings.HasPrefix(h, sha) || strings.HasPrefix(sha, h) {
			return true
		}
	}
	return false
}

// recordEnv is record.Validate's view: the records with pending ones, the
// ledger with pending lines.
func (a *assessor) recordEnv(pending map[string]*record.Record, pendingLedger []byte) record.Env {
	recs := map[string]*record.Record{}
	for f, r := range a.recs {
		recs[f] = r
	}
	for f, r := range pending {
		recs[f] = r
	}
	return record.Env{
		Today: a.today, Records: recs, KnownSession: a.knownCite, KnownCommit: a.knownCommit,
		QuoteBacked: ledger.Backing(append(append([]byte(nil), a.ledgerBytes...), pendingLedger...)),
		ToolUsed:    a.tools.Used,
		KnownRepo:   a.env.KnownRepo,
		HoldsFacts:  a.holdsFacts,
	}
}

// holdsFacts reports a folder with curated facts (live or not).
func (a *assessor) holdsFacts(folder string) bool {
	if a.factFolders == nil {
		a.factFolders = map[string]bool{}
		for _, f := range a.facts {
			if !f.InInbox() && !overlay.IsInbox(f.Path) {
				a.factFolders[f.Meta.Scope] = true
			}
		}
	}
	return a.factFolders[folder]
}

// candidates are the folders to decide this run (SPEC.md, "Folder
// records"): every repository of the catalog with no record, every project
// folder with facts and no record, and every record whose evidence changed
// since its last decision (new work after a dormant or dead status, the
// checkout gone), at most max, a repository's own name before a folder that
// is another name of one.
func (a *assessor) candidates(max int, only map[string]bool) []assess.Candidate {
	seen := map[string]bool{}
	var out []assess.Candidate
	consider := func(folder, reason string) {
		if seen[folder] || !record.IsFolder(folder) || (only != nil && !only[folder]) {
			return
		}
		name := strings.TrimPrefix(folder, "projects/")
		if a.recs[folder] != nil || record.RepoOwner(a.recs, name) != "" {
			return // a folder with a record: the records' turn below
		}
		seen[folder] = true
		out = append(out, a.candidate(folder, reason, nil))
	}
	for _, n := range a.catalog.Names() {
		consider("projects/"+n, "a repository of the catalog (this machine's or another's) with no record")
	}
	folders := map[string]int{}
	for _, f := range a.facts {
		if !f.InInbox() && !overlay.IsInbox(f.Path) && strings.HasPrefix(f.Meta.Scope, "projects/") {
			folders[f.Meta.Scope]++
		}
	}
	for _, f := range sortedKeysInt(folders) {
		consider(f, fmt.Sprintf("a project folder with %d fact(s) and no record", folders[f]))
	}
	// Records whose evidence changed materially.
	for _, f := range sortedRecordFolders(a.recs) {
		r := a.recs[f]
		if seen[f] || (only != nil && !only[f]) || r.Kind != record.KindRepo && r.Kind != record.KindFolder || strings.HasPrefix(r.SuppressedBy, "orgs/") {
			continue // an org's folder is code's (orgFolders)
		}
		c := a.candidate(f, "", r)
		ev := c.Evidence
		if a.local && a.storeRecs[f] != nil {
			// The store's record, read in: this machine checks it against its
			// own work once, then again on a material change of that work,
			// and keeps its answer in the overlay (record.AsOverride).
			why := ""
			if lr := a.localRecs[f]; lr != nil {
				why = materialChange(lr, ev, a.host)
			} else if hostWorked(ev, a.host) {
				why = "the store's record, not yet checked against this machine's work"
			}
			if why != "" {
				seen[f] = true
				c.Reason = why
				out = append(out, c)
			}
			continue
		}
		if why := materialChange(r, ev, a.host); why != "" {
			seen[f] = true
			c.Reason = why
			if a.held != nil && r.Live() && r.Status == record.StatusNeedsReview {
				st := a.held.Review[f]
				a.deepen(&c, st.Attempts+1, st.Keys, st.At)
			}
			out = append(out, c)
			continue
		}
		// A folder left for review is worked on, not parked: each run it is
		// asked again with more evidence than it had (assess_deep.go), and it
		// waits only when that wider search found nothing new, until the
		// weekly retry.
		if a.held != nil && r.Live() && r.Status == record.StatusNeedsReview {
			if why, ok := a.reviewDue(&c); ok {
				seen[f] = true
				c.Reason = why
				out = append(out, c)
			}
		}
	}
	groupSame(out)
	assess.SortCandidates(out, func(folder string) string {
		return a.catalog.AliasOf(strings.TrimPrefix(folder, "projects/"))
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// candidate is folder as the model is asked about it: code's evidence, the
// check of its name as a repository, and, when that name exists nowhere or
// is an old one, the real repositories code proposes it belongs to.
func (a *assessor) candidate(folder, reason string, old *record.Record) assess.Candidate {
	ev, git, lines := a.evidence(folder)
	c := assess.Candidate{Folder: folder, Reason: reason, Old: old, GitRoot: git, Evidence: ev, Context: lines}
	if name, ok := strings.CutPrefix(folder, "projects/"); ok {
		rc := a.checker.Repo(a.ctx, name)
		c.Name = &rc
		c.Evidence = append(c.Evidence, record.Evidence{Gathered: a.today, Checks: rc.Results})
		c.Pattern = assess.NamePattern(name)
		c.Same = a.checker.Same(name)
		c.Canonical, c.CanonicalWhy = a.canonicalName(c)
		if rc.Verdict != check.Holds || rc.Renamed != "" {
			c.Real, c.ProposedKey = a.realRepos(folder, name, rc)
			c.Context = append(c.Context, a.namedElsewhere(folder, name, 3)...)
		}
	}
	return c
}

// projectFolders are the projects/ folders that hold curated facts.
func (a *assessor) projectFolders() map[string]bool {
	out := map[string]bool{}
	for _, f := range a.facts {
		if !f.InInbox() && !overlay.IsInbox(f.Path) && strings.HasPrefix(f.Meta.Scope, "projects/") {
			out[f.Meta.Scope] = true
		}
	}
	return out
}

// namedElsewhere are up to n live facts outside folder whose words name the
// folder's name as a whole word, newest first: evidence of what it is that
// sits in other folders (the owner's rule: run searches, the evidence may be
// elsewhere).
func (a *assessor) namedElsewhere(folder, name string, n int) []string {
	var hits []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope == folder || f.InInbox() || overlay.IsInbox(f.Path) || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		if record.WholeWord(strings.ToLower(f.Meta.Description+"\n"+f.Body), name) {
			hits = append(hits, f)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Meta.Created > hits[j].Meta.Created })
	var out []string
	for i, f := range hits {
		if i == n {
			break
		}
		out = append(out, fmt.Sprintf("  - [[%s]] (%s, %s): %s", f.Meta.Name, f.Meta.Scope, f.Meta.Created, fact.ClipDescription(f.Meta.Description, 200)))
	}
	if len(out) > 0 {
		out = append([]string{fmt.Sprintf("facts in other folders that name %s (%d, newest first):", name, len(hits))}, out...)
	}
	return out
}

// maxReal bounds the repositories proposed for one folder.
const maxReal = 5

// realRepos are the repositories that exist by code's check and that the
// evidence says folder's facts belong to: the forge's current name of a
// renamed one, the checkouts the sessions that filed its notes worked in,
// the repositories its facts' paths are in or whose remotes they name, and
// near names. Best first, at most maxReal; and, sorted, every name the
// evidence the live file system does not move named (the fingerprint's).
func (a *assessor) realRepos(folder, name string, rc check.RepoCheck) ([]assess.RealRepo, []string) {
	score := map[string]int{}
	why := map[string][]string{}
	stable := map[string]bool{} // named by evidence the live file system does not move
	note := func(n, w string, s int) {
		n = strings.ToLower(n)
		if n == "" || n == name {
			return
		}
		score[n] += s
		why[n] = append(why[n], w)
	}
	noteStable := func(n, w string, s int) {
		note(n, w, s)
		stable[strings.ToLower(n)] = true
	}
	if rc.Renamed != "" {
		noteStable(rc.Renamed, "the forge's current name of "+name, 1000)
	}
	sessions := map[string]string{}
	var live []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope != folder {
			continue
		}
		if id := f.Meta.Source.Session; id != "" {
			sessions[id] = f.Meta.Created
		}
		if !f.InInbox() && !overlay.IsInbox(f.Path) && f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			live = append(live, f)
		}
	}
	for n, k := range a.sessionCounts(sessions) {
		noteStable(n, fmt.Sprintf("%d of %d session(s) that filed its notes worked in its checkout", k, len(sessions)), 100*k)
	}
	paths, remotes, applies := map[string]int{}, map[string]int{}, map[string]int{}
	cache := repoid.NewCache(a.home)
	cache.Mains = a.catalog.Mains()
	for _, f := range live {
		text := f.Meta.Description + "\n" + f.Body + "\n" + strings.Join(f.Meta.AppliesTo.Paths, "\n") + "\n" + strings.Join(f.Meta.AppliesTo.Commands, "\n")
		inPaths, inRemotes := map[string]bool{}, map[string]bool{}
		for _, m := range machinePathRx.FindAllStringSubmatch(text, 20) {
			p := strings.TrimRight(m[1], ".,;:)")
			if r, ok := cache.Of(p); ok && r.Slug() != "" {
				inPaths[r.Slug()] = true
			}
		}
		for _, ref := range check.Refs(text) {
			if ref.Kind == check.KindRepo {
				inRemotes[ref.Value] = true
			}
		}
		for n := range inPaths {
			paths[n]++
		}
		for n := range inRemotes {
			remotes[n]++
		}
		seenRepo := map[string]bool{}
		for _, rp := range f.Meta.AppliesTo.Repos {
			n := strings.ToLower(rp)
			if own := a.catalog.AliasOf(n); own != "" {
				n = own
			} else if o := record.RepoOwner(a.recs, n); o != "" {
				n = strings.TrimPrefix(o, "projects/")
			}
			if !seenRepo[n] {
				seenRepo[n] = true
				applies[n]++
			}
		}
	}
	for n, k := range applies {
		noteStable(n, fmt.Sprintf("%d of its %d live fact(s) name it in applies_to.repos", k, len(live)), 20*k)
	}
	for n, k := range paths {
		note(n, fmt.Sprintf("%d of its %d live fact(s) name paths in its checkout", k, len(live)), 20*k)
	}
	for n, k := range remotes {
		noteStable(n, fmt.Sprintf("%d of its live fact(s) name its remote", k), 20*k)
	}
	pool := map[string]bool{}
	for _, n := range a.catalog.Names() {
		pool[n] = true
	}
	for f, r := range a.recs {
		if record.Broad(r.Kind) == "project" && strings.HasPrefix(f, "projects/") {
			pool[strings.TrimPrefix(f, "projects/")] = true
		}
	}
	// The store's project folders too: one may be the real repository with
	// no checkout here and no record yet (its name checked like any other).
	for f := range a.projectFolders() {
		pool[strings.TrimPrefix(f, "projects/")] = true
	}
	for n := range pool {
		if d, ok := assess.Near(name, n); ok {
			note(n, fmt.Sprintf("a near name (edit distance %d)", d), max(1, 10-d))
		}
	}
	var out []assess.RealRepo
	keys := make([]string, 0, len(score))
	for k := range score {
		keys = append(keys, k)
	}
	sort.Strings(keys) // one order every run: the same evidence renders the same
	for _, k := range keys {
		sc := score[k]
		// The live folder that holds it now.
		n, folderN := k, "projects/"+k
		ws := append([]string(nil), why[k]...)
		if w, _, err := record.Resolve(a.recs, folderN); err == nil && w != folderN {
			if !strings.HasPrefix(w, "projects/") {
				continue
			}
			ws = append(ws, k+" is suppressed into "+w)
			n, folderN = strings.TrimPrefix(w, "projects/"), w
		}
		if n == name || a.checker.Repo(a.ctx, n).Verdict != check.Holds {
			continue
		}
		if r := a.recs[folderN]; r != nil {
			ws = append(ws, "record: "+r.Kind+", "+r.Status)
		} else {
			ws = append(ws, "no record yet")
		}
		merged := false
		for i := range out {
			if out[i].Name == n {
				out[i].Score += sc
				out[i].Why = append(out[i].Why, ws...)
				merged = true
			}
		}
		if !merged {
			out = append(out, assess.RealRepo{Name: n, Why: ws, Score: sc})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxReal {
		out = out[:maxReal]
	}
	return out, sortedKeysBool(stable)
}

func sortedKeysBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// groupSame names each set of candidates code proved are one repository
// (Candidate.Same, taken both ways) by its first name, so they sort into one
// batch.
func groupSame(cands []assess.Candidate) {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		parent[x] = x
		return x
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if rb < ra {
				ra, rb = rb, ra
			}
			parent[rb] = ra
		}
	}
	in := map[string]bool{}
	for _, c := range cands {
		if n, ok := strings.CutPrefix(c.Folder, "projects/"); ok {
			in[n] = true
		}
	}
	for _, c := range cands {
		n, ok := strings.CutPrefix(c.Folder, "projects/")
		if !ok {
			continue
		}
		for _, r := range c.Same {
			if in[r.Value] {
				union(n, r.Value)
			}
		}
	}
	size := map[string]int{}
	for n := range in {
		size[find(n)]++
	}
	for i := range cands {
		if n, ok := strings.CutPrefix(cands[i].Folder, "projects/"); ok && size[find(n)] > 1 {
			cands[i].Group = find(n)
		}
	}
}

// hostWorked reports evidence that host has the folder as a checkout or
// worked in it.
func hostWorked(ev []record.Evidence, host string) bool {
	for _, e := range ev {
		if e.Host == host && (e.GitRoot || e.WorkSessions > 0) {
			return true
		}
	}
	return false
}

// materialChange says why a record's decision is worth asking again: this
// machine's evidence shows work after a dormant or dead status, or the
// checkout it recorded is gone. "" for none. A record whose evidence is
// unchanged is never asked again.
func materialChange(r *record.Record, now []record.Evidence, host string) string {
	var old, cur *record.Evidence
	for i := range r.Evidence {
		if r.Evidence[i].Host == host {
			old = &r.Evidence[i]
		}
	}
	for i := range now {
		if now[i].Host == host {
			cur = &now[i]
		}
	}
	if cur == nil || old == nil || len(r.Was) == 0 {
		return ""
	}
	if old.GitRoot && !cur.GitRoot {
		return "the checkout the record names is gone from this machine"
	}
	if (r.Status == record.StatusDormant || r.Status == record.StatusDead) && cur.WorkSessions > old.WorkSessions && cur.LastWork > r.Was[0].Date {
		return fmt.Sprintf("new work (%d session(s), last %s) since the %s status of %s", cur.WorkSessions-old.WorkSessions, cur.LastWork, r.Status, r.Was[0].Date)
	}
	return ""
}

// evidence gathers what code knows of a project folder: this machine's
// checkout (git root, remotes, own commits, work sessions), every other
// machine's catalog entry, a non-git folder's work, the store's facts; and
// the lines the model reads besides.
func (a *assessor) evidence(folder string) (ev []record.Evidence, gitRoot bool, lines []string) {
	name := strings.TrimPrefix(folder, "projects/")
	if r, ok := a.catalog.Repo(name); ok && r.Root != "" {
		e := record.Evidence{Host: a.host, Gathered: a.today, GitRoot: true, Checkout: "main", Remotes: r.URLs}
		if r.Clone != "" {
			e.Checkout = "clone"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if cm, ok := a.commits.Get(ctx, r.Root); ok {
			n := cm.Own
			e.OwnCommits, e.FirstCommit, e.LastCommit = &n, cm.First, cm.Last
		}
		cancel()
		work := a.catalog.Work(r.Root)
		e.WorkSessions, e.FirstWork, e.LastWork = repoid.WorkSummary(work)
		e.UpstreamClone = e.OwnCommits != nil && *e.OwnCommits == 0 && e.WorkSessions == 0
		ev = append(ev, e)
		gitRoot = true
		if len(r.Aliases) > 0 {
			lines = append(lines, "names: this checkout is also known as "+strings.Join(r.Aliases, ", ")+" (a symlink's or a remote's name, repoid)")
		}
		if len(r.Remotes) > 0 {
			lines = append(lines, "other remotes' names: "+strings.Join(r.Remotes, ", "))
		}
		if ss := lastWork(work, 6); len(ss) > 0 {
			lines = append(lines, "sessions that worked in it here (oldest first): "+strings.Join(ss, ", "))
		}
		if l := a.ownCommitsLine(folder, r.Root); l != "" {
			lines = append(lines, l)
		}
	} else if a.host != "" {
		// A folder of the projects directory: a non-git project folder, or nothing.
		var paths []string
		var work []repoid.Work
		for p, ws := range repoid.FolderWork(a.reg) {
			if filepath.Base(p) == name {
				paths = append(paths, homeRel(a.home, p))
				work = append(work, ws...)
			}
		}
		dir := filepath.Join(projectsRoot(a.home), name)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() && len(paths) == 0 {
			paths = append(paths, homeRel(a.home, dir))
		}
		if len(paths) > 0 {
			sort.Strings(paths)
			e := record.Evidence{Host: a.host, Gathered: a.today, Paths: paths}
			e.WorkSessions, e.FirstWork, e.LastWork = repoid.WorkSummary(work)
			ev = append(ev, e)
			if ss := lastWork(work, 6); len(ss) > 0 {
				lines = append(lines, "sessions that worked in it here (oldest first): "+strings.Join(ss, ", "))
			}
		}
	}
	if own := a.catalog.AliasOf(name); own != "" {
		lines = append(lines, fmt.Sprintf("names: repoid says %s is another name of the checkout %s on this machine (a symlink's or a remote's name)", name, own))
	}
	for _, h := range a.shared[name] {
		if h.Host == a.host {
			continue
		}
		e := record.Evidence{Host: h.Host, Gathered: a.today, GitRoot: true, Remotes: h.URLs, WorkSessions: h.WorkSessions, FirstWork: h.FirstWork, LastWork: h.LastWork,
			OwnCommits: h.OwnCommits, FirstCommit: h.FirstCommit, LastCommit: h.LastCommit}
		e.UpstreamClone = e.OwnCommits != nil && *e.OwnCommits == 0 && e.WorkSessions == 0
		ev = append(ev, e)
		gitRoot = true
		if len(h.Aliases) > 0 {
			lines = append(lines, fmt.Sprintf("names on %s: also known as %s", h.Host, strings.Join(h.Aliases, ", ")))
		}
	}
	// The store's facts and notes filed there.
	sessions := map[string]string{} // id -> latest day
	live, gone, rulings := 0, 0, 0
	first, last := "", ""
	var newest []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope != folder {
			continue
		}
		if id := f.Meta.Source.Session; id != "" {
			if d := f.Meta.Created; d > sessions[id] {
				sessions[id] = d
			}
		}
		if d := f.Meta.Created; d != "" {
			if first == "" || d < first {
				first = d
			}
			if d > last {
				last = d
			}
		}
		if f.InInbox() || overlay.IsInbox(f.Path) {
			continue
		}
		if f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			gone++
			continue
		}
		live++
		if fact.IsRuling(f) {
			rulings++
		}
		newest = append(newest, f)
	}
	if len(sessions) > 0 {
		ev = append(ev, record.Evidence{Gathered: a.today, FactSessions: len(sessions), FirstFact: first, LastFact: last})
		ids := make([]string, 0, len(sessions))
		for id := range sessions {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if sessions[ids[i]] != sessions[ids[j]] {
				return sessions[ids[i]] > sessions[ids[j]]
			}
			return ids[i] < ids[j] // one order every run
		})
		if len(ids) > 6 {
			ids = ids[:6]
		}
		var cites []string
		for _, id := range ids {
			cites = append(cites, "session:"+id+" "+sessions[id])
		}
		lines = append(lines, "sessions whose notes are filed here (newest first): "+strings.Join(cites, ", "))
		if w := a.sessionsWorkedIn(sessions, folder); w != "" {
			lines = append(lines, "those sessions worked in (the dreamer's registry): "+w)
		}
	}
	if live+gone > 0 {
		sort.Slice(newest, func(i, j int) bool {
			if newest[i].Meta.Created != newest[j].Meta.Created {
				return newest[i].Meta.Created > newest[j].Meta.Created
			}
			return newest[i].Meta.Name < newest[j].Meta.Name
		})
		l := fmt.Sprintf("facts: %d live (%d owner rulings), %d superseded or retired", live, rulings, gone)
		lines = append(lines, l)
		for i, f := range newest {
			if i == 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("  - %s (%s): %s", f.Meta.Created, f.Meta.Type, fact.ClipDescription(f.Meta.Description, 160)))
		}
	}
	lines = append(lines, a.ownerWords(name)...)
	return ev, gitRoot, lines
}

// sessionCounts is, for each repository (or project folder) name, how many
// of the given sessions worked there, by the dreamer's registry.
func (a *assessor) sessionCounts(sessions map[string]string) map[string]int {
	if a.sessionRoots == nil {
		a.sessionRoots = map[string][]string{}
		for root, ws := range repoid.LoadRegistryWork(a.reg) {
			name := filepath.Base(root)
			if r, ok := a.catalog.Of(root); ok {
				name = r.Name
			}
			for _, w := range ws {
				a.sessionRoots[w.Session] = append(a.sessionRoots[w.Session], strings.ToLower(name))
			}
		}
	}
	count := map[string]int{}
	for id := range sessions {
		seen := map[string]bool{}
		for _, n := range a.sessionRoots[id] {
			if !seen[n] {
				seen[n] = true
				count[n]++
			}
		}
	}
	return count
}

// ownCommitsShown bounds the own commits an evidence lists.
const ownCommitsShown = 5

// ownCommitsLine lists the checkout's newest commits under this machine's
// git identity, for a why to cite as commit:<sha> (knownCommit), their
// subjects redacted (left out when redaction fails); "" for none.
func (a *assessor) ownCommitsLine(folder, root string) string {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	cs := repoid.RecentOwnCommits(ctx, root, ownCommitsShown)
	if len(cs) == 0 {
		return ""
	}
	subj := make([]string, len(cs))
	for i, c := range cs {
		subj[i] = clipOne(c.Subject, 100)
	}
	red, err := digest.RedactAll(subj)
	var parts []string
	for i, c := range cs {
		a.commitsShown[folder] = append(a.commitsShown[folder], strings.ToLower(c.Hash))
		if err == nil {
			parts = append(parts, fmt.Sprintf("commit:%s %s %q", c.Hash, c.Day, red[i]))
		} else {
			parts = append(parts, fmt.Sprintf("commit:%s %s", c.Hash, c.Day))
		}
	}
	return "own commits under this machine's git identity, newest first (cite as commit:<sha>): " + strings.Join(parts, ", ")
}

// sessionsWorkedIn says where the given sessions did their work, by the
// dreamer's registry: each checkout's repository name (or a project
// folder's name) with how many of the sessions worked there, most first;
// "" when the registry knows none of them. A folder whose notes all came
// from sessions in another project's checkout is that project's.
func (a *assessor) sessionsWorkedIn(sessions map[string]string, folder string) string {
	count := a.sessionCounts(sessions)
	names := make([]string, 0, len(count))
	for n := range count {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if count[names[i]] != count[names[j]] {
			return count[names[i]] > count[names[j]]
		}
		return names[i] < names[j]
	})
	var parts []string
	for i, n := range names {
		if i == 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d of %d)", n, count[n], len(sessions)))
	}
	return strings.Join(parts, ", ")
}

// ownerWords are the owner's words that name the folder: lines of the quote
// ledger, then turns of theirs (redacted, cut), at most three each. A turn
// that cannot be redacted is left out (fail closed).
func (a *assessor) ownerWords(name string) []string {
	var out []string
	var qs []ledger.Quote
	for _, q := range ledger.Quotes(a.ledgerBytes) {
		if record.WholeWord(strings.ToLower(q.Quote), name) {
			qs = append(qs, q)
		}
	}
	// Newest first, in one order every run (the ledger is read into a map).
	sort.Slice(qs, func(i, j int) bool {
		if qs[i].At != qs[j].At {
			return qs[i].At > qs[j].At
		}
		if qs[i].From != qs[j].From {
			return qs[i].From < qs[j].From
		}
		return qs[i].Quote < qs[j].Quote
	})
	for _, q := range qs {
		dup := false
		for _, l := range out {
			dup = dup || strings.HasSuffix(l, fmt.Sprintf("%q", q.Quote))
		}
		if dup {
			continue
		}
		if q.Words {
			// A statement of theirs (said once) or other words of theirs a fact
			// holds: evidence, strong but soft.
			out = append(out, fmt.Sprintf("owner's statement, said once (quote ledger, %s, %s): %q", q.From, q.At, q.Quote))
		} else {
			out = append(out, fmt.Sprintf("owner's words (quote ledger, %s): %q", q.From, q.Quote))
		}
		if len(out) == 3 {
			break
		}
	}
	if a.corpus == nil {
		return out
	}
	ms := a.corpus.Mentions(name, 3)
	if len(ms) == 0 {
		return out
	}
	texts := make([]string, len(ms))
	for i, m := range ms {
		texts[i] = clipAround(m.Text, name, 500)
	}
	red, err := digest.RedactAll(texts)
	if err != nil {
		return out
	}
	for i, m := range ms {
		out = append(out, fmt.Sprintf("owner's turn (%s): %q", m.Ref, red[i]))
	}
	return out
}

// clipAround cuts text to about max bytes around the first place name is.
func clipAround(text, name string, max int) string {
	t := strings.Join(strings.Fields(text), " ")
	if len(t) <= max {
		return t
	}
	i := strings.Index(strings.ToLower(t), strings.ToLower(name))
	start := max/2 - len(name)
	if i < 0 || i < start {
		return t[:max] + "…"
	}
	from := i - start
	end := from + max
	if end > len(t) {
		end = len(t)
	}
	return "…" + t[from:end] + "…"
}

func lastWork(ws []repoid.Work, n int) []string {
	if len(ws) > n {
		ws = ws[len(ws)-n:]
	}
	var out []string
	for _, w := range ws {
		out = append(out, fmt.Sprintf("session:%s %s (%s)", w.Session, w.Seen, w.By))
	}
	return out
}

func homeRel(home, p string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

func sortedKeysInt(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedRecordFolders(m map[string]*record.Record) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// verifyOwner checks a decision's quote of the owner: in one of their
// turns (the ref it names), stated, not asked or negated. It returns the
// owner's entry and the quote ledger line that records it, or a problem.
func (a *assessor) verifyOwner(folder string, d assess.Decision) (*assess.Owner, []byte, string) {
	q, from := strings.TrimSpace(d.OwnerQuote), strings.TrimSpace(d.OwnerQuoteFrom)
	if q == "" && from == "" {
		return nil, nil, ""
	}
	if q == "" || from == "" {
		return nil, nil, "owner_quote and owner_quote_from go together"
	}
	// The words of a live ruling or decision of theirs that the quote ledger
	// backs (a promoted statement, a ruling from another machine's session)
	// are theirs without this machine's history.
	if hash, ok := a.ledgerDecision(q, from); ok {
		line := ledger.Quote{Name: ledger.RecordQuoteName(folder), Quote: q, From: from, TurnHash: hash, At: a.today}.Line()
		return &assess.Owner{Quote: q, From: from}, line, ""
	}
	if a.corpus == nil {
		return nil, nil, "no owner history on this machine to check owner_quote against: leave it out and cite the words in why"
	}
	m, ok := a.corpus.Find(q, record.CiteID(from), true)
	if !ok || !m.Clean() || m.Ref != from {
		return nil, nil, "owner_quote is not the owner's statement in " + from + ": copy their words exactly, or leave it out and cite them in why"
	}
	line := ledger.Quote{Name: ledger.RecordQuoteName(folder), Quote: q, From: from, TurnHash: m.TurnHash, At: a.today}.Line()
	return &assess.Owner{Quote: q, From: from}, line, ""
}

// ledgerDecision reports words of the owner's that a live ruling or decision
// of the store (or the layer) holds as its quote, recorded in the quote
// ledger for it, with the turn hash the ledger keeps.
func (a *assessor) ledgerDecision(q, from string) (string, bool) {
	norm := strings.Join(strings.Fields(q), " ")
	byName := map[string]*fact.Fact{}
	for _, f := range a.facts {
		byName[f.Meta.Name] = f
	}
	for _, l := range ledger.Quotes(a.ledgerBytes) {
		if l.Words || l.From != from || strings.Join(strings.Fields(l.Quote), " ") != norm {
			continue
		}
		f := byName[l.Name]
		if f != nil && liveFact(f) && (fact.IsRuling(f) || overlay.IsRuling(f)) && strings.Join(strings.Fields(f.Meta.Source.OwnerQuote), " ") == norm {
			return l.TurnHash, true
		}
	}
	return "", false
}

// decided is one accepted decision.
type decided struct {
	c    assess.Candidate
	rec  *record.Record // the record to write, nil for none (a rejected proposal)
	old  *record.Record // the record it replaces (the candidate's, or a folded-into one's)
	line []byte         // its quote ledger line, if the owner's words back it
	// A proposal's: the facts that move into the folder, and the folder
	// log's close lines.
	moves  []moveItem
	closes []folderEntry
}

// check judges an answer's decisions in candidate order, accepting each
// valid one into pending (so a later folder may be suppressed into an
// earlier one): it returns the accepted and the refused.
func (a *assessor) check(cands []assess.Candidate, ds []assess.Decision, pending map[string]*record.Record, pendingLedger *[]byte) (acc []decided, refused []assess.Refusal) {
	byFolder := map[string]assess.Decision{}
	for _, d := range ds {
		f := strings.TrimSpace(d.Folder)
		known := false
		for _, c := range cands {
			known = known || c.Folder == f
		}
		if !known {
			if pending[f] == nil && a.recs[f] == nil { // a folder decided earlier in the run is no refusal
				refused = append(refused, assess.Refusal{Folder: f, Problems: []string{"names no folder of this batch"}})
			}
			continue
		}
		byFolder[f] = d
	}
	// Decisions that suppress a folder into another come after the others,
	// so a folder may fold into one of its batch decided after it.
	ordered := make([]assess.Candidate, 0, len(cands))
	for _, pass := range []bool{false, true} {
		for _, c := range cands {
			if d, ok := byFolder[c.Folder]; ok && (strings.TrimSpace(d.SuppressedBy) != "") == pass {
				ordered = append(ordered, c)
			}
		}
	}
	for _, c := range ordered {
		d := byFolder[c.Folder]
		var ps []string
		switch d.Kind {
		case record.KindRepo, record.KindFolder, assess.KindNone:
			if c.Kind != "" {
				ps = append(ps, "kind: this is a proposed "+c.Kind+": answer "+c.Kind+", or reject or fold_into")
			}
		case record.KindArea, record.KindTopic:
			if c.Kind != d.Kind {
				ps = append(ps, "kind: "+d.Kind+" is for a proposed "+d.Kind)
			}
		default:
			ps = append(ps, fmt.Sprintf("kind: not repo, folder or none (got %q)", d.Kind))
		}
		if c.Kind != "" && (d.Reject || d.FoldInto != "") {
			if strings.TrimSpace(d.Why) == "" {
				refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: []string{"why: empty"}})
				continue
			}
			if d.Reject {
				acc = append(acc, decided{c: c, closes: closeProposals(c, "rejected: "+clipOne(d.Why, 300), a.today)})
				continue
			}
			into := strings.TrimSpace(d.FoldInto)
			target := pending[into]
			if target == nil {
				target = a.recs[into]
			}
			if target == nil || !target.Live() || (target.Kind != record.KindArea && target.Kind != record.KindTopic) {
				refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: []string{"fold_into: names no live area or topic"}})
				continue
			}
			rec := target.Clone()
			name := c.Folder[strings.LastIndex(c.Folder, "/")+1:]
			if name != rec.Name && !slices.Contains(rec.Aliases, name) {
				rec.Aliases = append(rec.Aliases, name)
			}
			if vps := record.Validate(target, rec, record.ByOpus, a.recordEnv(pending, *pendingLedger)); len(vps) > 0 {
				refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: messages(vps)})
				continue
			}
			pending[into] = rec
			acc = append(acc, decided{c: c, rec: rec, old: target, moves: proposalMoves(c, into), closes: closeProposals(c, "folded into "+into, a.today)})
			continue
		}
		owner, line, op := a.verifyOwner(c.Folder, d)
		if op != "" {
			ps = append(ps, op)
		}
		// Dry claims: what code checked refutes the decision, it is refused
		// with the check's output.
		if eps := assess.Enforce(c, d, assess.Checked{
			Repo: func(n string) check.RepoCheck { return a.checker.Repo(a.ctx, n) },
			Record: func(f string) *record.Record {
				if r := pending[f]; r != nil {
					return r
				}
				return a.recs[f]
			},
		}); len(eps) > 0 {
			a.checkRefused++
			ps = append(ps, eps...)
		}
		ps = append(ps, a.checkRoots(d)...)
		if len(ps) > 0 {
			refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: ps})
			continue
		}
		rec := assess.Build(c, d, a.today, owner)
		env := a.recordEnv(pending, append(append([]byte(nil), *pendingLedger...), line...))
		if vps := record.Validate(c.Old, rec, record.ByOpus, env); len(vps) > 0 {
			refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: messages(vps)})
			continue
		}
		pending[c.Folder] = rec
		*pendingLedger = append(*pendingLedger, line...)
		dd := decided{c: c, rec: rec, old: c.Old, line: line}
		if c.Kind != "" {
			dd.moves, dd.closes = proposalMoves(c, c.Folder), closeProposals(c, "made "+c.Folder, a.today)
		}
		acc = append(acc, dd)
	}
	return a.waitForSame(acc, refused, pending)
}

// waitForSame takes back a live decision for one name of a repository when
// another of its names was refused in the same answer and has no record:
// the names of one repository are decided together, so the one refused
// now cannot end up folded into a name decided without it.
func (a *assessor) waitForSame(acc []decided, refused []assess.Refusal, pending map[string]*record.Record) ([]decided, []assess.Refusal) {
	open := map[string]string{} // refused folder with no record -> its problems
	for _, r := range refused {
		if pending[r.Folder] == nil && a.recs[r.Folder] == nil {
			open[r.Folder] = strings.Join(r.Problems, " ")
		}
	}
	if len(open) == 0 {
		return acc, refused
	}
	var keep []decided
	for _, d := range acc {
		var waiting []string
		if d.rec != nil && d.rec.Live() && d.rec.Folder == d.c.Folder {
			for _, r := range d.c.Same {
				// Not one refused because this decision is the live folder.
				if p, ok := open["projects/"+r.Value]; ok && !strings.Contains(p, "same repository as "+d.c.Folder+",") && !strings.Contains(p, "same repository as "+d.c.Folder+" ") {
					waiting = append(waiting, "projects/"+r.Value)
				}
			}
		}
		if len(waiting) == 0 {
			keep = append(keep, d)
			continue
		}
		delete(pending, d.c.Folder)
		refused = append(refused, assess.Refusal{Folder: d.c.Folder, Problems: []string{fmt.Sprintf("%s is the same repository as %s, whose decision was refused: decide them together (one live folder, the others suppressed into it)", strings.TrimPrefix(d.c.Folder, "projects/"), strings.Join(waiting, ", "))}})
	}
	return keep, refused
}

// checkRoots refuses roots code cannot confirm: a root on another machine
// (code checks only this one's paths), or a path that does not exist here.
// No roots is fine: code fills them in from its evidence.
func (a *assessor) checkRoots(d assess.Decision) []string {
	var ps []string
	for _, rt := range d.Roots {
		if !strings.EqualFold(rt.Host, a.host) {
			ps = append(ps, fmt.Sprintf("roots: %s is another machine's; code checks only this machine's (%s): leave roots out and code fills them in from its evidence", rt.Host, a.host))
			continue
		}
		p := rt.Path
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(a.home, strings.TrimPrefix(p[1:], "/"))
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() || !filepath.IsAbs(p) {
			ps = append(ps, fmt.Sprintf("roots: %s is no folder on %s (checked: it does not exist)", rt.Path, a.host))
		}
	}
	return ps
}

func messages(ps []record.Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Message())
	}
	return out
}

// proposalMoves are the moves of a proposal's facts into folder.
func proposalMoves(c assess.Candidate, folder string) []moveItem {
	var out []moveItem
	for _, f := range c.Facts {
		out = append(out, moveItem{Name: f, To: folder, Renames: map[string]string{}})
	}
	return out
}

// closeProposals are the folder log lines that end a candidate's proposals.
func closeProposals(c assess.Candidate, outcome, today string) []folderEntry {
	var out []folderEntry
	for _, id := range c.Proposals {
		out = append(out, folderEntry{Kind: "close", ID: folderID("close", id), At: today, Closes: id, Outcome: outcome})
	}
	return out
}

// proposalCandidates are the proposals of areas and topics in the folder
// log (the store's, and the overlay's with the local layer) that no line
// closed, grouped by name (lower case; one a record already holds goes to
// that folder), each group put to the model once code counts at least
// record.MinAreaSessions distinct sessions behind it, with the sessions'
// shared tool usage as evidence.
func (a *assessor) proposalCandidates(n int, only map[string]bool) []assess.Candidate {
	if n <= 0 {
		return nil
	}
	log := readFolderLog(a.dir)
	if a.local {
		log = append(log, readFolderLog(filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir)))...)
	}
	closed := closedFolderIDs(log)
	groups := map[string][]folderEntry{}
	var names []string
	for _, e := range log {
		if e.Kind != "propose" || closed[e.ID] != "" || e.Name == "" {
			continue
		}
		k := strings.ToLower(e.Name)
		if groups[k] == nil {
			names = append(names, k)
		}
		groups[k] = append(groups[k], e)
	}
	sort.Strings(names)
	var out []assess.Candidate
	for _, name := range names {
		g := groups[name]
		sessions := map[string]bool{}
		kinds := map[string]int{}
		areas := map[string]int{}
		var ids, facts, descs []string
		seenFact := map[string]bool{}
		for _, e := range g {
			if e.Session != "" {
				sessions[e.Session] = true
			}
			kinds[e.FolderKind]++
			if e.Area != "" {
				areas[e.Area]++
			}
			ids = append(ids, e.ID)
			if !seenFact[e.Fact] {
				seenFact[e.Fact] = true
				facts = append(facts, e.Fact)
			}
			if len(descs) < 5 && e.Description != "" && !slices.Contains(descs, e.Description) {
				descs = append(descs, e.Description)
			}
		}
		if len(sessions) < record.MinAreaSessions {
			continue // waits for a second session
		}
		kind := record.KindArea
		if kinds[record.KindTopic] > kinds[record.KindArea] {
			kind = record.KindTopic
		}
		folder := "areas/" + name
		if kind == record.KindTopic {
			folder = "topics/" + name
			if ar := topKey(areas); ar != "" {
				if r := a.recs["areas/"+ar]; r != nil && r.Live() && r.Kind == record.KindArea {
					folder = "areas/" + ar + "/topics/" + name
				}
			}
		}
		if owner := record.RepoOwner(a.recs, name); owner != "" && a.recs[folder] == nil {
			_ = owner
		}
		if only != nil && !only[folder] {
			continue
		}
		var ss []string
		for s := range sessions {
			ss = append(ss, "session:"+s)
		}
		sort.Strings(ss)
		c := assess.Candidate{Folder: folder, Kind: kind, Sessions: ss, Proposals: ids, Facts: facts, Old: a.recs[folder],
			Reason:   fmt.Sprintf("a proposed %s: %d proposal(s) from %d distinct session(s)", kind, len(g), len(sessions)),
			Evidence: []record.Evidence{{Gathered: a.today, Sessions: ss}}}
		for _, d := range descs {
			c.Context = append(c.Context, "proposed as: "+d)
		}
		for f, r := range a.recs {
			if r.Kind == record.KindArea || r.Kind == record.KindTopic {
				for _, x := range r.Names() {
					if x == name && f != folder {
						c.Context = append(c.Context, fmt.Sprintf("a folder already answers to %s: %s (fold_into it?)", name, f))
					}
				}
			}
		}
		byName := map[string]*fact.Fact{}
		for _, f := range a.facts {
			byName[f.Meta.Name] = f
		}
		for i, fn := range facts {
			if i == 8 {
				c.Context = append(c.Context, fmt.Sprintf("  ... and %d more fact(s)", len(facts)-8))
				break
			}
			if f := byName[fn]; f != nil {
				c.Context = append(c.Context, fmt.Sprintf("  - fact %s (%s): %s", fn, f.Meta.Scope, fact.ClipDescription(f.Meta.Description, 160)))
			}
		}
		c.Context = append(c.Context, a.sharedTools(sessions)...)
		out = append(out, c)
		if len(out) == n {
			break
		}
	}
	return out
}

// sharedTools are the tool names and command programs that two or more of
// the sessions used (this machine's tool usage), as lines for the model.
func (a *assessor) sharedTools(sessions map[string]bool) []string {
	tools, progs := map[string]int{}, map[string]int{}
	for s := range sessions {
		u := a.tools.Sessions[s]
		for _, t := range u.Tools {
			tools[t]++
		}
		for _, p := range u.Commands {
			progs[p]++
		}
	}
	pick := func(m map[string]int) []string {
		var out []string
		for k, v := range m {
			if v >= record.MinAreaSessions {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		if len(out) > 30 {
			out = out[:30]
		}
		return out
	}
	var out []string
	if ts := pick(tools); len(ts) > 0 {
		out = append(out, "tools two or more of its sessions used: "+strings.Join(ts, ", "))
	}
	if ps := pick(progs); len(ps) > 0 {
		out = append(out, "programs two or more of its sessions ran: "+strings.Join(ps, ", "))
	}
	return out
}

func topKey(m map[string]int) string {
	best, n := "", 0
	for k, v := range m {
		if v > n || v == n && k < best {
			best, n = k, v
		}
	}
	return best
}

func clipOne(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// assessStep runs one assessment: candidates in batches, one model call
// each and at most one retry for the refused, every batch's records in one
// commit (or the overlay). It returns the errors.
func assessStep(ctx context.Context, s *store.Store, o assessOpts, rep *curate.Report) int {
	local := localLayer(ctx, s)
	if local && !overlay.Outside(overlay.Dir(), s.Dir) {
		fmt.Fprintln(os.Stderr, "assess:", overlay.ErrInside)
		return 1
	}
	if !local && !o.dry {
		if err := writerCheck(ctx, s, "curator"); err != nil {
			fmt.Fprintln(os.Stderr, "assess: nothing done:", err)
			return 1
		}
	}
	a, err := newAssessor(ctx, s, local)
	if err != nil {
		fmt.Fprintln(os.Stderr, "assess:", err)
		return 1
	}
	a.retry = o.retry
	max := o.max
	if max <= 0 {
		max = assessMax
	}
	held := loadHeld(filepath.Join(platform.StateDir(a.home), assessHeldFile))
	a.held = held
	// Code's own decisions first: the folders named for forge organizations
	// become their orgs' folders.
	errs0 := a.codeSteps(ctx, s, "a forge organization's folder", a.orgFolders(o.only), o.dry, rep)
	all := a.candidates(max+len(held.Folders), o.only)
	rep.ReviewWaiting += a.reviewWaiting
	// A folder refused in assessHoldRuns runs on the same evidence waits
	// until its evidence (what the model reads of it) changes.
	var cands []assess.Candidate
	for _, c := range all {
		a.refresh(&c, nil)
		if h, ok := held.Folders[c.Folder]; ok {
			// A refused folder is asked again with more evidence, and waits
			// only when that found nothing new, until the weekly retry.
			if c.Attempt == 0 {
				n := h.Attempts
				if h.Runs > n {
					n = h.Runs
				}
				a.deepen(&c, n+1, h.Keys, h.At)
			}
			if h.Runs >= assessHoldRuns && h.Fingerprint == assess.Fingerprint(c) && !a.retry && !retryDue(h.At, a.today) {
				rep.AssessHeld++
				a.attempts = append(a.attempts, attemptOf(c, a.today, "refused on the same evidence", "waits"))
				fmt.Fprintf(os.Stderr, "assess: %s waits: refused in %d runs on the same evidence, and its wider search found nothing new; asked again on %s (%s)\n", c.Folder, h.Runs, nextRetry(h.At), clipOne(h.Problems, 300))
				continue
			}
		}
		if len(cands) < max {
			cands = append(cands, c)
		}
	}
	cands = append(cands, a.proposalCandidates(max-len(cands), o.only)...)
	rep.AssessCandidates += len(cands)
	errs := errs0
	pending := map[string]*record.Record{}
	var pendingLedger []byte
	var decidedRun []decidedShape // this run's decisions, for folders alike in later batches
	var refusedRun []refusedItem  // refused twice in this run
	var acceptedRun []decided     // decided in this run
	deferred := map[string]bool{} // waiting for the repository they fold into
	n := 0
	finish := func(acc []decided) {
		for _, d := range acc {
			delete(held.Folders, d.c.Folder)
			if d.rec != nil && d.rec.Folder == d.c.Folder {
				decidedRun = append(decidedRun, decidedShape{shape: d.c.Shape(), absent: d.c.Absent(), rec: d.rec})
				acceptedRun = append(acceptedRun, d)
			}
		}
		// A decision that changes nothing but the evidence still writes the
		// refreshed evidence once; one identical to the record writes nothing.
		var write []decided
		for _, d := range acc {
			if d.old != nil && d.rec != nil && len(d.moves) == 0 && len(d.closes) == 0 {
				ob, _ := d.old.Encode()
				nb, _ := d.rec.Encode()
				if string(ob) == string(nb) {
					continue
				}
			}
			write = append(write, d)
		}
		if o.dry {
			for _, d := range write {
				if d.rec == nil {
					fmt.Printf("- %s: rejected\n", d.c.Folder)
					continue
				}
				fmt.Printf("- %s: %s, %s: %s\n", d.rec.Folder, d.rec.Kind, d.rec.Status, d.rec.Was[0].Why)
			}
			return
		}
		if len(write) == 0 {
			return
		}
		n, err := a.write(ctx, s, write)
		rep.AssessRecords += n
		if err != nil {
			fmt.Fprintln(os.Stderr, "assess: write:", err)
			errs++
		}
		for _, d := range write {
			if d.rec != nil {
				a.recs[d.rec.Folder] = d.rec
			}
		}
		a.ledgerBytes = append(a.ledgerBytes, pendingLedger...)
		pendingLedger = nil
		pending = map[string]*record.Record{}
	}
	for i := 0; i < len(cands); {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "assess:", ctx.Err())
			errs++
			break
		}
		batch := batchOf(cands, i)
		i += len(batch) // before any candidate waits to the end of the run
		n++
		for j := range batch {
			a.refresh(&batch[j], decidedRun)
		}
		// The candidates the checks settle are decided by code; the rest go to
		// the model.
		batch, codeAcc, later := a.codeDecided(batch, pending, &pendingLedger, deferred)
		cands = append(cands, later...)
		if len(codeAcc) > 0 {
			// Code's decisions are a commit of their own, before the model reads
			// the rest of the batch with them among the decisions on folders alike.
			finish(codeAcc)
			for j := range batch {
				a.refresh(&batch[j], decidedRun)
			}
		}
		var acc []decided
		if len(batch) > 0 {
			var refused []assess.Refusal
			var err error
			acc, refused, err = a.askModel(ctx, o, rep, n, batch, acc, pending, &pendingLedger)
			if err != nil {
				fmt.Fprintln(os.Stderr, "assess: model:", err)
				errs++
			}
			if len(refused) > 0 {
				rep.AssessRefused += len(refused)
				for _, r := range refused {
					for _, c := range batch {
						if c.Folder == r.Folder {
							refusedRun = append(refusedRun, refusedItem{c: c, problems: strings.Join(r.Problems, "; ")})
						}
					}
				}
			}
		}
		finish(acc)
	}
	// The refused folders' fingerprints are taken as the next run will see
	// them (the records as this run left them), so a folder whose evidence
	// does not change waits.
	// A folder left for review keeps the fingerprint of its evidence: it is
	// asked again when that changes.
	for _, d := range acceptedRun {
		f := d.c.Folder
		_, had := held.Review[f]
		if d.c.Attempt > 0 || had {
			rep.ReviewRetried++
			if d.rec.Status != record.StatusNeedsReview || !d.rec.Live() {
				rep.ReviewSettled++
			}
			outcome := d.rec.Status
			if d.rec.SuppressedBy != "" {
				outcome += " (suppressed by " + d.rec.SuppressedBy + ")"
			}
			a.attempts = append(a.attempts, attemptOf(d.c, a.today, d.c.Reason, outcome))
			if d.c.NewExtra() > 0 || d.c.Attempt > 0 {
				fmt.Fprintf(os.Stderr, "assess: %s: re-attempt %d with %d extra item(s), %d new: %s\n", f, d.c.Attempt, len(d.c.ExtraKeys()), d.c.NewExtra(), outcome)
			}
		}
		if d.rec.Status == record.StatusNeedsReview && d.rec.Live() {
			held.Review[f] = reviewState{Fingerprint: a.fingerprint(d.c), At: a.today, Attempts: d.c.Attempt, Keys: d.c.ExtraKeys()}
		} else {
			delete(held.Review, f)
		}
	}
	for _, r := range refusedRun {
		c := r.c
		fp := a.fingerprint(c)
		h := held.Folders[c.Folder]
		if h.Fingerprint == fp {
			h.Runs++
		} else {
			h = heldFolder{Fingerprint: fp, Runs: 1, Attempts: h.Attempts}
		}
		h.Attempts++
		h.Problems, h.At, h.Keys = r.problems, a.today, c.ExtraKeys()
		a.attempts = append(a.attempts, attemptOf(c, a.today, c.Reason, "refused"))
		held.Folders[c.Folder] = h
		next := "asked again next run, with wider evidence"
		if h.Runs >= assessHoldRuns {
			next = fmt.Sprintf("refused in %d runs: asked again when a wider search finds something new, and on %s in any case", h.Runs, nextRetry(h.At))
		}
		fmt.Fprintf(os.Stderr, "assess: %s refused twice, %s: %s\n", c.Folder, next, r.problems)
	}
	// The forge's current names and orgs of the live projects, by code.
	if ctx.Err() == nil {
		errs += a.codeSteps(ctx, s, "the forge's name and org", a.forgeNames(o.only), o.dry, rep)
	}
	_ = a.commits.Save()
	_ = a.checker.Forge.Save()
	rep.CheckRefused += a.checkRefused
	if !o.dry {
		held.Reviewed = nil // v0.9.1's: by fingerprint alone, now held.Review
		if err := held.save(); err != nil {
			fmt.Fprintln(os.Stderr, "assess: held folders:", err)
		}
		a.logAttempts(a.attempts)
	}
	return errs
}

// askModel puts a batch to the model and checks its answer, with at most
// one retry for the refused: it returns acc with the accepted decisions,
// and the folders refused twice.
func (a *assessor) askModel(ctx context.Context, o assessOpts, rep *curate.Report, n int, batch []assess.Candidate, acc []decided, pending map[string]*record.Record, pendingLedger *[]byte) ([]decided, []assess.Refusal, error) {
	prompt := fmt.Sprintf(assess.Prompt, assess.Render(batch))
	res, cost, err := assessRun(ctx, o.model, prompt)
	rep.CostUSD += cost
	fmt.Fprintf(os.Stderr, "assess: batch %d of %d folder(s): $%.3f\n", n, len(batch), cost)
	if err != nil {
		return acc, nil, err
	}
	ds, perr := assess.Parse(res)
	var refused []assess.Refusal
	if perr == nil {
		more, r := a.check(batch, ds, pending, pendingLedger)
		acc, refused = append(acc, more...), r
	} else {
		for _, c := range batch {
			refused = append(refused, assess.Refusal{Folder: c.Folder, Problems: []string{perr.Error()}})
		}
	}
	if len(refused) > 0 {
		rep.Retries++
		for _, r := range refused {
			for _, p := range r.Problems {
				rep.InvalidEntries = append(rep.InvalidEntries, "assess (sent back): "+r.Folder+": "+firstClause(p))
			}
		}
		res2, c2, err := assessRun(ctx, o.model, assess.RetryPrompt(prompt, res, refused))
		rep.CostUSD += c2
		fmt.Fprintf(os.Stderr, "assess: batch %d retry for %d folder(s): $%.3f\n", n, len(refused), c2)
		if err == nil {
			if ds2, perr := assess.Parse(res2); perr == nil {
				var left []assess.Candidate
				done := map[string]bool{}
				for _, d := range acc {
					done[d.c.Folder] = true
				}
				for _, c := range batch {
					if !done[c.Folder] {
						left = append(left, c)
					}
				}
				more, still := a.check(left, ds2, pending, pendingLedger)
				acc = append(acc, more...)
				refused = still
			}
		}
	}
	return acc, refused, nil
}

// codeDecided splits a batch into the candidates code decides on its own
// (codeDecision), each checked like any decision (a refused one goes to the
// model), and the rest, for the model.
func (a *assessor) codeDecided(batch []assess.Candidate, pending map[string]*record.Record, pendingLedger *[]byte, deferred map[string]bool) (rest []assess.Candidate, acc []decided, later []assess.Candidate) {
	get := func(f string) *record.Record {
		if r := pending[f]; r != nil {
			return r
		}
		return a.recs[f]
	}
	for _, c := range batch {
		d, ok, wait := a.codeDecision(c, get)
		if !ok {
			if wait && !deferred[c.Folder] {
				// The repository it folds into is decided later in this run.
				deferred[c.Folder] = true
				later = append(later, c)
				continue
			}
			rest = append(rest, c)
			continue
		}
		got, refused := a.check([]assess.Candidate{c}, []assess.Decision{d}, pending, pendingLedger)
		if len(refused) > 0 || len(got) == 0 {
			for _, r := range refused {
				fmt.Fprintf(os.Stderr, "assess: %s: code's decision refused, the model decides: %s\n", r.Folder, clipOne(strings.Join(r.Problems, "; "), 300))
			}
			rest = append(rest, c)
			continue
		}
		fmt.Fprintf(os.Stderr, "assess: %s: decided by code: %s\n", c.Folder, clipOne(d.Why, 300))
		acc = append(acc, got...)
	}
	return rest, acc, later
}

// assessHoldRuns is how many runs a folder may be refused in, on the same
// evidence, before it waits for that evidence to change. Each run already
// sends a refused decision back once with code's problems; a second run asks
// again with a fresh prompt and the decisions other folders got since; a
// third on identical evidence asks the identical question and pays for the
// identical answer (the shadow run: one folder refused 16 times in 8 runs).
const assessHoldRuns = 2

// assessHeldFile is the held folders' state file.
const assessHeldFile = "assess-held.json"

// heldState is the folders refused on the same evidence, by folder.
type heldState struct {
	path    string
	Folders map[string]heldFolder `json:"folders"`
	// Reviewed: v0.9.1's folders left for review, by fingerprint alone; read
	// and dropped (each is asked again with extra evidence).
	Reviewed map[string]string `json:"reviewed,omitempty"`
	// Review: the folders left for review, with what they were last decided
	// on (assess_deep.go).
	Review map[string]reviewState `json:"review,omitempty"`
}

type heldFolder struct {
	Fingerprint string `json:"fingerprint"` // assess.Fingerprint of what the model read
	Runs        int    `json:"runs"`        // runs it was refused in with that fingerprint
	Problems    string `json:"problems"`    // the last refusal
	At          string `json:"at"`
	// Attempts counts every run it was refused in (the search widens with
	// it); Keys are the extra items the last attempt was shown.
	Attempts int      `json:"attempts,omitempty"`
	Keys     []string `json:"keys,omitempty"`
}

func loadHeld(path string) *heldState {
	h := &heldState{path: path, Folders: map[string]heldFolder{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, h)
	}
	if h.Folders == nil {
		h.Folders = map[string]heldFolder{}
	}
	if h.Reviewed == nil {
		h.Reviewed = map[string]string{}
	}
	if h.Review == nil {
		h.Review = map[string]reviewState{}
	}
	h.path = path
	return h
}

func (h *heldState) save() error {
	b, err := json.MarshalIndent(h, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// refresh fills in what c's batch shows besides its own evidence: the
// decisions made on folders alike (run: this run's), and, for a name that
// exists nowhere, the repositories folders alike were folded into.
func (a *assessor) refresh(c *assess.Candidate, run []decidedShape) {
	c.Similar = a.similar(*c, run)
	c.Alike = nil
	if !c.Absent() || !strings.Contains(c.Pattern, "#") {
		return
	}
	into := map[string]int{}
	for _, f := range sortedRecordFolders(a.recs) {
		r := a.recs[f]
		if f == c.Folder || r.SuppressedBy == "" || !strings.HasPrefix(f, "projects/") || assess.NamePattern(strings.TrimPrefix(f, "projects/")) != c.Pattern {
			continue
		}
		if w, _, err := record.Resolve(a.recs, f); err == nil && strings.HasPrefix(w, "projects/") {
			into[w]++
		}
	}
	for _, w := range sortedKeysInt(into) {
		n := strings.TrimPrefix(w, "projects/")
		if a.checker.Repo(a.ctx, n).Verdict != check.Holds {
			continue
		}
		c.Alike = append(c.Alike, assess.RealRepo{Name: n, Score: 50 * into[w],
			Why: []string{fmt.Sprintf("%d folder(s) of the same name pattern %s are suppressed into it", into[w], c.Pattern)}})
	}
	sort.SliceStable(c.Alike, func(i, j int) bool { return c.Alike[i].Score > c.Alike[j].Score })
}

// fingerprint is assess.Fingerprint of c as the next run will see it: with
// the records as they are now.
func (a *assessor) fingerprint(c assess.Candidate) string {
	a.refresh(&c, nil)
	return assess.Fingerprint(c)
}

// refusedItem is a candidate refused twice in a run, with the last problems.
type refusedItem struct {
	c        assess.Candidate
	problems string
}

// decidedShape is one decision of this run with its candidate's evidence
// shape (assess.Candidate.Shape).
type decidedShape struct {
	shape  string
	absent bool
	rec    *record.Record
}

// similar are the lines that show c the decisions already made on folders
// alike: the records of folders whose names follow c's pattern (a pattern
// with a number in it), and this run's decisions on folders whose evidence
// has c's shape, when c's name exists nowhere.
func (a *assessor) similar(c assess.Candidate, run []decidedShape) []string {
	if c.Kind != "" {
		return nil
	}
	var out []string
	seen := map[string]bool{c.Folder: true}
	if strings.Contains(c.Pattern, "#") {
		var recs []*record.Record
		for _, f := range sortedRecordFolders(a.recs) {
			r := a.recs[f]
			if seen[f] || !strings.HasPrefix(f, "projects/") || assess.NamePattern(strings.TrimPrefix(f, "projects/")) != c.Pattern {
				continue
			}
			seen[f] = true
			recs = append(recs, r)
		}
		out = append(out, assess.SimilarLines("the same name pattern "+c.Pattern, recs)...)
	}
	if c.Absent() {
		var recs []*record.Record
		for _, d := range run {
			if seen[d.rec.Folder] || !d.absent || d.shape != c.Shape() {
				continue
			}
			seen[d.rec.Folder] = true
			recs = append(recs, d.rec)
		}
		out = append(out, assess.SimilarLines("the same evidence: "+c.Shape(), recs)...)
	}
	return out
}

// batchOf is the batch that starts at cands[i]: assessBatch candidates, and
// the rest of one repository's names when the batch would cut them apart
// (at most twice assessBatch in all), so they are decided together.
func batchOf(cands []assess.Candidate, i int) []assess.Candidate {
	j := min(i+assessBatch, len(cands))
	for j < len(cands) && j-i < 2*assessBatch && cands[j].Group != "" && cands[j].Group == cands[j-1].Group {
		j++
	}
	return cands[i:j]
}

func firstClause(s string) string {
	if i := strings.Index(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}

// write puts a batch's records (and their quote ledger lines), a
// proposal's moves of its facts into the new folder and its close lines in
// the store in one curator commit, each record checked again on the synced
// clone; with the local layer, in the overlay. It returns how many records
// it wrote.
func (a *assessor) write(ctx context.Context, s *store.Store, ds []decided) (int, error) {
	kinds := map[string]int{}
	for _, d := range ds {
		k := "rejected"
		if d.rec != nil {
			k = d.rec.Kind
			if strings.HasPrefix(d.rec.Was[0].Why, assess.NotProject) {
				k = "not a project"
			}
		}
		kinds[k]++
	}
	var parts []string
	for _, k := range sortedKeysInt(kinds) {
		parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
	}
	msg := fmt.Sprintf("curator: assess: %d folder(s): %s", len(ds), strings.Join(parts, ", "))
	var body []string
	for _, d := range ds {
		switch {
		case d.rec == nil:
			body = append(body, d.c.Folder+": rejected")
		case d.rec.Folder != d.c.Folder:
			body = append(body, fmt.Sprintf("%s: folded into %s", d.c.Folder, d.rec.Folder))
		default:
			body = append(body, fmt.Sprintf("%s: %s, %s", d.c.Folder, d.rec.Kind, d.rec.Status))
		}
		if len(d.moves) > 0 {
			body[len(body)-1] += fmt.Sprintf(" (%d fact(s) move in)", len(d.moves))
		}
	}
	msg += "\n\n" + strings.Join(body, "\n")
	if a.local {
		return a.writeLocal(s, ds)
	}
	written := 0
	_, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		written = 0
		files := map[string][]byte{}
		fresh, _ := record.Walk(s.Dir)
		ql, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
		var lines []byte
		var moves []moveItem
		var closes []folderEntry
		pending := map[string]*record.Record{}
		for _, d := range ds {
			if d.rec != nil {
				now := fresh[d.rec.Folder]
				if !assess.Same(now, d.old) {
					continue // changed since it was read: the next run decides again
				}
				env := a.recordEnv(pending, nil)
				env.Records = map[string]*record.Record{}
				for f, r := range fresh {
					env.Records[f] = r
				}
				for f, r := range pending {
					env.Records[f] = r
				}
				env.QuoteBacked = ledger.Backing(append(append(append([]byte(nil), ql...), lines...), d.line...))
				if ps := record.Validate(now, d.rec, record.ByOpus, env); len(ps) > 0 {
					continue
				}
				b, err := d.rec.Encode()
				if err != nil {
					return nil, "", err
				}
				files[record.Rel(d.rec.Folder)] = b
				pending[d.rec.Folder] = d.rec
				lines = append(lines, d.line...)
				written++
			}
			moves = append(moves, d.moves...)
			closes = append(closes, d.closes...)
		}
		if len(files) == 0 && len(closes) == 0 {
			return nil, "", nil
		}
		var log []byte
		if len(moves) > 0 {
			facts, _, err := fact.Walk(s.Dir)
			if err != nil {
				return nil, "", err
			}
			open := map[string]bool{}
			for _, q := range openFolderAsks(s.Dir) {
				open[q.ID] = true
			}
			res := planMoves(s.Dir, "", facts, moves, append(append([]byte(nil), ql...), lines...), open, a.today, len(moves))
			for k, v := range res.files {
				files[k] = v
			}
			lines = append(lines, res.ledger...)
			for _, q := range res.asks {
				log = append(log, q.line()...)
			}
		}
		for _, c := range closes {
			log = append(log, c.line()...)
		}
		if len(lines) > 0 {
			files[ledger.QuotesPath] = append(append([]byte(nil), ql...), lines...)
		}
		if len(log) > 0 {
			cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.FoldersPath)))
			files[ledger.FoldersPath] = append(append([]byte(nil), cur...), log...)
		}
		return files, msg, nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// writeLocal is write for the local layer: the records and ledger lines go
// into this machine's overlay (curated/<folder>/_record.md), never the
// clone; the overlay's folder indexes are regenerated.
func (a *assessor) writeLocal(s *store.Store, ds []decided) (int, error) {
	dir := overlay.Dir()
	curated := filepath.Join(dir, filepath.FromSlash(overlay.CuratedDir))
	n := 0
	var moves []moveItem
	var log []byte
	for _, d := range ds {
		moves = append(moves, d.moves...)
		for _, c := range d.closes {
			log = append(log, c.line()...)
		}
		if d.rec == nil {
			continue
		}
		rec := d.rec
		if sr := a.storeRecs[rec.Folder]; sr != nil {
			// A store folder: this machine's status for it, nothing else.
			rec = record.AsOverride(sr, rec)
		}
		b, err := rec.Encode()
		if err != nil {
			return n, err
		}
		if err := overlay.Write(dir, s.Dir, overlay.CuratedDir+"/"+record.Rel(d.rec.Folder), b); err != nil {
			return n, err
		}
		if len(d.line) > 0 {
			if err := overlay.Append(dir, s.Dir, overlay.QuotesRel, d.line); err != nil {
				return n, err
			}
		}
		n++
	}
	if len(moves) > 0 {
		all, _ := overlay.Load(dir)
		var facts []*fact.Fact
		for _, f := range all {
			if overlay.IsCurated(f.Path) {
				facts = append(facts, f)
			}
		}
		quotes, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.QuotesRel)))
		open := map[string]bool{}
		for _, q := range openFolderAsks(curated) {
			open[q.ID] = true
		}
		res := planMoves(curated, overlay.Prefix+overlay.CuratedDir+"/", facts, moves, quotes, open, a.today, len(moves))
		for _, rel := range sortedFileKeys(res.files) {
			if err := overlay.Write(dir, s.Dir, overlay.CuratedDir+"/"+rel, res.files[rel]); err != nil {
				return n, err
			}
		}
		if len(res.ledger) > 0 {
			if err := overlay.Append(dir, s.Dir, overlay.QuotesRel, res.ledger); err != nil {
				return n, err
			}
		}
		for _, q := range res.asks {
			log = append(log, q.line()...)
		}
	}
	if len(log) > 0 {
		if err := overlay.Append(dir, s.Dir, overlay.CuratedDir+"/"+ledger.FoldersPath, log); err != nil {
			return n, err
		}
	}
	local, _ := overlay.Load(dir)
	return n, regenLocalIndexes(dir, s.Dir, local)
}
