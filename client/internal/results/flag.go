package results

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A saved fact quotes a row of a series in one of two ways that matter: it
// states the value git holds now (the row is named by a curated fact, Named),
// or a value that a newer commit replaced (flagged). A number that matches no
// row, one inside the noise of the newest values, and one the fact dates, is
// nothing.

// Hit is a saved fact that quotes a number of a series.
type Hit struct {
	Fact    *fact.Fact
	S       *Series
	Literal string // as the fact wrote it
	Old     Point  // the value it quotes (the current point when Named)
	Named   bool   // it quotes the current value
	Repl    Point  // the point that replaced Old; the zero Point when Named
}

type entry struct {
	s     *Series
	p     Point
	i     int // index of p in s.Points
	isCur bool
}

// Index finds the rows that a number could mean.
type Index struct {
	by   map[key][]entry
	dist map[*Series][]string // tokens that tell a row from its group's other rows
}

type key struct {
	dec int
	v   float64
}

// NewIndex indexes the values of every series by the number as it would be
// written with 0 to 3 decimals.
func NewIndex(all []*Series) *Index {
	ix := &Index{by: map[key][]entry{}, dist: map[*Series][]string{}}
	groups := map[string][]*Series{}
	for _, s := range all {
		groups[s.Repo+"\x00"+s.File+"\x00"+s.Group] = append(groups[s.Repo+"\x00"+s.File+"\x00"+s.Group], s)
	}
	for _, rows := range groups {
		common := map[string]int{}
		for _, s := range rows {
			seen := map[string]bool{}
			for _, t := range s.Terms {
				if !seen[t] {
					seen[t] = true
					common[t]++
				}
			}
		}
		for _, s := range rows {
			var d []string
			seen := map[string]bool{}
			for _, t := range s.Terms {
				if seen[t] || isNumberToken(t) && len(t) < 2 {
					continue
				}
				seen[t] = true
				if len(rows) == 1 || common[t] < len(rows) {
					d = append(d, t)
				}
			}
			ix.dist[s] = d
		}
	}
	for _, s := range all {
		seen := map[float64]bool{}
		for i := len(s.Points) - 1; i >= 0; i-- {
			p := s.Points[i]
			if seen[p.Value] {
				continue
			}
			seen[p.Value] = true
			for dec := 0; dec <= 3; dec++ {
				k := key{dec, roundTo(p.Value, dec)}
				ix.by[k] = append(ix.by[k], entry{s, p, i, i == len(s.Points)-1})
			}
		}
	}
	return ix
}

func isNumberToken(t string) bool {
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var numLit = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
var unitLook = regexp.MustCompile(`(?i)(?:^|[^a-z])(tokens/sec|tokens/s|tok/sec|tok/s|t/s|tps|req/s|r/s|rps|qps|ops/s|gb/s|mb/s|tflops|ms|µs|us|seconds|sec|%|×|x)(?:[^a-z]|$)`)

// unitWindow is the text after a number that can name its unit: up to 40
// characters, ending at a line end, a semicolon, a bracket, a comma or a
// full stop that is not part of a number. "120.7 -> 391.2 tok/s" gives both
// numbers the unit; "4.07 (12 lanes, p95 652 ms)" gives 4.07 none.
func unitWindow(rest string) string {
	for i := 0; i < len(rest) && i < 40; i++ {
		switch rest[i] {
		case '\n', ';', '(', ')', '[', ']':
			return rest[:i]
		case ',', '.':
			if i+1 >= len(rest) || rest[i+1] < '0' || rest[i+1] > '9' {
				return rest[:i]
			}
		}
	}
	if len(rest) > 40 {
		return rest[:40]
	}
	return rest
}

// findUnit returns the first unit in s, canonical, or "".
func findUnit(s string) string {
	m := unitLook.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	u := strings.ToLower(m[1])
	if c := canonUnit(u); c != "" {
		return c
	}
	if u == "seconds" {
		return "s"
	}
	return ""
}

type literal struct {
	raw  string
	val  float64
	dec  int
	unit string // the first unit within the next 40 characters, canonical
	pos  int    // byte offset in the text
}

// literals reads the numbers a text states, with the unit that follows each.
// A number inside a name (llama3.1, 8b, #412), a date, a range or a list item
// is not a measurement and is skipped.
func literals(text string) []literal {
	var out []literal
	for _, m := range numLit.FindAllStringIndex(text, -1) {
		raw := strings.TrimRight(text[m[0]:m[1]], ",")
		if m[0] > 0 {
			p := rune(text[m[0]-1])
			if unicode.IsLetter(p) || unicode.IsDigit(p) || p == '.' || p == '_' || p == '#' || p == '/' {
				continue
			}
			if p == '-' && m[0] > 1 && unicode.IsDigit(rune(text[m[0]-2])) {
				continue // range or date
			}
		}
		rest := text[m[1]:]
		if len(rest) > 0 {
			n := rune(rest[0])
			if n == '-' && len(rest) > 1 && unicode.IsDigit(rune(rest[1])) {
				continue
			}
			if unicode.IsLetter(n) && findUnit(rest[:min(len(rest), 6)]) == "" {
				continue // 9b, 3d
			}
		}
		v, dec, ok := parseNumber(raw)
		if !ok {
			continue
		}
		out = append(out, literal{raw: raw, val: v, dec: dec, unit: findUnit(unitWindow(rest)), pos: m[0]})
	}
	return out
}

// Match finds the series each saved fact quotes. facts are the curated,
// live, hand- or agent-saved ones; derived facts are never matched.
func (ix *Index) Match(facts []*fact.Fact) []Hit {
	var hits []Hit
	for _, f := range facts {
		if recordType[f.Meta.Type] {
			continue // narrates what happened; its numbers are dated by what it tells
		}
		text := f.Meta.Description + "\n" + f.Body
		lits := literals(text)
		if len(lits) == 0 {
			continue
		}
		toks := map[string]bool{}
		for _, t := range tokens(text) {
			toks[t] = true
		}
		// All the fact's numbers for a row are weighed together: a fact that
		// states the current value is about the current value, whatever else
		// it says ("went from 212.4 to 247.4", "247.4 now (was 212.4)").
		current := map[*Series]Hit{}
		stale := map[*Series]Hit{}
		for _, l := range lits {
			if threshold(text, l) {
				continue // a floor, a target, a budget: not a claim about what was measured
			}
			for _, e := range ix.by[key{l.dec, roundTo(l.val, l.dec)}] {
				if !ix.setupMatches(e.s, toks, l) {
					continue
				}
				cur := e.s.Cur()
				switch {
				case e.isCur || sameAs(l.val, l.dec, cur.Value):
					if _, ok := current[e.s]; !ok {
						current[e.s] = Hit{Fact: f, S: e.s, Literal: l.raw, Old: cur, Named: true}
					}
				default:
					repl, ri := replacement(e.s, e.i)
					if !material(e.s, e.p, e.s.Points[ri:]) || !plausible(f, e.p) || datedBefore(text, l.pos, repl.Date) {
						continue
					}
					if _, ok := stale[e.s]; !ok {
						stale[e.s] = Hit{Fact: f, S: e.s, Literal: l.raw, Old: e.p, Repl: repl}
					}
				}
			}
		}
		for s, h := range current {
			_ = s
			hits = append(hits, h)
		}
		for s, h := range stale {
			if _, ok := current[s]; !ok {
				hits = append(hits, h)
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Fact.Path != hits[j].Fact.Path {
			return hits[i].Fact.Path < hits[j].Fact.Path
		}
		if hits[i].S.Repo != hits[j].S.Repo {
			return hits[i].S.Repo < hits[j].S.Repo
		}
		if hits[i].S.File != hits[j].S.File {
			return hits[i].S.File < hits[j].S.File
		}
		if hits[i].S.Group != hits[j].S.Group {
			return hits[i].S.Group < hits[j].S.Group // one label and column under two headings of a file
		}
		return hits[i].S.Key < hits[j].S.Key
	})
	return hits
}

// recordType: facts that tell what happened. They state numbers as of a past
// day, and "it ran 212.4 tok/s" in a history note stays true when the next
// run reads 247.4.
var recordType = map[string]bool{"history": true, "past_mistake": true, "code_change": true}

var (
	thresholdBefore = regexp.MustCompile(`(?i)(\bmust\b|\bshould\b|\bshall\b|\bneeds?\b|\brequires?\b|\brequired\b|\bat least\b|\bat most\b|\bno (more|less|fewer) than\b|\babove\b|\bbelow\b|\bunder\b|\bover\b|\bexceed(s|ed)?\b|\bbeat(s)?\b|\bfloor\b|\btarget\b|\bbudget\b|\blimit\b|\bthreshold\b|\bceiling\b|\bbar\b|\bgate\b|\bgoal\b|\bminimum\b|\bmaximum\b|\bcap\b|\baims?\b|\bwants?\b|>=|<=|(?:^|[^-=])[<>\x{2265}\x{2264}]\s*$)`)
	thresholdAfter  = regexp.MustCompile(`(?i)^[^.\n;]{0,14}?\b(floor|target|minimum|maximum|budget|limit|ceiling|threshold|bar|gate|goal|cap|or (more|less|better|worse|higher|lower|above|below)|at least|at most)\b|^\s*\+`)
)

// threshold: the number sits in a bound, not in a measurement. "must stay
// above 212.4 tok/s", "target 200", "a 1000 ms p95 bar", "at least 40".
func threshold(text string, l literal) bool {
	lo := l.pos - 36
	if lo < 0 {
		lo = 0
	}
	before := text[lo:l.pos]
	if k := strings.LastIndexAny(before, "\n;"); k >= 0 {
		before = before[k+1:]
	}
	if k := strings.LastIndex(before, ". "); k >= 0 {
		before = before[k+2:]
	}
	if thresholdBefore.MatchString(before) {
		return true
	}
	end := l.pos + len(l.raw)
	if end > len(text) {
		end = len(text)
	}
	after := text[end:]
	if len(after) > 40 {
		after = after[:40]
	}
	return thresholdAfter.MatchString(after)
}

// setupMatches decides whether the text is about this row: the number's unit
// agrees with the row's, and the tokens that tell the row from its group's
// other rows are in the text.
func (ix *Index) setupMatches(s *Series, toks map[string]bool, l literal) bool {
	cur := s.Cur()
	if s.Unit != "" && l.unit != s.Unit {
		return false
	}
	d := ix.dist[s]
	if len(d) < 2 {
		return false // a row told from the others by one word ("prefill") is in every note that has that word
	}
	hit := 0
	for _, t := range d {
		if toks[t] {
			hit++
		}
	}
	need := max(2, int(math.Ceil(0.6*float64(len(d)))))
	if s.Unit == "" {
		need = len(d) // a ratio or score has no unit to place it: every setup word must be there
	}
	if hit < need {
		return false
	}
	// A number of one or two digits (3, 18, 01) is in every note; a measured
	// value has three or more. A whole number for a value that has decimals
	// (125 for 124.95) is a round figure, not a recorded value, unless it has
	// four digits.
	if sigDigits(l.raw) < 3 || (l.dec == 0 && cur.Dec > 0 && sigDigits(l.raw) < 4) {
		return false
	}
	if s.Unit == "" { // a ratio carries no unit: its metric must be named too
		named := false
		for _, w := range words(s.Metric) {
			if toks[w] {
				named = true
			}
		}
		if !named {
			return false
		}
	}
	return true
}

// material: the quoted value is far enough from what the setup measures now
// that it is a different result and not the same measurement read twice.
//
// More than 1 percent from the current value, always. A table or a board is
// rewritten with a number, so that is all. A log measures the same setup
// again and again and its values wander, so the quoted value must also lie
// outside the noise band of the values since it was replaced (newState: the
// replacing point and everything after it, the state the setup is in now).
// Noise needs a sample to be called noise:
//
//   - three points or more: the median of newState and the larger of 2
//     percent and the band the spread gives. Under five points the band is
//     half the observed range (a deviation from three points is one point's);
//     from five it is three times the median absolute deviation;
//   - fewer than three: there is no sample, the change is the only evidence,
//     and 5 percent stands in for it. GPU benchmark cells can move that
//     much between runs of the same code, so a smaller difference between
//     a quoted value and one or two later rows is not shown to be a change.
func material(s *Series, old Point, newState []Point) bool {
	cur := s.Cur().Value
	if math.Abs(old.Value-cur) <= 0.01*math.Abs(cur) {
		return false
	}
	if !s.Log {
		return true
	}
	vals := make([]float64, len(newState))
	for i, p := range newState {
		vals[i] = p.Value
	}
	med := median(vals)
	var band float64
	switch {
	case len(vals) < 3:
		band = 0.05 * math.Abs(med)
	case len(vals) < 5:
		lo, hi := vals[0], vals[0]
		for _, v := range vals {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		band = math.Max(0.02*math.Abs(med), (hi-lo)/2)
	default:
		dev := make([]float64, len(vals))
		for i, v := range vals {
			dev[i] = math.Abs(v - med)
		}
		band = math.Max(0.02*math.Abs(med), 3*median(dev))
	}
	return math.Abs(old.Value-med) > band
}

func median(xs []float64) float64 {
	ys := append([]float64(nil), xs...)
	sort.Float64s(ys)
	n := len(ys)
	if n%2 == 1 {
		return ys[n/2]
	}
	return (ys[n/2-1] + ys[n/2]) / 2
}

// replacement is the first point after i whose value is more than 1 percent
// away from point i's: the point that made the older value stale, not the
// next row of a noisy log.
func replacement(s *Series, i int) (Point, int) {
	old := s.Points[i].Value
	for j := i + 1; j < len(s.Points); j++ {
		if math.Abs(s.Points[j].Value-old) > 0.01*math.Abs(old) {
			return s.Points[j], j
		}
	}
	return s.Cur(), len(s.Points) - 1
}

var dateRe = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)

// datedBefore: the sentence around the number carries a date on or before the
// day the value was replaced. A fact that dates its number ("on 2026-09-11 it
// was published at 74") is a record of that day, which stays true; it is not a
// claim about now.
func datedBefore(text string, pos int, replaced string) bool {
	lo, hi := pos, pos
	for lo > 0 && pos-lo < 200 && text[lo-1] != '\n' && !(lo >= 2 && text[lo-1] == ' ' && text[lo-2] == '.') {
		lo--
	}
	for hi < len(text) && hi-pos < 200 && text[hi] != '\n' && !(hi+1 < len(text) && text[hi] == '.' && text[hi+1] == ' ') {
		hi++
	}
	for _, m := range dateRe.FindAllString(text[lo:hi], -1) {
		if m <= replaced {
			return true
		}
	}
	return false
}

// plausible: the fact was written after the value was recorded (with a week
// of slack, since results are often committed after the note about them).
func plausible(f *fact.Fact, old Point) bool {
	fd, od := factDate(f), old.Date
	if fd == "" || od == "" {
		return true
	}
	ft, e1 := time.Parse("2006-01-02", fd)
	ot, e2 := time.Parse("2006-01-02", od)
	if e1 != nil || e2 != nil {
		return true
	}
	return !ft.Before(ot.AddDate(0, 0, -7))
}

func factDate(f *fact.Fact) string {
	if f.Meta.Updated != "" {
		return f.Meta.Updated
	}
	return f.Meta.Created
}

// pointText says where a value is in git: the commit that wrote it and, for
// a log row, the time and the code the row itself names.
func pointText(p Point) string {
	s := "commit " + p.Commit + " on " + p.Date
	if p.At != "" && tsRe.MatchString(p.At) {
		s += ", row time " + p.At
	}
	if p.Code != "" {
		s += ", code " + p.Code
	}
	return s
}

// Finding is one saved fact that quotes replaced values of one result fact.
type Finding struct {
	Old    *fact.Fact
	New    string // name of the result fact
	Repo   string // the repo that holds the result fact
	Hits   []Hit
	Ruling bool // the saved fact is an owner ruling
}

// Line is the review line: the format review.go reads. A ruling is never
// proposed for retirement by an agent's step; it goes to the owner as a
// ruling change.
func (f Finding) Line() string {
	var b strings.Builder
	if f.Ruling {
		fmt.Fprintf(&b, "- RULING CHANGE? new [[%s]] may supersede owner ruling [[%s]]", f.New, f.Old.Meta.Name)
	} else {
		fmt.Fprintf(&b, "- result-superseded? [[%s]] may replace [[%s]]", f.New, f.Old.Meta.Name)
	}
	b.WriteString(" - quotes ")
	for i, h := range f.Hits {
		if i >= 3 {
			fmt.Fprintf(&b, "; and %d more", len(f.Hits)-3)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		cur := h.S.Cur()
		fmt.Fprintf(&b, "%s for %s, which git recorded in %s and replaced in %s; git now holds %s in %s (%s)",
			valueText(h.Literal, h.S.Unit), strings.TrimSpace(h.S.Label), pointText(h.Old), pointText(h.Repl),
			valueText(cur.Raw, h.S.Unit), pointText(cur), basisText(h.S, cur))
	}
	b.WriteString(". Source: " + f.Hits[0].S.File + ".")
	return b.String()
}
