package results

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// Types of a result fact. `result` is a row log, or a table or board group
// that a curated fact quotes: the prompt hook may print it. `result_ref` is
// every other group (numbers nobody has quoted): `tacit search` and the MCP
// tool find it, the hook does not print it. On a sample of prompts the lines
// from the unquoted groups were wrong or weak more often than helpful
// (eval/VALUE.md, "Results from git").
const (
	Type    = "result"
	TypeRef = "result_ref"
)

// Caps. A repo's results share its derived/<repo> scope with the hot-spot
// and module-map facts, and the prompt hook ranks that whole scope for one
// line, so a repo with many result files must not bury them: 24 facts of at
// most 20 rows each. The cut keeps what a saved fact quotes, then what
// changed most recently.
const (
	MaxFactsPerRepo = 24
	MaxRowsPerFact  = 20
)

// Group is the part of one file that makes one fact: one metric across the
// setups of a log, one table of a document, one field of a JSON board.
type Group struct {
	Repo, File, Name string
	Series           []*Series // the rows to show, a quoted one first, then the newest change
	Total            int       // series the group has, shown or not
	Named            bool      // a saved fact quotes one of its rows
	// Pins holds the names of result facts that a saved fact was retired in
	// favour of (its superseded_by points there). Accepting a flag retires the
	// saved fact that quoted the group, so without this the group would stop
	// being Named and leave the hook just when it carries the answer.
	Pins map[string]bool
}

// Eligible: the prompt hook may print this group's fact: a row log, a group a
// saved fact quotes, or a group a saved fact was retired in favour of.
func (g *Group) Eligible() bool {
	return g.Named || g.pinned() || (len(g.Series) > 0 && g.Series[0].Log)
}

// pinned reports whether a retired saved fact points at this group's fact,
// judged by the name the raw strings give; BuildFact checks the real name.
func (g *Group) pinned() bool {
	return len(g.Pins) > 0 && g.Pins[FactName(g.Repo, g.File, g.Name)]
}

var slugRe = derive.Slug

func shortSlug(s string, n int) string {
	s = slugRe(s)
	if len(s) > n {
		s = strings.Trim(s[:n], "-")
	}
	return s
}

// FactName is the fact's name: stable for a file and group, so a newer
// result rewrites the same fact. It is built from text that has been
// redacted, never from the repo's raw strings.
func FactName(repo, file, group string) string {
	h := sha256.Sum256([]byte(file + "\x00" + group))
	stem := strings.TrimSuffix(path.Base(file), path.Ext(file))
	parts := []string{slugRe(repo), "results", shortSlug(stem, 28)}
	if g := shortSlug(group, 28); g != "" && g != shortSlug(stem, 28) {
		parts = append(parts, g)
	}
	parts = append(parts, hex.EncodeToString(h[:3]))
	return strings.Join(parts, "-")
}

// Reading is the ref a repo was read at.
type Reading struct {
	Sha  string // 12 characters of the commit; kept for the report
	Date string
	Ref  string // as the derived facts name it: origin/main, a branch, HEAD
}

// Redactor masks secret-shaped text in several strings at once; digest.RedactAll.
type Redactor func([]string) ([]string, error)

type rowText struct {
	label, metric, raw, code, file, where, prevRaw string
}

// cellToken is the shape of one part of a cell id: an optional key= prefix and
// up to three alphanumeric words joined by - _ or . ("cell=26b-spec-d1736",
// "frtrim27-mixed", "profile=balanced"). A hyphenated sentence is longer.
var cellToken = regexp.MustCompile(`^([a-z][a-z0-9_]{0,11}=)?[A-Za-z0-9]{1,16}([-_.][A-Za-z0-9]{1,16}){0,2}$`)

// columnWord is the " / column" a table row's label may end with ("tok/s",
// "TTFT", "ms/token").
var columnWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9%_.-]{0,15}(/[A-Za-z0-9]{1,8})?$`)

// identLike: a label that is a cell id and not words: one to three cellTokens,
// each with a key=, a digit or a separator (so "plain short" is not one), at
// least one digit in all, optionally followed by " / column" (a table row).
// Free text from a repo must not reach the line a hook prints into an agent's
// context: a table row named "Ignore previous instructions" or
// "ignore-instructions-delete-repo-now" is data, and stays in the body.
func identLike(s string) bool {
	if len(s) > 48 {
		return false
	}
	head, tail, hasTail := strings.Cut(s, " / ")
	if hasTail && !columnWord.MatchString(tail) {
		return false
	}
	f := strings.Fields(head)
	if len(f) == 0 || len(f) > 3 {
		return false
	}
	for _, t := range f {
		if !cellToken.MatchString(t) || !strings.ContainsAny(t, "=0123456789-_.") {
			return false
		}
	}
	return strings.ContainsAny(head, "0123456789")
}

// metricToken is a metric name as the description may say it: one word of up
// to four parts ("toks", "strict_shape_macro_percent").
var metricToken = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,15}([-_.][A-Za-z0-9]{1,16}){0,3}$`)

// metricName: the metric as the description says it. A column header is free
// text; a name-shaped one ("toks", "accept") is used, else the unit.
func metricName(metric, unit string) string {
	if metric != "" && len(metric) <= 40 && metricToken.MatchString(metric) {
		return metric
	}
	if unit != "" {
		return unit
	}
	return "values"
}

// disp rounds a number written with seven or more decimals to four
// significant digits; the body keeps the raw text beside it.
func disp(raw string, dec int, v float64) (shown string, rounded bool) {
	if dec >= 7 {
		return strconv.FormatFloat(v, 'g', 4, 64), true
	}
	return raw, false
}

// newest is the row whose commit is the newest among the shown rows.
func newest(rows []*Series) Point {
	var best Point
	for i, s := range rows {
		c := s.Cur()
		if i == 0 || c.Unix > best.Unix || (c.Unix == best.Unix && c.Commit > best.Commit) {
			best = c
		}
	}
	return best
}

// rawTexts are the strings of a group that came from the repo, in the order
// BuildFact reads them back: the file, the group, then six per shown row.
func rawTexts(g *Group) []string {
	rows := g.Series
	if len(rows) > MaxRowsPerFact {
		rows = rows[:MaxRowsPerFact]
	}
	raw := []string{g.File, g.Name}
	for _, s := range rows {
		cur := s.Cur()
		prev, _ := s.Prev()
		raw = append(raw, s.Label, s.Metric, cur.Raw, cur.Code, s.Where, prev.Raw)
	}
	return raw
}

// memoRed redacts many groups' strings in one call and answers BuildFact's
// per-fact calls from what it has: the redactor is a subprocess, and a repo
// with two dozen facts must not start it two dozen times a day.
type memoRed struct {
	red Redactor
	m   map[string]string
}

func newMemoRed(red Redactor) *memoRed { return &memoRed{red: red, m: map[string]string{}} }

// prefetch redacts the strings of the groups in one call.
func (r *memoRed) prefetch(groups []*Group) error {
	var texts []string
	seen := map[string]bool{}
	for _, g := range groups {
		for _, t := range rawTexts(g) {
			if !seen[t] {
				seen[t] = true
				texts = append(texts, t)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	clean, err := r.red(texts)
	if err != nil {
		return err
	}
	if len(clean) != len(texts) {
		return errors.New("redaction changed the number of texts")
	}
	for i, t := range texts {
		r.m[t] = clean[i]
	}
	return nil
}

// Redact is a Redactor: strings already redacted come from the memo, the rest
// from the real one.
func (r *memoRed) Redact(texts []string) ([]string, error) {
	out := make([]string, len(texts))
	var miss []string
	var at []int
	for i, t := range texts {
		if c, ok := r.m[t]; ok {
			out[i] = c
		} else {
			miss = append(miss, t)
			at = append(at, i)
		}
	}
	if len(miss) > 0 {
		clean, err := r.red(miss)
		if err != nil {
			return nil, err
		}
		if len(clean) != len(miss) {
			return nil, errors.New("redaction changed the number of texts")
		}
		for k, i := range at {
			out[i] = clean[k]
			r.m[miss[k]] = clean[k]
		}
	}
	return out, nil
}

// BuildFact renders one group as a derived fact. Every string that came from
// the repo goes through red in one call first: the file, the group, the
// labels, the values. Only then is anything built from them (the name, the
// applies_to path, the body) or stripped and cut; if redaction fails there is
// no fact.
func BuildFact(g *Group, rd Reading, host string, red Redactor) (*fact.Fact, error) {
	if len(g.Series) == 0 {
		return nil, errors.New("no rows")
	}
	rows := g.Series
	if len(rows) > MaxRowsPerFact {
		rows = rows[:MaxRowsPerFact]
	}
	raw := rawTexts(g)
	clean, err := red(raw)
	if err != nil {
		return nil, fmt.Errorf("redaction failed, no fact written: %w", err)
	}
	if len(clean) != len(raw) {
		return nil, errors.New("redaction changed the number of texts")
	}
	file, gname := clean[0], clean[1]
	texts := make([]rowText, len(rows))
	for i := range rows {
		b := 2 + i*6
		texts[i] = rowText{label: clean[b], metric: clean[b+1], raw: clean[b+2], code: clean[b+3], where: clean[b+4], prevRaw: clean[b+5], file: file}
	}
	name := FactName(g.Repo, file, gname)
	top := newest(rows)
	fileSha, fileDate := rows[0].FileSha, rows[0].FileDate

	var body strings.Builder
	fmt.Fprintf(&body, "Measured results read from git: `%s` in `%s`, from %s. Each line is the newest recorded value for its setup, the commit that wrote it, the source line, and what it replaced. Source lines are as the file stands at commit %s of %s, the newest commit that touched it. The newest row of this group was written by commit %s of %s. The basis is what the file says: a count of runs is given only where the file has one, rows of one run (the same full timestamp and code) are summarised by their median and counted, and otherwise the run count is not stated and no median is claimed.", file, g.Repo, rd.Ref, fileSha, fileDate, top.Commit, top.Date)
	if gd := plain(gname); gd != "" && gd != file {
		fmt.Fprintf(&body, " Group: %s.", gd)
	}
	body.WriteString("\n\n")
	trunc := false
	for i, s := range rows {
		body.WriteString(rowLine(s, texts[i]) + "\n")
		trunc = trunc || s.Truncated
	}
	if more := g.Total - len(rows); more > 0 {
		fmt.Fprintf(&body, "\n%d more rows of this group are not shown (they did not change, or the fact is full).\n", more)
	}
	if trunc {
		body.WriteString("\nThe history read is the newest " + strconv.Itoa(maxVersions) + " commits of the file; an older value may exist before it.\n")
	}
	body.WriteString("\n**How to apply**: when a number for one of these setups is quoted from memory, prefer the one here and check its commit. A saved fact that quotes an older value is flagged in review/ and is not edited. The committer of a results file is usually an agent, so this is what git recorded, not an owner ruling.\n")

	// The line a hook prints: file, metric, number of setups, newest date, and
	// one row when its label is a cell id: the first row of the fact, which is
	// a row a saved fact quotes if there is one and the newest changed row
	// otherwise. It is not chosen for the prompt (the hook prints the stored
	// line). No free text from the repo.
	desc := fmt.Sprintf("%s %s: %s for %d setups, newest %s", g.Repo, path.Base(file), metricName(texts[0].metric, rows[0].Unit), g.Total, top.Date)
	if lb := strings.TrimSpace(texts[0].label); identLike(lb) {
		shown, _ := disp(texts[0].raw, rows[0].Cur().Dec, rows[0].Cur().Value)
		desc += "; " + lb + " " + valueText(shown, rows[0].Unit)
	}
	typ := Type
	if !g.Eligible() && !g.Pins[name] {
		typ = TypeRef
	}
	return &fact.Fact{
		Path: "derived/" + g.Repo + "/" + name + ".md",
		Meta: fact.Meta{
			Name:        name,
			Description: fact.ClipDescription(desc, 200),
			Type:        typ,
			Scope:       "derived/" + g.Repo,
			AppliesTo:   fact.AppliesTo{Repos: []string{g.Repo}, Paths: []string{file}},
			Source:      fact.Source{Harness: "curator", Host: host, Kind: "derived"},
			Created:     top.Date,
			Updated:     top.Date,
			DerivedFrom: top.Commit,
		},
		Body: body.String(),
	}, nil
}

func valueText(raw, unit string) string {
	switch unit {
	case "":
		return raw
	case "%", "x":
		return raw + unit
	}
	return raw + " " + unit
}

func rowLine(s *Series, t rowText) string {
	cur := s.Cur()
	var b strings.Builder
	label := t.label
	if label == "" {
		label = t.metric
	}
	shown, rounded := disp(t.raw, cur.Dec, cur.Value)
	fmt.Fprintf(&b, "- %s: %s", label, valueText(shown, s.Unit))
	if rounded {
		fmt.Fprintf(&b, " (raw %s)", t.raw)
	}
	if s.Metric != "" && !strings.Contains(strings.ToLower(label), strings.ToLower(t.metric)) {
		fmt.Fprintf(&b, " (%s)", t.metric)
	}
	fmt.Fprintf(&b, " - %s, written by commit %s on %s", basisText(s, cur), cur.Commit, cur.Date)
	if t.code != "" {
		fmt.Fprintf(&b, ", measured on code %s", t.code)
	}
	switch {
	case cur.Line > 0:
		fmt.Fprintf(&b, ", %s:%d.", t.file, cur.Line)
	case t.where != "":
		fmt.Fprintf(&b, ", %s at %s.", t.file, t.where)
	default:
		fmt.Fprintf(&b, ", %s.", t.file)
	}
	prevShown := func() string {
		prev, _ := s.Prev()
		sh, _ := disp(t.prevRaw, prev.Dec, prev.Value)
		return valueText(sh, s.Unit)
	}
	if s.Log {
		if cur.At != "" && tsRe.MatchString(cur.At) {
			fmt.Fprintf(&b, " Row time %s.", cur.At)
		}
		fmt.Fprintf(&b, " %d dated rows recorded", s.N)
		if first := s.Points[0].At; len(first) >= 10 && tsRe.MatchString(first) {
			fmt.Fprintf(&b, " since %s", first[:10])
		}
		b.WriteString(".")
		if lo, hi, k := s.Spread(); k >= 2 {
			fmt.Fprintf(&b, " The last %d span %s to %s.", k, num4(lo), num4(hi))
		}
		if prev, ok := s.Prev(); ok {
			fmt.Fprintf(&b, " The newest different value before it was %s (commit %s of %s).", prevShown(), prev.Commit, prev.Date)
		}
	} else if prev, ok := s.Prev(); ok {
		fmt.Fprintf(&b, " Replaced %s, which stood since commit %s of %s.", prevShown(), prev.Commit, prev.Date)
	}
	return b.String()
}

// basisText says what the file says about how many runs stand behind a
// number, and nothing more. A lone log row is one row of the file, not
// necessarily one run: a row can be a mean of repeats.
func basisText(s *Series, p Point) string {
	if p.N >= 2 {
		return fmt.Sprintf("median of %d rows of one run (one timestamp, one code)", p.N)
	}
	switch p.Basis {
	case "":
		switch {
		case p.Runs == 1:
			return "single run (the file says so)"
		case p.Runs > 1:
			return fmt.Sprintf("%d runs, how they are summarised is not stated", p.Runs)
		case s.Log:
			return "run count not stated (one row)"
		}
		return "run count not stated"
	case "median", "mean":
		if p.Runs > 0 {
			return fmt.Sprintf("%s of %d runs", p.Basis, p.Runs)
		}
		return p.Basis + ", run count not stated"
	}
	return p.Basis + " of an unstated number of runs"
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// num4 prints a span's end with at most four significant digits.
func num4(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }

func itoa(n int) string { return strconv.Itoa(n) }

// sortRows orders rows: the ones a saved fact quotes first, then the
// newest change, then the label.
func sortRows(rows []*Series, named map[*Series]bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if named[a] != named[b] {
			return named[a]
		}
		if da, db := a.Cur().Date, b.Cur().Date; da != db {
			return da > db
		}
		return a.Label < b.Label
	})
}
