package main

// Find the ancestor (the owner, 2026-10-23: "when building an index of
// content each piece that clearly related to another should tried to find
// it ancestor"). Worktrees, clones, scratch and task folders are almost
// always part of something else, so the assessment presumes a folder that is
// clearly related to another belongs to it, and the step's job is to find
// which. Code finds the ancestor where it can be found reliably, and
// otherwise ranks the near evidence for the model:
//
//   - git: the folder's name is the directory of a linked worktree or a
//     separate clone of a repository (the registry records git's metadata
//     while the checkout exists; the shared catalogs carry the names). Code
//     folds such a folder on its own.
//   - path: paths in facts put a directory of the folder's name inside a
//     known repository's tree, or in its worktrees directory (<repo>-worktrees).
//   - name: the folder's name contains a known repository's name as whole
//     words.
//   - sessions: the sessions that filed the folder's facts filed facts in a
//     repository's folder too, and the folder's facts point at that
//     repository by their own paths, forge URLs or repository-relative paths.
//   - facts: half or more of the folder's live facts point at it so.
//
// A related folder stands alone only on evidence of an identity of its own
// (its own commits, a remote or forge repository of its own name that no
// ancestor has, sessions working in a folder of its own name), which code
// lists and the decision must cite (assess.Enforce).

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// maxAncestors bounds the ancestor candidates shown for one folder.
const maxAncestors = 5

// ancestorIndex is what the store's live facts say about folders' places,
// read once per run: for a directory name, the repositories whose trees
// paths put it inside (with the facts); for a name, the sessions whose facts
// name paths in a folder of that name in a projects directory; and, for each
// session, the folders its live facts are filed in.
type ancestorIndex struct {
	inside     map[string]map[string]*pathHits
	own        map[string]map[string]string // name -> session -> an example path
	sessScopes map[string]map[string]int
	noteSess   map[string]string // inbox note name -> its session
}

// session is the session behind f: its own, else its origin note's (a
// curated fact carries the note it came from, the note the session).
func (ix *ancestorIndex) session(f *fact.Fact) string {
	if s := f.Meta.Source.Session; s != "" {
		return s
	}
	return ix.noteSess[f.Meta.Origin]
}

// pathHits are the facts whose paths put a directory inside a repository's
// tree: fact name -> where it is filed and the session that wrote it, with
// one path for the model to read.
type pathHits struct {
	facts   map[string]pathFact
	example string
}

type pathFact struct{ scope, session, path string }

// homePrefixRx is a path's home part: ~, /home/<user> or /Users/<user>.
var homePrefixRx = regexp.MustCompile(`^(?:~|/(?:home|Users)/[^/]+)(?:/|$)`)

// worktreesDir is the repository a worktrees directory is named for
// (<repo>-worktrees, <repo>.worktrees, <repo>-worktree, <repo>_worktrees).
func worktreesDir(n string) (string, bool) {
	for _, suf := range []string{"-worktrees", ".worktrees", "_worktrees", "-worktree"} {
		if b, ok := strings.CutSuffix(n, suf); ok && b != "" {
			return b, true
		}
	}
	return "", false
}

// knownRepoName is the repository a name stands for: a catalog checkout's
// name or another name of one, or a project record's name or alias; "" for
// none.
func (a *assessor) knownRepoName(n string) string {
	n = strings.ToLower(n)
	if n == "" || !fact.IsSlug(n) {
		return ""
	}
	if own := a.catalog.AliasOf(n); own != "" {
		return own
	}
	if a.catalog.Has(n) {
		return n
	}
	if o := record.RepoOwner(a.arecs(), n); o != "" && strings.HasPrefix(o, "projects/") {
		return path.Base(o)
	}
	return ""
}

// checkoutName is the repository a name is a checkout's directory of in
// some machine's catalog (its name or another name of it); "" for none.
func (a *assessor) checkoutName(n string) string {
	n = strings.ToLower(n)
	if own := a.catalog.AliasOf(n); own != "" {
		return own
	}
	if a.catalog.Has(n) {
		return n
	}
	return ""
}

func (a *assessor) ancestorIx() *ancestorIndex {
	if a.ancIx != nil {
		return a.ancIx
	}
	ix := &ancestorIndex{inside: map[string]map[string]*pathHits{}, own: map[string]map[string]string{}, sessScopes: map[string]map[string]int{}, noteSess: map[string]string{}}
	a.ancIx = ix
	for _, f := range a.facts {
		if (f.InInbox() || overlay.IsInbox(f.Path)) && f.Meta.Source.Session != "" {
			// One answer whatever the order the facts were read in.
			if s, ok := ix.noteSess[f.Meta.Name]; !ok || f.Meta.Source.Session < s {
				ix.noteSess[f.Meta.Name] = f.Meta.Source.Session
			}
		}
	}
	for _, f := range a.facts {
		// Where each session's notes went: its inbox notes' scopes and the
		// facts curated from them, live or not (where it worked, not what
		// holds now).
		if s := ix.session(f); s != "" && f.Meta.Scope != "" {
			if ix.sessScopes[s] == nil {
				ix.sessScopes[s] = map[string]int{}
			}
			ix.sessScopes[s][f.Meta.Scope]++
		}
		if !liveFact(f) {
			continue
		}
		text := f.Meta.Description + "\n" + f.Body + "\n" + strings.Join(f.Meta.AppliesTo.Paths, "\n") + "\n" + strings.Join(f.Meta.AppliesTo.Commands, "\n")
		for _, m := range machinePathRx.FindAllStringSubmatch(text, 40) {
			a.indexPath(ix, f, strings.TrimRight(m[1], ".,;:)'\"`"))
		}
	}
	return ix
}

// indexPath reads one machine path of fact f into ix.
func (a *assessor) indexPath(ix *ancestorIndex, f *fact.Fact, p string) {
	rest := p
	if loc := homePrefixRx.FindStringIndex(p); loc != nil {
		rest = p[loc[1]:]
	} else {
		rest = strings.TrimPrefix(p, "/")
	}
	comps := strings.Split(rest, "/")
	start := 0
	if len(comps) > 1 && comps[0] == "projects" {
		// A folder of the projects directory under its own name (or, past a
		// folder of checkouts, the checkout's).
		top := strings.ToLower(comps[1])
		a.noteOwn(ix, top, f, p)
		if len(comps) > 2 && a.knownRepoName(top) == "" {
			a.noteOwn(ix, strings.ToLower(comps[2]), f, p)
		}
		start = 1
	}
	last, hidden := "", false
	for _, comp := range comps[start:] {
		lc := strings.ToLower(comp)
		if strings.HasPrefix(lc, ".") {
			hidden = true // a tool's tree (~/.claude/projects/...)
			continue
		}
		if lc == "" {
			continue
		}
		// A checkout's directory starts its own tree (one checked out inside
		// another's folder); any other known name starts the outermost one,
		// outside a tool's hidden tree: a directory inside a tree is inside it
		// whether or not a folder of its name has a record yet.
		if r := a.checkoutName(lc); r != "" {
			last = r
			continue
		}
		if b, ok := worktreesDir(lc); ok {
			if r := a.knownRepoName(b); r != "" {
				last = r
				continue
			}
		}
		if last == "" && !hidden {
			if r := a.knownRepoName(lc); r != "" {
				last = r
				continue
			}
		}
		if last == "" || lc == last || !fact.IsSlug(lc) {
			continue
		}
		if ix.inside[lc] == nil {
			ix.inside[lc] = map[string]*pathHits{}
		}
		h := ix.inside[lc][last]
		if h == nil {
			h = &pathHits{facts: map[string]pathFact{}}
			ix.inside[lc][last] = h
		}
		pf := pathFact{scope: f.Meta.Scope, session: ix.session(f), path: homeTilde(p)}
		if old, ok := h.facts[f.Meta.Name]; !ok || pathFactLess(pf, old) {
			h.facts[f.Meta.Name] = pf
		}
	}
}

func (a *assessor) noteOwn(ix *ancestorIndex, n string, f *fact.Fact, p string) {
	s := ix.session(f)
	if s == "" || !fact.IsSlug(n) {
		return
	}
	if ix.own[n] == nil {
		ix.own[n] = map[string]string{}
	}
	if old, ok := ix.own[n][s]; !ok || homeTilde(p) < old {
		ix.own[n][s] = homeTilde(p)
	}
}

// pathFactLess orders the readings of one fact's paths, so the index is the
// same whatever the order the facts were read in.
func pathFactLess(x, y pathFact) bool {
	if x.scope != y.scope {
		return x.scope < y.scope
	}
	if x.session != y.session {
		return x.session < y.session
	}
	return x.path < y.path
}

// homeTilde writes a path's home part as ~.
func homeTilde(p string) string {
	if loc := homePrefixRx.FindStringIndex(p); loc != nil {
		return "~/" + p[loc[1]:]
	}
	return p
}

// ancestorFolder is the live project folder that holds the repository or
// project n now (n itself, or the end of its suppression), when it exists
// (a live record, a folder of the store or the catalogs with no record yet,
// which the graph stands in for, else a name code's check finds) and is no
// folder that is not a project, nor self; "" otherwise.
func (a *assessor) ancestorFolder(n, self string) string {
	n = strings.ToLower(n)
	if a.ancMemo == nil {
		a.ancMemo = map[string]string{}
	}
	w, ok := a.ancMemo[n]
	if !ok {
		w = a.ancestorFolderOf(n)
		a.ancMemo[n] = w
	}
	if w == self {
		return ""
	}
	return w
}

// ancestorFolderOf is ancestorFolder before the self check.
func (a *assessor) ancestorFolderOf(n string) string {
	if own := a.catalog.AliasOf(n); own != "" {
		n = own
	}
	folder := "projects/" + n
	recs := a.arecs()
	if o := record.RepoOwner(recs, n); o != "" {
		folder = o
	}
	w, _, err := record.Resolve(recs, folder)
	if err != nil || !strings.HasPrefix(w, "projects/") {
		return ""
	}
	if r := recs[w]; r != nil {
		if notProject(r) || record.Broad(r.Kind) != "project" {
			return ""
		}
		return w
	}
	if a.checker.Repo(a.ctx, path.Base(w)).Verdict != check.Holds {
		return ""
	}
	return w
}

// tokensContain reports whether name's dash-separated words hold all of
// part's, in order and next to each other, part being fewer words.
func tokensContain(name, part string) bool {
	nt, pt := strings.Split(name, "-"), strings.Split(part, "-")
	if len(pt) >= len(nt) {
		return false
	}
	for i := 0; i+len(pt) <= len(nt); i++ {
		ok := true
		for j := range pt {
			if nt[i+j] != pt[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ancestorAcc is one ancestor found so far, with its evidence and weight.
type ancestorAcc struct {
	name  string
	why   []string
	src   map[string]bool
	score int
}

// directAncestors fills c's own ancestor candidates (the folders its own
// evidence relates it to, each the live folder or the folder with no record
// yet that holds the repository now) and the independence evidence against
// them. The graph (assess_graph.go) resolves them to their final ancestors.
func (a *assessor) directAncestors(c *assess.Candidate) {
	name, ok := strings.CutPrefix(c.Folder, "projects/")
	if !ok || c.Kind != "" {
		return
	}
	ix := a.ancestorIx()
	by := map[string]*ancestorAcc{}
	add := func(repo, src, why string, score int) {
		w := a.ancestorFolder(repo, c.Folder)
		if w == "" {
			return
		}
		x := by[w]
		if x == nil {
			x = &ancestorAcc{name: path.Base(w), src: map[string]bool{}}
			by[w] = x
		}
		if !x.src[src] {
			x.why = append(x.why, why)
		}
		x.src[src] = true
		x.score += score
	}
	// git's metadata, kept while the checkout existed.
	if repo, kind, ok := a.catalog.CheckoutOwner(name); ok {
		add(repo, assess.SourceGit, fmt.Sprintf("its name is the directory of a %s of %s (git's metadata, recorded while the checkout existed)", kind, repo), 1000)
	}
	// The sessions that filed its facts.
	sessions := map[string]bool{}
	var live []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope != c.Folder {
			continue
		}
		if s := ix.session(f); s != "" {
			sessions[s] = true
		}
		if liveFact(f) {
			live = append(live, f)
		}
	}
	// Paths that put a directory of its name inside a repository's tree, in
	// facts about it: its own, the repository's own (filed in its folder), or
	// written by its sessions. A path in a fact about something else (a
	// machine's note naming ~/x/memory) says nothing of this folder.
	for _, repo := range sortedPathRepos(ix.inside[name]) {
		w := a.ancestorFolder(repo, c.Folder)
		n, example := 0, ""
		for _, fn := range sortedKeysAny(ix.inside[name][repo].facts) {
			pf := ix.inside[name][repo].facts[fn]
			if pf.scope == c.Folder || (w != "" && pf.scope == w) || "projects/"+repo == pf.scope || (pf.session != "" && sessions[pf.session]) {
				n++
				if example == "" {
					example = pf.path
				}
			}
		}
		if n > 0 {
			add(repo, assess.SourcePath, fmt.Sprintf("%d fact(s) about it or %s name paths with a %s directory inside %s's tree (e.g. %s)", n, repo, name, repo, example), 40*min(n, 8))
		}
	}
	// A name that holds another's, unless that name is a family's: two or
	// more other names that hold it are repositories of their own
	// (valkey-glide, valkey-grove hold valkey), so holding it says nothing.
	var family []string
	for _, p := range a.ancestorNamePool() {
		if p != name && len(p) >= 4 && tokensContain(name, p) {
			if a.familyName(p) {
				family = append(family, p)
				continue
			}
			add(p, assess.SourceName, "its name contains "+p, 50+10*len(strings.Split(p, "-")))
		}
	}
	// Sessions and the facts' own evidence, for a name that is no repository
	// by code's check (one that is has its own identity to weigh; these
	// checks cost forge calls).
	if c.Name != nil && c.Name.Verdict == check.Holds {
		a.finishAncestors(c, name, ix, by, live)
		return
	}
	shared := map[string]map[string]bool{} // repo -> sessions
	for s := range sessions {
		for scope := range ix.sessScopes[s] {
			n, ok := strings.CutPrefix(scope, "projects/")
			if !ok || scope == c.Folder {
				continue
			}
			if shared[n] == nil {
				shared[n] = map[string]bool{}
			}
			shared[n][s] = true
		}
	}
	probe := map[string]int{}
	for n, ss := range shared {
		probe[n] += 10 * len(ss)
	}
	for _, x := range by {
		probe[x.name] += x.score
	}
	point, sample := a.factsPointAt(c.Folder, live, probe)
	for _, repo := range sortedKeysAny(point) {
		k := len(shared[repo])
		m := point[repo]
		switch {
		// Sessions shared, and facts that point: half its sessions, or a
		// quarter of the facts read (a session that also touched another
		// project, with one fact naming its path, is no relation).
		case k > 0 && (2*k >= len(sessions) || 4*m >= sample):
			add(repo, assess.SourceSessions, fmt.Sprintf("%d of its %d session(s) filed facts in %s too, and %d of %d live fact(s) read point at %s by their own paths, forge URLs or repository-relative paths", k, len(sessions), repo, m, sample, repo), 30*min(k, 5)+20*min(m, 10))
		case 2*m >= sample:
			add(repo, assess.SourceFacts, fmt.Sprintf("%d of %d live fact(s) read point at %s by their own paths, forge URLs or repository-relative paths", m, sample, repo), 20*min(m, 10))
		}
	}
	// Where the store talks about it: the project folder that holds most of
	// the facts of other project folders that name it, more than it holds
	// itself (negeig's work is written down in darklanes).
	if w, n, total := a.mentionedIn(c.Folder, name); w != "" && n > len(live) {
		add(path.Base(w), assess.SourceMentions, fmt.Sprintf("%d of the %d fact(s) in other project folders that name %s are %s's, more than its own %d live fact(s) and far more than its share of the store's facts", n, total, name, path.Base(w), len(live)), 5*min(n, 10))
	}
	if len(by) > 0 {
		// Related: the other repositories its evidence names join the ranking
		// (they do not make a folder related on their own).
		a.rankMore(c, live, by)
		for _, p := range family {
			add(p, assess.SourceName, "its name contains "+p+" (a name other repositories of their own hold too)", 10)
		}
	}
	a.finishAncestors(c, name, ix, by, live)
}

// finishAncestors ranks the ancestors found (best first, at most
// maxAncestors) and gathers the independence evidence against them.
func (a *assessor) finishAncestors(c *assess.Candidate, name string, ix *ancestorIndex, by map[string]*ancestorAcc, live []*fact.Fact) {
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]].score != by[keys[j]].score {
			return by[keys[i]].score > by[keys[j]].score
		}
		return keys[i] < keys[j]
	})
	c.Ancestors = nil
	for i, k := range keys {
		if i == maxAncestors {
			break
		}
		x := by[k]
		var srcs []string
		for s := range x.src {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		c.Ancestors = append(c.Ancestors, assess.Ancestor{Name: x.name, Folder: k, Why: x.why, Sources: srcs, Score: x.score})
	}
	c.Independence = nil
	if len(c.Ancestors) > 0 {
		c.Independence = a.independence(c, name, ix, live)
	}
}

// liveOf are the live facts filed in folder.
func (a *assessor) liveOf(folder string) []*fact.Fact {
	var out []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope == folder && liveFact(f) {
			out = append(out, f)
		}
	}
	return out
}

func sortedPathRepos(m map[string]*pathHits) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysAny[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// familyName reports a name that two or more names of the pool hold
// (tokensContain) as repositories of their own: a checkout in some
// machine's catalog with a remote of its own name. Such a name is a family's
// (a project and the libraries named after it), not an ancestor's.
func (a *assessor) familyName(p string) bool {
	if a.families == nil {
		a.families = map[string]bool{}
	}
	if v, ok := a.families[p]; ok {
		return v
	}
	own := func(n string) bool {
		var urls []string
		if r, ok := a.catalog.Repo(n); ok {
			urls = append(urls, r.URLs...)
		}
		for _, h := range a.shared[n] {
			urls = append(urls, h.URLs...)
		}
		for _, u := range urls {
			if repoid.URLName(u) == n {
				return true
			}
		}
		return false
	}
	k := 0
	for _, n := range a.ancestorNamePool() {
		if n != p && tokensContain(n, p) && own(n) {
			k++
		}
	}
	a.families[p] = k >= 2
	return k >= 2
}

// ancestorNamePool are the names a folder's name may hold: the catalogs'
// repositories and their other names, and the project records' names and
// aliases (live or folded: a folded one resolves to its winner).
func (a *assessor) ancestorNamePool() []string {
	if a.namePool != nil {
		return a.namePool
	}
	set := map[string]bool{}
	for _, n := range a.catalog.Names() {
		set[n] = true
	}
	for _, n := range a.catalog.AliasNames() {
		set[n] = true
	}
	for f, r := range a.arecs() {
		if strings.HasPrefix(f, "projects/") && record.Broad(r.Kind) == "project" {
			for _, n := range r.Names() {
				set[strings.ToLower(n)] = true
			}
		}
	}
	a.namePool = sortedKeysBool(set)
	return a.namePool
}

// maxProbe bounds the repositories a folder's relative paths are checked
// in, maxPointFacts the facts read for where they point.
const (
	maxProbe      = 4
	maxPointFacts = 30
)

// factsPointAt counts, for each repository, the folder's live facts whose
// own evidence points at it: the machine paths and forge URLs they name
// (pointedRepos), and the repository-relative paths they name that exist in
// exactly one of the repositories probed (the best of probe by weight, at
// most maxProbe, each with a checkout here or a forge repository;
// relPathRepos). Names are the live folders' (projects/<name>, less the
// prefix).
func (a *assessor) factsPointAt(self string, live []*fact.Fact, probe map[string]int) (map[string]int, int) {
	out := map[string]int{}
	if len(live) == 0 {
		return out, 0
	}
	names := make([]string, 0, len(probe))
	for n := range probe {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if probe[names[i]] != probe[names[j]] {
			return probe[names[i]] > probe[names[j]]
		}
		return names[i] < names[j]
	})
	roots := map[string]pathSource{}
	for _, n := range names {
		if len(roots) == maxProbe {
			break
		}
		w := a.ancestorFolder(n, self)
		if w == "" {
			continue
		}
		b := path.Base(w)
		if r, ok := a.catalog.Repo(b); ok && r.Root != "" {
			roots[b] = pathSource{root: r.Root}
		} else if full := a.forgeFullOf(b); full != "" {
			roots[b] = pathSource{forge: full}
		}
	}
	cache := a.repoCache()
	// The newest facts first, at most maxPointFacts: what a folder is shows
	// in a sample; a folder of hundreds of facts costs no more.
	live = append([]*fact.Fact(nil), live...)
	sort.Slice(live, func(i, j int) bool {
		if live[i].Meta.Created != live[j].Meta.Created {
			return live[i].Meta.Created > live[j].Meta.Created
		}
		return live[i].Meta.Name < live[j].Meta.Name
	})
	if len(live) > maxPointFacts {
		live = live[:maxPointFacts]
	}
	for _, f := range live {
		set := map[string]bool{}
		for n := range pointedRepos(cache, a.catalog, a.arecs(), f) {
			if w := a.ancestorFolder(n, self); w != "" {
				set[path.Base(w)] = true
			}
		}
		for n := range a.relPathRepos(f, roots) {
			set[n] = true
		}
		for n := range set {
			out[n]++
		}
	}
	return out, len(live)
}

// rankMore adds to a related folder's ancestors (by) the repositories code
// proposed for it (Candidate.Real: its sessions' checkouts, the paths and
// remotes its facts name, applies_to) and those its live facts name as a
// whole word, two facts or more: ranking evidence, never a relation alone.
func (a *assessor) rankMore(c *assess.Candidate, live []*fact.Fact, by map[string]*ancestorAcc) {
	addTo := func(repo, src, why string, score int) {
		w := a.ancestorFolder(repo, c.Folder)
		if w == "" {
			return
		}
		x := by[w]
		if x == nil {
			x = &ancestorAcc{name: path.Base(w), src: map[string]bool{}}
			by[w] = x
		}
		if !x.src[src] {
			x.why = append(x.why, why)
		}
		x.src[src] = true
		x.score += score
	}
	for _, r := range c.Real {
		addTo(r.Name, assess.SourceProposed, strings.Join(r.Why, "; "), min(r.Score, 200))
	}
	name := path.Base(c.Folder)
	texts := make([]string, len(live))
	for i, f := range live {
		texts[i] = strings.ToLower(f.Meta.Description + "\n" + f.Body)
	}
	for _, p := range a.ancestorNamePool() {
		if p == name || len(p) < 4 {
			continue
		}
		n := 0
		for _, t := range texts {
			if strings.Contains(t, p) && record.WholeWord(t, p) {
				n++
			}
		}
		if n >= 2 {
			addTo(p, assess.SourceNamed, fmt.Sprintf("%d of its %d live fact(s) name %s", n, len(live), p), 10*min(n, 10))
		}
	}
}

// forgeFullOf is the forge repository (owner/name) of the repository n: the
// forge's answer to its name check, else the forge remote of its checkout
// here or on another machine whose name is n; "" for none.
func (a *assessor) forgeFullOf(n string) string {
	rc := a.checker.Repo(a.ctx, n)
	if rc.Verdict != check.Holds {
		return ""
	}
	if rc.Forge != "" {
		return rc.Forge
	}
	var urls []string
	if r, ok := a.catalog.Repo(n); ok {
		urls = append(urls, r.URLs...)
	}
	for _, h := range a.shared[n] {
		urls = append(urls, h.URLs...)
	}
	if a.checker.Forge == nil {
		return ""
	}
	fh := a.checker.Forge.Forge.Host()
	sort.Strings(urls)
	for _, u := range urls {
		host, full, ok := strings.Cut(u, "/")
		if ok && host == fh && repoid.URLName(u) == n && strings.Count(full, "/") == 1 {
			return full
		}
	}
	return ""
}

// independence is the evidence that the folder name has an identity of its
// own, against its ancestors: its own commits in a checkout of its own name,
// a remote of that checkout no ancestor has, a forge repository of its own
// name that is no ancestor's, sessions that worked in a checkout or folder of
// its own name, or facts of two or more sessions that name paths in a folder
// of its name in a projects directory.
func (a *assessor) independence(c *assess.Candidate, name string, ix *ancestorIndex, live []*fact.Fact) []assess.Independence {
	var out []assess.Independence
	ancURLs, ancForge := map[string]bool{}, map[string]bool{}
	for _, an := range c.Ancestors {
		if r, ok := a.catalog.Repo(an.Name); ok {
			for _, u := range r.URLs {
				ancURLs[u] = true
			}
		}
		for _, h := range a.shared[an.Name] {
			for _, u := range h.URLs {
				ancURLs[u] = true
			}
		}
		if full := a.forgeFullOf(an.Name); full != "" {
			ancForge[strings.ToLower(full)] = true
		}
	}
	commits, work := "", ""
	seenURL := map[string]bool{}
	var remotes []string
	for _, e := range c.Evidence {
		if e.Host == "" {
			continue
		}
		if e.GitRoot && e.OwnCommits != nil && *e.OwnCommits > 0 && commits == "" {
			commits = fmt.Sprintf("%d own commit(s) in its own checkout on %s (%s..%s)", *e.OwnCommits, e.Host, e.FirstCommit, e.LastCommit)
		}
		if (e.GitRoot || len(e.Paths) > 0) && e.WorkSessions >= 2 && work == "" {
			work = fmt.Sprintf("%d session(s) worked in its own checkout or folder on %s (%s..%s)", e.WorkSessions, e.Host, e.FirstWork, e.LastWork)
		}
		if e.GitRoot {
			for _, u := range e.Remotes {
				if !ancURLs[u] && !seenURL[u] {
					seenURL[u] = true
					remotes = append(remotes, u)
				}
			}
		}
	}
	if commits != "" {
		out = append(out, assess.Independence{Key: "commits", Line: commits})
	}
	sort.Strings(remotes)
	for i, u := range remotes {
		if i == 2 {
			break
		}
		out = append(out, assess.Independence{Key: "remote:" + u, Line: "its own checkout's remote " + u + ", which no ancestor candidate has"})
	}
	forge := ""
	if c.Name != nil && c.Name.Verdict == check.Holds && c.Name.Renamed == "" && c.Name.Forge != "" && repoid.URLName(c.Name.Forge) == name && !ancForge[strings.ToLower(c.Name.Forge)] {
		forge = strings.ToLower(c.Name.Forge)
		out = append(out, assess.Independence{Key: "forge:" + forge, Line: "the forge has a repository of its own name, " + c.Name.Forge + ", which is no ancestor candidate's"})
	}
	if forge == "" {
		// A forge repository of its own name that its own facts name by URL
		// (an owner the name check does not ask under), when the forge has it.
		owners := map[string]bool{}
		rx := regexp.MustCompile(`(?i)(?:^|[\s(\x60"'])([a-z0-9][a-z0-9-]{0,38})/` + regexp.QuoteMeta(name) + `(?:$|[^a-z0-9._/-])`)
		for _, f := range live {
			text := f.Meta.Description + "\n" + f.Body
			for _, ref := range check.Refs(text) {
				if ref.Kind == check.KindRepo && ref.Value == name {
					owners[ref.Owner] = true
				}
			}
			for _, m := range rx.FindAllStringSubmatch(text, 4) {
				if o := strings.ToLower(m[1]); o != "projects" && len(owners) < 4 {
					owners[o] = true
				}
			}
		}
		for _, o := range sortedKeysBool(owners) {
			if r := a.checker.ForgeRepo(a.ctx, o, name); r.Verdict == check.Holds && strings.EqualFold(path.Base(r.Value), name) && !ancForge[strings.ToLower(r.Value)] {
				out = append(out, assess.Independence{Key: "forge:" + strings.ToLower(r.Value), Line: "its own facts name the forge repository " + r.Value + " of its own name, which the forge has (check: " + r.Output + ") and no ancestor candidate is"})
				break
			}
		}
	}
	if work != "" {
		out = append(out, assess.Independence{Key: "work", Line: work})
	}
	if ss := ix.own[name]; len(ss) >= 2 {
		ids := sortedKeysAny(ss)
		out = append(out, assess.Independence{Key: "work:paths", Line: fmt.Sprintf("facts of %d session(s) name paths in a folder of its own name in a projects directory (e.g. %s, session:%s)", len(ss), ss[ids[0]], ids[0])})
	}
	return out
}
