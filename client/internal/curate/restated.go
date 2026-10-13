package curate

import (
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// RestateOverlap is the share of description terms (Jaccard) at which a
// pending note restates a fact. Measured on the 2,094 pending dreamer notes
// of 2026-10-11 with the token guard only: 0.5 dropped 4.9 percent and let
// through pairs that differ in a detail (one task's lease path against
// another's), 0.6 dropped 3.4 percent, 0.7 dropped 1.8 percent. With every
// guard below, 0.6 drops 23 of the 2,108 pending notes (1.1 percent), each
// a restatement in a sample of six.
const RestateOverlap = 0.6

// specificRx finds the tokens that carry a fact's specifics: anything with a
// digit (ports, versions, dates, SHAs, task ids), paths, and dotted names
// (file.go, a.b.c).
var specificRx = regexp.MustCompile(`[\w./~@:-]*\d[\w./~@:-]*|[\w~.-]*/[\w./~-]+|\w+\.\w[\w.]*`)

// specifics is the set of specific tokens in s, compared whole: port 778 is
// not in a fact that says 7781.
func specifics(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range specificRx.FindAllString(s, -1) {
		m = strings.ToLower(strings.Trim(m, ".,:;`'\"()"))
		if len(m) >= 2 {
			out[m] = true
		}
	}
	return out
}

// changeWords mark a reversal or an update. search.Terms drops most of them
// as stop words, so without this a note saying a fact no longer holds reads
// as a restatement of it.
var changeWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("not no never none nothing nobody nowhere neither nor without longer anymore now instead moved removed deleted deprecated replaced renamed retired obsolete gone dead dropped stopped disabled enabled reverted changed switched until unless except but only") {
		changeWords[w] = true
	}
}

var wordRx = regexp.MustCompile(`[\p{L}\p{N}_']+`)

// contractions read as "not": don't, isn't, doesnt, cant.
var notRx = regexp.MustCompile(`^(do|does|did|is|are|was|were|wo|ca|can|could|should|would|has|have|had|must|need)n'?t$`)

// changes is the set of change words in s.
func changes(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRx.FindAllString(strings.ToLower(s), -1) {
		switch {
		case notRx.MatchString(w):
			out["not"] = true
		case changeWords[w]:
			out[w] = true
		}
	}
	return out
}

// allTerms is search.Terms without its cap of 24 terms.
func allTerms(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		for _, t := range search.Terms(w) {
			out[t] = true
		}
	}
	return out
}

// Restated returns the pending notes that only restate what the store
// already holds, keyed by note name, with the fact (or earlier pending note)
// they restate. No model. A note restates a fact when most of its
// description's terms are the fact's (RestateOverlap), every specific token
// of the note (a number, a path, a dotted name) is in the fact, and every
// word of change or negation of the note (not, no longer, now, moved,
// removed, instead) is in the fact too: a note that says the same thing with
// a new port, path or version, or says it stopped being true, is an update
// the conflict check must see, and it stays.
//
// The fact must be one the note's scope can see: the same scope, user or
// system (a user or system note against a repo's fact would widen it, and
// stays). An earlier pending note must be of the same scope. Only a note
// whose body is its description is considered (every dreamer note): a body
// that adds a quote or a reason is new. A note in the owner's words (kind
// user) goes only against a ruling, or an earlier pending note in their words,
// so their quote is never lost to an agent's paraphrase.
//
// Pending is in curation order: of two pending notes that restate each
// other, the first stays.
func Restated(pending, curated []*fact.Fact) map[string]string {
	type entry struct {
		f       *fact.Fact
		terms   map[string]bool
		spec    map[string]bool // specific tokens of description and body
		changes map[string]bool
		user    bool // a ruling, or a note in their words
		curated bool
	}
	mk := func(f *fact.Fact, user, curated bool) *entry {
		text := f.Meta.Description + " " + f.Body
		return &entry{f: f, terms: allTerms(f.Meta.Description), spec: specifics(text), changes: changes(text), user: user, curated: curated}
	}
	var pool []*entry
	inv := map[string][]int{}
	add := func(e *entry) {
		for t := range e.terms {
			inv[t] = append(inv[t], len(pool))
		}
		pool = append(pool, e)
	}
	for _, f := range curated {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Meta.Scope, "derived/") {
			continue
		}
		add(mk(f, fact.IsRuling(f), true))
	}
	out := map[string]string{}
	for _, n := range pending {
		e := mk(n, n.Meta.Source.Kind == "user", false)
		if len(e.terms) == 0 || norm(n.Body) != norm(n.Meta.Description) {
			add(e)
			continue
		}
		shared := map[int]int{}
		for t := range e.terms {
			for _, i := range inv[t] {
				shared[i]++
			}
		}
		type cand struct {
			i int
			j float64
		}
		var cs []cand
		for i, k := range shared {
			if j := float64(k) / float64(len(e.terms)+len(pool[i].terms)-k); j >= RestateOverlap {
				cs = append(cs, cand{i, j})
			}
		}
		sort.Slice(cs, func(a, b int) bool {
			if cs[a].j != cs[b].j {
				return cs[a].j > cs[b].j
			}
			return cs[a].i < cs[b].i
		})
		hit := ""
		for _, c := range cs {
			o := pool[c.i]
			if o.curated {
				if o.f.Meta.Scope != n.Meta.Scope && o.f.Meta.Scope != "user" && o.f.Meta.Scope != "system" {
					continue
				}
			} else if o.f.Meta.Scope != n.Meta.Scope {
				continue
			}
			if e.user && !o.user {
				continue
			}
			if subset(e.spec, o.spec) && subset(e.changes, o.changes) {
				hit = o.f.Meta.Name
			}
			break // only the closest fact the note can see decides
		}
		if hit != "" {
			out[n.Meta.Name] = hit
			continue
		}
		add(e)
	}
	return out
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }
