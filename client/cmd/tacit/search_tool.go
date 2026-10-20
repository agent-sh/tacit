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
// names another (that name alone), and host.
func readSession(cwd, repo, host string) tree.Session {
	sess := tree.Session{Host: host}
	if r, ok := sessionRepo(cwd); ok && cwd != "" && (repo == "" || strings.EqualFold(repo, r.Name)) {
		sess.Repos = repoAliases(r)
	} else if repo != "" {
		sess.Repos = []string{repo}
	}
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
		return []string{sc}, nil
	}
	return readSession(a.Cwd, a.Repo, store.ShortHost()).Scopes(), nil
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
		hits, err = ix.Query(ctx, search.Situation{Repo: a.Repo, Host: store.ShortHost(), Query: a.Query, Derived: true, Scopes: qs, Type: a.Type, RulingsOnly: a.RulingsOnly}, pool, a.Inbox)
		if a.Scope != "" {
			var in []search.Hit
			for _, h := range hits {
				if inScope(h, scopes[0]) {
					in = append(in, h)
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
	return page(keep, max(0, a.Offset), limit), nil
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
