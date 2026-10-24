package ownerwords

import (
	"strings"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// TurnTexts is what a person typed on one transcript line of a session that
// can hold their turns (Who said "owner"), by the same rules the corpus reads
// their turns with: Claude Code's turn marks, Codex's and pi's user messages
// less harness frames and pastes, text an agent or a harness injected
// (AgentInjected) left out, and the lines of agent text they relayed
// (dropRelayed). nil for a line that holds none of theirs. The reviewer
// reads an owner's answer with it, so a turn counts as theirs by one rule
// everywhere (internal/agentreview.OwnerTurn).
func TurnTexts(f digest.Format, line []byte) []string {
	var raw []string
	switch f {
	case digest.Claude:
		raw, _ = claudeUserTurns(line)
	case digest.Codex:
		raw = codexUserTurns(line)
	case digest.Pi:
		raw = piUserTurns(line)
	}
	var out []string
	for _, t := range raw {
		if AgentInjected(t) {
			continue
		}
		t, _ = dropRelayed(t)
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}
