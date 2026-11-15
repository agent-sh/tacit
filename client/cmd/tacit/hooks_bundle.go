package main

import (
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// withoutNearDuplicates gives one bundle line to facts that only restate one
// another. It uses the curator's measured description-overlap rule and its
// guards: the scope must match, and a different number, path, identifier or
// change word keeps both facts. Only the text a hook prints is compared.
//
// This is delivery dedup, not a verdict on the store: no fact is retired or
// rewritten. The curator's protections for owner rulings, owner evidence,
// conflicts and system facts apply here too.
func withoutNearDuplicates(hits []search.Hit) []search.Hit {
	if len(hits) < 2 {
		return hits
	}
	facts := make([]*fact.Fact, 0, len(hits))
	for _, h := range hits {
		source := fact.Source{Kind: "observed"}
		if h.Ruling {
			source.Kind = "user"
		}
		if h.Words {
			// The index carries the verified-words bit, not the quote. Its
			// presence is enough to activate the curator's owner guard.
			source.OwnerWords = "verified owner words"
		}
		facts = append(facts, &fact.Fact{Path: h.Path, Body: h.Name + " " + h.Description, Meta: fact.Meta{
			Name: h.Name, Description: h.Description, Type: h.Type, Scope: h.Scope,
			AppliesTo: h.Applies, Source: source, Created: h.Created, ConflictsWith: h.Conflicts,
		}})
	}
	clusters := curate.Duplicates(facts, curate.DedupeGuard{})
	if len(clusters) == 0 {
		return hits
	}
	drop := map[string]bool{}
	for _, c := range clusters {
		for _, copy := range c.Copies {
			drop[copy.Path] = true
		}
	}
	out := hits[:0:0]
	for _, h := range hits {
		if !drop[h.Path] {
			out = append(out, h)
		}
	}
	return out
}
