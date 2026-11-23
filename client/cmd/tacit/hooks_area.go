package main

// Area and topic delivery (SPEC.md, "Delivery", areas and topics): an
// area's facts reach a session when it is about to use the area's tools,
// a topic's when the prompt names the topic. Code matches names; no model.

import (
	"context"
	"sort"

	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/tree"
)

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

// withOrgs is the situation with its repository's display name (the
// record's: "storefront (GitHub: shop-v2)") and the org folders of its forge
// organization (the records' org, tree.Session.Orgs).
func withOrgs(sit search.Situation, recs map[string]*record.Record) search.Situation {
	if len(recs) == 0 || len(repoNames(sit)) == 0 {
		return sit
	}
	if f := record.RepoOwner(recs, sit.Repo); sit.Repo != "" && f != "" {
		if w, _, err := record.Resolve(recs, f); err == nil && recs[w] != nil {
			sit.RepoDisplay = recs[w].Display()
			// A dead repository another project replaced: that project's rules
			// come lightly with its own (successorStartHits).
			if sc := recs[w].Successor; sc != "" && recs[sc] != nil && recs[sc].Live() && !recs[sc].Quiet() {
				sit.Successors = []string{sc}
				sit.RepoDisplay += " (dead, replaced by " + recs[sc].Display() + ")"
			}
		}
	}
	sit.Orgs = tree.Session{Repos: repoNames(sit), Records: recs}.Orgs()
	return sit
}

// repoTitle is the repository's name in a hook's title: its display name.
func repoTitle(sit search.Situation) string {
	if sit.RepoDisplay != "" {
		return sit.RepoDisplay
	}
	return sit.Repo
}

// orgStartLimit bounds the org lines of the SessionStart block: the owner's
// org rules come lightly, after the repository's own.
const orgStartLimit = 2

// orgStartHits are the owner's own rules of the org (Rule: their words, not
// an agent's standard and not a ruling in its task) for the SessionStart
// block, newest first, at most orgStartLimit in all, none the session has,
// none stopped by outcomes in the repository.
func orgStartHits(ctx context.Context, ix *search.Index, sit search.Situation, have []search.Hit) []search.Hit {
	return scopeStartHits(ctx, ix, sit, sit.Orgs, have, search.Hit.Rule)
}

// scopeStartHits are the facts of these scopes that pass, for the
// SessionStart block: newest first in subRank's order, at most
// orgStartLimit, none the session has or outcomes stopped in its repository.
func scopeStartHits(ctx context.Context, ix *search.Index, sit search.Situation, scopes []string, have []search.Hit, pass func(search.Hit) bool) []search.Hit {
	if len(scopes) == 0 {
		return nil
	}
	hs, err := ix.List(ctx, scopes, "", false, false)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, h := range have {
		seen[h.Name] = true
	}
	var keep []search.Hit
	for _, h := range hs {
		if pass(h) && !h.Quiet && !h.HeldBack() && !seen[h.Name] {
			keep = append(keep, h)
		}
	}
	keep = outcomesFor(outcomeScope(repoNames(sit))).withoutStopped(keep)
	sort.SliceStable(keep, func(i, j int) bool {
		if a, b := subRank(keep[i]), subRank(keep[j]); a != b {
			return a < b
		}
		if keep[i].Created != keep[j].Created {
			return keep[i].Created > keep[j].Created
		}
		return keep[i].Name < keep[j].Name
	})
	if n := deliveryInt("ORG_START", orgStartLimit); len(keep) > n {
		keep = keep[:n]
	}
	return keep
}

// successorStartHits are the rulings and standards (tier 0) of the project
// that replaced the session's dead repository (Situation.Successors), for
// the SessionStart block: lightly, as an org's (at most orgStartLimit),
// newest first, none the session has.
func successorStartHits(ctx context.Context, ix *search.Index, sit search.Situation, have []search.Hit) []search.Hit {
	if len(sit.Successors) == 0 {
		return nil
	}
	return scopeStartHits(ctx, ix, sit, sit.Successors, have, func(h search.Hit) bool { return tier(h) == 0 })
}

// withRecordAliases is the situation with the names the records give its
// repository: a folder suppressed into it, the folder it is suppressed
// into, and the winner's aliases (suppressionNames), read from the index.
func withRecordAliases(sit search.Situation, recs map[string]*record.Record) search.Situation {
	if len(recs) == 0 {
		return sit
	}
	if sit.Repo == "" {
		// A workspace: the names the records give each of its repositories.
		if len(sit.Workspace) > 0 {
			sit.Workspace = lowerDistinct(suppressionNames(recs, sit.Workspace))
		}
		return sit
	}
	names := suppressionNames(recs, repoNames(sit))
	sit.RepoAliases = names[1:]
	return sit
}
