package dream

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// HeldChars is the size of the held list when it is on
// (TACIT_DREAM_HELD_CHARS=<chars>; it is off by default). The dreamer wrote
// the same operational facts again in session after session (a
// generated-file rule, a tool's environment, a heartbeat command, an ssh
// wrapper), and most of the copies the curator then paid to split and judge
// started there. Replayed on real windows (eval/VALUE.md, "Facts already
// held"), a list of this size cut the notes and the copies among them, for
// more dreamer cost, by amounts within the run-to-run noise, so it does not
// pay for itself yet and stays off.
const HeldChars = 4000

// heldMinShare is the least share of a held fact's weighted terms the
// window must hold for the fact to be listed: the window is about it.
const heldMinShare = 0.5

// HeldPool is the curated facts a dreamer's windows may be shown as held:
// live, an agent's observation (never a ruling, a statement or a fact with
// the owner's words, which the dreamer writes whenever they are said again:
// repetition is their evidence), no folder file, result or generated fact.
type HeldPool struct {
	items []heldFact
	df    map[string]int
}

type heldFact struct {
	f     *fact.Fact
	terms map[string]bool
}

// heldHomeRx is a home directory spelled out, as a fact may write ~.
var heldHomeRx = regexp.MustCompile(`/(?:Users|home)/[^/\s]+/`)

// heldTerms is the terms of a text, a home directory spelled out as ~.
func heldTerms(s string) map[string]bool {
	s = heldHomeRx.ReplaceAllString(s, "~/")
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		for _, t := range search.Terms(w) {
			out[t] = true
		}
	}
	return out
}

// NewHeldPool indexes the facts a window may be shown as held.
func NewHeldPool(facts []*fact.Fact) *HeldPool {
	p := &HeldPool{df: map[string]int{}}
	for _, f := range facts {
		m, s := f.Meta, f.Meta.Source
		if f.InInbox() || m.SupersededBy != "" || m.Retired != "" || strings.HasPrefix(m.Scope, "derived/") || strings.HasPrefix(path.Base(f.Path), "_") ||
			fact.IsResult(m.Type) || m.Type == "hot_spot" || m.Type == fact.StatementType || s.Kind == "user" || s.Kind == "statement" || fact.ClaimsEvidence(f) || fact.IsRuling(f) {
			continue
		}
		h := heldFact{f: f, terms: heldTerms(m.Description + " " + f.Body)}
		for t := range h.terms {
			p.df[t]++
		}
		p.items = append(p.items, h)
	}
	return p
}

// Held is the facts already held for a window of repo on host whose digest
// is lines, as the prompt lists them (their descriptions), best first, at
// most maxChars of them: the facts delivery would show in the session (the
// repository's, tied by scope or applies_to, and the machine's, the user's
// and the system's) that the window holds at least heldMinShare of, by
// weighted terms (a rare term weighs more), most first; the newest on a
// tie.
func (p *HeldPool) Held(repo, host string, lines []string, maxChars int) []string {
	if p == nil || maxChars <= 0 || len(p.items) == 0 {
		return nil
	}
	window := heldTerms(strings.Join(lines, "\n"))
	repo = strings.ToLower(repo)
	type scored struct {
		h     heldFact
		share float64
	}
	var cs []scored
	n := float64(len(p.items))
	for _, h := range p.items {
		if !p.visible(h.f, repo, host) {
			continue
		}
		in, all := 0.0, 0.0
		for t := range h.terms {
			w := math.Log(1 + n/float64(p.df[t]))
			all += w
			if window[t] {
				in += w
			}
		}
		if all > 0 && in/all >= heldMinShare {
			cs = append(cs, scored{h, in / all})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].share != cs[j].share {
			return cs[i].share > cs[j].share
		}
		a, b := cs[i].h.f.Meta, cs[j].h.f.Meta
		if da, db := max(a.Created, a.Updated), max(b.Created, b.Updated); da != db {
			return da > db
		}
		return a.Name < b.Name
	})
	var out []string
	size := 0
	for _, c := range cs {
		line := "- " + strings.Join(strings.Fields(c.h.f.Meta.Description), " ")
		if size+len(line)+1 > maxChars {
			continue // a shorter one may still fit
		}
		size += len(line) + 1
		out = append(out, line)
	}
	return out
}

// visible: the fact is the repository's (its scope, or its applies_to
// names it), the machine's, the user's or the system's.
func (p *HeldPool) visible(f *fact.Fact, repo, host string) bool {
	s := f.Meta.Scope
	if s == "user" || s == "system" || host != "" && s == "machines/"+host {
		return true
	}
	if repo == "" {
		return false
	}
	if s == "projects/"+repo {
		return true
	}
	for _, r := range f.Meta.AppliesTo.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// heldSection is the prompt's list of facts already held (v9).
const heldSection = `ALREADY HELD: tacit already has these facts for this repository and machine. Do not write a fact again when it only restates one of them, in any words. Write it when the digest adds to one (a new path, value, command, step or reason) or contradicts one (it moved, changed, stopped holding, is wrong): then write the whole fact as it stands now. This list never limits the owner's words: a kind user or statement fact is written whenever the owner says it, held or not.
%s
`

// PromptFor is the dreamer's prompt for a window of repo on host: Prompt,
// with the facts already held (Held) before the digest. With none it is
// Prompt as before, byte for byte.
func PromptFor(repo, host string, held []string, lines []string) string {
	p := fmt.Sprintf(Prompt, repo, host, strings.Join(lines, "\n"))
	if len(held) == 0 {
		return p
	}
	i := strings.LastIndex(Prompt, "DIGEST:\n")
	head := fmt.Sprintf(Prompt[:i], repo, host)
	return head + fmt.Sprintf(heldSection, strings.Join(held, "\n")) + "DIGEST:\n" + strings.Join(lines, "\n") + "\n"
}
