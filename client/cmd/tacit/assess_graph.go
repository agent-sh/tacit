package main

// The ancestor graph (the owner's rule: each piece that is clearly related
// to another belongs under its ancestor), computed from all the evidence at
// once before any decision of a run, and again after each batch's records
// are written.
//
// Finding a folder's ancestor one candidate at a time made the answer
// depend on the order of the run: a folder related to another that had no
// record yet found no ancestor (a part decided before its whole), and one
// decided in a later run found it. The graph takes every project folder of
// the store and the catalogs: a folder with a record stands as its record
// says; one with none yet stands in as a project folder of its own (it will
// get a record), with the ancestors its own evidence finds. Chains resolve
// to their end (a part of b, b part of c: a is c's), and a cycle is broken by
// code: the member with the strongest identity of its own (a checkout, a
// remote of its own, the most own commits; then the name) keeps no ancestor
// in the cycle. The same evidence gives the same graph whatever the order the
// folders, facts or decisions come in.

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// ancestorGraph is every folder with no record's own ancestors (best first,
// a cycle broken) over the run's records.
type ancestorGraph struct {
	node   map[string]bool
	direct map[string][]assess.Ancestor
	// broken: a cycle's winner -> the other members, whose edges back to it
	// it does not take.
	broken map[string][]string
}

// arecs are the records the ancestor code reads: the run's records with a
// stand-in for each folder with no record (buildGraph), else the records.
func (a *assessor) arecs() map[string]*record.Record {
	if a.ancView != nil {
		return a.ancView
	}
	return a.recs
}

// ancestorUniverse are the project folders the graph is over: the catalogs'
// repositories and the store's project folders that hold facts, sorted.
func (a *assessor) ancestorUniverse() []string {
	set := map[string]bool{}
	for _, n := range a.catalog.Names() {
		set["projects/"+n] = true
	}
	for f := range a.projectFolders() {
		set[f] = true
	}
	return sortedKeysBool(set)
}

// repoCache is the run's one repoid cache: where a path is, read once (the
// graph reads every folder's facts again after each batch).
func (a *assessor) repoCache() *repoid.Cache {
	if a.pathCache == nil {
		a.pathCache = repoid.NewCache(a.home)
		a.pathCache.Mains = a.catalog.Mains()
	}
	return a.pathCache
}

// nodeCandidate is folder (no record) as first gathered this run.
func (a *assessor) nodeCandidate(folder string) assess.Candidate {
	if a.nodeBase == nil {
		a.nodeBase = map[string]assess.Candidate{}
	}
	c, ok := a.nodeBase[folder]
	if !ok {
		c = a.candidate(folder, "", nil)
		a.nodeBase[folder] = c
	}
	return c
}

// buildGraph computes the graph over the records as they are now.
func (a *assessor) buildGraph() {
	a.ancIx, a.families, a.namePool, a.ancMemo = nil, nil, nil, nil
	view := make(map[string]*record.Record, len(a.recs))
	for f, r := range a.recs {
		view[f] = r
	}
	g := &ancestorGraph{node: map[string]bool{}, direct: map[string][]assess.Ancestor{}, broken: map[string][]string{}}
	var nodes []string
	for _, f := range a.ancestorUniverse() {
		n := path.Base(f)
		if a.recs[f] != nil || record.RepoOwner(a.recs, n) != "" || !record.IsFolder(f) {
			continue
		}
		view[f] = &record.Record{Kind: record.KindFolder, Name: n, Status: record.StatusNeedsReview, Folder: f}
		g.node[f] = true
		nodes = append(nodes, f)
	}
	a.ancView = view
	for _, f := range nodes {
		c := a.nodeCandidate(f)
		a.directAncestors(&c)
		g.direct[f] = c.Ancestors
	}
	// Break the cycles, one at a time, in the order of their members' names.
	for {
		cyc := g.cycle(nodes)
		if cyc == nil {
			break
		}
		w := a.cycleWinner(cyc)
		in := map[string]bool{}
		for _, m := range cyc {
			in[m] = true
		}
		var keep []assess.Ancestor
		for _, an := range g.direct[w] {
			if !in[an.Folder] {
				keep = append(keep, an)
			}
		}
		g.direct[w] = keep
		for _, m := range cyc {
			if m != w {
				g.broken[w] = append(g.broken[w], m)
			}
		}
	}
	a.graph = g
}

// cycle is the first cycle of best-ancestor edges among nodes (members
// sorted), nil for none.
func (g *ancestorGraph) cycle(nodes []string) []string {
	state := map[string]int{} // 1: on the current walk, 2: done
	for _, start := range nodes {
		if state[start] != 0 {
			continue
		}
		var walk []string
		x := start
		for {
			if state[x] == 2 {
				break
			}
			if state[x] == 1 {
				i := 0
				for walk[i] != x {
					i++
				}
				cyc := append([]string(nil), walk[i:]...)
				sort.Strings(cyc)
				return cyc
			}
			state[x] = 1
			walk = append(walk, x)
			ds := g.direct[x]
			if !g.node[x] || len(ds) == 0 || !g.node[ds[0].Folder] {
				break
			}
			x = ds[0].Folder
		}
		for _, w := range walk {
			state[w] = 2
		}
	}
	return nil
}

// cycleWinner is the member of a cycle with the strongest identity of its
// own: a checkout on some machine, then a remote, then the most own commits;
// then the first name.
func (a *assessor) cycleWinner(cyc []string) string {
	type id struct {
		git, remote bool
		commits     int
	}
	of := func(f string) id {
		var x id
		for _, e := range a.nodeCandidate(f).Evidence {
			x.git = x.git || e.GitRoot
			x.remote = x.remote || len(e.Remotes) > 0
			if e.OwnCommits != nil {
				x.commits += *e.OwnCommits
			}
		}
		return x
	}
	best, bi := "", id{}
	for _, f := range cyc { // sorted: the first name wins a tie
		x := of(f)
		if best == "" || x.git && !bi.git || x.git == bi.git && (x.remote && !bi.remote || x.remote == bi.remote && x.commits > bi.commits) {
			best, bi = f, x
		}
	}
	return best
}

// final is where folder x's content goes as an ancestor: through the best
// ancestors of the folders with no record, to the first folder with a
// record or with no ancestor; via are the folders passed on the way.
func (g *ancestorGraph) final(x string) (string, []string) {
	var via []string
	seen := map[string]bool{}
	for g.node[x] && len(g.direct[x]) > 0 && !seen[x] {
		seen[x] = true
		via = append(via, x)
		x = g.direct[x][0].Folder
	}
	return x, via
}

// applyGraph fills c's ancestors from the graph (each resolved to its final
// ancestor), its independence evidence, and whether nothing says what it is.
func (a *assessor) applyGraph(c *assess.Candidate) {
	c.Ancestors, c.Independence, c.NoEvidence = nil, nil, false
	name, ok := strings.CutPrefix(c.Folder, "projects/")
	if !ok || c.Kind != "" {
		return
	}
	if a.graph == nil {
		a.buildGraph()
	}
	g := a.graph
	var direct []assess.Ancestor
	if g.node[c.Folder] {
		direct = g.direct[c.Folder]
	} else {
		d := *c
		a.directAncestors(&d)
		direct = d.Ancestors
	}
	by := map[string]*assess.Ancestor{}
	var order []string
	for _, an := range direct {
		f, via := g.final(an.Folder)
		if f == c.Folder || contains(via, c.Folder) {
			continue // presumed part of this folder: not its ancestor
		}
		why := append([]string(nil), an.Why...)
		if len(via) > 0 {
			var vs []string
			for _, v := range via {
				vs = append(vs, path.Base(v))
			}
			why = append([]string{fmt.Sprintf("by way of %s, which code presumes part of %s", strings.Join(vs, ", then "), path.Base(f))}, why...)
		}
		x := by[f]
		if x == nil {
			x = &assess.Ancestor{Name: path.Base(f), Folder: f, Score: an.Score}
			by[f] = x
			order = append(order, f)
		} else if an.Score > x.Score {
			x.Score = an.Score
		}
		x.Why = append(x.Why, why...)
		x.Sources = sortedUnion(x.Sources, an.Sources)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if by[order[i]].Score != by[order[j]].Score {
			return by[order[i]].Score > by[order[j]].Score
		}
		return order[i] < order[j]
	})
	for i, f := range order {
		if i == maxAncestors {
			break
		}
		c.Ancestors = append(c.Ancestors, *by[f])
	}
	ix := a.ancestorIx()
	live := a.liveOf(c.Folder)
	ind := a.independence(c, name, ix, live)
	if len(c.Ancestors) > 0 {
		c.Independence = ind
		return
	}
	// A checkout is evidence: git says whether it is real work and whose.
	// So is a repository code proposes from its evidence.
	c.NoEvidence = len(ind) == 0 && !c.GitRoot && !sessionEvidence(*c) && len(c.Real) == 0
}

// sessionEvidence reports evidence of a session: one that worked in its
// checkout or folder, or filed its notes or facts.
func sessionEvidence(c assess.Candidate) bool {
	for _, e := range c.Evidence {
		if e.WorkSessions > 0 || e.FactSessions > 0 || len(e.Sessions) > 0 {
			return true
		}
	}
	return false
}

// sortedUnion is a ∪ b, sorted.
func sortedUnion(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		set[x] = true
	}
	return sortedKeysBool(set)
}

// mentionedIn is the live project folder that holds the most of the facts
// of other project folders naming name as a word (paths left out: a word of
// a path names a directory, not the folder), with how many it holds and how
// many there are; "" unless it holds at least three, at least half of them,
// and four times or more its share of all their facts: a common word
// (worktree, used everywhere) is spread like the facts are, a name is not.
func (a *assessor) mentionedIn(self, name string) (string, int, int) {
	if len(name) < 4 {
		return "", 0, 0
	}
	if a.mentions == nil {
		a.mentions = map[string]map[string]int{}
		a.scopeFacts = map[string]int{}
		for _, f := range a.facts {
			if !liveFact(f) || !strings.HasPrefix(f.Meta.Scope, "projects/") {
				continue
			}
			a.scopeFacts[f.Meta.Scope]++
			seen := map[string]bool{}
			for _, w := range proseWords(f.Meta.Description + "\n" + f.Body) {
				if seen[w] {
					continue
				}
				seen[w] = true
				if a.mentions[w] == nil {
					a.mentions[w] = map[string]int{}
				}
				a.mentions[w][f.Meta.Scope]++
			}
		}
	}
	by := map[string]int{}
	total := 0
	for _, scope := range sortedKeysInt(a.mentions[name]) {
		if scope == self {
			continue
		}
		n := a.mentions[name][scope]
		total += n
		if w := a.ancestorFolder(path.Base(scope), self); w != "" {
			by[w] += n
		}
	}
	best, bn := "", 0
	for _, w := range sortedKeysInt(by) {
		if by[w] > bn {
			best, bn = w, by[w]
		}
	}
	if bn < 3 || 2*bn < total {
		return "", bn, total
	}
	// Its share of the facts: those of the folders it holds now.
	held, all := 0, 0
	for _, scope := range sortedKeysInt(a.scopeFacts) {
		if scope == self {
			continue
		}
		all += a.scopeFacts[scope]
		if a.ancestorFolder(path.Base(scope), self) == best {
			held += a.scopeFacts[scope]
		}
	}
	if bn*all < 4*held*total {
		return "", bn, total
	}
	return best, bn, total
}

// proseWords are text's words (letters, digits, dashes and underscores,
// lower case), leaving out every whitespace-separated piece with a slash in
// it (paths, URLs).
func proseWords(text string) []string {
	var out []string
	for _, field := range strings.Fields(strings.ToLower(text)) {
		if strings.Contains(field, "/") {
			continue
		}
		start := -1
		for i := 0; i <= len(field); i++ {
			if i < len(field) && wordChar(field[i]) {
				if start < 0 {
					start = i
				}
				continue
			}
			if start >= 0 {
				out = append(out, strings.Trim(field[start:i], "-_"))
				start = -1
			}
		}
	}
	return out
}

func wordChar(b byte) bool {
	return b == '-' || b == '_' || ('a' <= b && b <= 'z') || ('0' <= b && b <= '9')
}
