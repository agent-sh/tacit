package search

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// hydrateLocations runs after ranking/limit, so discarded candidates do not
// require body parsing or link resolution. Global uniqueness is established
// before scope filtering: an inaccessible duplicate still makes a link ambiguous.
func (ix *Index) hydrateLocations(ctx context.Context, hits []Hit, sit Situation, repos []string, allowRouting bool, evidence map[string]*RetrievalEvidence) error {
	var links []string
	for _, h := range hits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.Evidence != nil {
			links = append(links, (&fact.Fact{Body: h.Body}).Links()...)
		}
	}
	if len(links) == 0 {
		return nil
	}
	data, _ := json.Marshal(links)
	rows, err := ix.db.QueryContext(ctx, `SELECT f.name,f.path,f.scope,f.applies FROM facts f JOIN evidence e ON e.id=f.id WHERE f.name IN (SELECT value FROM json_each(?))`, string(data))
	if err != nil {
		return err
	}
	defer rows.Close()
	byName := map[string][]LocationEvidence{}
	for rows.Next() {
		var l LocationEvidence
		var applies string
		if err := rows.Scan(&l.Fact, &l.Path, &l.Scope, &applies); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(applies), &l.AppliesTo); err != nil {
			return err
		}
		l.Via = "[[" + l.Fact + "]]"
		byName[l.Fact] = append(byName[l.Fact], l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range hits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.Evidence == nil {
			continue
		}
		for _, name := range (&fact.Fact{Body: h.Body}).Links() {
			targets := byName[name]
			if len(targets) == 1 && locationEligible(targets[0], sit, repos, allowRouting, evidence) {
				h.Evidence.Locations = append(h.Evidence.Locations, targets[0])
			}
		}
	}
	return nil
}

func locationEligible(l LocationEvidence, sit Situation, repos []string, allowRouting bool, evidence map[string]*RetrievalEvidence) bool {
	matchesRepo := func(repo string) bool {
		for _, r := range repos {
			if r != "" && strings.EqualFold(repo, r) {
				return true
			}
		}
		return false
	}
	supported := allowRouting && evidence[l.Path] != nil && evidence[l.Path].Supported
	scopeOK := l.Scope == "system" || l.Scope == "user" || (sit.Host != "" && strings.EqualFold(l.Scope, "machines/"+sit.Host))
	if strings.HasPrefix(l.Scope, "projects/") {
		scopeOK = matchesRepo(strings.TrimPrefix(l.Scope, "projects/")) || supported
	}
	if !scopeOK {
		return false
	}
	if !sit.RestrictRepos || len(l.AppliesTo.Repos) == 0 {
		return true
	}
	for _, repo := range l.AppliesTo.Repos {
		if matchesRepo(repo) {
			return true
		}
	}
	return supported
}
