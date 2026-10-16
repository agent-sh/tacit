package search

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// TermEvidence reports corpus-relative information, in nats. Unknown terms
// have no weight: a typo is not evidence against every stored fact.
type TermEvidence struct {
	Term      string
	Documents int
	IDF       float64
	Matched   bool
}

// RelationEvidence is an observed ordered summary phrase, not a semantic edge.
// SupportDiscountedLift removes this fact's support before comparing the pair with
// the independent marginal frequencies. A lone coincidence cannot support itself.
type RelationEvidence struct {
	Terms                 []string
	Documents             int
	SupportDiscountedLift float64
}

type LocationEvidence struct {
	Fact      string
	Path      string
	Scope     string
	Via       string
	AppliesTo fact.AppliesTo
}

// RetrievalEvidence explains selection, not authority or permission to act.
type RetrievalEvidence struct {
	CorpusDocuments int
	Terms           []TermEvidence
	Coverage        float64
	Relations       []RelationEvidence
	Locations       []LocationEvidence
	Supported       bool
	LocationBound   bool // a relation term also occurs in the fact ID or stored applicability
}

type evidenceDocument struct {
	id                                   int
	name, path, scope, description, body string
	applies                              fact.AppliesTo
	terms                                map[string]bool
	pairs                                map[string]bool
}

// evidenceTokens preserves order and repetitions; unlike Terms it does not cap
// document length. Hyphenated identifiers use the same splitting as Terms.
func evidenceTokens(text string) []string {
	var out []string
	for _, token := range tokenRe.FindAllString(strings.ToLower(text), -1) {
		for _, t := range strings.FieldsFunc(token, func(r rune) bool { return r == '.' || r == '/' || r == '-' }) {
			if len([]rune(t)) < 2 || (len([]rune(t)) < 3 && !hasNonASCII(t)) || stop[t] {
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

var phraseToken = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func evidencePairs(text string) map[string]bool {
	// Keep stop words and short tokens as boundaries. Removing them would
	// invent phrases such as "add new" from "add a new".
	tokens := phraseToken.FindAllString(strings.ToLower(text), -1)
	pairs := map[string]bool{}
	for i := 1; i < len(tokens); i++ {
		if tokens[i-1] != tokens[i] {
			pairs[tokens[i-1]+"\x00"+tokens[i]] = true
		}
	}
	return pairs
}

// promptEvidence is deliberately query-local: no graph, transitive expansion,
// body cooccurrence, or inferred filesystem paths. Curated active summaries
// supply both the corpus marginals and direct phrase witnesses.
func (ix *Index) promptEvidence(ctx context.Context, terms []string, query string) (map[string]*RetrievalEvidence, []int, error) {
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM evidence`).Scan(&n); err != nil {
		return nil, nil, err
	}
	df, err := ix.evidenceFrequency(ctx, 0, terms)
	if err != nil {
		return nil, nil, err
	}
	queryPairs := evidencePairs(query)
	queryTerms := map[string]bool{}
	for _, term := range terms {
		queryTerms[term] = true
	}
	var pairs []string
	for p := range queryPairs {
		ts := strings.Split(p, "\x00")
		if !queryTerms[ts[0]] || !queryTerms[ts[1]] {
			delete(queryPairs, p)
			continue
		}
		pairs = append(pairs, p)
	}
	pairDF, err := ix.evidenceFrequency(ctx, 1, pairs)
	if err != nil {
		return nil, nil, err
	}
	queryJSON, _ := json.Marshal(terms)
	rows, err := ix.db.QueryContext(ctx, `SELECT f.id,f.name,f.path,f.scope,f.description,f.body,f.applies,e.terms,e.pairs FROM facts f JOIN evidence e ON f.id=e.id WHERE f.id IN (SELECT id FROM evidence_postings WHERE kind=0 AND token IN (SELECT value FROM json_each(?)))`, string(queryJSON))
	if err != nil {
		return nil, nil, err
	}
	var docs []evidenceDocument
	for rows.Next() {
		var d evidenceDocument
		var applies, ts, ps string
		if err := rows.Scan(&d.id, &d.name, &d.path, &d.scope, &d.description, &d.body, &applies, &ts, &ps); err != nil {
			rows.Close()
			return nil, nil, err
		}
		for _, v := range []struct {
			text   string
			target any
		}{{applies, &d.applies}, {ts, &d.terms}, {ps, &d.pairs}} {
			if err := json.Unmarshal([]byte(v.text), v.target); err != nil {
				rows.Close()
				return nil, nil, err
			}
		}
		docs = append(docs, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	byName := map[string][]LocationEvidence{}
	var links []string
	for _, d := range docs {
		links = append(links, (&fact.Fact{Body: d.body}).Links()...)
	}
	linksJSON, _ := json.Marshal(links)
	rows, err = ix.db.QueryContext(ctx, `SELECT f.name,f.path,f.scope,f.applies FROM facts f JOIN evidence e ON e.id=f.id WHERE f.name IN (SELECT value FROM json_each(?))`, string(linksJSON))
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var l LocationEvidence
		var applies string
		if err := rows.Scan(&l.Fact, &l.Path, &l.Scope, &applies); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if err := json.Unmarshal([]byte(applies), &l.AppliesTo); err != nil {
			rows.Close()
			return nil, nil, err
		}
		l.Via = "[[" + l.Fact + "]]"
		byName[l.Fact] = append(byName[l.Fact], l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	weights := map[string]float64{}
	var total float64
	for _, t := range terms {
		if df[t] > 0 {
			weights[t] = math.Log(float64(n+1) / float64(df[t]+1))
			total += weights[t]
		}
	}
	result := map[string]*RetrievalEvidence{}
	var approved []int
	for _, d := range docs {
		e := &RetrievalEvidence{CorpusDocuments: n}
		var matched float64
		for _, t := range terms {
			e.Terms = append(e.Terms, TermEvidence{t, df[t], weights[t], d.terms[t]})
			if d.terms[t] {
				matched += weights[t]
			}
		}
		if matched == 0 {
			continue
		}
		if total > 0 {
			e.Coverage = matched / total
		}
		for p := range queryPairs {
			if !d.pairs[p] || pairDF[p] < 2 {
				continue
			}
			ts := strings.Split(p, "\x00")
			if df[ts[0]] == 0 || df[ts[1]] == 0 {
				continue
			}
			lift := float64((pairDF[p]-1)*n) / float64(df[ts[0]]*df[ts[1]])
			if lift > 1 {
				e.Relations = append(e.Relations, RelationEvidence{ts, pairDF[p], lift})
			}
		}
		sort.Slice(e.Relations, func(i, j int) bool {
			return strings.Join(e.Relations[i].Terms, " ") < strings.Join(e.Relations[j].Terms, " ")
		})
		// Majority of the known query information plus independently supported
		// local phrase evidence, rather than a BM25 multiplier or a rare-word veto.
		bindings := map[string]bool{}
		fields := append(append(append([]string{d.name}, d.applies.Commands...), d.applies.Paths...), d.applies.Symbols...)
		for _, field := range fields {
			for _, term := range evidenceTokens(field) {
				bindings[term] = true
			}
		}
		for _, relation := range e.Relations {
			for _, term := range relation.Terms {
				if bindings[term] {
					e.LocationBound = true
				}
			}
		}
		e.Supported = e.Coverage > 0.5 && len(e.Relations) > 0 && e.LocationBound
		e.Locations = append(e.Locations, LocationEvidence{d.name, d.path, d.scope, "summary", d.applies})
		f := &fact.Fact{Body: d.body}
		for _, name := range f.Links() {
			// Ambiguous/dangling links are not locations. This is one hop only and
			// never promotes a linked fact into the delivery set.
			if targets := byName[name]; len(targets) == 1 {
				e.Locations = append(e.Locations, targets[0])
			}
		}
		result[d.path] = e
		if e.Supported {
			approved = append(approved, d.id)
		}
	}
	return result, approved, nil
}
