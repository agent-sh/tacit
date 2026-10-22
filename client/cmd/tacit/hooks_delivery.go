package main

// Repo-first delivery (SPEC.md, Delivery): the SessionStart repo block, the
// prompt block's metadata and action lines, the instruction line, and the
// matching the PreToolUse push shares with the metadata line. Each part has
// its own setting (delivery.* in the config file, TACIT_DELIVERY_* in the
// environment), so it can be measured and turned off; eval/VALUE.md,
// "Repo-first delivery", has the measurements behind each default.
import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/search"
)

// Defaults of the delivery settings. The ceilings are hard limits against
// context creep, not targets: each block takes fewer when fewer qualify.
const (
	// startCeiling: the repo block's most lines. With the type threshold
	// (rules, standards, decisions, past mistakes of this repo), it is what
	// bounds a repo with many of them.
	startCeiling = 6
	// promptCeiling: the prompt block's most curated lines above the floor.
	promptCeiling = 5
	// promptRelative: a curated prompt line also needs this share of the
	// best candidate's score, so a weak tail does not ride on a strong head.
	// 0 turns it off (then promptCeiling alone bounds the block).
	promptRelative = 0.7
	// metaCeiling: lines matched on applies_to paths and commands, per
	// prompt and per tool call.
	metaCeiling = 1
)

// deliveryOn reads an on/off delivery setting: TACIT_DELIVERY_<key>.
func deliveryOn(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TACIT_DELIVERY_" + key))) {
	case "on", "1", "true", "yes":
		return true
	case "off", "0", "false", "no":
		return false
	}
	return def
}

func deliveryInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TACIT_DELIVERY_" + key))); err == nil && v >= 0 {
		return v
	}
	return def
}

func deliveryFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("TACIT_DELIVERY_"+key)), 64); err == nil && v >= 0 && v <= 1 {
		return v
	}
	return def
}

// instruction is the one line that says what tacit holds and when to ask
// it. Generic on purpose: no list of actions, so it reads the same in any
// repo and any store. It rides in the SessionStart title and in the MCP
// server's instructions, which the harness shows while the tools are
// deferred.
func instruction(mcp bool) string {
	tool := "`tacit search <what you will do>`"
	if mcp {
		tool = "`mcp__tacit__tacit_search`"
	}
	return "tacit holds this owner's rules, decisions and past mistakes. Before an action that changes something, run one " + tool + " for it."
}

// mcpInstructions is the MCP server's instructions, its one string: the
// instruction line with the tools' bare names, as the server names them,
// and where to see what the store holds. Generic like the line: no list of
// uses.
const mcpInstructions = "tacit holds this owner's rules, decisions and past mistakes. Before an action that changes something, run one tacit_search for it; tacit_tree shows what it holds."

// tier orders what changes how an agent acts in a repo: the owner's rules
// and standards (0), then decisions and past mistakes (1); everything else
// (history, references, host and machine facts) is 2 and stays out of the
// repo-first lines.
func tier(h search.Hit) int {
	switch {
	case h.Ruling || h.Type == "user_standard":
		return 0
	case h.Type == "design_decision" || h.Type == "past_mistake" || h.Type == fact.StatementType:
		return 1
	}
	return 2
}

// subRank orders inside the first tier: owner rules, then rulings in their
// task, then the standards an agent recorded.
func subRank(h search.Hit) int {
	switch {
	case h.Rule():
		return 0
	case h.Ruling:
		return 1
	case tier(h) == 0:
		return 2
	}
	return 3
}

// repoNames is the session's repo and its verified aliases, lowercased and
// distinct.
func repoNames(sit search.Situation) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range append([]string{sit.Repo}, sit.RepoAliases...) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// repoTied: the fact is this repo's by its metadata (Index.Tied's rule).
func repoTied(h search.Hit, names []string) bool {
	for _, r := range h.Applies.Repos {
		for _, n := range names {
			if strings.EqualFold(r, n) {
				return true
			}
		}
	}
	if len(h.Applies.Repos) > 0 {
		return false
	}
	for _, n := range names {
		if strings.EqualFold(h.Scope, "projects/"+n) {
			return true
		}
	}
	return false
}

// repoStartHits is the SessionStart repo block: the facts tied to this repo
// by metadata (Index.Tied), of the first two tiers only (rules and
// standards, then decisions and past mistakes; no history, reference or
// machine notes), each tier ordered by the BM25 score of the repo's names
// against the fact: a fact about the repo as a whole ("every commit to the
// repo is signed off") before one about a part of it. At most startCeiling.
// ok is false when the session has no repo, and the caller falls back to the
// host block.
func repoStartHits(ctx context.Context, ix *search.Index, sit search.Situation) (hits []search.Hit, ok bool, err error) {
	names := repoNames(sit)
	if sit.Repo == "" || len(names) == 0 {
		return nil, false, nil
	}
	tied, err := ix.Tied(ctx, names)
	if err != nil {
		return nil, true, err
	}
	var keep []search.Hit
	local := 0
	for _, h := range tied {
		if tier(h) > 1 || h.Quiet {
			continue // a dead or shipped folder's facts are quiet in hooks
		}
		if h.Local && !overlay.IsCurated(h.Path) {
			if local >= startLocalLimit {
				continue
			}
			local++
		}
		keep = append(keep, h)
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if a, b := subRank(keep[i]), subRank(keep[j]); a != b {
			return a < b
		}
		if keep[i].Created != keep[j].Created {
			return keep[i].Created > keep[j].Created
		}
		return keep[i].Name < keep[j].Name
	})
	keep = byScore(keep, startScores(ctx, ix, sit, strings.Join(repoNames(sit), " ")))
	if c := deliveryInt("START_CEILING", startCeiling); len(keep) > c {
		keep = keep[:c]
	}
	return keep, true, nil
}

// startScores is the BM25 score of each fact of the repo for query q.
func startScores(ctx context.Context, ix *search.Index, sit search.Situation, q string) map[string]float64 {
	score := map[string]float64{}
	if q == "" {
		return score
	}
	s := sit
	s.Query, s.RestrictRepos, s.PromptRelevance, s.Derived, s.OnlyDerived = q, true, false, false, false
	hits, err := ix.Query(ctx, s, 400, false)
	if err != nil {
		return score
	}
	for _, h := range hits {
		score[h.Path] = h.Score
	}
	return score
}

// byScore orders keep by tier (rules and standards, then decisions and past
// mistakes), then by score, keeping the kind order for ties.
func byScore(keep []search.Hit, score map[string]float64) []search.Hit {
	sort.SliceStable(keep, func(i, j int) bool {
		if a, b := tier(keep[i]), tier(keep[j]); a != b {
			return a < b
		}
		return score[keep[i].Path] > score[keep[j].Path]
	})
	return keep
}

// promptCaps are the prompt block's ceiling and relative threshold.
func promptCaps() (ceiling int, relative float64) {
	return max(1, deliveryInt("PROMPT_CEILING", promptCeiling)), deliveryFloat("PROMPT_RELATIVE", promptRelative)
}

// --- metadata matching: applies_to.paths and applies_to.commands ---

// promptPathRx finds what a prompt names as a path: a token with a slash
// (scripts/dump.sh, src/**) or a file name with an extension (tsup.config.ts).
var promptPathRx = regexp.MustCompile(`[\w.*-]*[\w*]/[\w./*-]*[\w*]|[\w-]+(?:\.[\w-]+)*\.[A-Za-z][\w]{1,7}\b`)

// pathMatch reports whether a fact's applies_to path glob covers the
// repo-relative path p: a glob match, a directory prefix (src/** or src
// covers src/a/b.ts), a **/ glob on the base name, or a bare file name
// anywhere.
func pathMatch(glob, p string) bool {
	glob = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(glob), "./"), "/")
	p = strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(p), "./"), "/")
	if glob == "" || p == "" || strings.HasPrefix(glob, "~") || glob == "**" || glob == "*" {
		return false
	}
	if rest, ok := strings.CutPrefix(glob, "**/"); ok {
		if m, _ := filepath.Match(rest, filepath.Base(p)); m {
			return true
		}
		glob = rest
	}
	if m, _ := filepath.Match(glob, p); m {
		return true
	}
	dir := strings.TrimSuffix(strings.TrimSuffix(glob, "/**"), "/")
	if !strings.ContainsAny(dir, "*?[") && (p == dir || strings.HasPrefix(p, dir+"/")) {
		return true
	}
	return !strings.Contains(glob, "/") && !strings.ContainsAny(glob, "*?[") && filepath.Base(p) == glob
}

// cmdWords splits a command into lowercase words.
func cmdWords(s string) []string { return strings.Fields(strings.ToLower(s)) }

// cmdBoundary splits a shell command line at its separators, so each part
// starts with a program.
var cmdBoundary = regexp.MustCompile(`&&|\|\||[;|&()\n]|\$\(|` + "`")

// commandMatch reports whether the fact's applies_to command (a prefix such
// as "git commit" or "tsup") is how one of the shell command's parts starts,
// after any environment assignments (FOO=1 make), or how it goes on after
// the part's first word when the second is not a flag: a program that runs
// the next word as its own (npx vitest, sudo make, do make in a shell loop)
// runs that command, with no list of such programs. The program is compared
// by base name, so ./node_modules/.bin/tsup runs tsup.
func commandMatch(factCmd, command string) bool {
	fw := cmdWords(factCmd)
	if len(fw) == 0 {
		return false
	}
	for _, part := range cmdBoundary.Split(command, -1) {
		w := cmdWords(part)
		for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") {
			w = w[1:]
		}
		if startsWith(w, fw) || (len(w) > 1 && !strings.HasPrefix(w[1], "-") && startsWith(w[1:], fw)) {
			return true
		}
	}
	return false
}

// startsWith reports whether the command words w start with the fact's
// command fw, the program by base name.
func startsWith(w, fw []string) bool {
	return len(w) >= len(fw) && filepath.Base(w[0]) == filepath.Base(fw[0]) && slicesEqual(w[1:len(fw)], fw[1:])
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// promptNamesCommand reports whether a prompt asks for the fact's command:
// the whole command as words, or its last word (the action: "commit" for
// "git commit", "tsup" for "tsup"), when that word is not a flag and has
// four letters or more.
func promptNamesCommand(factCmd, prompt string) bool {
	fw := cmdWords(factCmd)
	if len(fw) == 0 {
		return false
	}
	words := stems(search.Words(prompt))
	if run := stems(search.Words(factCmd)); len(run) > 1 && containsRun(words, run) {
		return true
	}
	last := fw[len(fw)-1]
	if strings.HasPrefix(last, "-") || len([]rune(last)) < 4 || strings.ContainsAny(last, "/=<>$.") {
		return false
	}
	return slicesContains(words, search.Stem(last))
}

func stems(ws []string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = search.Stem(w)
	}
	return out
}

func containsRun(words, run []string) bool {
	for i := 0; i+len(run) <= len(words); i++ {
		if slicesEqual(words[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// metaMatch is one fact matched on its metadata, with how specific the
// match is: a path match by the depth of its glob, above any command match.
type metaMatch struct {
	hit  search.Hit
	spec int
}

// metaSelect picks, among the facts tied to this repo, the rules, standards,
// decisions and past mistakes whose applies_to paths cover one of paths, or
// whose commands the shell command runs or the prompt names: the best limit
// of them, most specific first, then by tier, newest first, less the ones
// the session has seen. No backfill: a match the session already has keeps
// its place, so a repeated request or a second edit of a file does not pull
// in the next, weaker match.
func metaSelect(all []search.Hit, names []string, paths []string, command, prompt string, seen map[string]bool, limit int) []search.Hit {
	var ms []metaMatch
	for _, h := range all {
		if tier(h) > 1 || !repoTied(h, names) {
			continue
		}
		spec := 0
		for _, g := range h.Applies.Paths {
			for _, p := range paths {
				if pathMatch(g, p) {
					spec = max(spec, 10+strings.Count(strings.Trim(g, "/"), "/"))
				}
			}
		}
		for _, c := range h.Applies.Commands {
			if (command != "" && commandMatch(c, command)) || (prompt != "" && promptNamesCommand(c, prompt)) {
				spec = max(spec, 1+len(cmdWords(c)))
			}
		}
		if spec > 0 {
			ms = append(ms, metaMatch{h, spec})
		}
	}
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.spec != b.spec {
			return a.spec > b.spec
		}
		if subRank(a.hit) != subRank(b.hit) {
			return subRank(a.hit) < subRank(b.hit)
		}
		return a.hit.Created > b.hit.Created
	})
	var out []search.Hit
	for i, m := range ms {
		if i >= limit {
			break
		}
		if !seen[m.hit.Name] {
			out = append(out, m.hit)
		}
	}
	return out
}

// withSeen is seen plus the names of hits.
func withSeen(seen map[string]bool, hits ...[]search.Hit) map[string]bool {
	out := make(map[string]bool, len(seen))
	for k, v := range seen {
		out[k] = v
	}
	for _, hs := range hits {
		for _, h := range hs {
			out[h.Name] = true
		}
	}
	return out
}

// promptMetaHits is the prompt block's metadata line: a fact of this repo
// whose applies_to paths cover a path the prompt names, or whose command
// the prompt names.
func promptMetaHits(ctx context.Context, ix *search.Index, sit search.Situation, p string, seen map[string]bool) []search.Hit {
	if sit.Repo == "" || !deliveryOn("PROMPT_METADATA", true) {
		return nil
	}
	all, err := ix.MetaFacts(ctx)
	if err != nil {
		return nil
	}
	return metaSelect(all, repoNames(sit), promptPathRx.FindAllString(p, -1), "", p, seen, deliveryInt("META_CEILING", metaCeiling))
}

// promptActionHits is the prompt block's action line: the best-scoring rule,
// standard, decision or past mistake of this repo for the prompt, above the
// rule slot's floor, when the block carries none of them. It has no list of
// actions: the facts' own words decide, as for the rest of the block, but
// without the coherence gate, which general rules ("each commit carries its
// own test") rarely pass against task words. Like the rule slot it is not
// backfilled: when the best one was seen, nothing is added.
func promptActionHits(ctx context.Context, ix *search.Index, sit search.Situation, p string, seen map[string]bool, block []search.Hit) []search.Hit {
	if sit.Repo == "" || !deliveryOn("PROMPT_ACTION", true) {
		return nil
	}
	for _, h := range block {
		if tier(h) <= 1 && repoTied(h, repoNames(sit)) {
			return nil // the block already carries one
		}
	}
	s := sit
	s.Query, s.RestrictRepos, s.PromptRelevance, s.Derived, s.OnlyDerived = p, true, false, false, false
	hits, err := ix.Query(ctx, s, 50, false)
	if err != nil {
		return nil
	}
	names := repoNames(sit)
	for _, h := range hits {
		if h.Score < ruleSlotFloor {
			break
		}
		if tier(h) <= 1 && repoTied(h, names) && !h.Local {
			if seen[h.Name] || slices.ContainsFunc(block, func(b search.Hit) bool { return b.Name == h.Name }) {
				return nil
			}
			return []search.Hit{h}
		}
	}
	return nil
}
