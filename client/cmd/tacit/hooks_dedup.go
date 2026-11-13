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
			if strings.HasPrefix(g, "~"+string(filepath.Separator)) {
				home, _ := os.UserHomeDir()
				g = filepath.Join(home, g[2:])
			}
			if ms, err := filepath.Glob(g); err == nil {
				paths = append(paths, ms...)
			} else {
				paths = append(paths, g)
			}
		}
	}
	for _, p := range paths {
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

// holdsFact reports whether the corpus covers the fact as a duplicate: the
// shares of dedupWordCov of its name and description words and dedupNameCov
// of its name's, with dedupMinHit words matched at least. A fact with fewer
// words than the minimum is never held.
func (c dedupCorpus) holdsFact(h search.Hit) bool {
	if len(c) == 0 {
		return false
	}
	name := dedupWords(strings.ReplaceAll(h.Name, "-", " "))
	all := append(append([]string{}, name...), dedupWords(h.Description)...)
	if len(all) < dedupMinHit || len(name) == 0 {
		return false
	}
	hit := 0
	for _, w := range all {
		if c[w] {
			hit++
		}
	}
	nhit := 0
	for _, w := range name {
		if c[w] {
			nhit++
		}
	}
	return float64(hit)/float64(len(all)) >= dedupWordCov && float64(nhit)/float64(len(name)) >= dedupNameCov
}

// withoutDuplicates is hits less the facts the corpus already holds, logged
// as held by the dedup gate. Derived facts (generated maps, hot files) are
// never held: they are not rules restated anywhere.
func withoutDuplicates(hits []search.Hit, c dedupCorpus) []search.Hit {
	if len(c) == 0 {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if !isDerived(h) && c.holdsFact(h) {
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
