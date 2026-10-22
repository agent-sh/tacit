package main

// The statements step (SPEC.md, "Statements"): the owner's words said once
// (facts of type statement, written by the split from their verified
// words) become binding only by evidence code counts. The curator's model
// reads each new statement once: what kind of point it makes, whether an
// earlier statement or promoted fact makes the same point, and which dry
// claims its words make. Code checks the claims and decides
// (statement.Plan): the same point said in another session makes a
// decision (a choice) or a preference and then a rule (a standing
// instruction); one statement and an agreeing check make a decision; a
// disagreeing check flags the statement for the owner, optionally. Every
// judgement, check, promotion and flag is a line of review/statements.jsonl.
// With the local layer it reads and writes this machine's overlay only.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/statement"
	"github.com/agent-sh/tacit/client/internal/store"
)

// statementsRun is the statements step's model call; tests substitute it.
var statementsRun curate.Runner = curate.RunCLI

// statementsBatch is how many statements one model call reads.
const statementsBatch = 8

// statementsWrite reports a store whose writers may write statements: its
// format marker says every machine reads them (ledger.Format.Statements).
func statementsWrite(dir string) bool {
	f, ok, err := ledger.ReadFormat(dir)
	return err == nil && ok && f.Statements()
}

type statementsOpts struct {
	model string
	dry   bool
}

// stmtReport is what one run of the step did.
type stmtReport struct {
	Judged, Decisions, Preferences, Rules, Superseded, Flagged, Differs, Refused int
}

func (r stmtReport) String() string {
	return fmt.Sprintf("%d judged, %d decision(s), %d preference(s), %d rule(s) promoted, %d superseded, %d flagged by a check, %d differ from a decision, %d answer(s) refused", r.Judged, r.Decisions, r.Preferences, r.Rules, r.Superseded, r.Flagged, r.Differs, r.Refused)
}

// runStatements implements `tacit statements [--model M] [--dry-run]
// [--env-file F]`: the step alone (it is also a step of the full curate,
// before the assessment, so a decision reaches the folder records).
func runStatements(args []string) int {
	fs := flag.NewFlagSet("statements", flag.ExitOnError)
	model := fs.String("model", curate.DefaultModel, "the judging model")
	dry := fs.Bool("dry-run", false, "judge and check; write nothing")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables")
	fs.Parse(args)
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "statements:", err)
			return 2
		}
		refreshModels()
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "statements: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	rep := &curate.Report{At: time.Now().Format(time.RFC3339), ModelCalls: &modelrun.Trace{}}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	local := localLayer(ctx, s)
	if !local && !*dry && refuseWriter(ctx, s, "curator", "statements") {
		return 2
	}
	if !local && !*dry {
		if err := s.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "statements: sync:", err)
			return 1
		}
	}
	sr, errs := statementsStep(ctx, s, statementsOpts{model: *model, dry: *dry}, rep)
	fmt.Printf("statements: %s, %s, %d error(s)\n", sr, rep.ModelCalls.CostSummary(rep.CostUSD), errs)
	if errs > 0 {
		return 1
	}
	return 0
}

// curateStatementsStep is the full curate's statements step; tests
// substitute it.
var curateStatementsStep = func(ctx context.Context, s *store.Store, model string, dry bool, rep *curate.Report) int {
	_, errs := statementsStep(ctx, s, statementsOpts{model: model, dry: dry}, rep)
	return errs
}

// stmtWorld is what one run reads.
type stmtWorld struct {
	dir, host, today string
	local            bool
	facts            []*fact.Fact
	byName           map[string]*fact.Fact
	log              []statement.Entry
	quotes           map[string]ledger.Quote
}

func (w *stmtWorld) logRel() string {
	if w.local {
		return overlay.CuratedDir + "/" + ledger.StatementsPath
	}
	return ledger.StatementsPath
}

func loadStmtWorld(s *store.Store, local bool) (*stmtWorld, error) {
	w := &stmtWorld{dir: s.Dir, host: store.ShortHost(), today: time.Now().Format("2006-01-02"), local: local, byName: map[string]*fact.Fact{}}
	facts, _, err := fact.Walk(s.Dir)
	if err != nil {
		return nil, err
	}
	w.facts = facts
	ql, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
	if local {
		lf, _ := overlay.Load(overlay.Dir())
		w.facts = append(w.facts, lf...)
		lq, _ := os.ReadFile(filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.QuotesRel)))
		ql = append(append([]byte(nil), ql...), lq...)
	}
	w.quotes = ledger.Quotes(ql)
	for _, f := range w.facts {
		if _, dup := w.byName[f.Meta.Name]; !dup || overlay.IsCurated(f.Path) {
			w.byName[f.Meta.Name] = f
		}
	}
	b, _ := os.ReadFile(w.logPath())
	w.log = statement.ReadLog(b)
	return w, nil
}

func (w *stmtWorld) logPath() string {
	if w.local {
		return filepath.Join(overlay.Dir(), filepath.FromSlash(w.logRel()))
	}
	return filepath.Join(w.dir, filepath.FromSlash(ledger.StatementsPath))
}

func liveFact(f *fact.Fact) bool {
	return f != nil && f.Meta.SupersededBy == "" && f.Meta.Retired == "" && !f.InInbox() && !overlay.IsInbox(f.Path)
}

// ownStatement reports a live statement this run may act on: the store's
// (fact.IsStatement), or with the local layer the overlay's, judged by the
// overlay's ledger.
func (w *stmtWorld) ownStatement(f *fact.Fact) bool {
	if !liveFact(f) || f.Meta.Type != fact.StatementType {
		return false
	}
	if w.local {
		return overlay.IsCurated(f.Path) && f.Meta.Source.Harness == "curator" && f.WordsBacked && f.Meta.Source.OwnerWords != ""
	}
	return !strings.HasPrefix(f.Path, "local/") && fact.IsStatement(f)
}

func stmtItem(f *fact.Fact) statement.Item {
	day := f.Meta.Created
	if d, _, ok := fact.StatementLine(f.Body); ok {
		day = d
	}
	src := f.Meta.Source
	return statement.Item{Name: f.Meta.Name, Scope: f.Meta.Scope, Kind: "statement", Words: src.OwnerWords, From: src.OwnerWordsFrom,
		Day: day, Context: f.Meta.Description, Sessions: []string{src.OwnerWordsFrom}}
}

// isRuling reports a live ruling of the owner's this run reads: the
// store's (fact.IsRuling), or with the local layer the overlay's too.
func (w *stmtWorld) isRuling(f *fact.Fact) bool {
	if !liveFact(f) || f.Meta.Type == fact.StatementType {
		return false
	}
	if overlay.IsCurated(f.Path) {
		return w.local && overlay.IsRuling(f)
	}
	return !strings.HasPrefix(f.Path, "local/") && fact.IsRuling(f) && f.Meta.Source.OwnerQuote != ""
}

func rulingItem(f *fact.Fact) statement.Item {
	src := f.Meta.Source
	return statement.Item{Name: f.Meta.Name, Scope: f.Meta.Scope, Kind: statement.KindRuling, Words: src.OwnerQuote, From: src.OwnerQuoteFrom,
		Day: f.Meta.Created, Context: f.Meta.Description, Sessions: []string{src.OwnerQuoteFrom}}
}

// promoted are the live facts the log says code made of statements, and
// the live rulings the step has read (binding already: a statement that
// makes their point again stands for them).
func (w *stmtWorld) promoted() map[string]statement.Item {
	out := map[string]statement.Item{}
	for n := range statement.Judgements(w.log) {
		if f := w.byName[n]; w.isRuling(f) {
			out[n] = rulingItem(f)
		}
	}
	for _, e := range w.log {
		if e.Kind != statement.LogPromoted {
			continue
		}
		f := w.byName[e.Fact]
		if !liveFact(f) || (w.local && !overlay.IsCurated(f.Path)) {
			delete(out, e.Fact)
			continue
		}
		words, from := f.Meta.Source.OwnerQuote, f.Meta.Source.OwnerQuoteFrom
		if words == "" {
			words, from = f.Meta.Source.OwnerWords, f.Meta.Source.OwnerWordsFrom
		}
		ctx := fact.StatementContext(f.Body)
		if ctx == "" {
			ctx = f.Meta.Description
		}
		out[e.Fact] = statement.Item{Name: f.Meta.Name, Scope: f.Meta.Scope, Kind: e.Level, Words: words, From: from, Day: f.Meta.Created, Context: ctx, Sessions: e.Sessions}
	}
	return out
}

// folderNames are the store's project, area and topic names and aliases,
// and the catalogs' repository names: what Candidates matches whole.
func (w *stmtWorld) folderNames() map[string]bool {
	out := map[string]bool{}
	for f, r := range storeRecords(w.dir) {
		out[f[strings.LastIndex(f, "/")+1:]] = true
		for _, a := range r.Aliases {
			out[a] = true
		}
	}
	for _, f := range w.facts {
		if n, ok := strings.CutPrefix(f.Meta.Scope, "projects/"); ok {
			out[n] = true
		}
	}
	for n, hs := range repoid.StoreEvidence(w.dir) {
		out[n] = true
		for _, h := range hs {
			for _, a := range h.Aliases {
				out[strings.ToLower(a)] = true
			}
		}
	}
	return out
}

// statementsStep runs one pass: the model reads the new statements, code
// checks their claims and plans, and the plan is written in one curator
// commit (or into the overlay). It returns what it did and its errors.
func statementsStep(ctx context.Context, s *store.Store, o statementsOpts, rep *curate.Report) (stmtReport, int) {
	var sr stmtReport
	local := localLayer(ctx, s)
	if !local && !statementsWrite(s.Dir) {
		// No statement is in a store without the format; nothing to do.
		return sr, 0
	}
	if local && !overlay.Outside(overlay.Dir(), s.Dir) {
		fmt.Fprintln(os.Stderr, "statements:", overlay.ErrInside)
		return sr, 1
	}
	w, err := loadStmtWorld(s, local)
	if err != nil {
		fmt.Fprintln(os.Stderr, "statements:", err)
		return sr, 1
	}
	ch := storeChecker(ctx, s.Dir, local)
	defer func() {
		if ch.Forge != nil {
			_ = ch.Forge.Save()
		}
	}()
	own := map[string]statement.Item{}
	var pool []statement.Item // what a statement is compared with
	for _, f := range w.facts {
		if w.ownStatement(f) {
			own[f.Meta.Name] = stmtItem(f)
		}
		if local && !overlay.IsCurated(f.Path) && liveFact(f) && fact.IsStatement(f) {
			pool = append(pool, stmtItem(f)) // the store's, read in
		}
	}
	// The rulings whose words name a repository with a naming question (more
	// than one name, or a name that is another name of a checkout): their
	// claims are read once, so a ruling on a name is the owner's decision
	// the folder records follow.
	names := w.folderNames()
	judgedNow := statement.Judgements(w.log)
	madeHere := map[string]bool{} // decisions and rules this step made: their claims are in the log
	for _, e := range w.log {
		if e.Kind == statement.LogPromoted {
			madeHere[e.Fact] = true
		}
	}
	var rulingAsks []statement.Ask
	for _, f := range w.facts {
		if !w.isRuling(f) || madeHere[f.Meta.Name] {
			continue
		}
		if _, done := judgedNow[f.Meta.Name]; done {
			continue
		}
		if namingQuestion(ch, f.Meta.Source.OwnerQuote+"\n"+f.Meta.Description, names) {
			rulingAsks = append(rulingAsks, statement.Ask{S: rulingItem(f)})
		}
	}
	sort.Slice(rulingAsks, func(i, j int) bool { return rulingAsks[i].S.Name < rulingAsks[j].S.Name })
	if len(own) == 0 && len(rulingAsks) == 0 {
		return sr, 0
	}
	prom := w.promoted()
	for _, it := range own {
		pool = append(pool, it)
	}
	for _, it := range prom {
		pool = append(pool, it)
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Name < pool[j].Name })
	judged := statement.Judgements(w.log)
	asks := append([]statement.Ask(nil), rulingAsks...)
	for _, n := range sortedStmtNames(own) {
		if _, ok := judged[n]; ok {
			continue
		}
		s := own[n]
		var earlier []statement.Item
		for _, it := range pool {
			if it.Name == s.Name {
				continue
			}
			if it.Kind == "statement" && !(it.Day < s.Day || it.Day == s.Day && it.Name < s.Name) {
				continue
			}
			earlier = append(earlier, it)
		}
		asks = append(asks, statement.Ask{S: s, Earlier: statement.Candidates(s, earlier, names)})
	}
	errs := 0
	var newLog []statement.Entry
	for i := 0; i < len(asks); i += statementsBatch {
		batch := asks[i:min(i+statementsBatch, len(asks))]
		prompt := fmt.Sprintf(statement.Prompt, statement.Render(batch))
		res, cost, err := statementsRun(ctx, o.model, prompt)
		rep.CostUSD += cost
		if err != nil {
			fmt.Fprintln(os.Stderr, "statements: model:", err)
			errs++
			continue
		}
		js, problems, perr := statement.Parse(res, batch)
		if perr != nil {
			problems = map[string][]string{}
			for _, a := range batch {
				problems[a.S.Name] = []string{perr.Error()}
			}
		}
		if len(problems) > 0 {
			rep.Retries++
			var left []statement.Ask
			for _, a := range batch {
				if problems[a.S.Name] != nil {
					left = append(left, a)
				}
			}
			res2, c2, err := statementsRun(ctx, o.model, statement.RetryPrompt(prompt, res, problems))
			rep.CostUSD += c2
			if err == nil {
				js2, still, perr := statement.Parse(res2, left)
				if perr == nil {
					js = append(js, js2...)
					sr.Refused += len(still)
				} else {
					sr.Refused += len(left)
				}
			} else {
				sr.Refused += len(left)
			}
		}
		for _, j := range js {
			e := statement.Entry{Kind: statement.LogJudged, At: w.today, Name: j.Name, Says: j.Says, Same: j.Same, Claims: j.Claims}
			newLog = append(newLog, e)
			w.log = append(w.log, e)
			sr.Judged++
		}
	}
	// The dry claims, checked by code.
	judged = statement.Judgements(w.log)
	checks := map[string][]check.Result{}
	for n := range own {
		for _, cl := range judged[n].Claims {
			root := ""
			if cl.Kind == "path" || cl.Kind == "version" {
				_, root = factRepo(ch.Catalog, &fact.Fact{Meta: fact.Meta{Scope: "projects/" + strings.ToLower(cl.Repo)}})
			}
			checks[n] = append(checks[n], ch.Claim(ctx, cl, root))
		}
	}
	prom = w.promoted() // with the rulings read this run
	acts := statement.Plan(statement.State{Statements: own, Promoted: prom, Log: w.log, Checks: checks})
	if o.dry {
		for _, a := range acts {
			fmt.Println("- " + describeAct(a))
		}
		return sr, errs
	}
	if len(acts) == 0 && len(newLog) == 0 {
		return sr, errs
	}
	if err := w.apply(ctx, s, acts, newLog, &sr); err != nil {
		fmt.Fprintln(os.Stderr, "statements: write:", err)
		errs++
	}
	return sr, errs
}

func describeAct(a statement.Action) string {
	switch a.Kind {
	case statement.ActPromote:
		return fmt.Sprintf("%s by %s of %s (sessions %s): %q", a.Level, a.By, strings.Join(a.From, ", "), strings.Join(a.Sessions, " "), a.Words.Words)
	case statement.ActSupersede:
		return fmt.Sprintf("%s superseded by %s (%s)", a.Name, a.Into, a.By)
	case statement.ActFlag:
		return fmt.Sprintf("%s flagged: a check disagrees: %s", a.Name, strings.Join(check.Lines(a.Checks), " | "))
	case statement.ActDiffers:
		return fmt.Sprintf("%s differs from %s", a.Name, a.Into)
	}
	return a.Kind
}

func sortedStmtNames(m map[string]statement.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]].Day != m[out[j]].Day {
			return m[out[i]].Day < m[out[j]].Day
		}
		return out[i] < out[j]
	})
	return out
}

// stmtPlan is a run's writes, by path relative to where they go (the store,
// or the overlay).
type stmtPlan struct {
	files   map[string][]byte // whole files: facts changed or made
	ledger  []byte
	log     []byte
	review  []string
	summary []string
}

// apply writes the actions and the run's judgements: the promoted facts
// with their quote ledger lines, the replaced items' superseded_by, the
// flags in the statements' bodies, the review lines for the owner, the log.
func (w *stmtWorld) apply(ctx context.Context, s *store.Store, acts []statement.Action, newLog []statement.Entry, sr *stmtReport) error {
	plan := func() (*stmtPlan, error) {
		p := &stmtPlan{files: map[string][]byte{}}
		taken := map[string]bool{}
		for _, f := range w.facts {
			taken[f.Meta.Name] = true
		}
		for _, e := range newLog {
			p.log = append(p.log, e.Line()...)
		}
		cur := func(f *fact.Fact) ([]byte, string, error) {
			rel := f.Path
			if w.local {
				r, ok := overlay.Rel(f.Path)
				if !ok {
					return nil, "", fmt.Errorf("%s is not the overlay's", f.Path)
				}
				rel = r
			}
			if b, ok := p.files[rel]; ok {
				return b, rel, nil
			}
			full := filepath.Join(w.dir, filepath.FromSlash(rel))
			if w.local {
				full = filepath.Join(overlay.Dir(), filepath.FromSlash(rel))
			}
			b, err := os.ReadFile(full)
			return b, rel, err
		}
		supersede := func(name, into, by string) error {
			f := w.byName[name]
			if f == nil || !liveFact(f) {
				return nil
			}
			b, rel, err := cur(f)
			if err != nil {
				return err
			}
			g := *f
			g.Meta.SupersededBy, g.Meta.Updated = into, w.today
			nb, err := fact.PatchRelations(b, &g)
			if err != nil {
				return err
			}
			p.files[rel] = nb
			p.log = append(p.log, statement.Entry{Kind: statement.LogSuperseded, At: w.today, Name: name, Into: into, By: by}.Line()...)
			sr.Superseded++
			return nil
		}
		for _, a := range acts {
			switch a.Kind {
			case statement.ActPromote:
				nf, q := w.promotedFact(a, taken)
				if nf == nil {
					continue
				}
				b, err := nf.Encode()
				if err != nil {
					return nil, err
				}
				rel := nf.Path
				if w.local {
					rel, _ = overlay.Rel(nf.Path)
				}
				p.files[rel] = b
				p.ledger = append(p.ledger, q.Line()...)
				p.log = append(p.log, statement.Entry{Kind: statement.LogPromoted, At: w.today, Fact: nf.Meta.Name, Level: a.Level, By: a.By,
					From: a.From, Sessions: a.Sessions, Claims: a.Claims, Checks: a.Checks}.Line()...)
				for _, r := range a.Replaces {
					if err := supersede(r, nf.Meta.Name, "promoted"); err != nil {
						return nil, err
					}
				}
				switch a.Level {
				case statement.LevelDecision:
					sr.Decisions++
				case statement.LevelPreference:
					sr.Preferences++
				case statement.LevelRule:
					sr.Rules++
				}
				p.summary = append(p.summary, fmt.Sprintf("%s: %s, by %s, from %s", nf.Meta.Name, a.Level, a.By, strings.Join(a.From, ", ")))
			case statement.ActSupersede:
				if err := supersede(a.Name, a.Into, a.By); err != nil {
					return nil, err
				}
				p.summary = append(p.summary, fmt.Sprintf("%s: superseded by %s (%s)", a.Name, a.Into, a.By))
			case statement.ActFlag:
				f := w.byName[a.Name]
				if f == nil {
					continue
				}
				b, rel, err := cur(f)
				if err != nil {
					return nil, err
				}
				line := strings.Join(check.Lines(a.Checks), " | ")
				nb, err := fact.AppendCheckFlag(b, fact.CheckFlagLine(w.today, line))
				if err != nil {
					return nil, err
				}
				g := *f
				g.Meta.Updated = w.today
				if nb, err = fact.PatchRelations(nb, &g); err != nil {
					return nil, err
				}
				p.files[rel] = nb
				p.log = append(p.log, statement.Entry{Kind: statement.LogFlagged, At: w.today, Name: a.Name, Checks: a.Checks, Hash: a.Hash}.Line()...)
				p.review = append(p.review, checkLine(a.Name, f.Meta.Scope, "misread", w.today),
					"  A check disagrees, maybe misread: "+line,
					"  You said once ("+stmtItem(f).Day+"): \""+f.Meta.Source.OwnerWords+"\"",
					"  Optional: the statement stays as you said it, and nothing waits on your answer.")
				sr.Flagged++
				p.summary = append(p.summary, a.Name+": a check disagrees")
			case statement.ActDiffers:
				f, d := w.byName[a.Name], w.byName[a.Into]
				if f == nil || d == nil {
					continue
				}
				p.log = append(p.log, statement.Entry{Kind: statement.LogDiffers, At: w.today, Name: a.Name, Into: a.Into}.Line()...)
				p.review = append(p.review, checkLine(a.Name, f.Meta.Scope, "differs", w.today),
					"  You said once ("+stmtItem(f).Day+"): \""+f.Meta.Source.OwnerWords+"\", which differs from your "+d.Meta.Type+" [["+a.Into+"]].",
					"  The decision stands until a newer confirmed one replaces it (the same point said again in another session, or a check that agrees).",
					"  Optional: nothing waits on your answer.")
				sr.Differs++
				p.summary = append(p.summary, a.Name+": differs from "+a.Into)
			}
		}
		return p, nil
	}
	reviewRel := "review/" + w.today + "-statements.md"
	msg := func(p *stmtPlan) string {
		m := fmt.Sprintf("curator: statements: %s", sr.String())
		if len(p.summary) > 0 {
			m += "\n\n" + strings.Join(p.summary, "\n")
		}
		return m
	}
	if w.local {
		p, err := plan()
		if err != nil {
			return err
		}
		dir := overlay.Dir()
		for _, rel := range sortedFileKeys(p.files) {
			if err := overlay.Write(dir, s.Dir, rel, p.files[rel]); err != nil {
				return err
			}
		}
		if len(p.ledger) > 0 {
			if err := overlay.Append(dir, s.Dir, overlay.QuotesRel, p.ledger); err != nil {
				return err
			}
		}
		if len(p.review) > 0 {
			if err := overlay.Append(dir, s.Dir, overlay.ReviewDir+"/"+w.today+"-statements.md", []byte(strings.Join(p.review, "\n")+"\n")); err != nil {
				return err
			}
		}
		if len(p.log) > 0 {
			if err := overlay.Append(dir, s.Dir, w.logRel(), p.log); err != nil {
				return err
			}
		}
		lf, _ := overlay.Load(dir)
		return regenLocalIndexes(dir, s.Dir, lf)
	}
	_, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		// Planned on the fresh clone: a statement changed since it was read
		// is planned again from what is there now.
		fresh, err := loadStmtWorld(s, false)
		if err != nil {
			return nil, "", err
		}
		fresh.log = w.log
		*w = *fresh
		*sr = stmtReport{Judged: sr.Judged, Refused: sr.Refused}
		p, err := plan()
		if err != nil {
			return nil, "", err
		}
		files := p.files
		if len(p.ledger) > 0 {
			ql, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)))
			files[ledger.QuotesPath] = append(append([]byte(nil), ql...), p.ledger...)
		}
		if len(p.log) > 0 {
			cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.StatementsPath)))
			files[ledger.StatementsPath] = append(append([]byte(nil), cur...), p.log...)
		}
		if len(p.review) > 0 {
			cur, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(reviewRel)))
			files[reviewRel] = append(append([]byte(nil), cur...), []byte(strings.Join(p.review, "\n")+"\n")...)
		}
		if len(files) == 0 {
			return nil, "", nil
		}
		return files, msg(p), nil
	})
	return err
}

// promotedFact is the decision, preference or rule a promotion makes: the
// newest statement's words verbatim (the description holds them, so the
// quote proves the fact it states), their context, and the statements and
// checks it rests on; with its quote ledger line (a quote for a decision or
// a rule, words for a preference). nil when the newest statement is gone.
func (w *stmtWorld) promotedFact(a statement.Action, taken map[string]bool) (*fact.Fact, ledger.Quote) {
	src := w.byName[a.Words.Name]
	if src == nil {
		return nil, ledger.Quote{}
	}
	words := strings.Join(strings.Fields(a.Words.Words), " ")
	context := strings.Join(strings.Fields(a.Words.Context), " ")
	base := importer.Slug(a.Level+" "+context, 60)
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	taken[name] = true
	typ, kind := "design_decision", "user"
	head := "You decided"
	switch a.Level {
	case statement.LevelPreference:
		typ, kind, head = "user_standard", "observed", "You said it again"
	case statement.LevelRule:
		typ, head = "user_standard", "Your rule"
	}
	var from []string
	for _, n := range a.From {
		if f := w.byName[n]; f != nil {
			it := stmtItem(f)
			where := it.From
			if f.Meta.Type != fact.StatementType {
				where = strings.Join(a.Sessions, " ")
				it.Day = f.Meta.Created
			}
			from = append(from, fmt.Sprintf("[[%s]] (%s, %s)", n, it.Day, where))
		}
	}
	how := ""
	switch a.By {
	case statement.ByCheck:
		how = fmt.Sprintf("Made by code from your statement %s: a check agrees: %s.", strings.Join(from, " and "), strings.Join(check.Lines(a.Checks), " | "))
	default:
		how = fmt.Sprintf("Made by code from %s: said in %d separate sessions.", strings.Join(from, " and "), len(a.Sessions))
	}
	body := fmt.Sprintf("%s (%s): \"%s\"\n", head, a.Words.Day, words)
	if context != "" {
		body += "Context: " + context + "\n"
	}
	body += how + "\n"
	m := fact.Meta{Name: name, Description: fact.ClipDescription(words, 200), Type: typ, Scope: src.Meta.Scope,
		Source:  fact.Source{Harness: "curator", Host: w.host, Session: src.Meta.Source.Session, Kind: kind},
		Created: w.today, Updated: w.today}
	for _, n := range a.From {
		if f := w.byName[n]; f != nil {
			m.AppliesTo.Repos = unionCap(m.AppliesTo.Repos, f.Meta.AppliesTo.Repos)
			m.AppliesTo.Paths = unionCap(m.AppliesTo.Paths, f.Meta.AppliesTo.Paths)
			m.AppliesTo.Hosts = unionCap(m.AppliesTo.Hosts, f.Meta.AppliesTo.Hosts)
			m.AppliesTo.Commands = unionCap(m.AppliesTo.Commands, f.Meta.AppliesTo.Commands)
		}
	}
	from0 := a.Words.From
	hash := ""
	if q, ok := w.quotes[ledger.Quote{Name: src.Meta.Name, Quote: src.Meta.Source.OwnerWords, From: src.Meta.Source.OwnerWordsFrom, Words: true}.Key()]; ok {
		hash = q.TurnHash
	}
	q := ledger.Quote{Name: name, Quote: words, From: from0, TurnHash: hash, At: w.today}
	if a.Level == statement.LevelPreference {
		m.Source.OwnerWords, m.Source.OwnerWordsFrom = words, from0
		q.Words = true
	} else {
		m.Source.OwnerQuote, m.Source.OwnerQuoteFrom = words, from0
	}
	path := m.Scope + "/" + name + ".md"
	if w.local {
		path = overlay.Prefix + overlay.CuratedDir + "/" + path
	}
	return &fact.Fact{Path: path, Meta: m, Body: body}, q
}

func unionCap(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, x := range b {
		dup := false
		for _, y := range out {
			dup = dup || x == y
		}
		if !dup && len(out) < 8 {
			out = append(out, x)
		}
	}
	return out
}

// ownerDecisions are the folder names the owner's live decisions settle,
// read from the statements log: for each canonical claim of a promoted
// decision that is still live and a ruling, every name it names maps to
// the real name, with the decision (its name, words and where).
type ownerDecision struct {
	Fact, Words, From, Canonical string
	Names                        []string
}

func ownerDecisions(dir string, facts []*fact.Fact, local bool) map[string]ownerDecision {
	byName := map[string]*fact.Fact{}
	for _, f := range facts {
		byName[f.Meta.Name] = f
	}
	var b []byte
	b, _ = os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.StatementsPath)))
	if local {
		lb, _ := os.ReadFile(filepath.Join(overlay.Dir(), overlay.CuratedDir, filepath.FromSlash(ledger.StatementsPath)))
		b = append(append([]byte(nil), b...), lb...)
	}
	out := map[string]ownerDecision{}
	log := statement.ReadLog(b)
	judged := statement.Judgements(log)
	type src struct {
		fact   string
		claims []check.Claim
	}
	var srcs []src
	for _, e := range log {
		if e.Kind == statement.LogPromoted && e.Level == statement.LevelDecision {
			srcs = append(srcs, src{e.Fact, e.Claims})
		}
	}
	// A ruling the step read: its claims are the owner's decision as it is.
	var rulings []string
	for n := range judged {
		rulings = append(rulings, n)
	}
	sort.Strings(rulings)
	for _, n := range rulings {
		if f := byName[n]; f != nil && f.Meta.Type != fact.StatementType {
			srcs = append(srcs, src{n, judged[n].Claims})
		}
	}
	for _, sc := range srcs {
		f := byName[sc.fact]
		if !liveFact(f) || !(fact.IsRuling(f) || overlay.IsRuling(f)) {
			continue
		}
		for _, cl := range sc.claims {
			if cl.Kind != "canonical" {
				continue
			}
			c, o := strings.ToLower(cl.Name), strings.ToLower(cl.Other)
			d := ownerDecision{Fact: f.Meta.Name, Words: f.Meta.Source.OwnerQuote, From: f.Meta.Source.OwnerQuoteFrom, Canonical: c, Names: []string{c}}
			if o != "" {
				d.Names = append(d.Names, o)
			}
			for _, n := range d.Names {
				if prev, ok := out[n]; ok && prev.Canonical != c {
					continue // the first (oldest in the log) decision of a name stands; the owner's newer one supersedes it through the step
				}
				out[n] = d
			}
		}
	}
	return out
}

// namingQuestion reports text that names, as a whole word, a repository
// name code finds a naming question about: another name of a checkout, or a
// checkout known by other names (a symlink's, the origin remote's), by
// every machine's catalog. names are the store's folder and catalog names.
func namingQuestion(ch *check.Checker, text string, names map[string]bool) bool {
	low := strings.ToLower(text)
	var ns []string
	for n := range names {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	for _, n := range ns {
		if len(n) < 3 || !record.WholeWord(low, n) {
			continue
		}
		if len(ch.Same(n)) > 0 {
			return true
		}
		if cn, _ := ch.CheckoutName(n); cn != "" && cn != n {
			return true
		}
	}
	return false
}
