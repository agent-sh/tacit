package main

// What an agent asks the store for itself (SPEC.md, "Search and the tree"):
// tacit_search and `tacit search`, by words or by filters, one line per fact,
// paged; a body is tacit_fact's, the folders tacit_tree's (tree.go). Both
// read what readSession allows.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// browseArgs is what an agent may ask tacit_search: words, filters, a page.
type browseArgs struct {
	Query       string
	Repo        string // default: the repo of Cwd
	Cwd         string
	Scope       string // one scope; default: the eligible scopes
	Type        string
	RulingsOnly bool
	Path        string
	Command     string
	Inbox       bool
	Limit       int
	Offset      int
	// Records are the store's folder records (storeRecords), nil for none.
	Records map[string]*record.Record
}

const (
	treeSearchLimit = 8
	// treeListLimit is the page with no words (a scope's or a type's facts).
	treeListLimit = 20
	treeMaxLimit  = 50
	// treePool is how many ranked candidates a search with a path or
	// command filter reads before filtering, so the filter does not starve.
	treePool = 500
)

// readSession is the session a read of the store answers for (tree.Session,
// the one rule of tacit_tree and tacit_search): the repository of cwd
// (repoid) under every name delivery knows it by (repoAliases), unless repo
// names another (that name alone), with the folders suppression joins it
// with (suppressionNames), and host; recs are the store's folder records
// (which areas and topics it reads).
func readSession(recs map[string]*record.Record, cwd, repo, host string) tree.Session {
	sess := tree.Session{Host: host, Records: recs}
	if r, ok := sessionRepo(cwd); ok && cwd != "" && (repo == "" || strings.EqualFold(repo, r.Name)) {
		sess.Repos = repoAliases(r)
	} else if repo != "" {
		sess.Repos = []string{repo}
	}
	sess.Repos = suppressionNames(recs, sess.Repos)
	return sess
}

// treeScopes resolves the scopes of a request: the one scope it names
// (tree.Scope), or the session's (readSession); an error for a scope that
// is not one.
func treeScopes(ctx context.Context, a *browseArgs) ([]string, error) {
	if a.Repo == "" && a.Cwd != "" {
		a.Repo = store.RepoName(ctx, a.Cwd)
	}
	if a.Scope != "" {
		sc, err := tree.Scope(a.Scope)
		if err != nil {
			return nil, err
		}
		// A suppressed folder is searched as the folder that replaces it,
		// and its own facts the migration has not moved yet.
		if w, chain, err := record.Resolve(a.Records, sc); err == nil && len(chain) > 0 && a.Records[w] != nil {
			return []string{w, sc}, nil
		}
		return []string{sc}, nil
	}
	return readSession(a.Records, a.Cwd, a.Repo, store.ShortHost()).Scopes(), nil
}

// relPath is p relative to the checkout that holds cwd, when it is inside
// (repoRel); otherwise p as given, with forward slashes.
func relPath(p, cwd string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if r, ok := sessionRepo(cwd); ok {
		if rel, ok := repoRel(r, cwd, p); ok {
			return rel
		}
	}
	return filepath.ToSlash(p)
}

// appliesMatch is the prompt hook's metadata match (hooks_delivery.go) for
// an explicit path or command: a fact's applies_to path covers the path, or
// its command is how the given command starts, or the other way round
// ("git commit" asks for the facts on "git commit -s").
func appliesMatch(h search.Hit, path, command string) bool {
	if path != "" {
		ok := false
		for _, g := range h.Applies.Paths {
			ok = ok || pathMatch(g, path)
		}
		if !ok {
			return false
		}
	}
	if command != "" {
		ok := false
		for _, c := range h.Applies.Commands {
			ok = ok || commandMatch(c, command) || commandMatch(command, c)
		}
		if !ok {
			return false
		}
	}
	return true
}

// kindOrder orders facts with no score: owner rules, rulings in their
// task, standards, decisions and past mistakes, the rest; newest first.
func kindOrder(hits []search.Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := subRank(hits[i])+tier(hits[i]), subRank(hits[j])+tier(hits[j])
		if a != b {
			return a < b
		}
		if hits[i].Created != hits[j].Created {
			return hits[i].Created > hits[j].Created
		}
		return hits[i].Name < hits[j].Name
	})
}

// page renders hits[offset:offset+limit] as one line per fact, with the
// total and the next offset when there is more.
func page(hits []search.Hit, offset, limit int) string {
	if len(hits) == 0 || offset >= len(hits) {
		return "no matching facts"
	}
	end := min(len(hits), offset+limit)
	title := fmt.Sprintf("# %d-%d of %d fact(s); `tacit_fact <name>` opens one", offset+1, end, len(hits))
	out := search.Block(hits[offset:end], title)
	if end < len(hits) {
		out += fmt.Sprintf("more: offset %d\n", end)
	}
	return out
}

func clampLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, treeMaxLimit)
}

// treeFacts lists the facts of scopes. A project's scope also holds the
// facts filed elsewhere (user, a machine) whose applies_to.repos name that
// project, as the hooks treat them (Index.Tied).
func treeFacts(ctx context.Context, ix *search.Index, scopes []string, typ string, rulingsOnly, inbox bool) ([]search.Hit, error) {
	hits, err := ix.List(ctx, scopes, typ, rulingsOnly, inbox)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, h := range hits {
		have[h.Path] = true
	}
	for _, sc := range scopes {
		r, ok := strings.CutPrefix(sc, "projects/")
		if !ok {
			continue
		}
		tied, err := ix.Tied(ctx, []string{r})
		if err != nil {
			return nil, err
		}
		for _, h := range tied {
			if !have[h.Path] && (typ == "" || h.Type == typ) && (!rulingsOnly || h.Ruling) {
				have[h.Path] = true
				hits = append(hits, h)
			}
		}
	}
	return hits, nil
}

// inScope reports whether h belongs to scope as treeFacts counts it.
func inScope(h search.Hit, scope string) bool {
	if h.Scope == scope {
		return true
	}
	r, ok := strings.CutPrefix(scope, "projects/")
	return ok && repoTied(h, []string{r})
}

// treeSearch is tacit_search: words ranked by BM25 within the scopes, or,
// with no words, every fact the filters keep in kind order (a scope's facts,
// 20 a page: what browsing a scope takes). Filters: scope, type, rulings
// only, and an applies_to path or command.
func treeSearch(ctx context.Context, ix *search.Index, a browseArgs) (string, error) {
	if a.Records == nil {
		if recs, err := ix.Records(ctx); err == nil {
			a.Records = recs
		}
	}
	scopes, err := treeScopes(ctx, &a)
	if err != nil {
		return "", err
	}
	if a.Type != "" && !fact.Types[a.Type] {
		return "", fmt.Errorf("unknown type %q", a.Type)
	}
	a.Path = relPath(a.Path, a.Cwd)
	limit := clampLimit(a.Limit, treeSearchLimit)
	if strings.TrimSpace(a.Query) == "" {
		limit = clampLimit(a.Limit, treeListLimit)
	}
	filtered := a.Scope != "" || a.Type != "" || a.RulingsOnly || a.Path != "" || a.Command != ""
	var hits []search.Hit
	footer := ""
	if strings.TrimSpace(a.Query) == "" {
		if !filtered {
			return "", fmt.Errorf("give a query or a filter (scope, type, rulings_only, path, command)")
		}
		hits, err = treeFacts(ctx, ix, scopes, a.Type, a.RulingsOnly, a.Inbox)
		kindOrder(hits)
	} else {
		pool := a.Offset + limit
		if a.Path != "" || a.Command != "" {
			pool = treePool
		}
		qs := scopes
		if a.Scope != "" {
			// One scope asked for: read every scope, keep that one's facts
			// (a project's include those tied to it from elsewhere).
			pool = treePool
			if qs, err = ix.Scopes(ctx); err != nil {
				return "", err
			}
		}
		// With no filter the session's repository's facts weigh more
		// (sessionRepoBoost), over every candidate, not only a page's worth.
		ranked := a.Scope == "" && a.Path == "" && a.Command == ""
		if ranked {
			pool = treePool
		}
		hits, err = ix.Query(ctx, search.Situation{Repo: a.Repo, Host: store.ShortHost(), Query: a.Query, Derived: true, Scopes: qs, Type: a.Type, RulingsOnly: a.RulingsOnly, ShortTerms: true}, pool, a.Inbox)
		if ranked && err == nil {
			repos := readSession(a.Records, a.Cwd, a.Repo, store.ShortHost()).Repos
			hits = sessionRepoFirst(hits, repos)
			footer = repoTreeLine(ctx, ix, repos, "`tacit_tree")
		}
		if a.Scope != "" {
			var in []search.Hit
			for _, h := range hits {
				for _, sc := range scopes {
					if inScope(h, sc) {
						in = append(in, h)
						break
					}
				}
			}
			hits = in
		}
	}
	if err != nil {
		return "", err
	}
	var keep []search.Hit
	for _, h := range hits {
		if appliesMatch(h, a.Path, a.Command) {
			keep = append(keep, h)
		}
	}
	out := page(keep, max(0, a.Offset), limit)
	if footer != "" && len(keep) > 0 {
		out += footer
	}
	return out, nil
}

// sessionRepoBoost is how much more a fact of the session's repository
// (repoTied: its project scope or its applies_to.repos) weighs in a word
// search than one of the user, system or machine scopes. Measured on the
// held-out replay's 263 situations (their requests as queries) and the
// act-bench: the held-out facts found stay level from 1 to 2 and fall from
// 2.5, the act-bench's grow up to 1.5 to 2; 1.5 is the middle of the level
// range. eval/VALUE.md, "Search, the push and the tree".
const sessionRepoBoost = 1.5

// sessionRepoFirst weighs the facts of the session's repository by
// sessionRepoBoost and orders hits by score again.
func sessionRepoFirst(hits []search.Hit, repos []string) []search.Hit {
	if len(repos) == 0 {
		return hits
	}
	for i := range hits {
		if repoTied(hits[i], repos) {
			hits[i].Score *= sessionRepoBoost
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

// repoTreeLine points from a search or the SessionStart block to the
// session's repository folder in the tree: how many facts are tied to the
// repository (Index.Tied) and the tree call that lists them, tool being how
// the reader names the tree ("`tacit_tree", "`mcp__tacit__tacit_tree",
// "`tacit tree"). The folder is the first of repos that is a project scope
// of those facts. "" when the repository has none.
func repoTreeLine(ctx context.Context, ix *search.Index, repos []string, tool string) string {
	n, folder := repoFacts(ctx, ix, repos)
	return treeLine(n, folder, tool)
}

func treeLine(n int, folder, tool string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("# this repo's %d facts by folder: %s %s`\n", n, tool, folder)
}

// repoFacts counts the facts tied to repos and names their project folder.
func repoFacts(ctx context.Context, ix *search.Index, repos []string) (int, string) {
	tied, err := ix.Tied(ctx, repos)
	if err != nil || len(tied) == 0 {
		return 0, ""
	}
	scopes := map[string]bool{}
	for _, h := range tied {
		scopes[h.Scope] = true
	}
	folder := "projects/" + strings.ToLower(repos[0])
	for _, r := range repos {
		if sc := "projects/" + strings.ToLower(r); scopes[sc] {
			folder = sc
			break
		}
	}
	return len(tied), folder
}

// scopeCounts is the tree answer's first block: for each of scopes that
// holds live curated facts, how many of each type, a project's counting the
// facts filed elsewhere that are tied to it (applies_to.repos), as the hooks
// and tacit_search treat them. "" when none holds any.
func scopeCounts(ctx context.Context, ix *search.Index, scopes []string) (string, error) {
	counts, err := ix.Counts(ctx, scopes)
	if err != nil {
		return "", err
	}
	for _, sc := range scopes {
		r, ok := strings.CutPrefix(sc, "projects/")
		if !ok {
			continue
		}
		tied, err := ix.Tied(ctx, []string{r})
		if err != nil {
			return "", err
		}
		for _, h := range tied {
			if h.Scope != sc {
				if counts[sc] == nil {
					counts[sc] = map[string]int{}
				}
				counts[sc][h.Type]++
			}
		}
	}
	var b strings.Builder
	for _, sc := range scopes {
		c := counts[sc]
		if len(c) == 0 {
			continue
		}
		var types []string
		total := 0
		for t, n := range c {
			types = append(types, fmt.Sprintf("%s %d", t, n))
			total += n
		}
		sort.Strings(types)
		fmt.Fprintf(&b, "- %s (%d): %s\n", sc, total, strings.Join(types, ", "))
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "# facts by scope and type (a project's include those tied to it from elsewhere); `tacit_search scope=<scope>` lists one\n" + b.String(), nil
}

// runIndexed runs one search request against the store for the CLI.
func runIndexed(do func(ctx context.Context, ix *search.Index, cwd string) (string, error)) int {
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "no store; run `tacit clone <url> <harness>`")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ix, err := openIndex(ctx, s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer ix.Close()
	cwd, _ := os.Getwd()
	out, err := do(ctx, ix, cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Print(strings.TrimRight(out, "\n") + "\n")
	return 0
}
