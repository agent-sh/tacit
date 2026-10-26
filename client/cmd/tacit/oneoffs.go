package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/relevance"
	"github.com/agent-sh/tacit/client/internal/statement"
	"github.com/agent-sh/tacit/client/internal/store"
)

// One-offs on a store curated before them (SPEC.md, "One-offs"): a ruling
// resting on words the owner said in one session is their statement, never
// a rule. `tacit rulings one-offs` finds the store's live rulings whose quote
// code finds in one session of theirs (ownerwords.Corpus.SaidIn), or in none
// it can still read (the quote ledger's one session stands), and writes each
// as their statement (their words verbatim, the day, the session, the
// ruling's description as the context, its text as the gloss) that the
// ruling points to (superseded_by): never deleted. A ruling said in two
// sessions or more stays; so does one they wrote in the app, one they
// endorsed, and one the statements step promoted (it rests on two sessions
// or a check already).
//
// Not every sentence of the owner's is a statement (SPEC.md, "Relevance"):
// before a one-off is written, the deciding model judges, in batches, whether
// the remark would matter to future work (oneOffGate). Kept with a stated
// future use, it becomes their statement; dropped (an acknowledgement, a
// passing question, frustration, one-time steering), the ruling is retired
// with no statement, the reason in the statements log; a remark the model
// left unjudged, or kept with no category or reason, waits for the next run.

// oneOff is one ruling the step looked at.
type oneOff struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Type      string   `json:"type"`
	Scope     string   `json:"scope"`
	Quote     string   `json:"quote"`
	From      string   `json:"from"`
	Sessions  []string `json:"sessions"` // where code finds the quote now
	Statement string   `json:"statement,omitempty"`
	Why       string   `json:"why"`
	// Description and Created are the ruling's, Turn the owner's message
	// that holds the quote (as code finds it in their turns), for the gate.
	Description string `json:"description,omitempty"`
	Created     string `json:"created,omitempty"`
	Turn        string `json:"turn,omitempty"`
	// Gate is the relevance gate's outcome on a one-off (gateKeep,
	// gateDrop, gateUnjudged), Relevance the model's judgement, Problem why
	// code did not take it.
	Gate      string               `json:"gate,omitempty"`
	Relevance *relevance.Judgement `json:"relevance,omitempty"`
	Problem   string               `json:"gate_problem,omitempty"`
}

// The relevance gate's outcomes on a one-off.
const (
	gateKeep     = "keep"     // their statement
	gateDrop     = "drop"     // retired, no statement: the remark does not matter to future work
	gateUnjudged = "unjudged" // no judgement code takes: it waits for the next run
)

// oneOffsReport is what the step found.
type oneOffsReport struct {
	At       string         `json:"at"`
	Rulings  int            `json:"rulings"` // live curated rulings looked at
	Kept     map[string]int `json:"kept"`    // why a ruling stays one
	OneOffs  int            `json:"one_offs"`
	ByType   map[string]int `json:"one_offs_by_type"`
	NotFound int            `json:"one_offs_not_found_now"` // its quote is in no transcript here now: the ledger's one session stands
	// Words: live user_standard facts that hold words of theirs but are no
	// ruling (already never a rule), by how many sessions code finds them in.
	WordsOneSession   int      `json:"user_standard_words_one_session"`
	WordsManySessions int      `json:"user_standard_words_many_sessions"`
	Items             []oneOff `json:"items"`
	Pushed            bool     `json:"pushed,omitempty"`
	// The relevance gate: whether it ran, its outcomes, the categories, the
	// judgements taken from an earlier report (--verdicts), its calls, cost
	// and the model.
	Gated      bool           `json:"gated"`
	GateKept   int            `json:"gate_kept"`
	GateDrop   int            `json:"gate_dropped"`
	Unjudged   int            `json:"gate_unjudged"`
	Categories map[string]int `json:"gate_categories,omitempty"`
	Reused     int            `json:"gate_reused,omitempty"`
	GateCalls  int            `json:"gate_calls,omitempty"`
	GateFailed int            `json:"gate_failed_calls,omitempty"`
	CostUSD    float64        `json:"cost_usd,omitempty"`
	Model      string         `json:"model,omitempty"`
}

// ownerLookup is where the step finds the owner's words: the sessions a
// quote is said in, as a statement of theirs (ownerwords.Corpus.SaidIn), and
// the turn that holds it (ownerwords.Corpus.TurnIn).
type ownerLookup struct {
	saidIn func(quote string) []string
	turnIn func(quote, from string) string
}

func corpusLookup(c *ownerwords.Corpus) ownerLookup {
	return ownerLookup{saidIn: c.SaidIn, turnIn: c.TurnIn}
}

// reportLookup is the lookup an earlier report (--found) recorded: the
// sessions and the turn of each quote it looked at. A quote it does not hold
// is in no session code can read (the ledger's one session stands).
func reportLookup(r *oneOffsReport) ownerLookup {
	sessions, turns := map[string][]string{}, map[string]string{}
	for _, it := range r.Items {
		k := ownerwords.Normalize(it.Quote)
		if len(it.Sessions) > 0 {
			sessions[k] = it.Sessions // a ruling kept for another reason recorded none
		}
		if it.Turn != "" {
			turns[k] = it.Turn
		}
	}
	return ownerLookup{
		saidIn: func(q string) []string { return sessions[ownerwords.Normalize(q)] },
		turnIn: func(q, _ string) string { return turns[ownerwords.Normalize(q)] },
	}
}

// findOneOffs looks at the live curated rulings of facts with the owner's
// words. promoted names the statements step's promoted facts.
func findOneOffs(facts []*fact.Fact, look ownerLookup, promoted map[string]bool) *oneOffsReport {
	rep := &oneOffsReport{At: time.Now().UTC().Format(time.RFC3339), Kept: map[string]int{}, ByType: map[string]int{}}
	for _, f := range facts {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Path, "local/") {
			continue
		}
		s := f.Meta.Source
		if !fact.IsRuling(f) {
			if f.Meta.Type == "user_standard" && fact.OwnerWordsOf(f) != "" {
				if len(look.saidIn(s.OwnerWords)) >= 2 {
					rep.WordsManySessions++
				} else {
					rep.WordsOneSession++
				}
			}
			continue
		}
		rep.Rulings++
		it := oneOff{Name: f.Meta.Name, Path: f.Path, Type: f.Meta.Type, Scope: f.Meta.Scope, Quote: s.OwnerQuote, From: s.OwnerQuoteFrom}
		switch {
		case fact.OwnerWrote(f) || strings.HasPrefix(s.OwnerQuoteFrom, "note:"):
			it.Why = "written by them in the app"
		case s.Endorsed != "" && f.EndorsedBacked:
			it.Why = "endorsed by them"
		case promoted[f.Meta.Name]:
			it.Why = "promoted by the statements step"
		case s.OwnerQuote == "" || !f.QuoteBacked:
			it.Why = "no quote the ledger backs"
		default:
			it.Sessions = look.saidIn(s.OwnerQuote)
			if len(it.Sessions) >= 2 {
				it.Why = "said in two sessions or more"
			}
		}
		if it.Why != "" {
			rep.Kept[it.Why]++
			rep.Items = append(rep.Items, it)
			continue
		}
		it.Why = "said in one session"
		if len(it.Sessions) == 0 {
			it.Why = "said in one session (the ledger's; no transcript here holds it now)"
			rep.NotFound++
		}
		it.Statement = importer.Slug(f.Meta.Name+"-said-once", 80)
		it.Description, it.Created = f.Meta.Description, f.Meta.Created
		it.Turn = clipRunes(strings.TrimSpace(look.turnIn(s.OwnerQuote, s.OwnerQuoteFrom)), maxGateTurn)
		rep.OneOffs++
		rep.ByType[f.Meta.Type]++
		rep.Items = append(rep.Items, it)
	}
	sort.Slice(rep.Items, func(i, j int) bool { return rep.Items[i].Path < rep.Items[j].Path })
	return rep
}

// oneOffStatement is a one-off ruling as the owner's statement.
func oneOffStatement(f *fact.Fact, it oneOff, host string) *fact.Fact {
	words := strings.Join(strings.Fields(it.Quote), " ")
	context := strings.Join(strings.Fields(f.Meta.Description), " ")
	gloss := strings.Join(strings.Fields(strings.ReplaceAll(f.Body, fmt.Sprintf(" In the owner's words: \"%s\"", words), "")), " ")
	day := f.Meta.Created
	st := &fact.Fact{Meta: fact.Meta{
		Name: it.Statement, Description: fact.ClipDescription(context, 200), Type: fact.StatementType, Scope: f.Meta.Scope, AppliesTo: f.Meta.AppliesTo,
		Source:  fact.Source{Harness: "curator", Host: host, Kind: "observed", OwnerWords: words, OwnerWordsFrom: it.From},
		Created: day,
	}, Body: fact.StatementBody(day, words, context, gloss)}
	st.Path = f.Meta.Scope + "/" + st.Meta.Name + ".md"
	return st
}

// oneOffWrites is the step's commit: each statement, its words line in the
// quote ledger (the turn's hash the ruling's line recorded), and the ruling
// pointing at it.
func oneOffWrites(dir string, rep *oneOffsReport, byName map[string]*fact.Fact, today string) (map[string][]byte, error) {
	files := map[string][]byte{}
	ql, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
	quotes := ledger.Quotes(ql)
	add := withNewline(ql)
	base := len(add)
	host := store.ShortHost()
	var log []byte
	for _, it := range rep.Items {
		if it.Statement == "" {
			continue
		}
		f := byName[it.Name]
		if f == nil {
			continue
		}
		if rep.Gated && it.Gate == gateDrop {
			// Their remark does not matter to future work: no statement, and
			// the ruling no longer stands (retired, never deleted); the inbox
			// note it came from and the log keep it, with why.
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
			if err != nil {
				return nil, err
			}
			g := *f
			g.Meta.Retired, g.Meta.Updated = today, today
			out, err := fact.PatchRelations(data, &g)
			if err != nil {
				return nil, err
			}
			files[f.Path] = out
			log = append(log, statement.Entry{Kind: statement.LogDropped, At: today, Name: f.Meta.Name, Says: it.Relevance.Category, Words: it.Quote, Why: it.Relevance.Reason, From: []string{it.From}}.Line()...)
			continue
		}
		if rep.Gated && it.Gate != gateKeep {
			continue // unjudged: it waits for the next run
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Meta.Scope+"/"+it.Statement+".md"))); err == nil {
			continue // written before
		}
		st := oneOffStatement(f, it, host)
		b, err := st.Encode()
		if err != nil {
			return nil, err
		}
		files[st.Path] = b
		rq := quotes[ledger.Quote{Name: f.Meta.Name, Quote: f.Meta.Source.OwnerQuote, From: f.Meta.Source.OwnerQuoteFrom}.Key()]
		add = append(add, ledger.Quote{Name: st.Meta.Name, Quote: st.Meta.Source.OwnerWords, From: it.From, TurnHash: rq.TurnHash, At: st.Meta.Created, Words: true}.Line()...)
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, err
		}
		g := *f
		g.Meta.SupersededBy, g.Meta.Updated = st.Meta.Name, today
		out, err := fact.PatchRelations(data, &g)
		if err != nil {
			return nil, err
		}
		files[f.Path] = out
	}
	if len(add) > base {
		files[ledger.QuotesPath] = add
	}
	if len(log) > 0 {
		sl, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.StatementsPath)))
		files[ledger.StatementsPath] = append(withNewline(sl), log...)
	}
	return files, nil
}

// promotedFacts are the facts the statements step promoted (its log).
func promotedFacts(dir string) map[string]bool {
	b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.StatementsPath)))
	out := map[string]bool{}
	for _, e := range statement.ReadLog(b) {
		if e.Kind == statement.LogPromoted && e.Fact != "" {
			out[e.Fact] = true
		}
	}
	return out
}

// maxGateTurn bounds the owner's message the gate shows, in characters.
const maxGateTurn = 1500

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// oneOffsRun is the gate's model call (role review, Opus by default: every
// deciding call runs on Opus); tests substitute it.
var oneOffsRun agentreview.Runner = func(ctx context.Context, prompt string) (string, float64, string, error) {
	return reviewRun(ctx, prompt)
}

// gatePrompt is the gate's prompt for a batch of one-offs: each remark, the
// owner's whole message when code found it, the rule an agent made of it,
// its scope and day.
func gatePrompt(items []*oneOff) string {
	var b strings.Builder
	b.WriteString(`You judge remarks the owner of a coding-agent memory store made once, each in one session of his. Agents saved each as a standing rule. Code will keep each as his statement (his words verbatim, dated, shown to later agents as "you said once") only if it would matter to future work; otherwise it is dropped, and kept only in the session history.

` + relevance.Rule + `

Judge his words in his message, not the agent's rule: the rule shows how an agent read them, and may widen them. A remark that steers the task in progress ("run it again", "now push", "go with option 2") is steering even when the agent wrote it as a rule. A decision or instruction about a project, a name, a tool or how he wants work done is kept, even when it was said in passing.

`)
	for i, it := range items {
		fmt.Fprintf(&b, "<item n=\"%d\">\nhis words: %s\n", i+1, strings.Join(strings.Fields(it.Quote), " "))
		if it.Turn != "" && ownerwords.Normalize(it.Turn) != ownerwords.Normalize(it.Quote) {
			fmt.Fprintf(&b, "his whole message: %s\n", strings.Join(strings.Fields(it.Turn), " "))
		}
		fmt.Fprintf(&b, "the rule an agent made of it: %s\nscope: %s; type: %s; day: %s\n</item>\n\n", strings.Join(strings.Fields(it.Description), " "), it.Scope, it.Type, it.Created)
	}
	b.WriteString(`Reply with only a JSON array, one object per item in order, no other text:
[{"n": 1, "keep": true | false, "category": "<category>", "reason": "<one plain sentence>"}]`)
	return b.String()
}

// parseGate reads the gate's reply: the first JSON array, one judgement per
// item by its number.
func parseGate(text string, n int) (map[int]*relevance.Judgement, error) {
	i := strings.Index(text, "[")
	if i < 0 {
		return nil, fmt.Errorf("the gate's reply held no JSON array")
	}
	var rows []struct {
		N int `json:"n"`
		relevance.Judgement
	}
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&rows); err != nil {
		return nil, fmt.Errorf("the gate's reply: %w", err)
	}
	out := map[int]*relevance.Judgement{}
	for _, r := range rows {
		if r.N >= 1 && r.N <= n && out[r.N] == nil {
			j := r.Judgement
			out[r.N] = &j
		}
	}
	return out, nil
}

// reusedVerdict is an earlier report's judgement of the same ruling on the
// same words (--verdicts): the gate does not ask again.
func reusedVerdicts(r *oneOffsReport) map[string]*relevance.Judgement {
	out := map[string]*relevance.Judgement{}
	if r == nil {
		return out
	}
	for _, it := range r.Items {
		if it.Relevance != nil && (it.Gate == gateKeep || it.Gate == gateDrop) {
			j := *it.Relevance
			out[it.Name+"\x00"+ownerwords.Normalize(it.Quote)] = &j
		}
	}
	return out
}

// oneOffGate is the relevance gate on the report's one-offs: a judgement
// code takes (relevance.Check) settles each, kept or dropped; a missing or
// refused one leaves it unjudged. Batches of batch go to the model, jobs at
// a time. It returns the calls that failed.
func oneOffGate(ctx context.Context, rep *oneOffsReport, reuse map[string]*relevance.Judgement, batch, jobs int) int {
	rep.Gated, rep.Categories = true, map[string]int{}
	var todo []*oneOff
	settle := func(it *oneOff, j *relevance.Judgement) {
		c, p := relevance.Check(j)
		switch {
		case j == nil:
			it.Gate, it.Problem = gateUnjudged, "the model gave no judgement"
		case p != "":
			it.Gate, it.Problem, it.Relevance = gateUnjudged, p, &c
		case c.Keep:
			it.Gate, it.Relevance = gateKeep, &c
		default:
			it.Gate, it.Relevance = gateDrop, &c
		}
	}
	for i := range rep.Items {
		it := &rep.Items[i]
		if it.Statement == "" {
			continue
		}
		if j := reuse[it.Name+"\x00"+ownerwords.Normalize(it.Quote)]; j != nil {
			settle(it, j)
			rep.Reused++
			continue
		}
		todo = append(todo, it)
	}
	var groups [][]*oneOff
	for len(todo) > 0 {
		n := min(batch, len(todo))
		groups = append(groups, todo[:n])
		todo = todo[n:]
	}
	var mu sync.Mutex
	failed := 0
	sem := make(chan struct{}, max(1, jobs))
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(g []*oneOff) {
			defer wg.Done()
			defer func() { <-sem }()
			text, cost, model, err := oneOffsRun(ctx, gatePrompt(g))
			var js map[int]*relevance.Judgement
			if err == nil {
				js, err = parseGate(text, len(g))
			}
			mu.Lock()
			defer mu.Unlock()
			rep.GateCalls++
			rep.CostUSD += cost
			if model != "" {
				rep.Model = model
			}
			if err != nil {
				failed++
				fmt.Fprintln(os.Stderr, "rulings one-offs: gate:", err)
			}
			for k, it := range g {
				settle(it, js[k+1])
			}
		}(g)
	}
	wg.Wait()
	rep.GateFailed = failed
	for _, it := range rep.Items {
		switch it.Gate {
		case gateKeep:
			rep.GateKept++
			rep.Categories["keep "+it.Relevance.Category]++
		case gateDrop:
			rep.GateDrop++
			rep.Categories["drop "+it.Relevance.Category]++
		case gateUnjudged:
			rep.Unjudged++
		}
	}
	return failed
}

// gateSample prints n kept and n dropped one-offs, chosen by seed.
func gateSample(rep *oneOffsReport, n int, seed int64) {
	if n <= 0 || !rep.Gated {
		return
	}
	rnd := rand.New(rand.NewSource(seed))
	for _, g := range []string{gateKeep, gateDrop} {
		var xs []oneOff
		for _, it := range rep.Items {
			if it.Gate == g {
				xs = append(xs, it)
			}
		}
		rnd.Shuffle(len(xs), func(i, j int) { xs[i], xs[j] = xs[j], xs[i] })
		for _, it := range xs[:min(n, len(xs))] {
			fmt.Printf("  %s %s: %q (%s): %s\n", g, it.Relevance.Category, clipRunes(strings.Join(strings.Fields(it.Quote), " "), 160), it.Name, it.Relevance.Reason)
		}
	}
}

func readOneOffsReport(path string) (*oneOffsReport, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r oneOffsReport
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// runOneOffs implements `tacit rulings one-offs [--dry-run] [--json FILE]
// [--digests DIR] [--verdicts FILE] [--found FILE] [--no-gate] [--batch N]
// [--jobs N] [--sample N] [--seed N] [--env-file F]`.
func runOneOffs(args []string) int {
	fs := flag.NewFlagSet("rulings one-offs", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "find, judge and report; write nothing, do not sync")
	home, _ := os.UserHomeDir()
	digests := fs.String("digests", ownerwords.DefaultDigests(home), "session digests (eval/mine/digest.py output); empty for none")
	jsonOut := fs.String("json", "", "write the report with every ruling looked at to this file")
	verdicts := fs.String("verdicts", "", "an earlier report (--json) whose gate judgements of the same rulings on the same words are taken, not asked again")
	found := fs.String("found", "", "with --dry-run: take the sessions and turns of the owner's words from an earlier report (--json), not from this machine's transcripts")
	noGate := fs.Bool("no-gate", false, "with --dry-run: find only, no model call")
	batch := fs.Int("batch", 20, "one-offs per gate call")
	jobs := fs.Int("jobs", 4, "gate calls at a time")
	sample := fs.Int("sample", 5, "print N kept and N dropped one-offs")
	seed := fs.Int64("seed", 1, "seed of the sample")
	envFile := fs.String("env-file", "", "KEY=VALUE lines with the runner's provider variables")
	fs.Parse(args)
	if (*found != "" || *noGate) && !*dry {
		fmt.Fprintln(os.Stderr, "rulings one-offs: --found and --no-gate go with --dry-run only: a write reads the owner's turns here and judges every one-off")
		return 2
	}
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "rulings one-offs: no store")
		return 2
	}
	ctx := context.Background()
	if !*dry && refuseWriter(ctx, s, "curator", "rulings one-offs") {
		return 2
	}
	if !*dry && !statementsWrite(s.Dir) {
		fmt.Fprintln(os.Stderr, "rulings one-offs: the store's format predates statements (tacit format --set)")
		return 2
	}
	var look ownerLookup
	if *found != "" {
		r, err := readOneOffsReport(*found)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
		look = reportLookup(r)
	} else {
		corpus, err := loadOwnerCorpus(home, *digests, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs: reading the owner's turns:", err)
			return 2
		}
		look = corpusLookup(corpus)
	}
	reuse := map[string]*relevance.Judgement{}
	if *verdicts != "" {
		r, err := readOneOffsReport(*verdicts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
		reuse = reusedVerdicts(r)
	}
	failed := 0
	var rep *oneOffsReport
	if *dry {
		facts, _, err := fact.Walk(s.Dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
		rep = findOneOffs(facts, look, promotedFacts(s.Dir))
		if !*noGate {
			failed = oneOffGate(ctx, rep, reuse, *batch, *jobs)
		}
	} else {
		// The model is asked before the write, so the curator's write holds
		// the store for code alone; the write takes the judgements again
		// (only a ruling new since is asked then).
		if err := s.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs: sync:", err)
			return 1
		}
		facts, _, err := fact.Walk(s.Dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
		first := findOneOffs(facts, look, promotedFacts(s.Dir))
		failed = oneOffGate(ctx, first, reuse, *batch, *jobs)
		for k, v := range reusedVerdicts(first) {
			reuse[k] = v
		}
		pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
			facts, _, err := fact.Walk(s.Dir)
			if err != nil {
				return nil, "", err
			}
			rep = findOneOffs(facts, look, promotedFacts(s.Dir))
			failed += oneOffGate(ctx, rep, reuse, *batch, *jobs)
			for k, v := range reusedVerdicts(rep) {
				reuse[k] = v
			}
			rep.Reused = first.Reused // the judgements --verdicts gave, not those the first pass made
			rep.GateCalls += first.GateCalls
			rep.GateFailed += first.GateFailed
			rep.CostUSD += first.CostUSD
			if rep.Model == "" {
				rep.Model = first.Model
			}
			byName := map[string]*fact.Fact{}
			for _, f := range facts {
				byName[f.Meta.Name] = f
			}
			files, err := oneOffWrites(s.Dir, rep, byName, time.Now().Format("2006-01-02"))
			msg := fmt.Sprintf("curator: one-offs: %d ruling(s) said in one session are the owner's statements, %d retired as not mattering to future work\n\nSPEC.md, One-offs and Relevance: words said once are a statement, never a rule, and only\nwhen they would matter to future work; only the statements step's ladder (said again in a\nseparate session, or a check) promotes them. %d left unjudged for the next run.", rep.GateKept, rep.GateDrop, rep.Unjudged)
			return files, msg, err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 1
		}
		rep.Pushed = pushed
	}
	fmt.Printf("rulings one-offs: %d live ruling(s); %d said in one session (%d no longer in a transcript here); kept: %v; user_standard facts holding their words, no ruling: %d one session, %d more\n",
		rep.Rulings, rep.OneOffs, rep.NotFound, rep.Kept, rep.WordsOneSession, rep.WordsManySessions)
	if rep.Gated {
		fmt.Printf("rulings one-offs: relevance gate: %d kept as statements, %d dropped, %d unjudged (%d judgement(s) reused); %v; %d call(s), %d failed, $%.2f\n",
			rep.GateKept, rep.GateDrop, rep.Unjudged, rep.Reused, rep.Categories, rep.GateCalls, rep.GateFailed, rep.CostUSD)
		gateSample(rep, *sample, *seed)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", " ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}
