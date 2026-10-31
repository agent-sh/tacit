package derive

import (
	"path"
	"slices"
	"strings"
)

// Every number below was picked from backtests on real repositories with
// eval/derived/backtest.py; the output is in eval/VALUE.md. Change a number by
// re-running that script, not by feel.
const (
	// Window is how many of the most recent code-touching, non-merge commits
	// are read. A count of commits, not days, so it follows the repo's pace
	// and stays a function of the commit: 400 code commits are about a week of
	// a busy repo and the whole history of a small one.
	//
	// Backtest: take the K commits before a split point, rank files by touch
	// count, and see how many of the top 10 are touched again in the next 100
	// commits (mean over split points and repos), for K of 50, 100, 200 and
	// 400. The window barely matters across that range. 400 has the best mean,
	// is the largest that every repo in the backtest can still be tested at,
	// and keeps the list full on a wide repo: there K=50 finds only a few
	// files, so its higher score is a score over fewer files.
	Window = 400

	// MaxChangeset: a commit that touches more code files than this is a mass
	// edit (rename, format sweep, vendor drop), not work on a file, and is left
	// out of every count. 30 is about the 99th percentile of code files per
	// commit on the repos measured (the 97th on the widest), so a few percent
	// of commits are over it. At 60, the co-change rule applications grew
	// several times and their precision swung widely: a few bulk commits were
	// deciding the result.
	MaxChangeset = 30

	// MinTouches: a hot spot needs 3 commits in the window. Two commits are
	// a creation and its fix.
	MinTouches = 3

	// TopFiles: the backtest scored the top 10, and ten lines with their
	// partners is about 2 KB, a screen.
	TopFiles = 10

	// MinCoChanges and MinConfidence: file g is listed as a partner of hot
	// file f when they changed in the same commit at least 3 times and in at
	// least half of f's commits. Backtest of the rule f -> g (learn on the
	// window, test whether g is in the same commit as f over the next 150
	// commits), at a changeset limit of 30: precision at 3 and 0.5 stayed
	// around the stated "half of the time" out of sample on every repo
	// measured. 5 changes instead of 3 raised precision a few points and cut
	// many of the rules. At a confidence of 0.3 precision fell.
	MinCoChanges  = 3
	MinConfidence = 0.5

	// PartnersPerFile caps the partner list of one hot file at three. Most
	// hot-spot lines in the backtest had partners, spread about evenly over
	// one, two and the cap. Three keeps a line under about 300 characters.
	PartnersPerFile = 3

	// MaxMapEntries caps the module map. A map that fills all 30 entries is
	// about 4 KB, about what one fact costs in an MCP search result.
	MaxMapEntries = 30

	// MaxMapDepth: the module map opens a directory only above this depth, so
	// entries go three levels down at most (crates/core-engine/src/).
	MaxMapDepth = 3
)

// Partner is a file that tends to change in the same commit as a hot spot.
type Partner struct {
	Path string
	Co   int // commits containing both
}

// Hot is one hot spot.
type Hot struct {
	Path    string
	Touches int
	Last    string // date of the newest window commit touching it
	With    []Partner
}

// HotSpots ranks the window's files by the commits that touched them.
// Bulk commits (more than MaxChangeset code files) are not counted. Order:
// touches, newest touch, path.
func HotSpots(win []Commit) []Hot {
	type acc struct {
		n    int
		last string
	}
	files := map[string]*acc{}
	for _, c := range win {
		if c.Total > MaxChangeset {
			continue
		}
		for _, f := range c.Files {
			a := files[f]
			if a == nil {
				a = &acc{last: c.Date} // newest first: the first sighting is the newest
				files[f] = a
			}
			a.n++
		}
	}
	var ranked []string
	for f, a := range files {
		if a.n >= MinTouches {
			ranked = append(ranked, f)
		}
	}
	slices.SortFunc(ranked, func(x, y string) int {
		a, b := files[x], files[y]
		switch {
		case a.n != b.n:
			return b.n - a.n
		case a.last != b.last:
			return strings.Compare(b.last, a.last)
		}
		return strings.Compare(x, y)
	})
	if len(ranked) > TopFiles {
		ranked = ranked[:TopFiles]
	}
	hot := make([]Hot, len(ranked))
	idx := map[string]int{}
	for i, f := range ranked {
		hot[i] = Hot{Path: f, Touches: files[f].n, Last: files[f].last}
		idx[f] = i
	}
	co := make([]map[string]int, len(ranked))
	for _, c := range win {
		if c.Total > MaxChangeset {
			continue
		}
		for _, f := range c.Files {
			i, ok := idx[f]
			if !ok {
				continue
			}
			if co[i] == nil {
				co[i] = map[string]int{}
			}
			for _, g := range c.Files {
				if g != f {
					co[i][g]++
				}
			}
		}
	}
	for i := range hot {
		var ps []Partner
		for g, n := range co[i] {
			// n/touches >= MinConfidence, in integers
			if n >= MinCoChanges && float64(n) >= MinConfidence*float64(hot[i].Touches) {
				ps = append(ps, Partner{Path: g, Co: n})
			}
		}
		slices.SortFunc(ps, func(a, b Partner) int {
			if a.Co != b.Co {
				return b.Co - a.Co
			}
			return strings.Compare(a.Path, b.Path)
		})
		if len(ps) > PartnersPerFile {
			ps = ps[:PartnersPerFile]
		}
		hot[i].With = ps
	}
	return hot
}

// ExtCount is a file count for one extension.
type ExtCount struct {
	Ext string
	N   int
}

// Entry is one line of the module map.
type Entry struct {
	Path    string // directory with a trailing slash
	Own     bool   // only the files directly in Path (its subdirectories are listed on their own)
	Files   int
	Code    int
	Exts    []ExtCount // largest first, at most 3, each at least a tenth of the files
	Commits int        // window commits (bulk excluded) that touched code in it
	Last    string     // date of the newest commit that touched anything in it; filled by the caller
}

// Map is the module map of one commit.
type Map struct {
	Entries   []Entry
	RootFiles []string // tracked files at the top level
	More      int      // entries dropped by MaxMapEntries
	MoreFiles int      // files in them
}

type dirNode struct {
	files, code, direct, directCode int
	exts                            map[string]int
	directExts                      map[string]int
	kids                            map[string]bool
}

// ModuleMap lists the directories of the tree. It starts with one entry per
// top-level directory and then opens the code-rich directory with the most
// code files into its subdirectories (crates/ in a Cargo workspace, client/ in
// a repo whose whole tree sits under one folder), again and again, until the
// next opening would pass MaxMapEntries or MaxMapDepth. One line for "all the
// code" says nothing, and a fixed rule such as "open the biggest" would open
// the wrong thing in some repos: a research/ folder can hold thousands of
// scripts next to a hundred times as many run outputs, so a directory is
// opened only if at least half of its own files are code. Entries are sorted by path. If there
// are more top-level directories than MaxMapEntries, the smallest (by code
// files, then files) are dropped and counted in More.
func ModuleMap(tree []TreeFile, win []Commit) Map {
	var entries []Entry
	var rootFiles []string
	var more, moreFiles int
	nodes := map[string]*dirNode{"": {exts: map[string]int{}, directExts: map[string]int{}, kids: map[string]bool{}}}
	get := func(d string) *dirNode {
		n := nodes[d]
		if n == nil {
			n = &dirNode{exts: map[string]int{}, directExts: map[string]int{}, kids: map[string]bool{}}
			nodes[d] = n
		}
		return n
	}
	for _, f := range tree {
		dir := path.Dir(f.Path)
		if dir == "." {
			dir = ""
			rootFiles = append(rootFiles, f.Path)
		}
		ext := path.Ext(f.Path)
		if ext == "" {
			ext = "(none)"
		}
		code := IsCode(f.Path)
		// every ancestor, root included, counts the file
		parts := []string{}
		if dir != "" {
			parts = strings.Split(dir, "/")
		}
		for i := 0; i <= len(parts); i++ {
			anc := strings.Join(parts[:i], "/")
			n := get(anc)
			n.files++
			n.exts[ext]++
			if code {
				n.code++
			}
			if i > 0 {
				get(strings.Join(parts[:i-1], "/")).kids[anc] = true
			}
			if i == len(parts) {
				n.direct++
				n.directExts[ext]++
				if code {
					n.directCode++
				}
			}
		}
	}
	// window activity per directory: commits that touched code under it
	commits := map[string]int{}
	for _, c := range win {
		if c.Total > MaxChangeset {
			continue
		}
		seen := map[string]bool{}
		for _, f := range c.Files {
			parts := strings.Split(path.Dir(f), "/")
			for i := 1; i <= len(parts); i++ {
				if parts[0] == "." {
					break
				}
				seen[strings.Join(parts[:i], "/")] = true
			}
		}
		for d := range seen {
			commits[d]++
		}
	}
	kids := func(d string) []string {
		var ks []string
		for k := range nodes[d].kids {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return ks
	}
	mk := func(d string, own bool) Entry {
		n := nodes[d]
		e := Entry{Path: d + "/", Own: own, Files: n.files, Code: n.code, Commits: commits[d]}
		ex := n.exts
		if own {
			e.Files, e.Code = n.direct, n.directCode
			ex = n.directExts
			e.Commits = 0
		}
		e.Exts = topExts(ex, e.Files)
		return e
	}
	// Start from the top-level directories, then keep opening the code-rich
	// directory with the most code files into its subdirectories until the
	// map would pass MaxMapEntries. A directory is opened only if it is
	// mostly code itself, most of that code sits in its subdirectories (tools/
	// with hundreds of scripts of its own and a few fixture folders stays one line),
	// it has two or more subdirectories and it is above MaxMapDepth.
	type slot struct {
		dir string
		own bool
	}
	var cur []slot
	for _, k := range kids("") {
		cur = append(cur, slot{dir: k})
	}
	for {
		best := -1
		for i, sl := range cur {
			if sl.own {
				continue
			}
			n := nodes[sl.dir]
			if strings.Count(sl.dir, "/")+1 >= MaxMapDepth || len(kids(sl.dir)) < 2 || n.code*2 < n.files || n.directCode*2 > n.code {
				continue
			}
			if best < 0 || n.code > nodes[cur[best].dir].code {
				best = i
			}
		}
		if best < 0 {
			break
		}
		d := cur[best].dir
		ks := kids(d)
		grow := len(ks) - 1
		if nodes[d].directCode > 0 {
			grow++
		}
		if len(cur)+grow > MaxMapEntries {
			break
		}
		var repl []slot
		if nodes[d].directCode > 0 {
			repl = append(repl, slot{dir: d, own: true})
		}
		for _, k := range ks {
			repl = append(repl, slot{dir: k})
		}
		cur = slices.Concat(cur[:best], repl, cur[best+1:])
	}
	for _, sl := range cur {
		entries = append(entries, mk(sl.dir, sl.own))
	}
	if len(entries) > MaxMapEntries { // more top-level directories than the cap
		order := slices.Clone(entries)
		slices.SortStableFunc(order, func(a, b Entry) int {
			if a.Code != b.Code {
				return b.Code - a.Code
			}
			if a.Files != b.Files {
				return b.Files - a.Files
			}
			return strings.Compare(a.Path, b.Path)
		})
		drop := map[string]bool{}
		for _, e := range order[MaxMapEntries:] {
			drop[e.Path] = true
			more++
			moreFiles += e.Files
		}
		kept := entries[:0:0]
		for _, e := range entries {
			if !drop[e.Path] {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	slices.Sort(rootFiles)
	return Map{Entries: entries, RootFiles: rootFiles, More: more, MoreFiles: moreFiles}
}

// topExts: at most three extensions, each holding at least a tenth of the
// files, largest first (ties by name); always at least the largest one.
func topExts(m map[string]int, total int) []ExtCount {
	var xs []ExtCount
	for e, n := range m {
		xs = append(xs, ExtCount{e, n})
	}
	slices.SortFunc(xs, func(a, b ExtCount) int {
		if a.N != b.N {
			return b.N - a.N
		}
		return strings.Compare(a.Ext, b.Ext)
	})
	var out []ExtCount
	for i, x := range xs {
		if i >= 3 || (i > 0 && x.N*10 < total) {
			break
		}
		out = append(out, x)
	}
	return out
}
