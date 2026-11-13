package main

// Instruction dedup (SPEC.md, "Delivery", "Duplicates"): a fact whose content
// words the session's own instruction files already hold is not delivered
// again. The corpus is each repository's AGENTS.md plus the files
// delivery.dedup_files lists; the match is word coverage, measured on the
// graded deliveries of a real installation (eval/DELIVERY.md, "Duplicates"):
// a fact whose name and description words the corpus covers this widely is
// one the reader already has, whatever its wording.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The coverage a duplicate needs: this share of the fact's name and
// description content words in the corpus, this share of its name's, and this
// many matched words at least. Below the name share a fact is one the corpus
// only talks around: the two kept it from holding measured useful lines.
const (
	dedupWordCov = 0.75
	dedupNameCov = 0.60
	dedupMinHit  = 4
)

// dedupStop are words too common to say a fact and its corpus text are the
// same rule. The grammar words the search terms drop, and the words every
// instruction file carries (run, file, session).
var dedupStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("the and for with that this from into over under about than then there here what when where which who why how use using used should can could would will are was were been being have has had not does did you your our its it's dont don't don doesn isn won please want need like just also a an to of in on at by as is it be or if so we they he she them his her their these those") {
		dedupStop[w] = true
	}
}

// dedupWords are a text's content words: lowercase runs of letters, digits
// and _ . - three characters or more, no stop words, as the measured matcher
// tokenized them. A run keeps its separators: fast-forward is one word.
func dedupWords(text string) []string {
	var out []string
	for _, w := range dedupToken.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(w)) >= 3 && !dedupStop[w] {
			out = append(out, w)
		}
	}
	return out
}

var dedupToken = regexp.MustCompile(`[a-z0-9_][a-z0-9_.-]*`)

// dedupCorpus is the content-word set of the instruction files a session
// already carries.
type dedupCorpus map[string]bool

// loadDedupCorpus reads them: each repository's AGENTS.md, and the colon
// separated globs of delivery.dedup_files (TACIT_DELIVERY_DEDUP_FILES, ~
// expanded). A file that is missing or unreadable reads empty; the corpus
// never fails a hook.
func loadDedupCorpus(repos []repoid.Repo) dedupCorpus {
	c := dedupCorpus{}
	if !deliveryOn("DEDUP", true) {
		return c
	}
	var paths []string
	for _, r := range repos {
		if r.Worktree != "" {
			paths = append(paths, filepath.Join(r.Worktree, "AGENTS.md"))
		}
	}
	if list := deliveryString("DEDUP_FILES"); list != "" {
		for _, g := range strings.Split(list, ":") {
			if g == "" {
				continue
			}
			// A leading ~ is the home directory under either separator;
			// a ~user path is not this home and reads empty.
			if g == "~" || strings.HasPrefix(g, "~/") || strings.HasPrefix(g, "~\\") {
				home, err := os.UserHomeDir()
				if err != nil || home == "" {
					continue
				}
				g = filepath.Join(home, strings.TrimLeft(g[1:], `/\`))
			}
			if strings.HasPrefix(g, "~") {
				continue
			}
			if ms, err := filepath.Glob(g); err == nil {
				paths = append(paths, ms...)
			} else {
				paths = append(paths, g)
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, w := range dedupWords(string(b)) {
			c[w] = true
		}
	}
	return c
}

// lazyDedupCorpus loads the dedup corpus at most once per hook call and only
// when a candidate fact can actually be held.
type lazyDedupCorpus struct {
	repos  []repoid.Repo
	loaded bool
	corpus dedupCorpus
}

func (l *lazyDedupCorpus) get() dedupCorpus {
	if !l.loaded {
		l.corpus = loadDedupCorpus(l.repos)
		l.loaded = true
	}
	return l.corpus
}

// holdsFact reports whether the corpus covers the fact as a duplicate: the
// shares of dedupWordCov of its name and description words and dedupNameCov
// of its name's, with dedupMinHit words matched at least (a fact the corpus
// only talks around, or covers with fewer words, stays).
func (c dedupCorpus) holdsFact(h search.Hit) bool {
	if len(c) == 0 {
		return false
	}
	name := dedupWords(strings.ReplaceAll(h.Name, "-", " "))
	desc := dedupWords(h.Description)
	total := len(name) + len(desc)
	if total < dedupMinHit || len(name) == 0 {
		return false
	}
	// A corpus file that literally cites the hyphenated fact name holds its
	// name words even though dedupWords keeps hyphenated tokens whole.
	nameHeld := c[strings.ToLower(h.Name)]
	hit, nhit := 0, 0
	for _, w := range name {
		if nameHeld || c[w] {
			hit++
			nhit++
		}
	}
	for _, w := range desc {
		if c[w] {
			hit++
		}
	}
	return hit >= dedupMinHit && float64(hit)/float64(total) >= dedupWordCov && float64(nhit)/float64(len(name)) >= dedupNameCov
}

// withoutDuplicates is hits less the facts the corpus already holds, logged
// as held by the dedup gate. Only a rule, standard, decision or past mistake
// is held (tier 0 or 1: what an instruction file restates), never a derived
// fact, which restates nothing.
func withoutDuplicates(hits []search.Hit, load func() dedupCorpus) []search.Hit {
	if len(hits) == 0 || !deliveryOn("DEDUP", true) {
		return hits
	}
	eligible := false
	for _, h := range hits {
		if tier(h) <= 1 && !isDerived(h) {
			eligible = true
			break
		}
	}
	if !eligible {
		return hits
	}
	c := load()
	if len(c) == 0 {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if tier(h) <= 1 && !isDerived(h) && c.holdsFact(h) {
			continue
		}
		out = append(out, h)
	}
	logDropped(hits, out, "dedup")
	return out
}

// coversAnyPath keeps the facts whose applies_to paths cover one of the
// repo-relative paths the call writes; a command-keyed fact without a path
// match is left out.
func coversAnyPath(hits []search.Hit, rel []string) []search.Hit {
	if len(rel) == 0 {
		return nil
	}
	var out []search.Hit
	for _, h := range hits {
		found := false
		for _, g := range h.Applies.Paths {
			for _, p := range rel {
				if pathMatch(g, p) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			out = append(out, h)
		}
	}
	return out
}

// pushDedupRepos are the repositories whose AGENTS.md a tool call's dedup
// corpus reads: the session's repository when the call is in one, else the
// workspace's repositories when it matched one of them.
func pushDedupRepos(repo repoid.Repo, inRepo bool, ws []repoid.Repo) []repoid.Repo {
	if inRepo {
		return []repoid.Repo{repo}
	}
	return ws
}
