package curate

import (
	"sort"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// FactOverlap is the share of description terms (Jaccard) at which a curated
// fact restates another, with the guards of restatesFact.
var FactOverlap = RestateOverlap

// Copies is one cluster of near-identical curated facts: the fact kept and
// the copies that only restate it.
type Copies struct {
	Kept   *fact.Fact
	Copies []*fact.Fact
}

// DedupeGuard is what the store's decision log says about the facts: the
// ones a standing decision of the owner keeps, and the pairs the owner
// answered. Both stay as they are.
type DedupeGuard struct {
	Kept     map[string]bool
	Answered func(a, b string) bool
	// Refuted reports that code's checks refute kept's dry references where
	// copy's hold (check.Refutes): the copy is not retired into it. Nil: no
	// checks.
	Refuted func(kept, copy *fact.Fact) bool
}

// Duplicates finds the curated facts that only restate another live curated
// fact (restatesFact: same scope, RestateOverlap of the description's terms,
// no specific token or change word the other lacks) and names the fact each
// one restates. No model.
//
// Facts are taken best first: a ruling, then a fact with words of the
// owner's, then the one whose applies_to names the most situations, then the
// newest, then by name. A fact is checked against the facts already kept, and
// only the closest one in its scope decides, as for notes. So a copy is
// always retired into a fact that says everything it says, and a kept fact
// is never a copy.
//
// Never a copy: a ruling, a fact with owner evidence or words of theirs
// (only the owner changes those), a system fact, a fact with conflicts_with
// (an open finding), a fact the owner kept in a decision, and a fact whose
// pair with the closest kept fact the owner already answered, and a fact
// whose checked references hold where the kept fact's are refuted
// (DedupeGuard.Refuted).
func Duplicates(facts []*fact.Fact, g DedupeGuard) []Copies {
	type cand struct {
		f      *fact.Fact
		ruling bool
	}
	var cs []cand
	for _, f := range facts {
		if live(f) && !fact.IsResult(f.Meta.Type) {
			cs = append(cs, cand{f, fact.IsRuling(f)})
		}
	}
	day := func(f *fact.Fact) string { return max(f.Meta.Created, f.Meta.Updated) }
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.ruling != b.ruling {
			return a.ruling
		}
		if wa, wb := fact.ClaimsEvidence(a.f), fact.ClaimsEvidence(b.f); wa != wb {
			return wa
		}
		if na, nb := situations(a.f), situations(b.f); na != nb {
			return na > nb
		}
		if da, db := day(a.f), day(b.f); da != db {
			return da > db
		}
		return a.f.Meta.Name < b.f.Meta.Name
	})
	var p pool
	by := map[string]*Copies{}
	var order []string
	for _, c := range cs {
		e := newStatement(c.f, c.ruling, true)
		if copyable(c.f, c.ruling, g) && len(e.terms) > 0 {
			if o := closestInScope(&p, e); o != nil && restatesFact(e, o) && (g.Answered == nil || !g.Answered(c.f.Meta.Name, o.f.Meta.Name)) && (g.Refuted == nil || !g.Refuted(o.f, c.f)) {
				k := o.f.Meta.Name
				if by[k] == nil {
					by[k] = &Copies{Kept: o.f}
					order = append(order, k)
				}
				by[k].Copies = append(by[k].Copies, c.f)
				continue
			}
		}
		p.add(e)
	}
	out := make([]Copies, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kept.Path < out[j].Kept.Path })
	return out
}

// closestInScope is the closest pooled statement in e's scope, or nil.
func closestInScope(p *pool, e *statement) *statement {
	for _, o := range p.closest(e, FactOverlap) {
		if o.f.Meta.Scope == e.f.Meta.Scope && o.f.Meta.Name != e.f.Meta.Name {
			return o
		}
	}
	return nil
}

// copyable is a fact the curator may retire as a copy.
func copyable(f *fact.Fact, ruling bool, g DedupeGuard) bool {
	return !ruling && !fact.OwnerWrote(f) && !fact.ClaimsEvidence(f) && f.Meta.Scope != "system" && len(f.Meta.ConflictsWith) == 0 && !g.Kept[f.Meta.Name]
}

// situations counts the entries of a fact's applies_to.
func situations(f *fact.Fact) int {
	a := f.Meta.AppliesTo
	return len(a.Paths) + len(a.Symbols) + len(a.Repos) + len(a.Hosts) + len(a.Commands)
}
