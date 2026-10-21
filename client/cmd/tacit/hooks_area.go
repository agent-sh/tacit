package main

// Area and topic delivery (SPEC.md, "Delivery", areas and topics): an
// area's facts reach a session when it is about to use the area's tools,
// a topic's when the prompt names the topic. Code matches names; no model.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
)

// areaCeiling is how many of an area's facts the PreToolUse hook prints
// the first time the session uses the area's tools. Not measured yet: no
// replay case holds an area (eval/VALUE.md); TACIT_DELIVERY_AREA_CEILING.
const areaCeiling = 3

// areaSeen is the delivered-set line that says an area was pushed in the
// session. A fact name is a slug, so it never collides.
func areaSeen(folder string) string { return "area:" + folder }

// matchedAreas are the areas whose tools the call's tool name matches, or
// whose commands its shell command runs, that push (record.Pushes), in
// folder order.
func matchedAreas(recs map[string]*record.Record, toolName, command string) []string {
	var out []string
	for f, r := range recs {
		if !r.Pushes() || strings.Count(f, "/") != 1 {
			continue
		}
		hit := r.MatchesTool(toolName)
		for _, c := range r.Commands {
			if command != "" && commandMatch(c, command) {
				hit = true
			}
		}
		if hit {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// areaHits picks, for one area, the facts the push prints: tiers 0 and 1
// (rulings first, then standards, then decisions and past mistakes), a
// fact whose own applies_to.commands the call runs first, newest first
// within each, at most limit, less what the session has seen. Its topics'
// facts are not pushed; prompt and search reach them.
func areaHits(facts []search.Hit, command string, seen map[string]bool, limit int) []search.Hit {
	var keep []search.Hit
	for _, h := range facts {
		if tier(h) <= 1 && !seen[h.Name] {
			keep = append(keep, h)
		}
	}
	runs := func(h search.Hit) bool {
		for _, c := range h.Applies.Commands {
			if command != "" && commandMatch(c, command) {
				return true
			}
		}
		return false
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if a, b := runs(keep[i]), runs(keep[j]); a != b {
			return a
		}
		if a, b := subRank(keep[i]), subRank(keep[j]); a != b {
			return a < b
		}
		if keep[i].Created != keep[j].Created {
			return keep[i].Created > keep[j].Created
		}
		return keep[i].Name < keep[j].Name
	})
	if len(keep) > limit {
		keep = keep[:limit]
	}
	return keep
}

// areaPush is the area part of the PreToolUse block: for each matching
// area the session has not been pushed yet, its facts (areaHits) and one
// line pointing at the area in the tree. pushed are the areas it printed.
func areaPush(ctx context.Context, ix *search.Index, recs map[string]*record.Record, toolName, command string, seen map[string]bool) (hits []search.Hit, lines []string, pushed []string) {
	limit := deliveryInt("AREA_CEILING", areaCeiling)
	for _, f := range matchedAreas(recs, toolName, command) {
		if seen[areaSeen(f)] || ctx.Err() != nil {
			continue
		}
		facts, err := ix.List(ctx, []string{f}, "", false, false)
		if err != nil {
			continue
		}
		hs := areaHits(facts, command, withSeen(seen, hits), limit)
		pushed = append(pushed, f)
		if len(hs) == 0 {
			continue
		}
		hits = append(hits, hs...)
		lines = append(lines, fmt.Sprintf("# more on %s: `tacit_tree %s` (%d facts)", recs[f].Name, f, len(facts)))
	}
	return hits, lines, pushed
}

// topicScopes are the scopes of the live topics a prompt names: the
// topic's name or one of its aliases as a whole run of words, compared by
// stem (as the prompt's command line is, promptNamesCommand). They join the
// prompt's scopes for this prompt only; their facts still pass the score
// gates.
func topicScopes(recs map[string]*record.Record, prompt string) []string {
	words := stems(search.Words(prompt))
	var out []string
	for f, r := range recs {
		if r.Kind != record.KindTopic || !r.ReadByDefault() {
			continue
		}
		for _, n := range r.Names() {
			if run := stems(search.Words(n)); len(run) > 0 && containsRun(words, run) {
				out = append(out, f)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// hookRecords are the index's folder records, nil when it has none (an
// index of an older schema, or a store without records): delivery then
// runs as before records existed.
func hookRecords(ctx context.Context, ix *search.Index) map[string]*record.Record {
	recs, err := ix.Records(ctx)
	if err != nil || len(recs) == 0 {
		return nil
	}
	return recs
}

// withRecordAliases is the situation with the names the records give its
// repository: a folder suppressed into it, the folder it is suppressed
// into, and the winner's aliases (suppressionNames), read from the index.
func withRecordAliases(sit search.Situation, recs map[string]*record.Record) search.Situation {
	if sit.Repo == "" || len(recs) == 0 {
		return sit
	}
	names := suppressionNames(recs, repoNames(sit))
	sit.RepoAliases = names[1:]
	return sit
}
