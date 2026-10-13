package results

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func cm(sha, date string) Commit {
	return Commit{SHA: sha + strings.Repeat("0", 40-len(sha)), Date: date}
}

func snap(label, group string, v float64, raw string) Obs {
	return Obs{Group: group, Key: label + "|tok/s", Label: label, Terms: tokens(label), Metric: "tok/s", Unit: "tok/s", Value: v, Raw: raw, Line: 3}
}

// A board rewritten as results change: one point per change, attributed to
// the commit that wrote it, and the previous value is the one it replaced.
func TestSnapshotSeriesKeepsTheCommitOfEachChange(t *testing.T) {
	vs := []Version{
		{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{snap("q9 plain short", "g", 138.5, "138.5"), snap("q9 spec short", "g", 281, "281")}},
		{cm("bbbbbbbbbbbb", "2026-09-05"), []Obs{snap("q9 plain short", "g", 138.5, "138.5"), snap("q9 spec short", "g", 281, "281")}},
		{cm("cccccccccccc", "2026-09-10"), []Obs{snap("q9 plain short", "g", 161.3, "161.3"), snap("q9 spec short", "g", 281, "281")}},
	}
	ss := BuildSeries("r", "BENCH.md", vs, false)
	if len(ss) != 2 {
		t.Fatalf("%d series", len(ss))
	}
	plain := ss[0]
	if len(plain.Points) != 2 || plain.Cur().Raw != "161.3" || plain.Cur().Commit != "cccccccccccc" || plain.Cur().Date != "2026-09-10" {
		t.Fatalf("plain: %+v", plain.Points)
	}
	prev, ok := plain.Prev()
	if !ok || prev.Raw != "138.5" || prev.Commit != "aaaaaaaaaaaa" {
		t.Fatalf("the replaced value stood since its first commit: %+v", prev)
	}
	if !plain.Recurs() || ss[1].Recurs() {
		t.Fatalf("only the changed row recurs: %v %v", plain.Recurs(), ss[1].Recurs())
	}
}

func logRowObs(at, setup string, v float64, raw string, n int) Obs {
	return Obs{Group: "toks", Key: setup + "|toks", Label: setup, Terms: tokens(setup), Metric: "toks", Unit: "tok/s", Value: v, Raw: raw, Dec: 2, At: at, Row: fmt.Sprintf("%s %s %s %d", at, setup, raw, n), Line: n}
}

// A log is ordered by its rows' own timestamps. Rows that share one are
// repeats measured together and become one point: their median, and the
// point says how many rows it is.
func TestLogSeriesGroupsRowsByTimestampAndTakesTheMedian(t *testing.T) {
	vs := []Version{
		{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 100, "100", 1),
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 90, "90", 2),
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 110, "110", 3),
		}},
		{cm("bbbbbbbbbbbb", "2026-09-08"), []Obs{
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 100, "100", 1),
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 90, "90", 2),
			logRowObs("2026-08-30T01:00:00Z", "cell=a", 110, "110", 3),
			logRowObs("2026-09-07T12:00:00Z", "cell=a", 120.5, "120.5", 4),
		}},
	}
	ss := BuildSeries("r", "results.jsonl", vs, false)
	if len(ss) != 1 || !ss[0].Log || len(ss[0].Points) != 2 {
		t.Fatalf("%+v", ss)
	}
	first, cur := ss[0].Points[0], ss[0].Cur()
	if first.Value != 100 || first.N != 3 || first.Commit != "aaaaaaaaaaaa" {
		t.Fatalf("first point is the median of three rows: %+v", first)
	}
	if cur.Value != 120.5 || cur.N != 1 || cur.Commit != "bbbbbbbbbbbb" || cur.At != "2026-09-07T12:00:00Z" {
		t.Fatalf("cur: %+v", cur)
	}
	if got := basisText(ss[0], first); !strings.Contains(got, "median of 3 rows") {
		t.Fatalf("basis %q", got)
	}
	if got := basisText(ss[0], cur); got != "run count not stated (one row)" {
		t.Fatalf("a lone row is one row of the file, not a run it says so about, got %q", got)
	}
}

// Rows with no timestamp of their own are samples of one run, not results
// measured again later.
func TestLogRowsWithoutATimestampMakeNoSeries(t *testing.T) {
	o := func(n int) Obs {
		x := logRowObs("", "cell=a", float64(100+n), fmt.Sprint(100+n), n)
		x.Row = fmt.Sprint("req ", n)
		return x
	}
	ss := BuildSeries("r", "req.jsonl", []Version{{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{o(1), o(2), o(3)}}}, false)
	if len(ss) != 0 {
		t.Fatalf("%d series from samples", len(ss))
	}
}

func fx(name, scope, kind, updated, text string) *fact.Fact {
	return &fact.Fact{
		Path: scope + "/" + name + ".md",
		Meta: fact.Meta{Name: name, Description: text, Type: "design_decision", Scope: scope, Source: fact.Source{Harness: "claude", Host: "host-a", Kind: kind}, Created: updated, Updated: updated},
		Body: text,
	}
}

func fxt(name, typ, scope, kind, updated, text string) *fact.Fact {
	f := fx(name, scope, kind, updated, text)
	f.Meta.Type = typ
	return f
}

func matchSeries() []*Series {
	mk := func(label string, vals ...float64) *Series {
		s := &Series{Repo: "r", File: "RESULTS.md", Group: "g", Key: label, Label: label, Metric: "tok/s", Unit: "tok/s", Terms: tokens(label)}
		for i, v := range vals {
			s.Points = append(s.Points, Point{Value: v, Raw: num(v), Dec: 1, Commit: fmt.Sprintf("c%011d", i), Date: fmt.Sprintf("2026-09-%02d", 1+i*5), N: 1})
		}
		s.N = len(s.Points)
		return s
	}
	return []*Series{
		mk("q9 plain short", 138.5, 161.3),
		mk("q9 plain long", 120.4, 120.4),
		mk("q9 spec short", 281.0, 301.2),
	}
}

// A saved fact that quotes the replaced value of a row is a hit; one that
// quotes the current value only names the row; a different setup, a number
// of two digits, a value inside the noise and a note older than the value
// are nothing.
func TestMatchFindsFactsThatQuoteAReplacedValue(t *testing.T) {
	ix := NewIndex(matchSeries())
	facts := []*fact.Fact{
		fx("stale-quote", "projects/r", "observed", "2026-09-03", "The q9 plain short decode ran 138.5 tok/s on the host-a."),
		fx("current-quote", "projects/r", "observed", "2026-09-12", "q9 plain short decode is 161.3 tok/s now."),
		fx("other-setup", "projects/r", "observed", "2026-09-06", "The q9 spec long case ran 138.5 tok/s."),
		fx("two-digits", "projects/r", "observed", "2026-09-06", "q9 plain short ran 14 tok/s once."),
		fx("wrong-unit", "projects/r", "observed", "2026-09-06", "q9 plain short took 138.5 ms."),
		fx("before-the-value", "projects/r", "observed", "2026-08-01", "q9 plain short decode ran 138.5 tok/s."),
		fx("ruling", "user", "user", "2026-09-06", "Owner: q9 plain short decodes 138.5 tok/s on the host-a."),
		fx("floor-ruling", "user", "user", "2026-09-06", "Owner: q9 plain short must stay above 138.5 tok/s."),
		fx("target", "projects/r", "observed", "2026-09-06", "The target for q9 plain short is 138.5 tok/s."),
		fx("floor-after", "projects/r", "observed", "2026-09-06", "q9 plain short 138.5 tok/s floor in the gate."),
		fx("less-than", "projects/r", "observed", "2026-09-06", "alert when q9 plain short < 138.5 tok/s."),
		fx("from-to", "projects/r", "observed", "2026-09-06", "q9 plain short went from 138.5 to 161.3 tok/s."),
		fx("was-is", "projects/r", "observed", "2026-09-06", "q9 plain short was 138.5 tok/s, and is now 161.3 tok/s."),
		fx("now-was", "projects/r", "observed", "2026-09-06", "q9 plain short 161.3 tok/s now (was 138.5 tok/s)."),
		fxt("history-note", "history", "projects/r", "observed", "2026-09-06", "q9 plain short ran 138.5 tok/s."),
		fxt("mistake-note", "past_mistake", "projects/r", "observed", "2026-09-06", "q9 plain short ran 138.5 tok/s."),
		fxt("change-note", "code_change", "projects/r", "observed", "2026-09-06", "q9 plain short ran 138.5 tok/s."),
		fx("dated-record", "projects/r", "observed", "2026-09-07", "On 2026-09-02 the q9 plain short decode was 138.5 tok/s; the number is the first one published."),
		fx("round-figure", "projects/r", "observed", "2026-09-06", "q9 plain short decode is about 139 tok/s"),
	}
	hits := ix.Match(facts)
	got := map[string]Hit{}
	for _, h := range hits {
		got[h.Fact.Meta.Name] = h
	}
	if h, ok := got["stale-quote"]; !ok || h.Named || h.Old.Raw != "138.5" || h.S.Cur().Raw != "161.3" {
		t.Fatalf("stale-quote: %+v", h)
	}
	if h, ok := got["current-quote"]; !ok || !h.Named {
		t.Fatalf("current-quote: %+v", h)
	}
	if h, ok := got["ruling"]; !ok || h.Named {
		t.Fatalf("ruling: %+v", h)
	}
	for _, n := range []string{"other-setup", "two-digits", "wrong-unit", "before-the-value", "dated-record", "round-figure",
		"floor-ruling", "target", "floor-after", "less-than", "history-note", "mistake-note", "change-note"} {
		if _, ok := got[n]; ok {
			t.Fatalf("%s must not be a hit: %+v", n, got[n])
		}
	}
	// a fact that states both values is about the current one, in any word order
	for _, n := range []string{"from-to", "was-is", "now-was"} {
		if h, ok := got[n]; !ok || !h.Named {
			t.Fatalf("%s states the current value too, so it names the row and is not stale: %+v", n, h)
		}
	}
	if r := got["stale-quote"].Repl; r.Raw != "161.3" || r.Date != "2026-09-06" {
		t.Fatalf("the quoted value was replaced by the second point: %+v", r)
	}
}

func TestMatchIgnoresDifferencesInsideTheNoise(t *testing.T) {
	// eight re-measurements between 160.0 and 162.0; a note quoting 160.0 is the same measurement
	s := &Series{Repo: "r", File: "perf.jsonl", Group: "toks", Key: "k", Label: "cell=q9 plain short", Metric: "toks", Unit: "tok/s", Terms: tokens("cell=q9 plain short"), Log: true}
	for i, v := range []float64{160.0, 161.2, 160.5, 162.0, 161.0, 160.8, 161.7, 161.3} {
		s.Points = append(s.Points, Point{Value: v, Raw: num(v), Dec: 1, Commit: fmt.Sprintf("c%011d", i), Date: "2026-09-01", N: 1})
	}
	other := &Series{Repo: "r", File: "perf.jsonl", Group: "toks", Key: "k2", Label: "cell=q9 spec long", Metric: "toks", Unit: "tok/s", Terms: tokens("cell=q9 spec long"), Points: []Point{{Value: 300, Raw: "300", Commit: "x", Date: "2026-09-01", N: 1}}}
	ix := NewIndex([]*Series{s, other})
	hits := ix.Match([]*fact.Fact{fx("near", "projects/r", "observed", "2026-09-06", "q9 plain short ran 160.0 tok/s")})
	if len(hits) != 0 {
		t.Fatalf("160.0 is an older value of the row but inside its noise, so it is neither stale nor the current value: %+v", hits)
	}
	far := ix.Match([]*fact.Fact{fx("far", "projects/r", "observed", "2026-09-06", "q9 plain short ran 140.0 tok/s")})
	if len(far) != 0 {
		t.Fatalf("140.0 is no recorded value: %+v", far)
	}
}

// Few values do not make a quiet log: four points 100.0, 102.0, 99.0, 103.5
// are one setup wandering by a few percent, and a note that quotes 100.0 is
// not out of date against 103.5. Two points that differ by 20 percent are.
func TestNoiseBandHoldsWithFewPoints(t *testing.T) {
	mk := func(vals ...float64) *Series {
		s := &Series{Repo: "r", File: "perf.jsonl", Group: "toks", Key: "k", Label: "cell=q9 plain short", Metric: "toks", Unit: "tok/s", Terms: tokens("cell=q9 plain short"), Log: true}
		for i, v := range vals {
			s.Points = append(s.Points, Point{Value: v, Raw: fmt.Sprintf("%.1f", v), Dec: 1, Commit: fmt.Sprintf("c%011d", i), Date: "2026-09-01", N: 1})
		}
		return s
	}
	other := &Series{Repo: "r", File: "perf.jsonl", Group: "toks", Key: "k2", Label: "cell=q9 spec long", Metric: "toks", Unit: "tok/s", Terms: tokens("cell=q9 spec long"), Points: []Point{{Value: 300, Raw: "300", Commit: "x", Date: "2026-09-01", N: 1}}}
	note := fx("n", "projects/r", "observed", "2026-09-06", "q9 plain short ran 100.0 tok/s")
	if four := NewIndex([]*Series{mk(100.0, 102.0, 99.0, 103.5), other}).Match([]*fact.Fact{note}); len(four) != 0 {
		t.Fatalf("four points inside 5 percent are noise: %+v", four)
	}
	// one or two points after the change are not a sample: perf-ci's laptop
	// cells move 2 to 5 percent between runs of the same code, so 3.5 percent
	// is not shown to be a change
	if one := NewIndex([]*Series{mk(100.0, 103.5), other}).Match([]*fact.Fact{note}); len(one) != 0 {
		t.Fatalf("a 3.5 percent difference to one later row is inside the 5 percent that stands in for a sample: %+v", one)
	}
	two := NewIndex([]*Series{mk(100.0, 120.5), other}).Match([]*fact.Fact{note})
	if len(two) != 1 || two[0].Named {
		t.Fatalf("a 20 percent change between two points is a replacement: %+v", two)
	}
}

// A setup that the newest version of the file no longer holds (a renamed
// column, a deleted row) is not a result of the repo any more: its last
// value is not current.
func TestASeriesTheNewestVersionLacksIsDropped(t *testing.T) {
	vs := []Version{
		{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{snap("q9 plain short", "g", 138.5, "138.5"), snap("q9 spec short", "g", 281, "281")}},
		{cm("bbbbbbbbbbbb", "2026-09-05"), []Obs{snap("q9 spec short", "g", 301.2, "301.2")}},
	}
	ss := BuildSeries("r", "BENCH.md", vs, false)
	if len(ss) != 1 || ss[0].Label != "q9 spec short" || ss[0].Cur().Raw != "301.2" {
		t.Fatalf("%+v", ss)
	}
	// a log: a row the newest version dropped is no longer a point
	r1 := logRowObs("2026-08-30T01:00:00Z", "cell=a", 100, "100", 1)
	r2 := logRowObs("2026-09-07T12:00:00Z", "cell=a", 120.5, "120.5", 2)
	r3 := logRowObs("2026-09-08T12:00:00Z", "cell=a", 90, "90", 3)
	logs := BuildSeries("r", "results.jsonl", []Version{
		{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{r1, r2}},
		{cm("bbbbbbbbbbbb", "2026-09-09"), []Obs{r1, r3}},
	}, false)
	if len(logs) != 1 || len(logs[0].Points) != 2 || logs[0].Cur().Raw != "90" {
		t.Fatalf("%+v", logs)
	}
}

func sigRow(at, code, sig string, v float64, n int) Obs {
	o := logRowObs(at, "cell=a", v, num(v), n)
	o.Code, o.Sig = code, sig
	return o
}

// A median is taken only over rows of one run: the same full timestamp and
// the same code, saying the same thing in every column but their numbers.
func TestMedianNeedsOneRunNotOneDay(t *testing.T) {
	ver := func(obs ...Obs) []Version { return []Version{{cm("aaaaaaaaaaaa", "2026-09-01"), obs}} }
	// a bare date covers many runs
	if ss := BuildSeries("r", "f.jsonl", ver(sigRow("2026-08-30", "c1", "s", 1, 1), sigRow("2026-08-30", "c1", "s", 2, 2)), false); len(ss) != 0 {
		t.Fatalf("two rows on one day are not provably one run: %+v", ss)
	}
	// one timestamp, two codes: two runs
	ss := BuildSeries("r", "f.jsonl", ver(sigRow("2026-08-30T10:00:00Z", "c1", "s", 1, 1), sigRow("2026-08-30T10:00:00Z", "c2", "s", 3, 2)), false)
	if len(ss) != 1 || len(ss[0].Points) != 2 || ss[0].Points[0].N != 1 || ss[0].Points[1].N != 1 {
		t.Fatalf("rows of different code are not merged: %+v", ss)
	}
	// one timestamp, one code, different items: not repeats of anything
	if ss := BuildSeries("r", "f.jsonl", ver(sigRow("2026-08-30T10:00:00Z", "c1", "item=p1", 0, 1), sigRow("2026-08-30T10:00:00Z", "c1", "item=p2", 1, 2)), false); len(ss) != 0 {
		t.Fatalf("per-item rows of one run make no value: %+v", ss)
	}
	// a hundred items written by five runs (twenty items share a run's
	// timestamp): a list of items, not a series
	var many []Obs
	for i := 0; i < 100; i++ {
		many = append(many, sigRow(fmt.Sprintf("2026-08-30T10:%02d:00Z", i/20), "c1", fmt.Sprintf("item=%d", i), float64(i%2), i))
	}
	if ss := BuildSeries("r", "f.jsonl", ver(many...), false); len(ss) != 0 {
		t.Fatalf("a list of items is not one quantity measured again: %d series", len(ss))
	}
	// the real thing: repeats of one run
	ss = BuildSeries("r", "f.jsonl", ver(sigRow("2026-08-30T10:00:00Z", "c1", "s", 1, 1), sigRow("2026-08-30T10:00:00Z", "c1", "s", 3, 2), sigRow("2026-08-30T10:00:00Z", "c1", "s", 2, 3)), false)
	if len(ss) != 1 || ss[0].Cur().N != 3 || ss[0].Cur().Value != 2 {
		t.Fatalf("three repeats of one run: %+v", ss)
	}
}

// Two rows on one bare date with different codes are two runs with one
// timestamp. The later row of the file is the newer, whatever the codes sort
// like.
func TestRunsWithOneTimestampKeepFileOrder(t *testing.T) {
	ver := []Version{{cm("aaaaaaaaaaaa", "2026-09-01"), []Obs{sigRow("2026-08-30", "zzz", "s", 100, 1), sigRow("2026-08-30", "aaa", "s", 110, 2)}}}
	ss := BuildSeries("r", "f.jsonl", ver, false)
	if len(ss) != 1 || len(ss[0].Points) != 2 {
		t.Fatalf("two runs: %+v", ss)
	}
	if got := ss[0].Cur().Value; got != 110 {
		t.Fatalf("the later row of the file is the current value, got %v from codes %q then %q", got, ss[0].Points[0].Code, ss[0].Points[1].Code)
	}
	if ss[0].Points[0].Value != 100 || ss[0].Points[0].Code != "zzz" {
		t.Fatalf("the earlier row stays first: %+v", ss[0].Points)
	}
}

// A perf log whose rows carry a column that is unique to each row (a uuid, a
// note) differs from row to row in what it says besides its numbers. That is
// not a list of items: each row has a time of its own, and the setup is
// measured again and again.
func TestPerfLogWithAUniqueColumnIsStillASeries(t *testing.T) {
	var rows []Obs
	for i := 0; i < 10; i++ {
		rows = append(rows, sigRow(fmt.Sprintf("2026-08-%02dT10:00:00Z", 10+i), fmt.Sprintf("c%d", i), fmt.Sprintf("uuid=u%d", i), 100+float64(i), i+1))
	}
	ss := BuildSeries("r", "f.jsonl", []Version{{cm("aaaaaaaaaaaa", "2026-09-01"), rows}}, false)
	if len(ss) != 1 || len(ss[0].Points) != 10 || ss[0].Cur().Value != 109 {
		t.Fatalf("ten timed rows of one setup: %+v", ss)
	}
}

// Hits come in one order, however the maps that find them iterate: the same
// row label and column under two headings of one file is a tie on file and
// key, so the heading breaks it.
func TestMatchOrderIsDeterministic(t *testing.T) {
	mk := func(group string) []*Series {
		s := &Series{Repo: "r", File: "RESULTS.md", Group: group, Key: "q9 plain short|tok/s", Label: "q9 plain short", Metric: "tok/s", Unit: "tok/s", Terms: tokens("q9 plain short")}
		s.Points = []Point{{Value: 138.5, Raw: "138.5", Dec: 1, Commit: "c00000000001", Date: "2026-09-01", N: 1}, {Value: 161.3, Raw: "161.3", Dec: 1, Commit: "c00000000002", Date: "2026-09-10", N: 1}}
		s.N = 2
		other := &Series{Repo: "r", File: "RESULTS.md", Group: group, Key: "q9 spec long|tok/s", Label: "q9 spec long", Metric: "tok/s", Unit: "tok/s", Terms: tokens("q9 spec long"), Points: []Point{{Value: 300, Raw: "300", Commit: "x", Date: "2026-09-01", N: 1}}}
		return []*Series{s, other}
	}
	all := append(mk("Heading B"), mk("Heading A")...)
	note := fx("n", "projects/r", "observed", "2026-09-12", "q9 plain short ran 138.5 tok/s")
	for i := 0; i < 80; i++ {
		hits := NewIndex(all).Match([]*fact.Fact{note})
		if len(hits) != 2 {
			t.Fatalf("run %d: %d hits, want one per heading: %+v", i, len(hits), hits)
		}
		if hits[0].S.Group != "Heading A" || hits[1].S.Group != "Heading B" {
			t.Fatalf("run %d: hits in a different order: %q then %q", i, hits[0].S.Group, hits[1].S.Group)
		}
	}
}
