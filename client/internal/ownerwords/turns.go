package ownerwords

import (
	"encoding/json"
	"strings"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// TurnTexts is what a person typed on one transcript line of a session that
// can hold their turns (Who said "owner"), by the rules the corpus reads
// their turns with: Claude Code's turn marks and its messages queued while
// the agent worked (digest.ClaudeTyped, digest.ClaudeQueued), what in the
// turn's text a harness or another agent wrote left out (digest.TurnPieces),
// pastes, and the lines of agent text they relayed (dropRelayed). nil for a
// line that holds none of theirs. The reviewer reads an owner's turn with it
// (through a TurnReader, which keeps what a Codex agent's questions offered
// for the answers that follow), so a turn counts as theirs by one rule
// everywhere (internal/agentreview.OwnerTurn).
func TurnTexts(f digest.Format, line []byte) []string {
	own, _ := (&TurnReader{}).Parts(f, line)
	return own
}

// TurnReader reads a transcript's lines in order, each as TurnTexts reads
// it, keeping what it needs across them: the options a Codex agent's
// questions offered (digest.CodexAsked), and the commands Claude Code ran
// itself, whose arguments are not theirs (digest.ClaudeCommands).
type TurnReader struct {
	// Pastes are texts the owner pasted into their prompts (ClaudePastes),
	// cut from their words as the corpus cuts them; none when nil.
	Pastes []string
	asked  digest.CodexAsked
	cmds   digest.ClaudeCommands
}

// Parts is what a person typed on line (as TurnTexts), and the other text
// another agent wrote into that turn of theirs (a side agent's note), which
// is not their words but what they answered; other is nil when the line
// holds no turn of theirs.
func (r *TurnReader) Parts(f digest.Format, line []byte) (own, other []string) {
	if r.asked == nil {
		r.asked = digest.CodexAsked{}
	}
	var raw []string
	switch f {
	case digest.Claude:
		if !r.cmds.LocalLine(line) {
			raw, _ = claudeUserTurns(line)
		}
	case digest.Codex:
		r.asked.NoteLine(line)
		raw = codexUserTurns(line, r.asked)
	case digest.Pi:
		raw = piUserTurns(line)
	}
	for _, t := range raw {
		if t, ok := r.ownText(t); ok {
			own = append(own, t)
		}
	}
	if len(own) > 0 && f == digest.Claude {
		other = claudeAgentText(line)
	}
	return own, other
}

// ownText is what of a text of a turn of theirs is their words, as the
// corpus keeps it (cutWords); ok is false when nothing is left.
func (r *TurnReader) ownText(t string) (string, bool) {
	t, _, ok := cutWords(t, r.Pastes)
	return t, ok && strings.TrimSpace(t) != ""
}

// Hermes is what the owner wrote in a Hermes user row of a session of theirs
// (hermes.Session.Owner), by the rules the corpus reads their rows with
// (AddHermes): the gateway's frames, notices and attached context cut
// (digest.HermesOwnerText), a pasted table or log left out (PastedBlob), then
// their words as for a transcript's turn (ownText). ok is false when none of
// it is theirs.
func (r *TurnReader) Hermes(content, displayKind string, summary bool) (string, bool) {
	t, injected, pasted := hermesRow(content, displayKind, summary)
	if injected || pasted {
		return "", false
	}
	return r.ownText(t)
}

// claudeAgentText is the text another agent wrote into a Claude Code user
// turn (digest.TurnPieces: a side agent's note).
func claudeAgentText(line []byte) []string {
	texts, _, _, ok := digest.ClaudeQueuedLine(line)
	if !ok {
		var o struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(line, &o)
		texts = contentTexts(o.Message.Content)
	}
	var out []string
	for _, t := range texts {
		for _, p := range digest.TurnPieces(t) {
			if p.Who == digest.Agent {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

// contentTexts are the texts of a message's content: a string, or its text
// blocks.
func contentTexts(content json.RawMessage) []string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []string{s}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &blocks)
	var out []string
	for _, bl := range blocks {
		if bl.Type == "text" {
			out = append(out, bl.Text)
		}
	}
	return out
}
