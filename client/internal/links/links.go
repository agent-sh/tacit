// Package links repairs the dead [[links]] of curated facts, deterministically
// and without a model. SPEC.md "Link repair" is the contract.
//
// A link is dead when its target names no fact (lint L6), or names only a
// fact that something replaced, unless that fact's chain of replacements
// reaches the linking fact: a link to what a fact replaced says where it
// came from. Each dead link is rewritten to a fact only when one clear
// match exists; otherwise it becomes plain text. The package
// plans and rewrites bytes; the store lock, the sync, the lint and the push
// are the caller's (cmd/tacit/links.go).
package links

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// CommitPrefix starts the subject of every commit this step makes. gitev
// reads it: a link repair is not an edit of what the fact says, so it must not
// move the day the fact counts as current (gitev.Builder.Effective).
const CommitPrefix = "curator: links:"

// Why says how a dead link was settled.
type Why string

const (
	Successor  Why = "successor"  // the target is a superseded fact; its live successor
	Normalized Why = "normalized" // the target spelled as a fact name is exactly one live fact
	NoMatch    Why = "no match"   // no fact answers to the target
	Ambiguous  Why = "ambiguous"  // more than one fact answers to it
	Repo       Why = "repo"       // the target is a repo name, which is not a fact
	Self       Why = "self"       // the only match is the fact itself
)

// Edit is the repair of one dead target within one fact.
type Edit struct {
	Target string // as written
	To     string // the fact name the link now points at; empty is plain text
	Why    Why
	Detail string // one line for a person
	Count  int    // how many times the target appears in the fact
}

// Fix is the repair planned for one fact file.
type Fix struct {
	Path, Name, Scope string
	Edits             []Edit
	Data              []byte // the file after the repair; nil for a fact that is not edited
}

// Plan is what a run would do with a store tree.
type Plan struct {
	Fixes     []*Fix // curated facts to rewrite, in path order
	Rulings   []*Fix // owner rulings with a dead link: put to the owner, never edited
	Generated []*Fix // generated derived/ facts with a dead link: left to their generator

	// Uncommitted lists facts the clone does not track that have a dead link:
	// reported and never planned, written, committed or removed.
	Uncommitted []*Fix
}

// Links counts the dead link occurrences the plan would settle in fixes.
func (p *Plan) Links() (rewritten, unwrapped int) {
	for _, fx := range p.Fixes {
		for _, e := range fx.Edits {
			if e.To != "" {
				rewritten += e.Count
			} else {
				unwrapped += e.Count
			}
		}
	}
	return
}

// Normalize spells a link target the way fact names are spelled (slugs): lower
// case, a trailing .md dropped, every run of other characters one hyphen.
func Normalize(s string) string {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".md")
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func live(f *fact.Fact) bool { return f.Meta.SupersededBy == "" && f.Meta.Retired == "" }

type index struct {
	all     map[string]int // facts of each name anywhere in the tree, inbox too: what lint L6 resolves against
	curated map[string][]*fact.Fact
	repos   map[string]bool // repo names with a projects/ or derived/ scope
}

// buildIndex indexes the facts of a tree. A fact in uncommitted (paths the
// store clone does not track yet) still makes its name resolve, so a link to it
// is left alone, but it is never a match to rewrite a link to: the server does
// not have it.
func buildIndex(facts []*fact.Fact, uncommitted map[string]bool) *index {
	ix := &index{all: map[string]int{}, curated: map[string][]*fact.Fact{}, repos: map[string]bool{}}
	for _, f := range facts {
		ix.all[f.Meta.Name]++
		if f.InInbox() || uncommitted[f.Path] {
			continue
		}
		ix.curated[f.Meta.Name] = append(ix.curated[f.Meta.Name], f)
		if root, repo, ok := strings.Cut(f.Meta.Scope, "/"); ok && (root == "projects" || root == "derived") {
			ix.repos[repo] = true
		}
	}
	return ix
}

// outcome of looking a name up among the curated facts.
type outcome int

const (
	none outcome = iota
	found
	ambiguous
)

// resolve answers a name with the one live curated fact that stands for it:
// the fact itself when exactly one live fact has the name, else, when the name
// is held by one superseded fact, the live end of its superseded_by chain. A
// retired fact with no successor, a chain that dead-ends or loops, and several
// facts of one name answer nothing.
func (ix *index) resolve(name string) (string, outcome) {
	seen := map[string]bool{}
	for step := 0; step < 16 && !seen[name]; step++ {
		seen[name] = true
		cands := ix.curated[name]
		var liveOnes []*fact.Fact
		for _, c := range cands {
			if live(c) {
				liveOnes = append(liveOnes, c)
			}
		}
		switch {
		case len(liveOnes) == 1:
			return name, found
		case len(liveOnes) > 1:
			return "", ambiguous
		case len(cands) == 0:
			return "", none
		case len(cands) > 1:
			return "", ambiguous
		case cands[0].Meta.SupersededBy == "":
			return "", none // retired, no successor
		}
		name = cands[0].Meta.SupersededBy
	}
	return "", none
}

// replacedBy reports whether the superseded_by chain from target reaches the
// fact named name: the fact target was folded into, or one that later
// replaced that fact in turn.
func (ix *index) replacedBy(target, name string) bool {
	seen := map[string]bool{}
	for step := 0; step < 16 && !seen[target]; step++ {
		seen[target] = true
		cands := ix.curated[target]
		if len(cands) != 1 || cands[0].Meta.SupersededBy == "" {
			return false
		}
		target = cands[0].Meta.SupersededBy
		if target == name {
			return true
		}
	}
	return false
}

// classify settles one target of fact f. ok is false when the link needs no
// repair: it names a live fact, a fact f replaced (directly or through
// another), or only a fact something else than a successor can answer for
// (a retired fact, an inbox note), which lint resolves and the curator
// leaves alone.
func (ix *index) classify(f *fact.Fact, target string) (Edit, bool) {
	e := Edit{Target: target}
	if ix.all[target] > 0 {
		// Lint resolves it. Repair only a link to a superseded fact whose
		// live successor is clear.
		cands := ix.curated[target]
		for _, c := range cands {
			if live(c) {
				return e, false
			}
		}
		to, o := ix.resolve(target)
		if o != found || len(cands) != 1 || ix.replacedBy(target, f.Meta.Name) {
			// A link to a fact this one replaced, itself or through others,
			// says where it came from (the statements a ruling was made of):
			// it is history, not dead.
			return e, false
		}
		return ix.rewrite(f, e, to, Successor, fmt.Sprintf("[[%s]] is superseded by [[%s]]", target, to)), true
	}
	n := Normalize(target)
	if n != "" && n != target {
		if ix.replacedBy(n, f.Meta.Name) {
			// A misspelt link to what f replaced: spelt right, it stays a
			// link to where f came from.
			return ix.rewrite(f, e, n, Normalized, fmt.Sprintf("[[%s]] spelled as a fact name is [[%s]], which this fact replaced", target, n)), true
		}
		switch to, o := ix.resolve(n); o {
		case found:
			why, detail := Normalized, fmt.Sprintf("[[%s]] spelled as a fact name is [[%s]]", target, to)
			if to != n {
				why, detail = Successor, fmt.Sprintf("[[%s]] spelled as a fact name is [[%s]], superseded by [[%s]]", target, n, to)
			}
			return ix.rewrite(f, e, to, why, detail), true
		case ambiguous:
			e.Why, e.Detail = Ambiguous, fmt.Sprintf("more than one fact answers to %s", n)
			return e, true
		}
	}
	if ix.repos[target] || (n != "" && ix.repos[n]) {
		e.Why, e.Detail = Repo, fmt.Sprintf("%s names a repo (a projects/ or derived/ scope), not a fact", target)
		return e, true
	}
	e.Why, e.Detail = NoMatch, fmt.Sprintf("no fact is named %s", target)
	return e, true
}

func (ix *index) rewrite(f *fact.Fact, e Edit, to string, why Why, detail string) Edit {
	if to == f.Meta.Name {
		e.Why, e.Detail = Self, detail+", which is this fact"
		return e
	}
	e.To, e.Why, e.Detail = to, why, detail
	return e
}

// Planner plans the repair of one store tree.
type Planner struct {
	root        string
	facts       []*fact.Fact
	ix          *index
	isRuling    func(*fact.Fact) bool
	uncommitted map[string]bool
}

// NewPlanner indexes facts (fact.Walk of root). isRuling says which facts are
// owner rulings. uncommitted holds the paths of facts the clone does not track:
// they are never planned and never a match (see buildIndex).
func NewPlanner(root string, facts []*fact.Fact, isRuling func(*fact.Fact) bool, uncommitted map[string]bool) *Planner {
	return &Planner{root: root, facts: facts, ix: buildIndex(facts, uncommitted), isRuling: isRuling, uncommitted: uncommitted}
}

// History reports a link from f to target that says where f came from:
// target names a fact f replaced, directly or through another fact, and f
// still links it. Such a link is not dead (classify), and a LINK? line
// raised for it before is no finding.
func (p *Planner) History(f *fact.Fact, target string) bool {
	if !p.ix.replacedBy(target, f.Meta.Name) {
		return false
	}
	for _, sp := range fact.LinkSpans(f.Body) {
		if sp.Target == target {
			return true
		}
	}
	return false
}

// Compute plans the repair of every curated fact in facts (fact.Walk of root).
func Compute(root string, facts []*fact.Fact, isRuling func(*fact.Fact) bool) (*Plan, error) {
	return NewPlanner(root, facts, isRuling, nil).Plan()
}

// edits settles every dead link of f: nil when it has none.
func (p *Planner) edits(f *fact.Fact) (*Fix, map[string]Edit) {
	counts := map[string]int{}
	for _, sp := range fact.LinkSpans(f.Body) {
		counts[sp.Target]++
	}
	var targets []string
	for t := range counts {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	fx := &Fix{Path: f.Path, Name: f.Meta.Name, Scope: f.Meta.Scope}
	byTarget := map[string]Edit{}
	for _, t := range targets {
		if e, ok := p.ix.classify(f, t); ok {
			e.Count = counts[t]
			fx.Edits = append(fx.Edits, e)
			byTarget[t] = e
		}
	}
	if len(fx.Edits) == 0 {
		return nil, nil
	}
	return fx, byTarget
}

func (p *Planner) edited(f *fact.Fact) bool {
	return !strings.HasPrefix(f.Meta.Scope, "derived/") && !p.isRuling(f)
}

// Plan plans every curated fact. Rulings and the generated derived/ facts are
// reported and never given new bytes; inbox notes and untracked facts are not
// looked at.
func (p *Planner) Plan() (*Plan, error) {
	plan := &Plan{}
	for _, f := range p.facts {
		if f.InInbox() {
			continue
		}
		fx, byTarget := p.edits(f)
		if fx == nil {
			continue
		}
		switch {
		case p.uncommitted[f.Path]:
			plan.Uncommitted = append(plan.Uncommitted, fx)
		case strings.HasPrefix(f.Meta.Scope, "derived/"):
			plan.Generated = append(plan.Generated, fx)
		case p.isRuling(f):
			plan.Rulings = append(plan.Rulings, fx)
		default:
			data, err := os.ReadFile(filepath.Join(p.root, filepath.FromSlash(f.Path)))
			if err != nil {
				return nil, err
			}
			out, ok := Rewrite(data, byTarget)
			if !ok {
				continue
			}
			fx.Data = out
			plan.Fixes = append(plan.Fixes, fx)
		}
	}
	return plan, nil
}

// Expected is the bytes the step would write for the committed version of a
// fact file, head, as the tree stands now. ok is false for what the step never
// edits: a ruling, a derived/ fact, an inbox note, a file that does not parse.
// A file that has no dead link expects itself.
func (p *Planner) Expected(path string, head []byte) (out []byte, ok bool) {
	f, err := fact.Parse(path, head)
	if err == nil {
		// Judged as the store judges it: a ruling is not edited, and a store
		// whose state cannot be read edits nothing.
		err = fact.WithStore(p.root, f)
	}
	if err != nil || f.InInbox() || !p.edited(f) {
		return nil, false
	}
	fx, byTarget := p.edits(f)
	if fx == nil {
		return head, true
	}
	return Rewrite(head, byTarget)
}

// Rewrite applies edits to a fact file's bytes. Only the [[links]] of the body
// whose target has an edit change: the frontmatter and every other byte of the
// body stay as they are. A rewritten link keeps its `|label`; an unwrapped one
// becomes its label, else its target as written. ok is false when the file has
// no frontmatter block.
func Rewrite(data []byte, edits map[string]Edit) (out []byte, ok bool) {
	_, body, split := fact.Split(data)
	if !split {
		return data, false
	}
	start := len(data) - len(body)
	var b bytes.Buffer
	b.Write(data[:start])
	last := 0
	for _, sp := range fact.LinkSpans(string(body)) {
		e, has := edits[sp.Target]
		if !has {
			continue
		}
		b.Write(body[last:sp.Start])
		b.WriteString(Replacement(sp, e))
		last = sp.End
	}
	b.Write(body[last:])
	return b.Bytes(), true
}

// Replacement is the text that stands where the link was. The label is what
// follows the first `|` of the suffix (`[[a|label]]`, `[[a#section|label]]`); a
// section belongs to the old target and is dropped.
func Replacement(sp fact.LinkSpan, e Edit) string {
	lab := label(sp)
	if e.To != "" {
		return "[[" + e.To + lab + "]]"
	}
	if l := strings.TrimSpace(strings.TrimPrefix(lab, "|")); l != "" {
		return l
	}
	return sp.Target
}

// label is the part of a link's suffix that the step keeps: `|label`, or empty.
func label(sp fact.LinkSpan) string {
	if i := strings.Index(sp.Suffix, "|"); i >= 0 {
		return sp.Suffix[i:]
	}
	return ""
}

// IsRepair reports whether cur is old with some of old's [[links]] repaired
// the way this step does and nothing else changed: the frontmatter and every
// byte outside the links are the same, in order, and each link of old is either
// still there, replaced by a link to another target with the old label (the
// section dropped, nothing new added), or replaced by the plain text this step
// writes for an unwrapped link (its label, else its target). A sentence put
// where a link was, or a link that says something else, is not a repair.
func IsRepair(old, cur []byte) bool {
	_, body, ok := fact.Split(old)
	if !ok {
		return false
	}
	start := len(old) - len(body)
	spans := fact.LinkSpans(string(body))
	segs := make([][]byte, 0, len(spans)+1)
	opts := make([][2]string, len(spans)) // the link as written, the text it unwraps to
	from := 0
	for i, sp := range spans {
		segs = append(segs, old[from:start+sp.Start])
		opts[i] = [2]string{string(old[start+sp.Start : start+sp.End]), Replacement(sp, Edit{})}
		from = start + sp.End
	}
	segs = append(segs, old[from:])
	memo := map[[2]int]bool{}
	var match func(i, pos int) bool
	match = func(i, pos int) bool {
		if !bytes.HasPrefix(cur[pos:], segs[i]) {
			return false
		}
		pos += len(segs[i])
		if i == len(spans) {
			return pos == len(cur)
		}
		key := [2]int{i, pos}
		if v, seen := memo[key]; seen {
			return v
		}
		memo[key] = false
		rest := string(cur[pos:])
		res := false
		for _, o := range opts[i] {
			if strings.HasPrefix(rest, o) && match(i+1, pos+len(o)) {
				res = true
				break
			}
		}
		if !res {
			// The step writes a new target and the old label, and nothing else:
			// no section, no new label.
			if nl, isLink := fact.LinkAt(rest); isLink && nl.Suffix == label(spans[i]) && match(i+1, pos+nl.End) {
				res = true
			}
		}
		memo[key] = res
		return res
	}
	return match(0, 0)
}

// LineRe reads a LINK? line back: the fact, then the dead target.
var LineRe = regexp.MustCompile(`^- LINK\? \[\[([^\]]+)\]\] \([^)]*\) links to \[\[([^\]]+)\]\]`)

// Key identifies a fact and the dead link it was raised for.
func Key(factName, target string) string { return factName + "\x00" + target }

// Raised returns the keys of the LINK? lines in a review file's text.
func Raised(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := LineRe.FindStringSubmatch(l); m != nil {
			out = append(out, Key(m[1], m[2]))
		}
	}
	return out
}

// Line is the review line for one dead link in an owner ruling. It says what
// the curator would write, so the owner can make the edit or answer no.
func Line(fx *Fix, e Edit) string {
	would := "plain text"
	if e.To != "" {
		would = "[[" + e.To + "]]"
	}
	return fmt.Sprintf("- LINK? [[%s]] (%s) links to [[%s]], and %s; this is an owner ruling, so the curator left it; it would write %s", fx.Name, fx.Scope, e.Target, e.Detail, would)
}
