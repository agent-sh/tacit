package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/search"
)

// Hold-outs (SPEC.md, "Hold-outs"): a small random share of the prompt
// blocks that print an agent's note withhold one of those notes, drawn
// uniformly, and deny it for the rest of the session: no hook gives it
// again there. A fact's arm in a session is decided at the first prompt
// block it would have reached (its first eligible block), so that block's
// propensity is the session's. Comparing what follows in sessions that got
// a fact of a kind with sessions denied one (momentum's failure events,
// read later) measures the kind's effect. The owner's own lines are never
// withheld, and with the delivery log off there are no hold-outs.

// holdoutShare is the share of eligible prompt blocks that withhold one
// agent's note: 0.05, one line in about 1 in 20 such blocks. Its analysis
// is preregistered in eval/DELIVERY.md (step 4): pooled by kind of fact,
// read only once each kind holds 30 sessions with a withheld fact.
// TACIT_DELIVERY_HOLDOUT sets it, 0 for none.
const holdoutShare = 0.05

// holdoutDraw is the draw for a block of n eligible notes: the share's coin
// and the note; tests replace it.
var holdoutDraw = func(n int) (float64, int) { return rand.Float64(), rand.IntN(n) }

func deliveryHoldout() float64 { return deliveryFloat("HOLDOUT", holdoutShare) }

// denyFile holds the session's denied facts, givenFile every fact a hook
// gave it; neither is dropped by a clear or a compaction (forget), so a
// denial and an arm last the session.
func denyFile(id string) string  { return sessionSideFile(id, ".holdout") }
func givenFile(id string) string { return sessionSideFile(id, ".given") }

func sessionSideFile(id, ext string) string {
	if p := sessionFile(id); p != "" {
		return p + ext
	}
	return ""
}

func readNames(path string) map[string]bool {
	out := map[string]bool{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out[l] = true
		}
	}
	return out
}

func appendNames(path string, names []string) {
	if path == "" || len(names) == 0 {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprint(f, strings.Join(names, "\n")+"\n")
}

// withoutDenied is hits less the facts the session is denied, which every
// hook leaves out (logged dropped "holdout").
func withoutDenied(id string, hits []search.Hit) []search.Hit {
	deny := readNames(denyFile(id))
	if len(deny) == 0 {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if !deny[h.Name] {
			out = append(out, h)
		}
	}
	logDropped(hits, out, "holdout")
	return out
}

// holdOut is a prompt block's hits with one agent's note withheld and
// denied for the session when the draw says so. Eligible are the agent's
// notes the session was never given (their arm is not decided yet) and
// that no other line of the block names (a "contradicted by" mark would
// show it). It logs the share, the withheld fact (dropped "holdout") and
// each line's probability of being shown, 1 - q/k for k eligible notes (a
// withheld line's probability of being withheld is 1 - p), 1 for any other
// line; and p_nothing, q when the block's one line is its one eligible note.
func holdOut(id string, hits []search.Hit) []search.Hit {
	q := deliveryHoldout()
	if dlog == nil || q <= 0 || id == "" {
		return hits
	}
	given := readNames(givenFile(id))
	named := map[string]bool{}
	for _, h := range hits {
		for _, c := range h.Conflicts {
			named[c] = true
		}
	}
	var eligible []int
	for i, h := range hits {
		if !h.OwnerLine() && !given[h.Name] && !named[h.Name] {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return hits
	}
	k := float64(len(eligible))
	dlog.HoldoutQ = q
	dlog.propensity = map[string]float64{}
	for _, i := range eligible {
		dlog.propensity[hits[i].Path] = 1 - q/k
	}
	if len(hits) == 1 {
		dlog.pNothing = &q
	}
	coin, pick := holdoutDraw(len(eligible))
	if coin >= q || pick < 0 || pick >= len(eligible) {
		return hits
	}
	out := make([]search.Hit, 0, len(hits)-1)
	for i, h := range hits {
		if i == eligible[pick] {
			dlog.HeldOut = h.Name
			continue
		}
		out = append(out, h)
	}
	appendNames(denyFile(id), []string{dlog.HeldOut})
	logDropped(hits, out, "holdout")
	return out
}
