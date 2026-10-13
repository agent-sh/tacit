package results

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Obs is one measured number read from one version of one file.
type Obs struct {
	Group  string   // the table or metric the number belongs to
	Key    string   // identity of the series inside the file
	Label  string   // the setup in words
	Terms  []string // setup tokens, for matching a saved fact against the row
	Metric string
	Unit   string
	Value  float64
	Raw    string // as written in the file
	Dec    int    // decimals as written
	Line   int    // 1-based line in the file; 0 when the format has none (JSON)
	At     string // the row's own timestamp, when it has one
	Code   string // the row's own code reference (the commit measured), when it has one
	Basis  string // median | mean | best ... when the header says so
	Row    string // log rows: the line itself, the identity of one run
	Where  string // JSON: the path of the number inside the file
	// Sig is what a log row says besides its numbers: its non-numeric columns
	// and any id-like number. Two rows of one setup are repeats of one run only
	// if they agree on it; per-item rows differ in it.
	Sig  string
	Runs int // the file's own count of runs behind this row (a runs or reps column), 0 when it has none
}

// Roles say which columns of a row file are results, which name the setup
// and which only time or place a row. They are read from the newest version
// of the file and applied to the older ones, so a series keeps its key.
type Roles struct {
	Metrics []string
	Setup   []string
	Runs    string // numeric column that says how many runs a row summarises
	Time    string
	Code    string
	Void    []voidCol
}

type voidCol struct {
	Name string
	When bool // the row is void when the column equals this
}

// Kind of a file by extension; "" for files the step does not read.
func Kind(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".jsonl":
		return "jsonl"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	case ".json":
		return "json"
	case ".md":
		return "md"
	}
	return ""
}

const (
	maxRows   = 20000
	maxLeaves = 4000
	maxLabel  = 60
)

// Extract reads the measured numbers of one version of one file. roles is
// nil for the newest version (it is inferred and returned) and the newest
// version's roles for older ones. It is deterministic and calls no model.
func Extract(p string, data []byte, roles *Roles) ([]Obs, *Roles) {
	switch Kind(p) {
	case "jsonl", "csv", "tsv":
		return extractRows(Kind(p), data, roles)
	case "json":
		return extractJSON(p, data), nil
	case "md":
		return extractMarkdown(data), nil
	}
	return nil, nil
}

// ---- row files ----

type val struct {
	kind byte // 'n' number, 's' string, 'b' bool
	s    string
	f    float64
	dec  int
	b    bool
}

type rrow struct {
	line int
	text string
	cols map[string]val
	ord  []string
}

var numRe = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][-+]?\d+)?$`)

// tsRe is the only shape of timestamp a row's time column may hold.
var tsRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$`)

func parseRows(kind string, data []byte) ([]rrow, []string) {
	var rows []rrow
	var header []string
	switch kind {
	case "jsonl":
		line := 0
		for _, ln := range bytes.Split(data, []byte("\n")) {
			line++
			ln = bytes.TrimSpace(ln)
			if len(ln) == 0 || ln[0] != '{' {
				continue
			}
			dec := json.NewDecoder(bytes.NewReader(ln))
			dec.UseNumber()
			var o map[string]any
			if dec.Decode(&o) != nil {
				continue
			}
			r := rrow{line: line, text: string(ln), cols: map[string]val{}}
			flat(r.cols, &r.ord, "", o, 0)
			rows = append(rows, r)
			if len(rows) > maxRows {
				return nil, nil
			}
		}
	case "csv", "tsv":
		rd := csv.NewReader(bytes.NewReader(data))
		rd.LazyQuotes = true
		rd.FieldsPerRecord = -1
		if kind == "tsv" {
			rd.Comma = '\t'
		}
		first := true
		for {
			rec, err := rd.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				continue
			}
			line, _ := rd.FieldPos(0)
			if first {
				header, first = rec, false
				continue
			}
			r := rrow{line: line, text: strings.Join(rec, "\x1f"), cols: map[string]val{}}
			for i, c := range rec {
				if i >= len(header) || header[i] == "" {
					continue
				}
				c = strings.TrimSpace(c)
				switch {
				case c == "":
				case numRe.MatchString(c):
					f, d, _ := parseNumber(c)
					r.cols[header[i]] = val{kind: 'n', s: c, f: f, dec: d}
					r.ord = append(r.ord, header[i])
				case c == "true" || c == "false" || c == "True" || c == "False":
					r.cols[header[i]] = val{kind: 'b', s: strings.ToLower(c), b: strings.EqualFold(c, "true")}
					r.ord = append(r.ord, header[i])
				default:
					r.cols[header[i]] = val{kind: 's', s: c}
					r.ord = append(r.ord, header[i])
				}
			}
			rows = append(rows, r)
			if len(rows) > maxRows {
				return nil, nil
			}
		}
	}
	return rows, header
}

// flat flattens one level of nesting: {"m":{"toks":3}} gives m.toks.
func flat(dst map[string]val, ord *[]string, prefix string, o map[string]any, depth int) {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := prefix + k
		switch x := o[k].(type) {
		case json.Number:
			f, d, ok := parseNumber(x.String())
			if ok {
				dst[name] = val{kind: 'n', s: x.String(), f: f, dec: d}
				*ord = append(*ord, name)
			}
		case string:
			dst[name] = val{kind: 's', s: x}
			*ord = append(*ord, name)
		case bool:
			dst[name] = val{kind: 'b', s: fmt.Sprint(x), b: x}
			*ord = append(*ord, name)
		case map[string]any:
			if depth < 1 {
				flat(dst, ord, name+".", x, depth+1)
			}
		}
	}
}

var timeNames = map[string]bool{"ts": true, "time": true, "timestamp": true, "date": true, "at": true, "created": true, "created_at": true, "measured_at": true, "run_at": true, "when": true, "datetime": true}
var codeNames = map[string]bool{"git": true, "sha": true, "commit": true, "rev": true, "revision": true, "build": true, "code": true}
var okWords = map[string]bool{"clean": true, "valid": true, "ok": true, "success": true, "passed": true, "healthy": true, "good": true}
var badWords = map[string]bool{"error": true, "err": true, "failed": true, "failure": true, "invalid": true, "dirty": true, "aborted": true, "timeout": true}

func inferRoles(rows []rrow, header []string) *Roles {
	r := &Roles{}
	names := map[string]bool{}
	var order []string
	add := func(n string) {
		if !names[n] {
			names[n] = true
			order = append(order, n)
		}
	}
	for _, h := range header {
		if h != "" {
			add(h)
		}
	}
	for _, row := range rows {
		for _, n := range row.ord {
			add(n)
		}
	}
	for _, n := range order {
		var num, str, boo int
		distinct := map[string]bool{}
		long := 0
		for _, row := range rows {
			v, ok := row.cols[n]
			if !ok {
				continue
			}
			switch v.kind {
			case 'n':
				num++
			case 's':
				str++
				if len(v.s) > maxLabel {
					long++
				}
			case 'b':
				boo++
			}
			if len(distinct) <= 80 {
				distinct[v.s] = true
			}
		}
		total := num + str + boo
		if total == 0 {
			continue
		}
		lower := strings.ToLower(n)
		switch {
		case timeNames[lower]:
			if r.Time == "" {
				r.Time = n
			}
		case codeNames[lower]:
			if r.Code == "" {
				r.Code = n
			}
		case boo*2 > total:
			ws := words(n)
			void := false
			for _, w := range ws {
				if okWords[w] {
					r.Void = append(r.Void, voidCol{n, false})
					void = true
				}
				if badWords[w] {
					r.Void = append(r.Void, voidCol{n, true})
					void = true
				}
			}
			if !void && len(distinct) <= 8 {
				r.Setup = append(r.Setup, n)
			}
		case num*2 > total:
			if isRunsCol(n) {
				if r.Runs == "" {
					r.Runs = n
				}
			} else if m, _ := isMetric(n); m {
				r.Metrics = append(r.Metrics, n)
			} else if isConfigNumeric(n) && len(distinct) <= 64 {
				r.Setup = append(r.Setup, n)
			}
		default:
			if long*4 > total || len(distinct) > 64 {
				continue // free text or an id, not a setup
			}
			if len(rows) > 8 && len(distinct)*2 > len(rows) {
				continue
			}
			r.Setup = append(r.Setup, n)
		}
	}
	return r
}

func extractRows(kind string, data []byte, roles *Roles) ([]Obs, *Roles) {
	rows, header := parseRows(kind, data)
	if len(rows) == 0 {
		return nil, roles
	}
	if roles == nil {
		roles = inferRoles(rows, header)
	}
	if len(roles.Metrics) == 0 {
		return nil, roles
	}
	var out []Obs
rowLoop:
	for _, row := range rows {
		for _, v := range roles.Void {
			if c, ok := row.cols[v.Name]; ok && c.kind == 'b' && c.b == v.When {
				continue rowLoop
			}
		}
		var parts, termParts []string
		for _, s := range roles.Setup {
			if c, ok := row.cols[s]; ok {
				if !safeText(c.s) || !safeText(s) {
					continue rowLoop
				}
				parts = append(parts, s+"="+c.s)
				termParts = append(termParts, c.s)
			}
		}
		setup := strings.Join(parts, " ")
		label := setup
		for _, m := range roles.Metrics {
			c, ok := row.cols[m]
			if !ok || c.kind != 'n' || !safeText(m) {
				continue
			}
			_, unit := isMetric(m)
			o := Obs{
				Group: m, Key: setup + "|" + m, Label: label, Terms: tokens(strings.Join(termParts, " ")),
				Metric: m, Unit: unit, Value: c.f, Raw: c.s, Dec: c.dec, Line: row.line,
				Basis: basisOf(m), Row: row.text,
			}
			if roles.Time != "" && tsRe.MatchString(row.cols[roles.Time].s) {
				o.At = row.cols[roles.Time].s
			}
			if roles.Code != "" && safeText(row.cols[roles.Code].s) {
				o.Code = row.cols[roles.Code].s
			}
			o.Sig = rowSig(row, roles)
			if roles.Runs != "" {
				if c := row.cols[roles.Runs]; c.kind == 'n' && c.f >= 1 && c.f < 1e6 {
					o.Runs = int(c.f)
				}
			}
			out = append(out, o)
		}
	}
	return out, roles
}

// runWords name a column that indexes the repeats of one run (the rep, the
// seed, the trial): rows that differ only there are repeats.
var runWords = map[string]bool{"rep": true, "repeat": true, "run": true, "trial": true, "iter": true, "iteration": true, "attempt": true, "seed": true, "pass": true}

// idWords name a number that identifies a row (an item, a line), not a
// measurement of it.
var idWords = map[string]bool{"id": true, "idx": true, "index": true, "item": true, "row": true, "line": true, "seq": true, "step": true, "no": true, "num": true, "sample": true, "case": true}

func isRunsCol(name string) bool {
	for _, w := range words(name) {
		switch w {
		case "runs", "reps", "repeats", "replicates", "nruns":
			return true
		}
	}
	return false
}

// rowSig is what a row says besides its measurements: every string and
// boolean column and every id-like number, except the run index. Numbers that
// are neither (the load at the time, a token count) are not part of what the
// row is about.
func rowSig(row rrow, roles *Roles) string {
	skip := map[string]bool{}
	for _, m := range roles.Metrics {
		skip[m] = true
	}
	if roles.Runs != "" {
		skip[roles.Runs] = true
	}
	if roles.Time != "" {
		skip[roles.Time] = true
	}
	if roles.Code != "" {
		skip[roles.Code] = true
	}
	names := make([]string, 0, len(row.cols))
	for n := range row.cols {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		c := row.cols[n]
		if skip[n] {
			continue
		}
		run, id := false, false
		for _, w := range words(n) {
			run = run || runWords[w]
			id = id || idWords[w]
		}
		if run || (c.kind == 'n' && !id) {
			continue
		}
		b.WriteString(n + "=" + c.s + "\x1f")
	}
	return b.String()
}

// ---- JSON ----

var identityFields = []string{"id", "name", "model", "key", "label", "case", "cell", "scenario", "setup", "config", "title", "arm", "tag", "prompt"}

type seg struct {
	name  string
	ident bool // an array element named by its identity field
}

// baselineFile: CI baselines and parity receipts hold thresholds and the
// record of one check, not results measured again.
var baselineFile = regexp.MustCompile(`(?i)baseline|parity`)

// statLeaf: the last key of a number that describes the uncertainty of an
// estimate (a confidence bound, a delta, a standard error, a p-value). These
// are statistics of a result, not results.
var statLeaf = regexp.MustCompile(`(?i)^(ci\d*|stderr|std|p_?value|delta.*|one_sided.*|.*_(lower|upper|stderr|std)|lower|upper)$`)

// itemsSegment: a path under per-item or per-slice records is one item's
// score, not the result of the run; stages_ms is a CI gate.
var itemsSegment = regexp.MustCompile(`(?i)^(items|cases|samples|examples|slices|stages_ms|per_item.*|per_task.*|per_slice.*)$`)

func extractJSON(file string, data []byte) []Obs {
	if baselineFile.MatchString(file) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if dec.Decode(&root) != nil {
		return nil
	}
	var out []Obs
	var walk func(v any, p []seg)
	walk = func(v any, p []seg) {
		if len(out) >= maxLeaves {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			named := len(p) > 0 && dictOfLike(x, keys)
			for _, k := range keys {
				walk(x[k], append(append([]seg(nil), p...), seg{name: k, ident: named}))
			}
		case []any:
			ids := arrayIdentities(x)
			for i, e := range x {
				s := seg{name: fmt.Sprintf("[%d]", i)}
				if ids != nil {
					s = seg{name: ids[i], ident: true}
				}
				walk(e, append(append([]seg(nil), p...), s))
			}
		case json.Number:
			f, d, ok := parseNumber(x.String())
			if !ok {
				return
			}
			if o, ok := jsonObs(p, f, d, x.String()); ok {
				out = append(out, o)
			}
		}
	}
	walk(root, nil)
	return out
}

// dictOfLike: an object whose entries are all objects, or all numbers, under
// names that are not themselves measured quantities (scenarios by name,
// stages by name) is a table keyed by its entries, like an array of objects
// named by an id. Its keys are setup, not metric.
func dictOfLike(m map[string]any, keys []string) bool {
	if len(keys) < 2 {
		return false
	}
	kind := ""
	for _, k := range keys {
		var kk string
		switch m[k].(type) {
		case map[string]any:
			kk = "object"
		case json.Number:
			kk = "number"
		case nil:
			continue // a missing measurement does not change what the entries are
		default:
			return false
		}
		if kind != "" && kk != kind {
			return false
		}
		kind = kk
		if ok, _ := isMetric(k); ok || !safeText(k) {
			return false
		}
	}
	return kind == "number" || len(keys) >= 3
}

// arrayIdentities names the elements of an array of objects by their first
// identity field, when every element has a distinct one.
func arrayIdentities(a []any) []string {
	if len(a) == 0 {
		return nil
	}
	for _, f := range identityFields {
		seen := map[string]bool{}
		ids := make([]string, 0, len(a))
		for _, e := range a {
			m, ok := e.(map[string]any)
			if !ok {
				return nil
			}
			s, ok := m[f].(string)
			if !ok || s == "" || seen[s] || len(s) > maxLabel {
				ids = nil
				break
			}
			seen[s] = true
			ids = append(ids, s)
		}
		if ids != nil {
			return ids
		}
	}
	return nil
}

func jsonObs(p []seg, f float64, dec int, raw string) (Obs, bool) {
	for i, s := range p {
		if !s.ident && strings.HasPrefix(s.name, "[") {
			if i == len(p)-1 {
				return Obs{}, false // one element of an array of numbers
			}
			continue
		}
		if !safeText(s.name) {
			return Obs{}, false
		}
		if !s.ident && itemsSegment.MatchString(s.name) {
			return Obs{}, false
		}
		if i == len(p)-1 && !s.ident && statLeaf.MatchString(s.name) {
			return Obs{}, false
		}
	}
	var metric, unit string
	for i := len(p) - 1; i >= 0 && i >= len(p)-4; i-- {
		if p[i].ident || strings.HasPrefix(p[i].name, "[") {
			continue
		}
		if ok, u := isMetric(p[i].name); ok {
			metric, unit = p[i].name, u
			break
		}
	}
	if metric == "" {
		return Obs{}, false
	}
	// The setup tokens are every segment of the path but the metric's own: the
	// quantity is told by the number and its unit, the setup by the rest.
	var schema, real, ident []string
	for _, s := range p {
		switch {
		case s.ident:
			schema = append(schema, "[*]")
			real = append(real, "["+s.name+"]")
			ident = append(ident, s.name)
		default:
			schema = append(schema, s.name)
			real = append(real, s.name)
			if s.name != metric && !strings.HasPrefix(s.name, "[") {
				ident = append(ident, s.name)
			}
		}
	}
	return Obs{
		Group: strings.Join(schema, "."), Key: strings.Join(real, "."), Label: strings.Join(real, "."),
		Terms: tokens(strings.Join(ident, " ")), Metric: metric, Unit: unit, Value: f, Raw: raw, Dec: dec,
		Basis: basisOf(metric), Where: strings.Join(real, "."),
	}, true
}

// ---- Markdown tables ----

var (
	headingRe = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	sepRe     = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	badLabel  = regexp.MustCompile("[\\[\\]{}<>|`\"':;$\\\\]")
	mdMark    = strings.NewReplacer("**", "", "__", "", "`", "")
)

func splitRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l) && l[i+1] == '|':
			cur.WriteByte('|')
			i++
		case l[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func cleanText(s string) string {
	s = strings.TrimSpace(mdMark.Replace(s))
	return strings.Join(strings.Fields(s), " ")
}

// safeText: text from a repo that may be written into a fact and printed into
// an agent's context. Short, one line, none of the characters that make a
// link, a code span, a quote or markup. Anything else is left out and counted.
func safeText(s string) bool {
	if s == "" || len([]rune(s)) > maxLabel || badLabel.MatchString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// plain strips what safeText refuses, for text that only names a group.
func plain(s string) string {
	s = badLabel.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 90 {
		s = string(r[:90])
	}
	return s
}

func okLabel(s string) bool {
	return s != "" && len([]rune(s)) <= maxLabel && !badLabel.MatchString(s) && hasLetter(s)
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			return true
		}
	}
	return false
}

func extractMarkdown(data []byte) []Obs {
	lines := strings.Split(string(data), "\n")
	var out []Obs
	heading := ""
	fence := false
	tableNo := map[string]int{}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		if m := headingRe.FindStringSubmatch(l); m != nil {
			heading = cleanText(m[1])
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(l), "|") || i+1 >= len(lines) || !sepRe.MatchString(strings.TrimSpace(lines[i+1])) {
			continue
		}
		header := splitRow(l)
		j := i + 2
		var body [][]string
		var bodyLine []int
		for ; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|"); j++ {
			body = append(body, splitRow(lines[j]))
			bodyLine = append(bodyLine, j+1)
		}
		i = j - 1
		sig := ""
		// The group name is kept as written (whitespace collapsed): it is
		// redacted with the rest of the repo's text before it is stripped or cut.
		group := heading
		if group == "" {
			group = "(no heading)"
		}
		first := ""
		if len(header) > 0 {
			first = cleanText(header[0])
		}
		group += " / " + first
		sig = heading + "|" + first
		tableNo[sig]++
		if n := tableNo[sig]; n > 1 {
			group += fmt.Sprintf(" #%d", n)
		}
		out = append(out, tableObs(group, heading, header, body, bodyLine)...)
	}
	return out
}

func tableObs(group, heading string, header []string, body [][]string, lineNo []int) []Obs {
	ncol := len(header)
	// the label column: the first column whose cells are mostly words
	label := -1
	for c := 0; c < ncol && label < 0; c++ {
		wordy, n := 0, 0
		for _, r := range body {
			if c < len(r) && r[c] != "" {
				n++
				if _, _, _, ok := parseCell(r[c]); !ok && hasLetter(r[c]) {
					wordy++
				}
			}
		}
		if n > 0 && wordy*2 >= n {
			label = c
		}
	}
	if label < 0 {
		return nil
	}
	// which columns are results
	type colInfo struct {
		ok     bool
		unit   string
		hdr    string // the unit the header itself declares: "(ms)", "TTFT ms"
		metric string
	}
	cols := make([]colInfo, ncol)
	for c := 0; c < ncol; c++ {
		if c == label {
			continue
		}
		h := cleanText(header[c])
		num, withUnit := 0, 0
		units := map[string]int{}
		for _, r := range body {
			if c >= len(r) {
				continue
			}
			if _, _, u, ok := parseCell(r[c]); ok {
				num++
				if u != "" {
					withUnit++
					units[u]++
				}
			}
		}
		if num == 0 {
			continue
		}
		isM, hu := isMetric(h)
		if p := parenUnit(h); p != "" {
			hu = p
		}
		if !(isM || withUnit*2 >= num) {
			continue
		}
		best, bn := "", 0
		for u, n := range units {
			if n > bn || (n == bn && u < best) {
				best, bn = u, n
			}
		}
		if best == "" {
			best = hu
		}
		cols[c] = colInfo{ok: true, unit: best, hdr: hu, metric: h}
	}
	// row labels, unique within the table
	labels := make([]string, len(body))
	count := map[string]int{}
	for k, r := range body {
		if label < len(r) {
			labels[k] = cleanText(r[label])
		}
		count[labels[k]]++
	}
	var out []Obs
	for k, r := range body {
		lb := labels[k]
		if count[lb] > 1 || !okLabel(lb) {
			continue
		}
		for c := 0; c < ncol && c < len(r); c++ {
			if !cols[c].ok {
				continue
			}
			v, dec, u, rawNum, ok := parseCellRaw(r[c])
			if !ok {
				continue
			}
			if u != "" && cols[c].hdr != "" && u != cols[c].hdr {
				continue // the header says one unit and the cell another: the source disagrees with itself, so no number is taken
			}
			if u == "" {
				u = cols[c].unit
			}
			m := cols[c].metric
			if !okLabel(m) {
				continue
			}
			out = append(out, Obs{
				Group: group, Key: lb + "|" + m, Label: lb + " / " + m,
				Terms:  tokens(lb), // the row is the setup; the column is told by the number and its unit
				Metric: m, Unit: u, Value: v, Raw: rawNum, Dec: dec, Line: lineNo[k],
				Basis: basisOf(m),
			})
		}
	}
	return out
}

var parenRe = regexp.MustCompile(`\(([^()]*)\)\s*$`)

func parenUnit(h string) string {
	if m := parenRe.FindStringSubmatch(h); m != nil {
		return canonUnit(m[1])
	}
	return ""
}
