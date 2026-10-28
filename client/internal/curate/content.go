package curate

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// The content rule of the Absorber: a fact of the split is not written when
// every claim it makes is already in a curated fact it can see. The
// description rule (restatesFact) compares descriptions only, and missed
// 77 of the 99 written facts the 2026-10-28 measurement confirmed by hand as
// copies of live facts: an agent's paraphrase shares the content, not the
// wording of the description (four agents-md-generated-by-agents-sync-*
// copies, each a curated fact of its own).
//
// A claim is a sentence of the fact's description or body with at least
// claimTerms terms. It is covered when one curated fact holds ContentCover of
// its terms, every specific token of it (a number, a path, a dotted name, a
// code span: addsSpecific) and every name with @ or _, and the same words of
// change or negation as the cover's sentences that match it (a claim that
// drops or adds a "not" against them is an update, or a contradiction, the
// judge must see). Each claim may be covered by another fact; the fact's
// specific tokens as a whole must be in its covers.

// ContentCover is the least share of a claim's terms its cover must hold.
// Measured on the 5,340 facts the rig's curator wrote 2026-10-21..28, each
// against the live facts its curation saw, hand-labelled (eval/VALUE.md,
// "The absorber's content rule"): at 0.75 one of 32 absorptions before the
// rebuild switch added a detail; from 0.78 none of 22, and none of the 18
// after the switch (held out). 0.8 takes the round value above.
var ContentCover = 0.8

// claimTerms is the fewest terms a sentence must have to be a claim: shorter
// ones ("Port 7790.") are checked through the fact's specific tokens.
const claimTerms = 3

// maxCoverChars bounds the curated facts that may cover a claim: a longer
// one (a folder overview, a long memory import) holds so many terms it
// covered unrelated claims in the measurement.
const maxCoverChars = 1200

var sentenceRx = regexp.MustCompile(`(?:[.!?;])\s+|\n+`)

// spanRx is a code span, which a sentence never ends inside: a command's
// "; " and ". " are its own.
var spanRx = regexp.MustCompile("`[^`\n]*`")

// abbrevRx are abbreviations whose dot ends no sentence.
var abbrevRx = regexp.MustCompile(`(?i)\b(e\.g|i\.e|etc|vs)\.`)

// sentences splits text into its sentences, trimmed of list marks.
func sentences(text string) []string {
	// Hide the code spans and the abbreviations' dots from the split, and put
	// them back after.
	var spans []string
	text = spanRx.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, m)
		return "\x00" + string(rune(0xE000+len(spans)-1)) + "\x00"
	})
	text = abbrevRx.ReplaceAllStringFunc(text, func(m string) string { return strings.TrimSuffix(m, ".") + "\x01" })
	restore := func(s string) string {
		s = strings.ReplaceAll(s, "\x01", ".")
		for i := len(spans) - 1; i >= 0; i-- {
			s = strings.ReplaceAll(s, "\x00"+string(rune(0xE000+i))+"\x00", spans[i])
		}
		return s
	}
	var out []string
	for _, s := range sentenceRx.Split(text, -1) {
		s = strings.Trim(strings.TrimSpace(restore(s)), "-*• \t")
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wholePathRx finds a path as written, with its placeholders and globs
// (/run/user/1000/<id>/apps/*.stderr.log), which specificRx cuts at < and
// *: the measurement's one wrong absorption at 0.8 was a new path under a
// covered directory that differed only past the cut.
var wholePathRx = regexp.MustCompile(`[\w~.<>*{}$@:-]*/[\w./~<>*{}$@:-]+`)

// homeDirRx is a home directory spelled out, as a fact may write ~.
var homeDirRx = regexp.MustCompile(`/(?:Users|home)/[^/\s]+/`)

// contentText is text as the content rule reads its terms: a home directory
// spelled out is ~, so "/home/u/.config/x" and "~/.config/x" share their
// terms.
func contentText(s string) string { return homeDirRx.ReplaceAllString(s, "~/") }

// contentSpecifics is specifics plus every whole path with its placeholders
// and globs.
func contentSpecifics(s string) map[string]bool {
	out := specifics(s)
	for _, m := range wholePathRx.FindAllString(s, -1) {
		m = strings.ToLower(strings.Trim(m, ".,:;`'\"()"))
		if strings.ContainsAny(m, "<>*{}") && len(m) >= 3 {
			out[m] = true
		}
	}
	return out
}

// sentence is one sentence of a cover, as the change-word guard reads it.
type sentence struct {
	terms   map[string]bool
	changes map[string]bool
}

// cover is a curated fact the content rule may absorb into.
type cover struct {
	st    *statement
	terms map[string]bool // every term of description and body
	spec  map[string]bool // contentSpecifics of description and body
	sents []sentence      // its sentences, read when a claim first reaches the cover
	split bool
}

// coverable is a curated fact that may cover claims: live (the pool's
// caller checked), not a folder file (_overview.md, _record.md, _index.md),
// not a result or hot spot (generated from git), not too long.
func coverable(f *fact.Fact) bool {
	if strings.HasPrefix(path.Base(f.Path), "_") || fact.IsResult(f.Meta.Type) || f.Meta.Type == "hot_spot" {
		return false
	}
	return len(f.Meta.Description)+len(f.Body) <= maxCoverChars
}

func newCover(e *statement) *cover {
	text := e.f.Meta.Description + "\n" + e.f.Body
	return &cover{st: e, terms: allTerms(contentText(text)), spec: e.spec}
}

// sentencesOf is the cover's sentences, split once.
func (c *cover) sentencesOf() []sentence {
	if !c.split {
		c.split = true
		for _, s := range sentences(c.st.f.Meta.Description + "\n" + c.st.f.Body) {
			c.sents = append(c.sents, sentence{terms: allTerms(contentText(s)), changes: changes(s)})
		}
	}
	return c.sents
}

// claim is one sentence of a new fact the content rule checks.
type claim struct {
	terms   map[string]bool
	spec    map[string]bool
	idents  map[string]bool
	changes map[string]bool
}

func claimsOf(f *fact.Fact) []claim {
	var out []claim
	seen := map[string]bool{}
	for _, s := range sentences(f.Meta.Description + "\n" + f.Body) {
		t := allTerms(contentText(s))
		if len(t) < claimTerms {
			continue
		}
		key := strings.Join(termList(t), " ")
		if seen[key] {
			continue // the description again, in the body
		}
		seen[key] = true
		out = append(out, claim{terms: t, spec: contentSpecifics(s), idents: identifiers(s), changes: changes(s)})
	}
	return out
}

func termList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// covers reports how much of claim cl the cover holds (the share of its
// terms), and whether the guards pass: its specific tokens and names in the
// cover, and the same change words as the cover's sentences that hold at
// least half its terms.
func (c *cover) covers(cl claim) (float64, bool) {
	k := 0
	for t := range cl.terms {
		if c.terms[t] {
			k++
		}
	}
	share := float64(k) / float64(len(cl.terms))
	if addsSpecific(cl.spec, c.spec) || !subset(cl.idents, c.st.idents) {
		return share, false
	}
	matched := map[string]bool{}
	for _, s := range c.sentencesOf() {
		n := 0
		for t := range cl.terms {
			if s.terms[t] {
				n++
			}
		}
		if 2*n >= len(cl.terms) {
			for w := range s.changes {
				matched[w] = true
			}
		}
	}
	a, b := polarity(cl.changes), polarity(matched)
	return share, subset(a, b) && subset(b, a)
}

// negations read as one word to the content guard: "never the generated
// file" and "not the generated file" say the same.
var negations = map[string]bool{"not": true, "no": true, "never": true, "none": true, "nothing": true, "nobody": true, "nowhere": true, "neither": true, "nor": true, "without": true}

// polarity is a set of change words with every negation as "not".
func polarity(ws map[string]bool) map[string]bool {
	out := map[string]bool{}
	for w := range ws {
		if negations[w] {
			w = "not"
		}
		out[w] = true
	}
	return out
}

// contentPool indexes covers by every term of their text.
type contentPool struct {
	items []*cover
	inv   map[string][]int
}

func (p *contentPool) add(e *statement) {
	if !coverable(e.f) {
		return
	}
	if p.inv == nil {
		p.inv = map[string][]int{}
	}
	c := newCover(e)
	for t := range c.terms {
		p.inv[t] = append(p.inv[t], len(p.items))
	}
	p.items = append(p.items, c)
}

// best is the cover of cl that passes its guards with the largest share
// (ContentCover at least), among the covers visible accepts; the earliest
// on a tie.
func (p *contentPool) best(cl claim, visible func(*cover) bool) (*cover, float64) {
	shared := map[int]int{}
	for t := range cl.terms {
		for _, i := range p.inv[t] {
			shared[i]++
		}
	}
	need := int(ContentCover*float64(len(cl.terms)) + 0.999999)
	var idx []int
	for i, n := range shared {
		if n >= need {
			idx = append(idx, i)
		}
	}
	sort.Ints(idx)
	var top *cover
	topShare := 0.0
	for _, i := range idx {
		c := p.items[i]
		if !visible(c) {
			continue
		}
		share, ok := c.covers(cl)
		if ok && share >= ContentCover && share > topShare {
			top, topShare = c, share
		}
	}
	return top, topShare
}

// quotedRx is a quoted span of six words or more: words of the owner's an
// agent's note carries in quotes.
var quotedRx = regexp.MustCompile(`["\x{201c}][^"\x{201d}\n]{0,400}?(?:\S+\s+){5,}[^"\x{201d}\n]*?["\x{201d}]`)

// ownerCarrying is a note or fact whose words may be the owner's: kind user
// or statement, owner evidence or words, written by the owner, a quoted span
// of six words or more, or a reviewer's trim of an agent's note. The content
// rule never absorbs what comes from one: their words are kept as written.
func ownerCarrying(f *fact.Fact) bool {
	if f == nil {
		return false
	}
	k := f.Meta.Source.Kind
	return k == "user" || k == KindStatement || f.Meta.Type == fact.StatementType || fact.ClaimsEvidence(f) || fact.OwnerWrote(f) ||
		strings.HasSuffix(f.Meta.Name, "-trimmed") || quotedRx.MatchString(f.Body)
}

// coversContent is the content rule for f, from note (nil when unknown):
// the cover of most of f's claims and the least share of a claim covered,
// when every claim is covered by a visible cover and f's specific tokens and
// names are all in its covers.
func (a *Absorber) coversContent(f *fact.Fact, ruling bool, note *fact.Fact) (*cover, float64, bool) {
	if ruling || ownerCarrying(f) || ownerCarrying(note) {
		return nil, 0, false
	}
	cls := claimsOf(f)
	if len(cls) == 0 {
		return nil, 0, false
	}
	noteScope := ""
	if note != nil {
		noteScope = note.Meta.Scope
	}
	visible := func(c *cover) bool {
		o := c.st.f
		if a.p.dropped[o.Path] || o.Meta.Name == f.Meta.Name {
			return false
		}
		s := o.Meta.Scope
		return s == f.Meta.Scope || s == "user" || s == "system" || ContentNoteScope && s == noteScope
	}
	count := map[*cover]int{}
	spec, idents := map[string]bool{}, map[string]bool{}
	least := 1.0
	var order []*cover
	for _, cl := range cls {
		c, share := a.c.best(cl, visible)
		if c == nil {
			return nil, 0, false
		}
		if count[c] == 0 {
			order = append(order, c)
			for t := range c.spec {
				spec[t] = true
			}
			for t := range c.st.idents {
				idents[t] = true
			}
		}
		count[c]++
		least = min(least, share)
	}
	text := f.Meta.Description + " " + f.Body
	if addsSpecific(contentSpecifics(text), spec) || !subset(identifiers(text), idents) {
		return nil, 0, false
	}
	top := order[0]
	for _, c := range order[1:] {
		if count[c] > count[top] {
			top = c
		}
	}
	if a.Refuted != nil {
		for _, c := range order {
			if a.Refuted(c.st.f, f) {
				return nil, 0, false
			}
		}
	}
	return top, least, true
}

// ContentNoteScope lets a claim be covered in the scope of the note the
// fact came from (the session's repository) as well as the fact's own scope,
// user and system.
var ContentNoteScope = true

// Absorbs is the Absorber's whole check of a fact of the split from note:
// the description rule (Restates), then the content rule. It names the
// curated fact f restates, and how.
func (a *Absorber) Absorbs(f *fact.Fact, ruling bool, note *fact.Fact) (Absorption, bool) {
	if by := a.Restates(f, ruling); by != "" {
		ab := Absorption{Fact: f.Meta.Description, By: by, Rule: "description"}
		if note != nil {
			ab.Note = note.Meta.Name
		}
		if o := a.byName(by); o != nil {
			ab.ByPath = o.Path
		}
		return ab, true
	}
	c, share, ok := a.coversContent(f, ruling, note)
	if !ok {
		return Absorption{}, false
	}
	ab := Absorption{Fact: f.Meta.Description, By: c.st.f.Meta.Name, ByPath: c.st.f.Path, Rule: "content", Coverage: float64(int(share*1000)) / 1000}
	if note != nil {
		ab.Note = note.Meta.Name
	}
	return ab, true
}

func (a *Absorber) byName(name string) *fact.Fact {
	for i := len(a.p.items) - 1; i >= 0; i-- {
		if f := a.p.items[i].f; f.Meta.Name == name && !a.p.dropped[f.Path] {
			return f
		}
	}
	return nil
}
