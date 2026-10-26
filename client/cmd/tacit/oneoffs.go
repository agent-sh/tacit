package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
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
// or a check already). No model is called.

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
}

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
}

// findOneOffs looks at the live curated rulings of facts with the owner's
// corpus. promoted names the statements step's promoted facts.
func findOneOffs(facts []*fact.Fact, corpus *ownerwords.Corpus, promoted map[string]bool) *oneOffsReport {
	rep := &oneOffsReport{At: time.Now().UTC().Format(time.RFC3339), Kept: map[string]int{}, ByType: map[string]int{}}
	for _, f := range facts {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Path, "local/") {
			continue
		}
		s := f.Meta.Source
		if !fact.IsRuling(f) {
			if f.Meta.Type == "user_standard" && fact.OwnerWordsOf(f) != "" {
				if len(corpus.SaidIn(s.OwnerWords)) >= 2 {
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
			it.Sessions = corpus.SaidIn(s.OwnerQuote)
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
	host := store.ShortHost()
	for _, it := range rep.Items {
		if it.Statement == "" {
			continue
		}
		f := byName[it.Name]
		if f == nil {
			continue
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
	if len(files) > 0 {
		files[ledger.QuotesPath] = add
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

// runOneOffs implements `tacit rulings one-offs [--dry-run] [--json FILE]
// [--digests DIR]`.
func runOneOffs(args []string) int {
	fs := flag.NewFlagSet("rulings one-offs", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "find and report, write nothing, do not sync")
	home, _ := os.UserHomeDir()
	digests := fs.String("digests", ownerwords.DefaultDigests(home), "session digests (eval/mine/digest.py output); empty for none")
	jsonOut := fs.String("json", "", "write the report with every ruling looked at to this file")
	fs.Parse(args)
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
	corpus, err := loadOwnerCorpus(home, *digests, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rulings one-offs: reading the owner's turns:", err)
		return 2
	}
	var rep *oneOffsReport
	if *dry {
		facts, _, err := fact.Walk(s.Dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
		rep = findOneOffs(facts, corpus, promotedFacts(s.Dir))
	} else {
		pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
			facts, _, err := fact.Walk(s.Dir)
			if err != nil {
				return nil, "", err
			}
			rep = findOneOffs(facts, corpus, promotedFacts(s.Dir))
			byName := map[string]*fact.Fact{}
			for _, f := range facts {
				byName[f.Meta.Name] = f
			}
			files, err := oneOffWrites(s.Dir, rep, byName, time.Now().Format("2006-01-02"))
			msg := fmt.Sprintf("curator: one-offs: %d ruling(s) said in one session are the owner's statements\n\nSPEC.md, One-offs: words said once are a statement, never a rule; only the statements\nstep's ladder (said again in a separate session, or a check) promotes them. No model was called.", rep.OneOffs)
			return files, msg, err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 1
		}
		rep.Pushed = pushed
	}
	fmt.Printf("rulings one-offs: %d live ruling(s); %d said in one session -> statements (%d no longer in a transcript here); kept: %v; user_standard facts holding their words, no ruling: %d one session, %d more\n",
		rep.Rulings, rep.OneOffs, rep.NotFound, rep.Kept, rep.WordsOneSession, rep.WordsManySessions)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", " ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-offs:", err)
			return 2
		}
	}
	return 0
}
