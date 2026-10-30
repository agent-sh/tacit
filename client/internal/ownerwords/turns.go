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
	own, other, _ = r.PartsCut(f, line)
	return own, other
}

// PartsCut is Parts with what the corpus cut from the pieces of theirs as not
// their words, each part as it was: a pasted table or log (PastedBlob), a
// paste their prompt history records (Pastes), a line of agent text they
// relayed, and a part the pieces' own split finds not theirs.
func (r *TurnReader) PartsCut(f digest.Format, line []byte) (own, other, cut []string) {
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
		raw, cut = codexTurns(line, r.asked)
	case digest.Pi:
		raw, cut = piTurns(line)
	}
	for _, t := range raw {
		t, c, _, ok := cutWords(t, r.Pastes)
		cut = append(cut, c...)
		if ok && strings.TrimSpace(t) != "" {
			own = append(own, t)
		}
	}
	if len(own) > 0 && f == digest.Claude {
		other = claudeAgentText(line)
	}
	return own, other, cut
}

// Hermes is what the owner wrote in a Hermes user row of a session of theirs
// (hermes.Session.Owner), by the rules the corpus reads their rows with
// (AddHermes): the gateway's frames, notices and attached context cut
// (digest.HermesOwnerText), a pasted table or log left out (PastedBlob), then
// their words as for a transcript's turn (cutWords). ok is false when none of
// it is theirs.
func (r *TurnReader) Hermes(content, displayKind string, summary bool) (string, bool) {
	own, _, ok := r.HermesCut(content, displayKind, summary)
	return own, ok
}

// HermesCut is Hermes with what it cut from the row's words of theirs as not
// theirs (as PartsCut): a pasted table or log, a paste their prompt history
// records, a line of agent text they relayed. The gateway's frames are not
// in it.
func (r *TurnReader) HermesCut(content, displayKind string, summary bool) (string, []string, bool) {
	t, injected, pasted := hermesRow(content, displayKind, summary)
	switch {
	case injected:
		return "", nil, false
	case pasted:
		// The row's own pieces are the cut; what a harness or another agent
		// wrote in it is a piece of its own (digest.TurnPieces).
		blob, _ := digest.HermesOwnerText(content, displayKind, summary)
		return "", digest.OwnerPieces(digest.TurnPieces(blob)), false
	}
	t, cut, _, ok := cutWords(t, r.Pastes)
	if !ok || strings.TrimSpace(t) == "" {
		return "", cut, false
	}
	return t, cut, true
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
