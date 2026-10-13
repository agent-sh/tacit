package gitev

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Subject is what facts are about, in terms git can be asked about.
type Subject struct {
	// Repos are checkouts under Root, most relevant first, at most MaxRepos.
	Repos []string `json:"repos"`
	// Paths are repo-relative paths, directories (trailing /) and globs. A
	// bare file name becomes the glob **/name.
	Paths []string `json:"paths,omitempty"`
	// Idents are distinctive strings: identifiers, flags, environment
	// variables. Generic words are never in here.
	Idents []string `json:"idents,omitempty"`
}

// Empty reports whether git has nothing to be asked.
func (s Subject) Empty() bool { return len(s.Repos) == 0 || len(s.Paths)+len(s.Idents) == 0 }

// Limits on what a subject keeps, each chosen so the queries stay fast and
// the result stays readable: six paths and eight identifiers are about what
// one fact names when it names its subject at all.
const (
	maxPaths  = 6
	maxIdents = 8
)

type cand struct {
	s     string
	path  bool
	score int
	order int
}

type collector struct {
	g       *Git
	cands   map[string]*cand
	n       int
	mention map[string]int // repo name -> weight
	hint    map[string]int // repo named by a ~/projects/<repo>/ path
}

func (c *collector) add(s string, isPath bool, score int) {
	key := s
	if isPath {
		key = "p:" + s
	}
	if old, ok := c.cands[key]; ok {
		if score > old.score {
			old.score = score
		}
		return
	}
	c.n++
	c.cands[key] = &cand{s: s, path: isPath, score: score, order: c.n}
}

var (
	reTick     = regexp.MustCompile("`([^`\n]{2,120})`")
	reWord     = regexp.MustCompile(`[A-Za-z0-9_./*~:=\-]+`)
	reSnake    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9]*(?:_[A-Za-z0-9]+)+$`)
	reCamel    = regexp.MustCompile(`^[A-Za-z][a-z0-9]+(?:[A-Z][a-z0-9]+){1,}$`)
	reScoped   = regexp.MustCompile(`^[A-Za-z_]\w*(?:::[A-Za-z_]\w*)+$`)
	reKebab    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)+$`)
	reLower    = regexp.MustCompile(`^[a-z]{6,}$`)
	reFlag     = regexp.MustCompile(`^--[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	reFile     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\.(?:py|rs|go|ts|tsx|js|mjs|sh|toml|ya?ml|json|md|service|timer|cu|cuh|cpp|cc|c|h|hpp|ex|exs|sql|html|css|lua|rb|java|kt|swift|proto|cfg|ini)$`)
	rePathSeg  = regexp.MustCompile(`^[A-Za-z0-9_.@*?{}\-]+$`)
	reExt      = regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`)
	reLetters  = regexp.MustCompile(`[A-Za-z]{3,}`)
	reRepoPath = regexp.MustCompile(`^~/projects/([A-Za-z0-9._-]+)(?:/(.*))?$`)
)

// genericFlags are flags nearly every tool has; naming one says nothing
// about a subject.
var genericFlags = set("--help", "--version", "--force", "--verbose", "--quiet", "--release", "--debug", "--json", "--all", "--yes", "--no-verify", "--dry-run", "--recursive", "--output", "--config", "--input", "--file", "--path", "--name", "--model", "--port", "--host", "--user", "--token", "--rebase", "--amend", "--squash", "--admin", "--delete", "--global", "--local", "--force-with-lease")

// genericWords are plain words that show up in backticks without naming a
// subject.
var genericWords = set("branch", "commit", "status", "rebase", "origin", "master", "format", "config", "should", "script", "failed", "passed", "string", "number", "return", "option", "output", "result", "reason", "action", "target", "source", "module", "public", "private", "cargo", "python", "docker", "gradle", "mutex", "stdout", "stderr", "stdin", "bash", "async", "error", "errors", "empty", "total", "daily", "hourly", "nightly", "latest", "stable", "default", "enabled", "disabled", "yes", "owner", "agent", "agents", "claude", "codex", "github", "gitlab", "sglang", "memra", "tacit", "valkey", "markdown", "frontmatter", "session", "sessions", "worktree", "workflow", "workflows", "timeout", "deadline", "release", "review", "reviews")

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// classify says what a token is. kind is "path", "ident" or "" (not a
// subject term). A ~/projects/<repo>/ prefix is returned as repo and
// stripped.
func classify(tok string, ticked bool) (kind, norm, repo string) {
	tok = strings.TrimRight(strings.Trim(tok, " \t,;:!?()[]{}\"'"), ".")
	if tok == "" || len(tok) > 120 || strings.ContainsAny(tok, "$<>|&@\\") || strings.Contains(tok, "://") {
		return "", "", ""
	}
	if m := reRepoPath.FindStringSubmatch(tok); m != nil {
		repo = m[1]
		tok = strings.TrimSpace(m[2])
		if tok == "" {
			return "", "", repo
		}
	}
	if i := strings.IndexByte(tok, '='); i > 0 {
		tok = tok[:i]
	}
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") {
		return "", "", repo
	}
	tok = strings.TrimPrefix(tok, "./")
	if len(tok) < 3 {
		return "", "", repo
	}
	if strings.HasPrefix(tok, "--") {
		if len(tok) >= 6 && reFlag.MatchString(tok) && !genericFlags[tok] {
			return "ident", tok, repo
		}
		return "", "", repo
	}
	if strings.Contains(tok, "/") {
		segs := strings.Split(strings.TrimSuffix(tok, "/"), "/")
		for _, s := range segs {
			// "." and ".." would send git outside the repo or nowhere.
			if s == "" || s == "." || s == ".." || !rePathSeg.MatchString(s) {
				return "", "", repo
			}
		}
		// A truncated token ("ADS-" from "ADS-<name>.md") or figures such as
		// 1.28x/2.33x name no file.
		if last := tok[len(tok)-1]; last == '-' || last == '_' || !reLetters.MatchString(tok) || !strings.ContainsAny(tok, "abcdefghijklmnopqrstuvwxyz._-") {
			return "", "", repo
		}
		dir := strings.HasSuffix(tok, "/")
		if len(tok) < 5 || !(dir || strings.ContainsAny(tok, "*") || reExt.MatchString(tok) || len(segs) >= 3) {
			return "", "", repo
		}
		// "none/ins/park/drop": a list of short words, not a path.
		if !dir && !reExt.MatchString(tok) && !strings.ContainsAny(tok, "*_-.") {
			long := false
			for _, sg := range segs {
				long = long || len(sg) >= 6
			}
			if !long {
				return "", "", repo
			}
		}
		// "and/or", "read/write": two plain words, no extension.
		if len(segs) == 2 && !dir && !reExt.MatchString(tok) && !strings.Contains(tok, "*") && !strings.ContainsAny(tok, "_-.") {
			return "", "", repo
		}
		return "path", tok, repo
	}
	if reFile.MatchString(tok) {
		return "path", "**/" + tok, repo
	}
	switch {
	case reSnake.MatchString(tok) && len(tok) >= 6:
		return "ident", tok, repo
	case ticked && reScoped.MatchString(tok) && len(tok) >= 6:
		return "ident", tok, repo
	case ticked && reCamel.MatchString(tok) && len(tok) >= 8:
		return "ident", tok, repo
	case ticked && reKebab.MatchString(tok) && len(tok) >= 8 && !genericWords[tok]:
		return "ident", tok, repo
	case ticked && reLower.MatchString(tok) && !genericWords[tok]:
		return "ident", tok, repo
	}
	return "", "", repo
}

// SubjectOf reads the subject terms and repos out of facts. Scores decide
// which terms survive the caps: what the writer put in applies_to (symbols
// 100, paths 90, command paths and flags 85) comes first, then backticked
// paths (80), flags and snake_case names (70), then bare tokens (60) and
// other backticked words (40).
func (g *Git) SubjectOf(facts ...*fact.Fact) Subject {
	repos := g.Repos()
	c := &collector{g: g, cands: map[string]*cand{}, mention: map[string]int{}, hint: map[string]int{}}
	var primary, secondary []string
	seen := map[string]bool{}
	addRepo := func(list *[]string, name string) {
		name = strings.TrimSpace(name)
		for _, n := range []string{name, strings.TrimSuffix(name, "-private"), strings.TrimSuffix(name, "-fork")} {
			if n != "" && repos[n] {
				if !seen[n] {
					seen[n] = true
					*list = append(*list, n)
				}
				return
			}
		}
	}
	for _, f := range facts {
		if f == nil {
			continue
		}
		if rest, ok := strings.CutPrefix(f.Meta.Scope, "projects/"); ok {
			addRepo(&primary, rest)
		}
	}
	for _, f := range facts {
		if f == nil {
			continue
		}
		for _, r := range f.Meta.AppliesTo.Repos {
			addRepo(&secondary, r)
		}
		a := f.Meta.AppliesTo
		for _, p := range a.Paths {
			if k, n, r := classify(p, true); k == "path" {
				c.add(n, true, 90)
				c.hint[r]++
			} else if k == "ident" {
				c.add(n, false, 90)
			}
		}
		for _, s := range a.Symbols {
			s = strings.Trim(strings.TrimSpace(s), "()")
			if len(s) >= 4 && !strings.ContainsAny(s, " \t") {
				c.add(s, false, 100)
			}
		}
		for _, cmd := range a.Commands {
			for _, tok := range strings.Fields(cmd) {
				if k, n, _ := classify(tok, false); k == "path" {
					c.add(n, true, 85)
				} else if k == "ident" && (strings.HasPrefix(n, "--") || strings.Contains(n, "_")) {
					c.add(n, false, 85)
				}
			}
		}
		text := f.Meta.Description + "\n" + f.Body
		inTick := map[string]bool{}
		for _, m := range reTick.FindAllStringSubmatchIndex(text, -1) {
			span := text[m[2]:m[3]]
			for _, tok := range strings.Fields(span) {
				if r := repoNameOf(tok); r != "" {
					c.mention[r] += 3
				}
				k, n, r := classify(tok, true)
				if r != "" {
					c.hint[r]++
				}
				switch k {
				case "path":
					c.add(n, true, 80)
				case "ident":
					sc := 40
					if strings.HasPrefix(n, "--") || strings.Contains(n, "_") {
						sc = 70
					}
					c.add(n, false, sc)
				}
				inTick[tok] = true
			}
		}
		plain := reTick.ReplaceAllString(text, " ")
		for _, tok := range reWord.FindAllString(plain, -1) {
			if k, n, r := classify(tok, false); k == "path" {
				c.add(n, true, 60)
				if r != "" {
					c.hint[r]++
				}
			} else if k == "ident" {
				c.add(n, false, 60)
			} else if r != "" {
				c.hint[r]++
			}
			if r := repoNameOf(tok); r != "" {
				c.mention[r]++
			}
		}
	}
	// Mentions: a repo named by a path into it or in backticks counts; a bare
	// name counts when it is not an ordinary word.
	type mr struct {
		name string
		w    int
	}
	var ms []mr
	for n := range repos {
		w := c.mention[n] + 3*c.hint[n]
		if w == 0 || len(n) < 4 || (ordinaryWord[n] || g.Ordinary[n]) && c.hint[n] == 0 && c.mention[n] < 3 {
			continue
		}
		if !seen[n] {
			ms = append(ms, mr{n, w})
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].w != ms[j].w {
			return ms[i].w > ms[j].w
		}
		return ms[i].name < ms[j].name
	})
	var mentioned []string
	for _, m := range ms {
		mentioned = append(mentioned, m.name)
	}
	var all []string
	all = append(all, primary...)
	all = append(all, secondary...)
	room := g.maxRepos() - len(all)
	if room > 0 && len(mentioned) > 0 {
		// A fact scoped to one repo names other repos in passing; a fact
		// outside every repo (user, system, machine) is about the ones it names.
		if len(primary) > 0 && room > 1 {
			room = 1
		}
		if len(mentioned) > room {
			mentioned = mentioned[:room]
		}
		all = append(all, mentioned...)
	}
	if len(all) > g.maxRepos() {
		all = all[:g.maxRepos()]
	}
	// Order terms by score, then by first appearance.
	var cs []*cand
	for _, x := range c.cands {
		// A repo's own name says nothing about a change inside it.
		if !x.path && repos[x.s] {
			continue
		}
		cs = append(cs, x)
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].order < cs[j].order
	})
	s := Subject{Repos: all}
	for _, x := range cs {
		if x.path && len(s.Paths) < maxPaths {
			s.Paths = append(s.Paths, x.s)
		} else if !x.path && len(s.Idents) < maxIdents {
			s.Idents = append(s.Idents, x.s)
		}
	}
	return s
}

// ordinaryWord are repo names that are also plain words; a bare mention of
// one is not evidence the fact is about that repo. A machine adds its own
// through Git.Ordinary (ordinary_names in derive.yaml).
var ordinaryWord = set("tools", "tacit", "docs", "web", "api", "app", "site", "notes")

func repoNameOf(tok string) string {
	tok = strings.Trim(tok, " \t.,;:!?()[]{}\"'`")
	if m := reRepoPath.FindStringSubmatch(tok); m != nil {
		return m[1]
	}
	return strings.ToLower(tok)
}

// isGlob reports whether a path term has glob characters.
func isGlob(p string) bool { return strings.ContainsAny(p, "*?[{") }

// literalPrefix is the directory part of a glob before its first wildcard.
func literalPrefix(p string) string {
	i := strings.IndexAny(p, "*?[{")
	if i < 0 {
		return p
	}
	pre := p[:i]
	if j := strings.LastIndexByte(pre, '/'); j >= 0 {
		return pre[:j+1]
	}
	return ""
}

// globMatch matches a repo-relative path against a pathspec glob where ** crosses
// directories and * does not.
func globMatch(pattern, name string) bool {
	return reGlob(pattern).MatchString(name)
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

func reGlob(p string) *regexp.Regexp {
	globMu.Lock()
	defer globMu.Unlock()
	if re, ok := globCache[p]; ok {
		return re
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch ch := p[i]; ch {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	if strings.HasSuffix(p, "/") {
		b.WriteString(".*")
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	globCache[p] = re
	return re
}
