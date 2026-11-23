package curate

// Near-copies (SPEC.md, Near-duplicates, "Near-copies"): facts of one
// folder that state the same rule or the same fact in other words, which
// the description rule (restatesFact) cannot see because they share too few
// terms. Code finds the candidates and keeps the clusters small; a model
// says which facts of a cluster say the same thing; code picks the fact
// kept and the copies, under the same guards as the store's dedupe and one
// more for the owner's words.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// NearOverlap is the share of content stems (Jaccard, over the description
// and the name) two facts of one folder share to be a candidate pair, and
// NearShared the fewest stems they share. Lower than FactOverlap on purpose:
// the model decides, so code only has to keep the pairs that cannot be the
// same rule out of the prompt.
var (
	NearOverlap = 0.35
	NearShared  = 3
)

// NearMaxCluster is the most facts one cluster shows the model.
const NearMaxCluster = 8

// nearCommon is how many facts of a folder may hold a stem before it no
// longer joins a pair there: a folder's own subject is in most of its facts.
const nearCommon = 300

// NearCluster is one group of candidate facts of one scope, and its key: a
// hash of its facts' paths and descriptions, so an answer is kept for the
// cluster as it was asked.
type NearCluster struct {
	Key   string
	Scope string
	Facts []*fact.Fact
}

// nearCandidate is a fact the near pass reads: live, curated, of no
// generated type and not a system fact (Walk passes folder files over).
func nearCandidate(f *fact.Fact) bool {
	m := f.Meta
	return live(f) && !strings.HasPrefix(m.Scope, "derived/") && m.Scope != "system" && !fact.IsResult(m.Type) && m.Type != "hot_spot"
}

func nearStems(f *fact.Fact) map[string]bool {
	out := map[string]bool{}
	for _, s := range ownerwords.ContentStems(f.Meta.Description + " " + strings.ReplaceAll(f.Meta.Name, "-", " ")) {
		out[s] = true
	}
	return out
}

// nearLine is how the prompt shows a fact: its type, day, name,
// description and the start of its body. A cluster's key is a hash of its
// facts' lines, so an answer stands only while the model would read the
// same thing.
func nearLine(f *fact.Fact) string {
	body := strings.Join(strings.Fields(f.Body), " ")
	return fmt.Sprintf("[%s, %s] %s: %s | %s", f.Meta.Type, f.Meta.Created, f.Meta.Name, ClipLine(f.Meta.Description, 200), ClipLine(body, 360))
}

// NearCandidates are the clusters of facts that may say the same thing: in
// one scope, every pair that shares NearShared content stems and
// NearOverlap of them (a stem more than nearCommon of the scope's facts
// hold is left out of both sides), joined strongest pair first while a
// cluster stays within NearMaxCluster facts. No model.
func NearCandidates(facts []*fact.Fact) []NearCluster {
	byScope := map[string][]*fact.Fact{}
	for _, f := range facts {
		if nearCandidate(f) {
			byScope[f.Meta.Scope] = append(byScope[f.Meta.Scope], f)
		}
	}
	scopes := make([]string, 0, len(byScope))
	for s := range byScope {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	var out []NearCluster
	for _, scope := range scopes {
		fs := byScope[scope]
		sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
		stems := make([]map[string]bool, len(fs))
		inv := map[string][]int{}
		for i, f := range fs {
			stems[i] = nearStems(f)
			for s := range stems[i] {
				inv[s] = append(inv[s], i)
			}
		}
		// A folder's own subject is in most of its facts: such a stem joins
		// no pair and counts on neither side.
		for s, is := range inv {
			if len(is) > nearCommon {
				for _, i := range is {
					delete(stems[i], s)
				}
				delete(inv, s)
			}
		}
		type edge struct {
			a, b int
			j    float64
		}
		var edges []edge
		for i := range fs {
			shared := map[int]int{}
			for s := range stems[i] {
				for _, k := range inv[s] {
					if k > i {
						shared[k]++
					}
				}
			}
			for k, n := range shared {
				if n < NearShared {
					continue
				}
				union := len(stems[i]) + len(stems[k]) - n
				if j := float64(n) / float64(union); j >= NearOverlap {
					edges = append(edges, edge{i, k, j})
				}
			}
		}
		sort.Slice(edges, func(x, y int) bool {
			if edges[x].j != edges[y].j {
				return edges[x].j > edges[y].j
			}
			if edges[x].a != edges[y].a {
				return edges[x].a < edges[y].a
			}
			return edges[x].b < edges[y].b
		})
		parent := make([]int, len(fs))
		size := make([]int, len(fs))
		for i := range parent {
			parent[i], size[i] = i, 1
		}
		var find func(int) int
		find = func(x int) int {
			for parent[x] != x {
				parent[x] = parent[parent[x]]
				x = parent[x]
			}
			return x
		}
		for _, e := range edges {
			ra, rb := find(e.a), find(e.b)
			if ra == rb || size[ra]+size[rb] > NearMaxCluster {
				continue
			}
			if rb < ra {
				ra, rb = rb, ra
			}
			parent[rb] = ra
			size[ra] += size[rb]
		}
		groups := map[int][]*fact.Fact{}
		var roots []int
		for i, f := range fs {
			r := find(i)
			if size[r] < 2 {
				continue
			}
			if groups[r] == nil {
				roots = append(roots, r)
			}
			groups[r] = append(groups[r], f)
		}
		sort.Ints(roots)
		for _, r := range roots {
			out = append(out, NearCluster{Key: nearKey(groups[r]), Scope: scope, Facts: groups[r]})
		}
	}
	return out
}

func nearKey(fs []*fact.Fact) string {
	h := sha256.New()
	for _, f := range fs {
		h.Write([]byte(f.Path + "\x00" + nearLine(f) + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// NearPrompt asks the model which facts of each cluster say the same thing.
func NearPrompt(cs []NearCluster) string {
	var b strings.Builder
	b.WriteString(`You check a fact store that coding agents share for near-copies: facts of one folder that state the same rule or the same fact in other words.
For each numbered group below, list the sets of facts that say the same thing: each one's content is in the other, so one of them could stand for the rest with nothing lost but wording.
Not the same: facts that only share a subject; that differ in a value, a path, a host, a version, a tool, a condition, a scope or a direction; or where one says something the other does not.
Answer ONLY a JSON object: {"groups": [{"group": 1, "same": [[1, 2], [3, 5]]}, {"group": 2, "same": []}]}, one entry for every group, each set two or more of that group's fact numbers, a fact in at most one set.

`)
	for i, c := range cs {
		fmt.Fprintf(&b, "Group %d (%s):\n", i+1, c.Scope)
		for j, f := range c.Facts {
			fmt.Fprintf(&b, "  %d. %s\n", j+1, nearLine(f))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ParseNear reads the model's answer for cs: each cluster's sets, by its
// key, as 0-based indices. A cluster the answer leaves out has no entry; an
// answer that does not read, names a group or a fact that is not there, or
// puts a fact in two sets is an error.
func ParseNear(answer string, cs []NearCluster) (map[string][][]int, error) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return nil, errors.New("no JSON object in the answer")
	}
	var v struct {
		Groups []struct {
			Group int     `json:"group"`
			Same  [][]int `json:"same"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &v); err != nil {
		return nil, fmt.Errorf("the answer does not read: %w", err)
	}
	out := map[string][][]int{}
	for _, g := range v.Groups {
		if g.Group < 1 || g.Group > len(cs) {
			return nil, fmt.Errorf("group %d is not in the prompt", g.Group)
		}
		c := cs[g.Group-1]
		if _, ok := out[c.Key]; ok {
			return nil, fmt.Errorf("group %d is answered twice", g.Group)
		}
		seen := map[int]bool{}
		sets := [][]int{}
		for _, s := range g.Same {
			var set []int
			for _, n := range s {
				if n < 1 || n > len(c.Facts) {
					return nil, fmt.Errorf("group %d has no fact %d", g.Group, n)
				}
				if seen[n] {
					return nil, fmt.Errorf("group %d puts fact %d in two sets", g.Group, n)
				}
				seen[n] = true
				set = append(set, n-1)
			}
			if len(set) >= 2 {
				sets = append(sets, set)
			}
		}
		out[c.Key] = sets
	}
	return out, nil
}

// nearRank orders a set's facts for the one kept: the owner's ruling, a
// fact they wrote in the app, a fact holding their words, then the earliest
// (created), then the one whose applies_to names the most situations, then
// by name.
func nearRank(a, b *fact.Fact) bool {
	if ta, tb := ownerTier(a), ownerTier(b); ta != tb {
		return ta > tb
	}
	if a.Meta.Created != b.Meta.Created {
		return a.Meta.Created < b.Meta.Created
	}
	if na, nb := situations(a), situations(b); na != nb {
		return na > nb
	}
	return a.Meta.Name < b.Meta.Name
}

// ownerTier is how much of the owner a fact holds: 3 their ruling, 2 a
// fact they wrote, 1 any evidence of theirs (their words, a quote, an
// endorsement, backed or not), 0 none.
func ownerTier(f *fact.Fact) int {
	switch {
	case fact.IsRuling(f):
		return 3
	case fact.OwnerWrote(f):
		return 2
	case fact.ClaimsEvidence(f):
		return 1
	}
	return 0
}

// NearCopies turns the model's sets into copies: in each set the fact
// ranked first (nearRank) is kept and the others are its copies, except a
// fact that may not be retired on a model's verdict: one that holds any of
// the owner (their ruling, a fact they wrote, their words, a quote or an
// endorsement: ownerTier), one kept in a decision of theirs or marked
// conflicting, one whose pair with the kept fact they answered, and one
// whose checked references hold where the kept fact's are refuted. So only
// agents' facts are retired, into the fact that holds most of the owner.
// A cluster with no verdict is left alone.
func NearCopies(cs []NearCluster, verdicts map[string][][]int, g DedupeGuard) []Copies {
	var out []Copies
	for _, c := range cs {
		sets, ok := verdicts[c.Key]
		if !ok {
			continue
		}
		for _, set := range sets {
			var fs []*fact.Fact
			for _, i := range set {
				if i >= 0 && i < len(c.Facts) {
					fs = append(fs, c.Facts[i])
				}
			}
			if len(fs) < 2 {
				continue
			}
			sort.SliceStable(fs, func(i, j int) bool { return nearRank(fs[i], fs[j]) })
			kept := fs[0]
			cp := Copies{Kept: kept}
			for _, x := range fs[1:] {
				if ownerTier(x) > 0 || g.Kept[x.Meta.Name] || len(x.Meta.ConflictsWith) > 0 ||
					(g.Answered != nil && g.Answered(x.Meta.Name, kept.Meta.Name)) || (g.Refuted != nil && g.Refuted(kept, x)) {
					continue
				}
				cp.Copies = append(cp.Copies, x)
			}
			if len(cp.Copies) > 0 {
				out = append(out, cp)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kept.Path < out[j].Kept.Path })
	return out
}
