package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/search"
)

// Outcomes (SPEC.md, "Learning from outcomes"): what the marks and the
// judge said of a delivered line moves its delivery in the scope it was
// delivered in (the session's repository, or none). Deterministic and
// small:
//
//   - used: an agent marked it used, or the judge labeled it relevant;
//   - bad: an agent marked it not relevant or wrong, or the judge labeled
//     it not relevant;
//   - a line with more bad than used is demoted there: a prompt line needs
//     twice its score (outcomeDemote) and a start line sorts after the rest
//     of its tier, so the ceiling drops it first;
//   - a line with outcomeStop or more bad and no used is stopped there: no
//     hook delivers it in that scope; search and fact still show it;
//   - a line with more used than bad is promoted: a prompt line's score is
//     outcomePromote times its own, a start line sorts first in its tier;
//   - the owner's own lines are never moved, and a deny reads only the
//     wrong verdicts (a prohibition is "not relevant" to most turns).
//
// The outcomes are an append-only file of their own in the delivery log's
// directory (outcomes/verdicts.jsonl, one line a verdict; not a day file of
// the log), read through a summary rebuilt when the file grows
// (outcomes/summary.json).

const (
	outcomeStop    = 3
	outcomeDemote  = 2.0
	outcomePromote = 1.5
	// outcomeWindow: older verdicts count no more.
	outcomeWindow = 180 * 24 * time.Hour
)

// outcomeLine is one verdict.
type outcomeLine struct {
	TS      string `json:"ts"`
	In      string `json:"in"` // the scope: the session's repository when it was delivered, "" for none
	Fact    string `json:"fact"`
	Verdict string `json:"v"`   // used, not_relevant, wrong
	Src     string `json:"src"` // mark, judge
	Block   string `json:"block,omitempty"`
}

// outcomeTally is a line's count in a scope.
type outcomeTally struct {
	Used  int `json:"u"`
	Bad   int `json:"b"`
	Wrong int `json:"w,omitempty"` // of Bad, the wrong verdicts
}

// outcomeClass of a tally.
const (
	outcomeNeutral = iota
	outcomePromoted
	outcomeDemoted
	outcomeStopped
)

func (t outcomeTally) class() int {
	switch {
	case t.Used == 0 && t.Bad >= outcomeStop:
		return outcomeStopped
	case t.Bad > t.Used:
		return outcomeDemoted
	case t.Used > t.Bad:
		return outcomePromoted
	}
	return outcomeNeutral
}

func outcomesDir() string  { return filepath.Join(deliveryLogDir(), "outcomes") }
func outcomesPath() string { return filepath.Join(outcomesDir(), "verdicts.jsonl") }

// recordOutcomes appends verdicts. A failure loses them: the mark itself
// is in the delivery log.
func recordOutcomes(lines []outcomeLine) {
	if len(lines) == 0 {
		return
	}
	if os.MkdirAll(outcomesDir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(outcomesPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	f.WriteString(b.String()) // one write: lines of two writers do not interleave
}

// outcomeSum is the summary: the file's size it was built at, and each
// scope's tallies.
type outcomeSum struct {
	Size   int64                              `json:"size"`
	Built  string                             `json:"built"`
	Scopes map[string]map[string]outcomeTally `json:"scopes"`
}

// loadOutcomes is the tallies by scope and fact, from the summary when it
// matches the file's size (and was built within a day: the window moves),
// else rebuilt from the file. No file: none.
func loadOutcomes(now time.Time) map[string]map[string]outcomeTally {
	fi, err := os.Stat(outcomesPath())
	if err != nil || fi.Size() == 0 {
		return nil
	}
	sumPath := filepath.Join(outcomesDir(), "summary.json")
	if b, err := os.ReadFile(sumPath); err == nil {
		var s outcomeSum
		if json.Unmarshal(b, &s) == nil && s.Size == fi.Size() {
			if t, err := time.Parse(time.RFC3339, s.Built); err == nil && now.Sub(t) < 24*time.Hour {
				return s.Scopes
			}
		}
	}
	s := outcomeSum{Size: fi.Size(), Built: now.UTC().Format(time.RFC3339), Scopes: tallyOutcomes(now)}
	if b, err := json.Marshal(s); err == nil {
		tmp := sumPath + ".tmp" + shortHash(now.String())
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, sumPath)
		}
	}
	return s.Scopes
}

// tallyOutcomes reads the file: per scope and fact, used and bad in the
// window. A mark and the judge's label of the same line in the same block
// both count: they are two readings.
func tallyOutcomes(now time.Time) map[string]map[string]outcomeTally {
	f, err := os.Open(outcomesPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]map[string]outcomeTally{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var l outcomeLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Fact == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, l.TS); err != nil || now.Sub(t) > outcomeWindow {
			continue
		}
		in := strings.ToLower(l.In)
		if out[in] == nil {
			out[in] = map[string]outcomeTally{}
		}
		t := out[in][l.Fact]
		switch l.Verdict {
		case "used":
			t.Used++
		case "not_relevant":
			t.Bad++
		case "wrong":
			t.Bad++
			t.Wrong++
		default:
			continue
		}
		out[in][l.Fact] = t
	}
	return out
}

// outcomeScope is the scope a block's outcomes are kept under: its first
// repository, or "".
func outcomeScope(repos []string) string {
	if len(repos) == 0 {
		return ""
	}
	return strings.ToLower(repos[0])
}

// outcomes is the hooks' view of one scope's tallies, loaded once a hook.
type outcomes map[string]outcomeTally

func outcomesFor(scope string) outcomes {
	return outcomes(loadOutcomes(time.Now())[strings.ToLower(scope)])
}

// classOf is a line's class; the owner's own lines (OwnerLine) are never
// moved: an agent's marks and a model's labels do not turn off a rule of
// theirs, which is "not relevant" to most turns by nature.
func (o outcomes) classOf(h search.Hit) int {
	if h.OwnerLine() {
		return outcomeNeutral
	}
	return o[h.Name].class()
}

// class is a line's class by name, for a line no hit stands for.
func (o outcomes) class(name string) int { return o[name].class() }

// withoutStopped is hits less the lines stopped in this scope, logged
// dropped "outcome".
func (o outcomes) withoutStopped(hits []search.Hit) []search.Hit {
	if len(o) == 0 {
		return hits
	}
	var keep []search.Hit
	for _, h := range hits {
		if o.classOf(h) == outcomeStopped {
			continue
		}
		keep = append(keep, h)
	}
	logDropped(hits, keep, "outcome")
	return keep
}

// withoutDemoted is hits less the stopped and the demoted lines: for the
// lines no score chose (metadata and action lines), where a demotion has
// nothing to weigh against.
func (o outcomes) withoutDemoted(hits []search.Hit) []search.Hit {
	if len(o) == 0 {
		return hits
	}
	var keep []search.Hit
	for _, h := range hits {
		if c := o.classOf(h); c == outcomeStopped || c == outcomeDemoted {
			continue
		}
		keep = append(keep, h)
	}
	logDropped(hits, keep, "outcome")
	return keep
}

// rescored is a prompt pool with the stopped lines out and the scores of
// the demoted and the promoted lines moved, best first again.
func (o outcomes) rescored(hits []search.Hit) []search.Hit {
	if len(o) == 0 {
		return hits
	}
	out := o.withoutStopped(hits)
	moved := false
	for i, h := range out {
		switch o.classOf(h) {
		case outcomeDemoted:
			out[i].Score /= outcomeDemote
			moved = true
		case outcomePromoted:
			out[i].Score *= outcomePromote
			moved = true
		}
	}
	if moved {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	}
	return out
}

// ordered is a start pool, already in tier order, with the stopped lines
// out and, inside each tier, the promoted lines first and the demoted last.
func (o outcomes) ordered(hits []search.Hit) []search.Hit {
	if len(o) == 0 {
		return hits
	}
	out := o.withoutStopped(hits)
	rank := func(h search.Hit) int {
		switch o.classOf(h) {
		case outcomePromoted:
			return 0
		case outcomeDemoted:
			return 2
		}
		return 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := tier(out[i]), tier(out[j]); a != b {
			return a < b
		}
		return rank(out[i]) < rank(out[j])
	})
	return out
}
