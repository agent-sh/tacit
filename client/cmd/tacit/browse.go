package main

// The store's tree for an agent that asks (SPEC.md, "Search and list"): the
// filters of tacit_search and `tacit search`, and tacit_list / `tacit list`.
// Results are one line per fact, paged; a body is tacit_fact's.
import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// browseArgs is what an agent may ask: words, filters, a page.
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
	treeListLimit   = 20
	treeMaxLimit    = 50
	// treePool is how many ranked candidates a search with a path or
	// command filter reads before filtering, so the filter does not starve.
	treePool = 500
)

var treeScopeRx = regexp.MustCompile(`^(user|system|(projects|machines|derived)/[a-z0-9][a-z0-9._-]*)$`)

// eligibleScopes are the scopes a search reads by default: system, user,
// this repo's project (with its checkout aliases) and derived scopes, and
// this machine. Another project's or machine's facts are read only when the
// agent names that scope.
func eligibleScopes(repo string, aliases []string, host string) []string {
	out := []string{"system", "user"}
	seen := map[string]bool{}
	for _, r := range append([]string{repo}, aliases...) {
		if r = strings.ToLower(r); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, "projects/"+r, "derived/"+r)
		}
	}
	if host != "" {
		out = append(out, "machines/"+strings.ToLower(host))
	}
	return out
}

// treeScopes resolves the scopes of a request, or an error for a scope that
// is not one.
func treeScopes(ctx context.Context, a *browseArgs) ([]string, error) {
	if a.Repo == "" && a.Cwd != "" {
		a.Repo = store.RepoName(ctx, a.Cwd)
	}
	if a.Scope != "" {
		sc := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a.Scope), "/"))
		if !treeScopeRx.MatchString(sc) {
			return nil, fmt.Errorf("scope must be user, system, projects/<repo>, machines/<host> or derived/<repo>")
		}
		return []string{sc}, nil
	}
	var aliases []string
	if a.Cwd != "" && a.Repo != "" && strings.EqualFold(a.Repo, store.RepoName(ctx, a.Cwd)) {
		aliases = hookRepoAliases(ctx, a.Cwd)
	}
	return eligibleScopes(a.Repo, aliases, store.ShortHost()), nil
}

// relPath is p relative to the checkout that holds cwd, when it is inside.
func relPath(p, cwd string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	root := gitRoot(cwd)
	if !filepath.IsAbs(p) {
		if root == "" {
			return filepath.ToSlash(p)
		}
		p = filepath.Join(cwd, p)
	}
	if root != "" {
		if r, err := filepath.Rel(root, filepath.Clean(p)); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
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
// with no words, every fact the filters keep in kind order. Filters: scope,
// type, rulings only, and an applies_to path or command.
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

// treeList is tacit_list: with no scope, the eligible scopes and how many
// facts of each type they hold; with a scope (and a type), its facts in
// kind order, paged.
func treeList(ctx context.Context, ix *search.Index, a browseArgs) (string, error) {
	scopes, err := treeScopes(ctx, &a)
	if err != nil {
		return "", err
	}
	if a.Type != "" && !fact.Types[a.Type] {
		return "", fmt.Errorf("unknown type %q", a.Type)
	}
	if a.Scope == "" && a.Type == "" {
		counts, err := ix.Counts(ctx, scopes)
		if err != nil {
			return "", err
		}
		for _, sc := range scopes {
			if r, ok := strings.CutPrefix(sc, "projects/"); ok {
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
		}
		var b strings.Builder
		b.WriteString("# scopes you can read here, facts by type; `tacit_list scope=<scope>` lists one\n")
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
		return b.String(), nil
	}
	hits, err := treeFacts(ctx, ix, scopes, a.Type, a.RulingsOnly, a.Inbox)
	if err != nil {
		return "", err
	}
	kindOrder(hits)
	return page(hits, max(0, a.Offset), clampLimit(a.Limit, treeListLimit)), nil
}

// runIndexed runs one browse request against the store for the CLI.
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

// runList implements `tacit list [--scope S] [--type T] [--limit N] [--offset K]`: tacit_list.
func runList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	scope := fs.String("scope", "", "one scope; none: the scopes this session can read, with counts by type")
	typ := fs.String("type", "", "only facts of this type")
	limit := fs.Int("limit", treeListLimit, "facts per page")
	offset := fs.Int("offset", 0, "skip this many, for the next page")
	fs.Parse(args)
	return runIndexed(func(ctx context.Context, ix *search.Index, cwd string) (string, error) {
		return treeList(ctx, ix, browseArgs{Cwd: cwd, Scope: *scope, Type: *typ, Limit: *limit, Offset: *offset})
	})
}
