package dream

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Owner content or agent content is decided per window, not per session
// (SPEC.md, "Owner windows and agent windows"). A session of the owner's can
// run on its own for weeks while they step in now and then; in Hermes they
// write almost every other message. A window with at least one turn of the
// owner's (a U: line: digest.SessionWho, digest.ClaudeTyped and
// digest.ClaudeQueued decide it) is owner content. A window with none is an
// agent's content, also inside an owner's session, and AgentWindows decides
// whether it is dreamed.

// AgentWindows is what the dreamer does with a window that holds no turn of
// the owner's: "all" dreams it (the default, the behaviour before the
// option), "recent:<days>" dreams it when its newest line is at most that many
// days old, "none" never dreams it. A window the policy does not take is
// passed over: its transcript's cursor moves past it as past a window with
// nothing to extract. In a window of the owner's that the policy would not
// take for its age, only the owner's turns are dreamed, with OwnerContext
// lines of the agent's around each one.
type AgentWindows struct {
	Mode string // "all", "recent" or "none"
	Days int    // Mode "recent": windows of the last Days days
}

const (
	agentAll    = "all"
	agentRecent = "recent"
	agentNone   = "none"
	// maxRecentDays bounds recent:<days> (ten years: "all" in effect).
	maxRecentDays = 3650
)

// ParseAgentWindows reads the option: "" or "all", "none", "recent:<days>"
// (1 to 3650 days).
func ParseAgentWindows(s string) (AgentWindows, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", agentAll:
		return AgentWindows{Mode: agentAll}, nil
	case agentNone:
		return AgentWindows{Mode: agentNone}, nil
	}
	if d, ok := strings.CutPrefix(v, agentRecent+":"); ok {
		n, err := strconv.Atoi(d)
		if err == nil && n >= 1 && n <= maxRecentDays {
			return AgentWindows{Mode: agentRecent, Days: n}, nil
		}
	}
	return AgentWindows{}, fmt.Errorf("agent windows must be all, none or recent:<days> (1 to %d), not %q", maxRecentDays, s)
}

// String is the option as ParseAgentWindows reads it.
func (p AgentWindows) String() string {
	switch p.Mode {
	case agentNone:
		return agentNone
	case agentRecent:
		return fmt.Sprintf("%s:%d", agentRecent, p.Days)
	}
	return agentAll
}

// All reports the default: every window is dreamed whole.
func (p AgentWindows) All() bool { return p.Mode == "" || p.Mode == agentAll }

// takes reports whether agent content written at at is dreamed. A window
// whose lines record no time (at zero) is taken: the policy errs on the side
// of the behaviour before it.
func (p AgentWindows) takes(at, now time.Time) bool {
	switch p.Mode {
	case agentNone:
		return false
	case agentRecent:
		return at.IsZero() || now.Sub(at) <= time.Duration(p.Days)*24*time.Hour
	}
	return true
}

// OwnerContext is how many digest lines before and after each turn of the
// owner's are kept with it when a window is cut to the owner's turns.
// Measured on the rig's rebuild selection (2026-10-24: 158 owner transcripts
// and 158 Hermes owner sessions, 912 windows, 4,359 owner turns): the agent's
// text the owner answers is within 12 lines before the turn for 93% of the
// turns, which is every turn whose window holds one; the agent's first text
// after it within 12 lines for 91%, its second for 73%. Twelve lines keep 38%
// of the characters of the owner's windows (8: 31%, 16: 44%).
const OwnerContext = 12

// HasOwnerTurn reports a window with at least one turn of the owner's.
func HasOwnerTurn(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "U: ") {
			return true
		}
	}
	return false
}

// OwnerTurns is a window cut to the owner's turns, each with up to n lines of
// context before and after it. A run of lines left out is replaced by one
// harness line that says how many, so the model sees where the window was
// cut. Lines are not redacted here; Finish redacts what is sent.
func OwnerTurns(lines []string, n int) []string {
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if !strings.HasPrefix(l, "U: ") {
			continue
		}
		for j := max(0, i-n); j <= min(len(lines)-1, i+n); j++ {
			keep[j] = true
		}
	}
	var out []string
	gap := 0
	flush := func() {
		if gap > 0 {
			out = append(out, fmt.Sprintf("H: [tacit: %d line(s) of the agent's own work left out]", gap))
			gap = 0
		}
	}
	for i, l := range lines {
		if !keep[i] {
			gap++
			continue
		}
		flush()
		out = append(out, l)
	}
	flush()
	return out
}

// Passed counts what a scan's policy passed over (ScanWindows).
type Passed struct {
	// Windows are the agent windows not dreamed, and Chars their size as the
	// model would have seen it.
	Windows int
	Chars   int
	// Cut are the owner's windows cut to their owner turns (OwnerTurns, when
	// that leaves something out), and CutChars the characters left out.
	Cut      int
	CutChars int
}

func (p *Passed) add(o Passed) {
	p.Windows += o.Windows
	p.Chars += o.Chars
	p.Cut += o.Cut
	p.CutChars += o.CutChars
}

// SentChars is the size of what the model is sent for the window, as chunk
// counts it.
func (w Window) SentChars() int { return windowChars(w.Sent()) }

// windowChars is a window's size as chunk counts it.
func windowChars(lines []string) int {
	n := 0
	for _, l := range lines {
		n += windowWidth(l) + 1
	}
	return n
}

// apply keeps the windows the policy dreams, cuts the owner's windows whose
// agent content it would not take, and counts the rest.
func (p AgentWindows) apply(ws []Window, now time.Time) ([]Window, Passed) {
	var passed Passed
	if p.All() {
		return ws, passed
	}
	kept := ws[:0:0]
	for _, w := range ws {
		owner := HasOwnerTurn(w.Lines)
		switch {
		case p.takes(w.At, now):
			kept = append(kept, w)
		case owner:
			// Cut only when something is left out: a window of the owner's
			// turns and their context alone is sent whole.
			if send := OwnerTurns(w.Lines, OwnerContext); len(send) != len(w.Lines) {
				w.Send = send
				passed.Cut++
				passed.CutChars += max(0, windowChars(w.Lines)-windowChars(send))
			}
			kept = append(kept, w)
		default:
			passed.Windows++
			passed.Chars += windowChars(w.Lines)
		}
	}
	return kept, passed
}
