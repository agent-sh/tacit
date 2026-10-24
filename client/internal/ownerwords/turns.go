package ownerwords

import (
	"encoding/json"
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
		if raw == nil {
			raw = claudeQueuedTurn(line)
		}
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

// claudeQueuedTurn is a message the owner typed while the agent was working:
// Claude Code delivers it as an attachment of type queued_command, marked
// origin.kind human and humanTurn (a task notification delivered the same way
// is not marked so). The corpus does not read these yet; the reviewer does,
// since an owner's mid-turn words are often the rule an agent saves.
func claudeQueuedTurn(line []byte) []string {
	var o struct {
		Type       string `json:"type"`
		Sidechain  bool   `json:"isSidechain"`
		Attachment struct {
			Type      string `json:"type"`
			Prompt    string `json:"prompt"`
			HumanTurn bool   `json:"humanTurn"`
			Origin    struct {
				Kind string `json:"kind"`
			} `json:"origin"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "attachment" || o.Sidechain || o.Attachment.Type != "queued_command" {
		return nil
	}
	a := o.Attachment
	if a.Origin.Kind != "human" || !a.HumanTurn || strings.TrimSpace(a.Prompt) == "" {
		return nil
	}
	t := pastedBlock.ReplaceAllString(a.Prompt, " ")
	if strings.TrimSpace(t) == "" || digest.HarnessText(digest.Claude, t) {
		return nil
	}
	return []string{t}
}
