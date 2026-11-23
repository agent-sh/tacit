package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// The Stop hook (SPEC.md, "Marks"): a turn that was given facts does not
// end silently. When the session's hooks printed facts since its last ask
// (the owed lines), the first Stop of the turn is blocked once with a short
// instruction: the facts' names and the one call that marks them, each as
// used, not relevant or wrong. The turn's second Stop (stop_hook_active) is
// never blocked, so the hook cannot loop, and an agent that does not mark
// is not asked again for those lines. The ask is a log entry of its own
// (hook "stop", mark_asked), which the mark names.

// owedFile holds the lines the session's hooks printed that no ask has
// listed yet: "<name>\t<scope>" a line.
func owedFile(id string) string {
	if p := sessionFile(id); p != "" {
		return p + ".owed"
	}
	return ""
}

// owedScope is the scope of the hook's block (its first repository), set by
// beginLog and kept after endLog: the restart paths log before they
// remember.
var owedScope string

// owedHook is the hook that printed them (start, prompt, deny, ...): a
// line a session start or a deny gave is owed a mark like any, but its
// "not relevant" does not count against it (delivery_outcomes.go).
var owedHook string

// owe adds printed lines to the session's owed set, under owedScope. A
// headless session owes nothing: nobody is asked there.
func owe(id string, names []string) {
	p := owedFile(id)
	if p == "" || len(names) == 0 || !deliveryLogOn() || hookHeadless {
		return
	}
	scope, hook := owedScope, owedHook
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	var b strings.Builder
	for _, n := range names {
		if n != "" && !strings.ContainsAny(n, "\t\n") {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", n, scope, hook)
		}
	}
	f.WriteString(b.String())
}

// owedLine is one owed fact.
type owedLine struct{ name, scope, hook string }

// takeOwed reads and clears the owed set: each name once, the first scope
// it was printed under, in print order.
func takeOwed(id string) []owedLine {
	p := owedFile(id)
	if p == "" {
		return nil
	}
	taken := p + ".taking"
	if os.Rename(p, taken) != nil {
		return nil
	}
	b, err := os.ReadFile(taken)
	os.Remove(taken)
	if err != nil {
		return nil
	}
	var out []owedLine
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.SplitN(l, "\t", 3)
		for len(f) < 3 {
			f = append(f, "")
		}
		name, scope, hook := f[0], f[1], f[2]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, owedLine{name, scope, hook})
	}
	return out
}

// stopAskTitle starts the ask; the prompt hook takes no prompt that starts
// with it for a request (Codex sends a Stop's reason as the next prompt).
const stopAskTitle = "tacit asks for one mark before you stop"

// stopAskCap bounds the facts one ask lists (the log's candidate cap).
const stopAskCap = logCandCap

// stopHook is `tacit hook stop`. It prints nothing (the stop goes on) but
// for a session with owed lines on the turn's first Stop, where it prints
// the block decision with the ask as its reason.
func stopHook(in hookInput) {
	if in.StopHookActive || in.SessionID == "" || !deliveryLogOn() {
		return
	}
	if sessionKind(in) != "" {
		takeOwed(in.SessionID) // a headless run, or a subagent's stop: nobody to ask
		return
	}
	owed := takeOwed(in.SessionID)
	if len(owed) == 0 {
		return
	}
	if len(owed) > stopAskCap {
		owed = owed[:stopAskCap]
	}
	now := time.Now()
	e := &logEntry{V: deliveryLogVersion, TS: now.UTC().Format(time.RFC3339Nano), Hook: "stop", Session: in.SessionID, Policy: "gates",
		Cwd: in.Cwd, Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath, MarkAsked: true, PNothing: 0}
	names := make([]string, len(owed))
	for i, o := range owed {
		names[i] = o.name
		e.Cands = append(e.Cands, logCand{Name: o.name, Delivered: true, P: 1, Rank: -1, In: o.scope, Via: o.hook})
		if o.scope != "" && !slices.Contains(e.Repos, o.scope) {
			e.Repos = append(e.Repos, o.scope)
		}
	}
	e.ID = logID(e)
	if appendLogEntry(e, now) != nil {
		return // no ask to name: the stop goes on
	}
	out, _ := json.Marshal(map[string]any{"decision": "block", "reason": stopAsk(e.ID, names, hookMCP)})
	fmt.Println(string(out))
}

// stopAsk is the ask: the names, the one call and the three verdicts.
func stopAsk(id string, names []string, mcp bool) string {
	call := fmt.Sprintf("`mcp__tacit__tacit_mark` with block %q and marks [{\"name\": \"<fact>\", \"verdict\": \"used\"}, ...]", id)
	if !mcp {
		call = fmt.Sprintf("`tacit mark %s <fact>=<verdict> ...`", id)
	}
	return fmt.Sprintf("%s. This turn was given these tacit facts: %s. In one call, %s, say for each one: used (it shaped what you did), not_relevant, or wrong (it is false or out of date). Then finish as you would have; you will not be asked again for these.",
		stopAskTitle, strings.Join(names, ", "), call)
}
