package statement

import (
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/check"
)

// State is what one run of the step knows.
type State struct {
	// Statements are the live statements, Promoted the live facts code made
	// of statements (the log's promoted lines whose fact is live), by name.
	Statements map[string]Item
	Promoted   map[string]Item
	// Log is the statements log as it stands, this run's judgements in it.
	Log []Entry
	// Checks are code's checks of each live statement's judged claims.
	Checks map[string][]check.Result
}

// Action kinds.
const (
	ActPromote   = "promote"   // write a decision, preference or rule
	ActSupersede = "supersede" // an older item is replaced by a newer one
	ActFlag      = "flag"      // a check disagrees with a statement
	ActDiffers   = "differs"   // a statement differs from a decision or rule that stands
)

// Action is one thing the step does.
type Action struct {
	Kind string
	// A promotion: its level, how it was earned, the statement whose words
	// it carries (the newest), the items it rests on (From), their distinct
	// sessions, the live items it replaces, its claims and the checks that
	// agreed (by check).
	Level, By string
	Words     Item
	From      []string
	Sessions  []string
	Replaces  []string
	Claims    []check.Claim
	Checks    []check.Result
	// Name is the item replaced (supersede) or the statement (flag,
	// differs); Into the newer item (supersede) or the decision it differs
	// from (differs).
	Name, Into string
	Hash       string
}

// Judgements are the latest judged line of each statement in the log.
func Judgements(log []Entry) map[string]Entry {
	out := map[string]Entry{}
	for _, e := range log {
		if e.Kind == LogJudged {
			out[e.Name] = e
		}
	}
	return out
}

// Plan decides from st what the step does (the package doc's rules). It
// changes nothing; the caller writes the actions.
func Plan(st State) []Action {
	judged := Judgements(st.Log)
	// Items whose point a newer statement changed are out of every count.
	changed := map[string]bool{}
	for _, e := range st.Log {
		if e.Kind == LogSuperseded && e.By == Changes {
			changed[e.Name] = true
		}
	}
	var acts []Action
	// A newer statement that changes an earlier live statement's point
	// replaces it.
	for _, name := range sortedItems(st.Statements) {
		j := judged[name]
		for _, r := range j.Same {
			if r.Relation != Changes {
				continue
			}
			if old, ok := st.Statements[r.Name]; ok && !changed[r.Name] && older(old, st.Statements[name]) {
				changed[r.Name] = true
				acts = append(acts, Action{Kind: ActSupersede, Name: r.Name, Into: name, By: Changes})
			}
		}
	}
	// Groups of one point: statements the model said make the same point,
	// and every promoted fact with what it rests on.
	uf := newUnion()
	for _, e := range st.Log {
		switch e.Kind {
		case LogJudged:
			uf.add(e.Name)
			if changed[e.Name] {
				continue
			}
			for _, r := range e.Same {
				if r.Relation == Same && !changed[r.Name] {
					uf.union(e.Name, r.Name)
				}
			}
		case LogPromoted:
			uf.add(e.Fact)
			for _, f := range e.From {
				if !changed[f] {
					uf.union(e.Fact, f)
				}
			}
		}
	}
	for n := range st.Statements {
		uf.add(n)
	}
	sessionsOf := map[string][]string{} // promoted fact -> its log sessions
	for _, e := range st.Log {
		if e.Kind == LogPromoted {
			sessionsOf[e.Fact] = e.Sessions
		}
	}
	groups := map[string][]string{}
	for _, n := range uf.names() {
		groups[uf.find(n)] = append(groups[uf.find(n)], n)
	}
	promotedNow := map[string]bool{} // statements a promotion of this run takes
	var roots []string
	for r := range groups {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	for _, root := range roots {
		var live []Item
		var prom *Item
		sessions := map[string]bool{}
		for _, n := range groups[root] {
			if s, ok := st.Statements[n]; ok && !changed[n] {
				live = append(live, s)
				if s.From != "" {
					sessions[s.From] = true
				}
			}
			if p, ok := st.Promoted[n]; ok {
				if prom == nil || p.Day > prom.Day || p.Day == prom.Day && p.Name > prom.Name {
					pp := p
					prom = &pp
				}
			}
			for _, x := range sessionsOf[n] {
				sessions[x] = true
			}
		}
		if len(live) == 0 {
			continue
		}
		sort.Slice(live, func(i, j int) bool { return older(live[i], live[j]) })
		newest := live[len(live)-1]
		says := judged[newest.Name].Says
		n := len(sessions)
		ss := sortedSet(sessions)
		var names []string
		for _, s := range live {
			names = append(names, s.Name)
		}
		promote := func(level, by string, checks []check.Result) {
			a := Action{Kind: ActPromote, Level: level, By: by, Words: newest, Sessions: ss, Checks: checks}
			a.From = append(a.From, names...)
			a.Replaces = append(a.Replaces, names...)
			if prom != nil {
				a.From = append(a.From, prom.Name)
				a.Replaces = append(a.Replaces, prom.Name)
			}
			for _, s := range live {
				a.Claims = append(a.Claims, judged[s.Name].Claims...)
				promotedNow[s.Name] = true
			}
			// What the group's statements changed: an older decision or rule
			// of the same point gives way to the newer confirmed one.
			for _, s := range live {
				for _, r := range judged[s.Name].Same {
					if r.Relation != Changes {
						continue
					}
					if p, ok := st.Promoted[r.Name]; ok {
						a.Replaces = appendNew(a.Replaces, p.Name)
					} else if w := winnerOf(uf, groups, st.Promoted, r.Name); w != "" {
						a.Replaces = appendNew(a.Replaces, w)
					}
				}
			}
			acts = append(acts, a)
		}
		switch {
		case prom != nil && prom.Kind == LevelPreference && says == Instruction && n >= 3:
			promote(LevelRule, BySessions, nil)
		case prom != nil:
			// Said again after it was decided: the statement is that point
			// once more, and the promoted fact stands for it.
			for _, s := range live {
				acts = append(acts, Action{Kind: ActSupersede, Name: s.Name, Into: prom.Name, By: Same})
				promotedNow[s.Name] = true
			}
		case says == Choice && n >= 2:
			promote(LevelDecision, BySessions, nil)
		case says == Instruction && n >= 3:
			promote(LevelRule, BySessions, nil)
		case says == Instruction && n == 2:
			promote(LevelPreference, BySessions, nil)
		default:
			for i := len(live) - 1; i >= 0; i-- {
				if rs := st.Checks[live[i].Name]; Verdict(rs) == check.Holds {
					newest = live[i]
					promote(LevelDecision, ByCheck, agreeing(rs))
					break
				}
			}
		}
	}
	// Statements that stay statements: a check that disagrees flags them, and
	// one that differs from a decision that stands is an item for the owner.
	flagged := map[string]bool{}
	differs := map[string]bool{}
	for _, e := range st.Log {
		switch e.Kind {
		case LogFlagged:
			flagged[e.Name+"\x00"+e.Hash] = true
		case LogDiffers:
			differs[e.Name+"\x00"+e.Into] = true
		}
	}
	for _, name := range sortedItems(st.Statements) {
		if promotedNow[name] || changed[name] {
			continue
		}
		if rs := st.Checks[name]; Verdict(rs) == check.Fails {
			fails := failing(rs)
			h := Hash(strings.Join(check.Lines(fails), "\n"))
			if !flagged[name+"\x00"+h] {
				acts = append(acts, Action{Kind: ActFlag, Name: name, Checks: fails, Hash: h})
			}
		}
		for _, r := range judged[name].Same {
			if r.Relation != Changes {
				continue
			}
			d := ""
			if _, ok := st.Promoted[r.Name]; ok {
				d = r.Name
			} else {
				d = winnerOf(uf, groups, st.Promoted, r.Name)
			}
			if d != "" && !differs[name+"\x00"+d] {
				differs[name+"\x00"+d] = true
				acts = append(acts, Action{Kind: ActDiffers, Name: name, Into: d})
			}
		}
	}
	return acts
}

// winnerOf is the live promoted fact of name's group, "" for none.
func winnerOf(uf *union, groups map[string][]string, promoted map[string]Item, name string) string {
	if !uf.has(name) {
		return ""
	}
	best := ""
	for _, n := range groups[uf.find(name)] {
		if p, ok := promoted[n]; ok && (best == "" || p.Day > promoted[best].Day) {
			best = n
		}
	}
	return best
}

func agreeing(rs []check.Result) []check.Result {
	var out []check.Result
	for _, r := range rs {
		if r.Verdict == check.Holds {
			out = append(out, r)
		}
	}
	return out
}

func failing(rs []check.Result) []check.Result {
	var out []check.Result
	for _, r := range rs {
		if r.Verdict == check.Fails {
			out = append(out, r)
		}
	}
	return out
}

func older(a, b Item) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	return a.Name < b.Name
}

func sortedItems(m map[string]Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return older(m[out[i]], m[out[j]]) })
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendNew(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

type union struct{ parent map[string]string }

func newUnion() *union { return &union{parent: map[string]string{}} }

func (u *union) add(x string) {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
	}
}

func (u *union) has(x string) bool { _, ok := u.parent[x]; return ok }

func (u *union) find(x string) string {
	u.add(x)
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *union) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
}

func (u *union) names() []string {
	out := make([]string, 0, len(u.parent))
	for k := range u.parent {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
