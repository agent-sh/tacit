package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"slices"
	"strings"
	"time"
)

// The Stop hook (SPEC.md, "Marks"): a session that was given facts is
// asked to mark them on about one turn in ten that was given some. When
// the session's hooks printed facts since its last Stop (the owed lines),
// the first Stop of an asked turn is blocked once with a short
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

// owedHook is the hook that printed them (prompt, deny, start, compact,
// summary, resume, ...). Every printed line is owed a mark, the start
// block's and what returns after a compaction or a resume too, so every
// line a session was given is graded. A standing line's "not relevant"
// still does not count against it in the outcomes (delivery_outcomes.go,
// standing).
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

// stopAskEvery spaces the asks: about one turn in stopAskEvery that owed
// lines is asked. Asking on every such turn cost the agent a call nearly
// every turn. Each turn is drawn on its own (stopAsked), so the turns asked
// are a sample of all of them, not mostly a session's first.
var stopAskEvery uint32 = 10 // a var so a test can ask on every turn

// turnsFile counts the session's turns that owed lines.
func turnsFile(id string) string {
	if p := sessionFile(id); p != "" {
		return p + ".turns"
	}
	return ""
}

// turnCount is the session's count of turns that owed lines; 0 when there
// is none or it cannot be read.
func turnCount(id string) int {
	n := 0
	if b, err := os.ReadFile(turnsFile(id)); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	}
	return n
}

// setTurnCount replaces the count through a rename, so a reader never sees
// the file half written.
func setTurnCount(id string, n int) {
	p := turnsFile(id)
	if p == "" {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", n)), 0o600) == nil {
		os.Rename(tmp, p)
	}
}

// stopAsked draws whether the session's n-th turn that owed lines is asked:
// a hash of the session and n, so the draw is fixed for a turn (a retry
// after an ask that could not be logged draws the same) and independent of
// what the turn was given.
func stopAsked(id string, n int) bool {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s\x00%d", id, n)
	return h.Sum32()%stopAskEvery == 0
}

// stopHook is `tacit hook stop`. It prints nothing (the stop goes on) but
// for a session with owed lines on the first Stop of an asked turn
// (stopAsked), where it prints the block decision with the ask as its reason.
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
	n := turnCount(in.SessionID)
	if !stopAsked(in.SessionID, n) {
		setTurnCount(in.SessionID, n+1)
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
		return // no ask to name: the stop goes on, and the next turn draws this one's count again
	}
	setTurnCount(in.SessionID, n+1)
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
