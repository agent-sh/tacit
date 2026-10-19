package curate

import (
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/search"
)

// RestateOverlap is the share of description terms (Jaccard) at which a
// pending note restates a fact. Measured on the 2,094 pending dreamer notes
// of 2026-10-11 with the token guard only: 0.5 dropped 4.9 percent and let
// through pairs that differ in a detail (one task's lease path against
// another's), 0.6 dropped 3.4 percent, 0.7 dropped 1.8 percent. The original guards
// dropped 23 of the 2,108 pending notes (1.1 percent), each
// a restatement in a sample of six. The negation guard now checks both
// directions after a live fixture showed a positive update being dropped.
const RestateOverlap = 0.6

// specificRx finds the tokens that carry a fact's specifics: anything with a
// digit (ports, versions, dates, SHAs, task ids), paths, and dotted names
// (file.go, a.b.c).
var specificRx = regexp.MustCompile(`[\w./~@:-]*\d[\w./~@:-]*|[\w~.-]*/[\w./~-]+|\w+\.\w[\w.]*`)

// codeRx finds a `code span`: a command or a name an agent marked as one
// ("use `aws logs start-query`"), which holds no digit or path as often as not.
var codeRx = regexp.MustCompile("`([^`\n]{2,120})`")

// specifics is the set of specific tokens in s, compared whole: port 778 is
// not in a fact that says 7781. A code span is one token, whole.
func specifics(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range specificRx.FindAllString(s, -1) {
		m = strings.ToLower(strings.Trim(m, ".,:;`'\"()"))
		if len(m) >= 2 {
			out[m] = true
		}
	}
	for _, m := range codeRx.FindAllStringSubmatch(s, -1) {
		out["`"+strings.Join(strings.Fields(strings.ToLower(m[1])), " ")+"`"] = true
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

// statement is a fact or note in a pool that later statements are checked
// against: its description's terms, the specific tokens and change words of
// its whole text, and the owner's phrase it holds.
type statement struct {
	f       *fact.Fact
	terms   map[string]bool
	spec    map[string]bool // specific tokens of description and body
	idents  map[string]bool // names with @ or _ in them (a plugin id, mb_readonly), for restatesFact
	changes map[string]bool
	// descChanges are the change words of the description alone: between two
	// curated facts the description is the statement, and a body's reasons
	// ("the agent cannot refresh it") carry negations of their own.
	descChanges map[string]bool
	user        bool   // a ruling, or a note in their words
	phrase      string // the owner's words the fact holds (owner_quote or owner_words), normalized
	curated     bool
}

func newStatement(f *fact.Fact, user, curated bool) *statement {
	text := f.Meta.Description + " " + f.Body
	phrase := f.Meta.Source.OwnerQuote
	if phrase == "" {
		phrase = f.Meta.Source.OwnerWords
	}
	return &statement{f: f, terms: allTerms(f.Meta.Description), spec: specifics(text), idents: identifiers(text), changes: changes(text), descChanges: changes(f.Meta.Description), user: user, phrase: ownerwords.Normalize(phrase), curated: curated}
}

// pool indexes statements by description term.
type pool struct {
	items   []*statement
	inv     map[string][]int
	dropped map[string]bool // paths of facts retired since they were added
}

func (p *pool) add(e *statement) {
	if p.inv == nil {
		p.inv = map[string][]int{}
	}
	for t := range e.terms {
		p.inv[t] = append(p.inv[t], len(p.items))
	}
	p.items = append(p.items, e)
}

// closest lists the statements that share at least overlap of e's
// description terms (Jaccard), closest first, earliest first on a tie.
func (p *pool) closest(e *statement, overlap float64) []*statement {
	shared := map[int]int{}
	for t := range e.terms {
		for _, i := range p.inv[t] {
			shared[i]++
		}
	}
	type cand struct {
		i int
		j float64
	}
	var cs []cand
	for i, k := range shared {
		if p.dropped[p.items[i].f.Path] {
			continue
		}
		if j := float64(k) / float64(len(e.terms)+len(p.items[i].terms)-k); j >= overlap {
			cs = append(cs, cand{i, j})
		}
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].j != cs[b].j {
			return cs[a].j > cs[b].j
		}
		return cs[a].i < cs[b].i
	})
	out := make([]*statement, len(cs))
	for k, c := range cs {
		out[k] = p.items[c.i]
	}
	return out
}

// adds reports whether e says something o does not: a specific token (a
// number, a path, a dotted name) o lacks, or another set of words of change
// or negation. A removed negation changes the fact too: one-way containment
// discarded a positive note against an older negative statement.
func adds(e, o *statement) bool {
	return !subset(e.spec, o.spec) || !subset(e.changes, o.changes) || !subset(o.changes, e.changes)
}

// Restated returns the pending notes that only restate what the store
// already holds, keyed by note name, with the fact (or earlier pending note)
// they restate. No model. A note restates a fact when most of its
// description's terms are the fact's (RestateOverlap), every specific token
// of the note (a number, a path, a dotted name) is in the fact, and the same set of
// words of change or negation in both statements (not, no longer, now, moved,
// removed, instead) is present: a note that says the same thing with
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
	var p pool
	for _, f := range curated {
		if live(f) {
			p.add(newStatement(f, fact.IsRuling(f), true))
		}
	}
	out := map[string]string{}
	for _, n := range pending {
		e := newStatement(n, n.Meta.Source.Kind == "user", false)
		if len(e.terms) == 0 || norm(n.Body) != norm(n.Meta.Description) {
			p.add(e)
			continue
		}
		hit := ""
		for _, o := range p.closest(e, RestateOverlap) {
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
			if !adds(e, o) {
				hit = o.f.Meta.Name
			}
			break // only the closest fact the note can see decides
		}
		if hit != "" {
			out[n.Meta.Name] = hit
			continue
		}
		p.add(e)
	}
	return out
}

// live is a curated fact delivery can still show and the curator may compare
// against: not an inbox note, not superseded or retired, not generated.
func live(f *fact.Fact) bool {
	return !f.InInbox() && f.Meta.SupersededBy == "" && f.Meta.Retired == "" && !strings.HasPrefix(f.Meta.Scope, "derived/")
}

// restatesFact is the rule for one curated fact against another: e only
// restates o when both are in the same scope, most of e's description terms
// are o's (the caller found o by pool.closest), e's text holds no specific
// token o's text does not cover (addsSpecific) and no name with @ or _ o
// lacks (identifiers), both descriptions hold the
// same words of change or negation, and e holds no words of the owner's
// that o lacks. A ruling
// restates only a ruling with the same quote: rulings are never folded into
// an agent's statement, nor one owner quote into another. An observation
// that restates a ruling is the ruling's.
func restatesFact(e, o *statement) bool {
	if o.f.Meta.Scope != e.f.Meta.Scope || o.f.Meta.Name == e.f.Meta.Name {
		return false
	}
	if e.user && !o.user {
		return false
	}
	if e.phrase != "" && e.phrase != o.phrase {
		return false
	}
	return !subsetChanges(e.descChanges, o.descChanges) && !addsSpecific(e.spec, o.spec) && subset(e.idents, o.idents)
}

// identRx finds names joined with @ or _: plugin and package ids
// (remember@claude-plugins-official), profiles and variables (mb_readonly).
// specificRx misses them when they hold no digit, dot or slash, and between
// two facts on the same setting they are often the only difference.
var identRx = regexp.MustCompile(`[\w.-]*[@_][\w.@-]*`)

func identifiers(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range identRx.FindAllString(strings.ToLower(s), -1) {
		m = strings.Trim(m, ".-_@")
		if len(m) >= 3 && strings.ContainsAny(m, "@_") {
			out[m] = true
		}
	}
	return out
}

func subsetChanges(a, b map[string]bool) bool { return !subset(a, b) || !subset(b, a) }

// homeRx is a home directory spelled out; a fact may write it as ~.
var homeRx = regexp.MustCompile(`^/(users|home)/[^/]+/`)

// specificForm is a specific token as two facts may spell it differently:
// the home directory as ~, no trailing slash.
func specificForm(t string) string {
	t = homeRx.ReplaceAllString(t, "~/")
	if len(t) > 1 {
		t = strings.TrimRight(t, "/")
	}
	return t
}

// notSpecific are abbreviations specificRx reads as dotted names.
var notSpecific = map[string]bool{"e.g": true, "i.e": true, "etc": true, "vs": true}

// addsSpecific reports whether a holds a specific token b does not cover.
// Between two curated facts a token is covered when b holds it spelled the
// same way, or, for a path or dotted name without a digit, inside a longer
// one at a separator: "/v1/rgstr" is in "https://ab.chatgpt.com/v1/rgstr",
// "~/.codex/memories" in "~/.codex/memories/memory.md". A token with a digit
// (a port, a version, a date, a SHA) is covered only whole: 778 is not 7781.
func addsSpecific(a, b map[string]bool) bool {
	var bs []string
	for t := range b {
		bs = append(bs, specificForm(t))
	}
outer:
	for t := range a {
		if notSpecific[t] {
			continue
		}
		t = specificForm(t)
		digit := strings.ContainsAny(t, "0123456789")
		for _, u := range bs {
			if u == t {
				continue outer
			}
			if !digit && len(t) >= 3 {
				for i := strings.Index(u, t); i >= 0; {
					end := i + len(t)
					if (i == 0 || strings.ContainsRune("/.:~@", rune(u[i-1])) || strings.ContainsRune("/.:~@", rune(t[0]))) &&
						(end == len(u) || strings.ContainsRune("/.:", rune(u[end]))) {
						continue outer
					}
					next := strings.Index(u[i+1:], t)
					if next < 0 {
						break
					}
					i += 1 + next
				}
			}
		}
		return true
	}
	return false
}

// Absorber checks the facts a split makes against the curated store, before
// they are written: a fact that only restates a curated one (restatesFact)
// is not written a second time, and its note is recorded as absorbed by that
// fact, as Restated does for a whole note.
type Absorber struct{ p pool }

// NewAbsorber indexes the live curated facts.
func NewAbsorber(curated []*fact.Fact) *Absorber {
	a := &Absorber{}
	for _, f := range curated {
		if live(f) {
			a.Add(f, fact.IsRuling(f))
		}
	}
	return a
}

// Add puts a fact into the pool; ruling says it is the owner's ruling (a new
// fact's evidence is settled by ApplyEvidence, before Walk could mark it).
func (a *Absorber) Add(f *fact.Fact, ruling bool) {
	a.p.add(newStatement(f, ruling, true))
}

// Drop takes the fact at path out of the pool: the run retired it (a
// supersedes verdict of the conflict check).
func (a *Absorber) Drop(path string) {
	if a.p.dropped == nil {
		a.p.dropped = map[string]bool{}
	}
	a.p.dropped[path] = true
}

// Restates names the curated fact that f only restates, or "". Only the
// closest fact in f's scope decides, as for notes.
func (a *Absorber) Restates(f *fact.Fact, ruling bool) string {
	e := newStatement(f, ruling, false)
	if len(e.terms) == 0 {
		return ""
	}
	if o := closestInScope(&a.p, e); o != nil && restatesFact(e, o) {
		return o.f.Meta.Name
	}
	return ""
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
