package results

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Point is one value a series held, with the commit that wrote it.
type Point struct {
	Value  float64
	Raw    string
	Dec    int
	Commit string // 12 characters
	Date   string // YYYY-MM-DD, UTC, of that commit
	At     string // the row's own timestamp
	Code   string // the code the row says it measured
	Line   int
	Basis  string
	N      int   // rows behind the value: >1 when rows of one run were summarised
	Runs   int   // the file's own count of runs behind the value, 0 when it has none
	Unix   int64 // committer time of Commit, to tell which of two commits is newer
}

// Series is one measured quantity for one setup in one file, oldest value
// first. A snapshot series (one value per setup in the file, rewritten as
// results change) has a point per change. A log series (a file that appends
// rows, each with its own timestamp) has a point per timestamp.
type Series struct {
	Repo, File string
	Group      string
	Key        string
	Label      string
	Metric     string
	Unit       string
	Terms      []string
	Where      string // JSON path
	Log        bool
	Points     []Point // at most keepPoints, the newest
	N          int     // points before the cut
	Truncated  bool    // the history window ended before the file's first commit
	// FileSha and FileDate are the newest commit that touched the file: the
	// state of the file whose lines the points cite.
	FileSha  string
	FileDate string
}

const keepPoints = 40

// Cur is the newest point.
func (s *Series) Cur() Point { return s.Points[len(s.Points)-1] }

// Recurs reports whether the series has held two different values: a result
// that a newer commit replaced.
func (s *Series) Recurs() bool {
	for _, p := range s.Points[1:] {
		if p.Value != s.Points[0].Value {
			return true
		}
	}
	return false
}

// Prev is the newest point whose value differs from the current one, and
// false when there is none.
func (s *Series) Prev() (Point, bool) {
	cur := s.Cur().Value
	for i := len(s.Points) - 2; i >= 0; i-- {
		if s.Points[i].Value != cur {
			return s.Points[i], true
		}
	}
	return Point{}, false
}

// Spread is the range of the last up to three values.
func (s *Series) Spread() (lo, hi float64, n int) {
	pts := s.Points
	if len(pts) > 3 {
		pts = pts[len(pts)-3:]
	}
	lo, hi = pts[0].Value, pts[0].Value
	for _, p := range pts {
		if p.Value < lo {
			lo = p.Value
		}
		if p.Value > hi {
			hi = p.Value
		}
	}
	return lo, hi, len(pts)
}

type logRow struct {
	o      Obs
	commit Commit
}

// BuildSeries turns the versions of one file, oldest first, into series.
//
// A file is a log when one version holds two rows for one setup. A log's
// rows are its own record of when they were measured, so each needs a
// timestamp: rows without one are samples of a single run (per-request
// timings) and make no series. Rows that share a timestamp are repeats of one
// setup measured together and become one point, their median, which says how
// many rows it came from. A snapshot file has one value per setup, rewritten
// as results change; each change is a point, attributed to the commit that
// wrote it.
func BuildSeries(repo, file string, vs []Version, truncated bool) []*Series {
	log := false
	for _, v := range vs {
		seen := map[string]bool{}
		for _, o := range v.Obs {
			if seen[o.Group+"\x00"+o.Key] {
				log = true
			}
			seen[o.Group+"\x00"+o.Key] = true
		}
	}
	// what the newest version holds: the truth about what is current
	inLast := map[string]bool{}
	rowInLast := map[string]bool{}
	if len(vs) > 0 {
		for _, o := range vs[len(vs)-1].Obs {
			ck := o.Group + "\x00" + o.Key
			inLast[ck] = true
			rowInLast[ck+"\x00"+o.Row] = true
		}
	}
	byKey := map[string]*Series{}
	rows := map[string][]logRow{}
	seenRow := map[string]map[string]int{} // key -> row text -> index in rows
	var order []string
	for _, v := range vs {
		for _, o := range v.Obs {
			ck := o.Group + "\x00" + o.Key
			s := byKey[ck]
			if s == nil {
				s = &Series{Repo: repo, File: file, Key: o.Key, Log: log, Truncated: truncated}
				byKey[ck] = s
				seenRow[ck] = map[string]int{}
				order = append(order, ck)
			}
			// the newest version's description wins
			s.Group, s.Label, s.Metric, s.Unit, s.Terms, s.Where = o.Group, o.Label, o.Metric, o.Unit, o.Terms, o.Where
			if log {
				if i, ok := seenRow[ck][o.Row]; ok {
					rows[ck][i].o.Line = o.Line // the row moved; keep the newest line
					continue
				}
				seenRow[ck][o.Row] = len(rows[ck])
				rows[ck] = append(rows[ck], logRow{o, v.Commit})
				continue
			}
			p := Point{Value: o.Value, Raw: o.Raw, Dec: o.Dec, Commit: v.Commit.Short(), Date: v.Commit.Date, Unix: v.Commit.Unix, At: o.At, Code: o.Code, Line: o.Line, Basis: o.Basis, N: 1, Runs: o.Runs}
			if n := len(s.Points); n > 0 && s.Points[n-1].Value == o.Value {
				s.Points[n-1].Line = o.Line
				s.Points[n-1].Raw, s.Points[n-1].Dec = o.Raw, o.Dec
				continue
			}
			s.Points = append(s.Points, p)
		}
	}
	var out []*Series
	for _, ck := range order {
		s := byKey[ck]
		if !inLast[ck] {
			continue
		}
		if log {
			var kept []logRow
			for _, r := range rows[ck] {
				if rowInLast[ck+"\x00"+r.o.Row] {
					kept = append(kept, r)
				}
			}
			s.Points = logPoints(kept)
			if len(s.Points) == 0 {
				continue
			}
		}
		s.N = len(s.Points)
		if len(s.Points) > keepPoints {
			s.Points = s.Points[len(s.Points)-keepPoints:]
		}
		out = append(out, s)
	}
	return out
}

// hasClock: the timestamp says a time of day, not only a date.
func hasClock(at string) bool { return len(at) > 10 }

// atKey makes timestamps in different spellings sort in time order.
func atKey(s string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000000000")
		}
	}
	return s
}

// logPoints groups a setup's rows by run and summarises each group.
//
// A row without a timestamp voids the series: those are samples of one run
// (per-request timings), not results measured again. Rows are repeats of one
// run only when they share a full timestamp (a bare date covers many runs) and
// the code they say they measured, and say the same thing in every column but
// their numbers and a run index. Rows that share a run but differ elsewhere
// (per-item scores that carry their run's timestamp) are not repeats of
// anything, and no value can be told from them: the series is dropped. Repeats
// become one point, their median, counted.
func logPoints(rs []logRow) []Point {
	// A list of items (one row per prompt, per case) is told by the rows that
	// share a run's timestamp and differ in what they say besides their
	// numbers: those make no series (below). Rows that each have a time of
	// their own are not told from a log by how much they differ: a perf log
	// whose rows carry a unique id or note column differs from row to row too,
	// and is one quantity measured again.
	type runKey struct{ at, code string }
	byRun := map[runKey][]logRow{}
	var keys []runKey
	for _, r := range rs {
		if r.o.At == "" {
			return nil
		}
		k := runKey{atKey(r.o.At), r.o.Code}
		if _, ok := byRun[k]; !ok {
			keys = append(keys, k)
		}
		byRun[k] = append(byRun[k], r)
	}
	// Runs are in time order; two runs with the same timestamp (a bare date
	// covers many) stay in the order the file has them, the later row being the
	// newer, not in the order of their code strings.
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].at < keys[j].at })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		g := byRun[k]
		last := g[len(g)-1]
		p := Point{Commit: last.commit.Short(), Date: last.commit.Date, Unix: last.commit.Unix, At: last.o.At, Code: last.o.Code, Line: last.o.Line, Basis: last.o.Basis, N: len(g), Runs: last.o.Runs}
		if len(g) == 1 {
			p.Value, p.Raw, p.Dec = last.o.Value, last.o.Raw, last.o.Dec
			out = append(out, p)
			continue
		}
		if !hasClock(last.o.At) {
			return nil // many rows on one day: not provably one run
		}
		vals := make([]float64, len(g))
		for i, r := range g {
			if r.o.Sig != last.o.Sig {
				return nil // same run, different rows: items, not repeats
			}
			vals[i] = r.o.Value
			if r.o.Dec > p.Dec {
				p.Dec = r.o.Dec
			}
		}
		sort.Float64s(vals)
		m := vals[len(vals)/2]
		if len(vals)%2 == 0 {
			m = (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
		}
		p.Value = roundTo(m, p.Dec+1)
		p.Raw = strconv.FormatFloat(p.Value, 'f', -1, 64)
		p.Basis = "median"
		out = append(out, p)
	}
	return out
}

// Stats counts what a repo's read looked at, for the survey and the report.
type Stats struct {
	Candidates int            // files touched at least twice in the newest commits
	Files      map[string]int // files with measured numbers, by kind
	Series     int
	Recurring  int // series that held two different values
	Groups     int
	Facts      int
	Flags      int
	Quoted     int // series a saved fact quotes (the current or a replaced value)
	Quoting    int // saved facts that quote one
}

// File records one file with measured numbers.
func (s *Stats) File(kind string) {
	if s.Files == nil {
		s.Files = map[string]int{}
	}
	s.Files[kind]++
}

// String is a one-line summary.
func (s Stats) String() string {
	var ks []string
	for k := range s.Files {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, k+" "+itoa(s.Files[k]))
	}
	return "files with results: " + strings.Join(parts, ", ") + "; series " + itoa(s.Series) + ", recurring " + itoa(s.Recurring) + ", quoted by " + itoa(s.Quoting) + " saved fact(s) in " + itoa(s.Quoted) + " series, groups " + itoa(s.Groups) + ", facts " + itoa(s.Facts) + ", flags " + itoa(s.Flags)
}
