package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
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
// each session classed at its first block with an eligible line of the
// kind, read only once each kind holds 30 denied sessions.
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

// deniedTo is the session's denied facts (its view hides them): read
// from its denial file, which the read refreshes so the cleanup of old
// sessions keeps it while the session lives (rememberNames).
func deniedTo(id string) map[string]bool {
	path := denyFile(id)
	deny := readNames(path)
	if len(deny) > 0 {
		now := time.Now()
		os.Chtimes(path, now, now)
	}
	return deny
}

// openSessionIndex is openHookIndex as the session's view of the store:
// the facts it is denied do not exist in it (search.Index.Hide). Every
// hook block, pointer, count and cross-reference, and the search and tree
// tools, read the store through it.
func openSessionIndex(ctx context.Context, s *store.Store, session string) (*search.Index, error) {
	ix, err := openHookIndex(ctx, s)
	if err == nil {
		ix.Hide(deniedTo(session))
	}
	return ix, err
}

// openExactSessionIndex is openIndex as the session's view.
func openExactSessionIndex(ctx context.Context, s *store.Store, session string) (*search.Index, error) {
	ix, err := openIndex(ctx, s)
	if err == nil {
		ix.Hide(deniedTo(session))
	}
	return ix, err
}

// holdoutKinds are the fact types the preregistered analysis reads (eval/
// DELIVERY.md, step 4): a rule, a decision, a past mistake, a machine fact.
// Only an agent's note of one of them may be withheld; a fact computed from
// git, a reference, a result or any other type never is.
var holdoutKinds = map[string]bool{"user_standard": true, "design_decision": true, "past_mistake": true, "machine_fact": true}

// holdoutEligible is a line a hold-out may withhold: an agent's own note
// (search.Hit.Provenance "agent": not the owner's, not computed from git)
// of an analysed kind.
func holdoutEligible(h search.Hit) bool {
	return !h.OwnerLine() && h.Provenance() == "agent" && holdoutKinds[h.Type]
}

// holdOut is a prompt block's hits with one agent's note withheld and
// denied for the session when the draw says so. Eligible are the agent's
// notes of an analysed kind (holdoutEligible) that the session was never
// given (its given set, and its delivered set for a session older than the
// given set) and that no other line of the block names (a "contradicted
// by" mark would show it), counted by name: a store fact and a local note
// of one name are one note, withheld together. It logs the share
// (holdout_q), the eligible notes' count (holdout_k), each eligible line
// (eligible), the withheld fact (held_out, dropped "holdout"), and each
// line's probability of being shown, 1 - q/k for k eligible notes (a
// withheld line's probability of being withheld is 1 - p), 1 for any other
// line; and p_nothing, q when the block's lines are all its one eligible
// note.
func holdOut(in hookInput, hits []search.Hit, mcp bool) []search.Hit {
	id := in.SessionID
	q := deliveryHoldout()
	if dlog == nil || q <= 0 || id == "" || denyFile(id) == "" {
		return hits
	}
	given := readNames(givenFile(id))
	for n := range delivered(id) {
		given[n] = true
	}
	named := map[string]bool{}
	for _, h := range hits {
		for _, c := range h.Conflicts {
			named[c] = true
		}
	}
	// A note another line's text names is not eligible either: withheld, it
	// would be masked there, which says something is.
	for _, h := range hits {
		for _, o := range hits {
			if o.Name != h.Name && fact.NamesAny(o.Description+" "+o.Body, map[string]bool{h.Name: true}) {
				named[h.Name] = true
			}
		}
	}
	// A name's kind is its store file's (a local note's only when the store
	// has none): one name, one kind.
	rep := map[string]search.Hit{}
	for _, h := range hits {
		if r, ok := rep[h.Name]; !ok || r.Local && !h.Local {
			rep[h.Name] = h
		}
	}
	var names []string
	seen := map[string]bool{}
	for _, h := range hits {
		if !seen[h.Name] && holdoutEligible(rep[h.Name]) && !given[h.Name] && !named[h.Name] {
			seen[h.Name] = true
			names = append(names, h.Name)
		}
	}
	if len(names) == 0 {
		return hits
	}
	// Drawn only in a Claude Code session (the harness whose failures
	// momentum records) whose tools will read its view: its harness process
	// recorded it, and its tacit MCP server, when it has one, is registered
	// under that process. Otherwise the block logs why, and the session is
	// out of the analysis.
	if !isClaudeHook(in) {
		dlog.HoldoutSkip = "harness"
		return hits
	}
	pid, why := holdoutResolvable(id, mcp)
	if pid == 0 {
		dlog.HoldoutSkip = why
		return hits
	}
	dlog.HarnessPID = pid
	k := float64(len(names))
	dlog.HoldoutQ, dlog.HoldoutK = q, len(names)
	dlog.propensity = map[string]float64{}
	only := true
	for _, h := range hits {
		if seen[h.Name] {
			dlog.propensity[h.Path] = 1 - q/k
		}
		only = only && h.Name == names[0]
	}
	if len(names) == 1 && only {
		dlog.pNothing = &q
	}
	coin, pick := holdoutDraw(len(names))
	if coin >= q || pick < 0 || pick >= len(names) {
		return hits
	}
	dlog.HeldOut = names[pick]
	out := make([]search.Hit, 0, len(hits)-1)
	for _, h := range hits {
		if h.Name != dlog.HeldOut {
			out = append(out, h)
		}
	}
	appendNames(denyFile(id), []string{dlog.HeldOut})
	logDropped(hits, out, "holdout")
	return out
}

// isClaudeHook reports a hook of a Claude Code session, by its transcript.
func isClaudeHook(in hookInput) bool {
	if in.TranscriptPath == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	f, ok := transcriptFormat(home, filepath.Clean(in.TranscriptPath))
	return ok && f == digest.Claude
}

// viewSession is the session an MCP tool call answers, for its view: a
// Claude Code server's harness process's current session (mcpSession),
// another harness's from its environment. A Claude Code call that cannot
// resolve one reads no session's view, and the delivery log records it
// (view-unresolved, with the harness process), so the analysis leaves that
// process's sessions out.
func viewSession(harness, tool string) string {
	if harness != "claude" {
		return harnessSession(harness)
	}
	if s := mcpSession(); s != "" {
		return s
	}
	if deliveryLogOn() {
		e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "view-unresolved", Tool: tool, HarnessPID: serverParent()}
		e.ID = logID(e)
		_ = appendLogEntry(e, time.Now())
	}
	return ""
}

// givenByAnswer adds the facts a search or tree answer listed to the
// session's given set: the agent has seen them, so no later block's
// hold-out may withhold one (its arm is decided).
func givenByAnswer(session string, names []string) {
	if session == "" || len(names) == 0 {
		return
	}
	given := readNames(givenFile(session))
	var fresh []string
	for _, n := range names {
		if !given[n] {
			given[n] = true
			fresh = append(fresh, n)
		}
	}
	appendNames(givenFile(session), fresh)
}

// answerLine is a fact's line in a search answer ("- name: ...", "- RULE
// name: ...") or a tree answer ("- `name` (...").
var answerLine = regexp.MustCompile("(?m)^- (?:RULE )?`?([a-z0-9][a-z0-9-]*)`?[: (]")

// answerNames is the facts an answer lists.
func answerNames(out string) []string {
	var names []string
	for _, m := range answerLine.FindAllStringSubmatch(out, -1) {
		names = append(names, m[1])
	}
	return names
}
