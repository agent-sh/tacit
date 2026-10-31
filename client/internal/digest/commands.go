package digest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ClaudeCommands follows a Claude Code transcript's lines in order for the
// slash commands Claude Code runs itself (a local command: /effort, /model,
// /plugin, the desktop app's /auto-mode-setup wizard). Claude Code writes such
// a command as three lines of one prompt (promptId): a meta line holding
// <local-command-caveat> ("The command below was run directly in Claude
// Code, not sent to you as a request"), the command's echo (<command-name>,
// <command-args>) as a user line, and its <local-command-stdout>. The
// arguments are input to Claude Code, not words to the agent, and some are
// not typed at all: the desktop app fills the wizard's
// "--expect-sha256 <hash> --apply-file <path>". A command whose text goes to
// the agent (a skill's, /goal's condition) has no caveat: its arguments are
// the person's when they typed the turn (TurnPieces).
//
// Measured on real Claude Code transcripts (Claude Code 2.1.280 to 2.1.296):
// the echoes with arguments split about evenly between local commands, each
// after its caveat (/plugin, /effort, /auto-mode-setup, /model, /rules), and
// commands that are not (/goal, a skill typed in the desktop app, a skill run
// by `claude -p`). A skill's text, with the arguments filled in
// ("ARGUMENTS: ..."), follows its echo as a meta line: the harness's.
type ClaudeCommands struct {
	caveat, prompt string // the last caveat line's uuid and promptId
}

// claudeCommandLine is what Local reads of a line.
type claudeCommandLine struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	PromptID   string `json:"promptId"`
	Message    struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// LocalLine is Local on a raw transcript line.
func (c *ClaudeCommands) LocalLine(line []byte) bool {
	if !bytes.Contains(line, []byte("<local-command-caveat>")) && !bytes.Contains(line, []byte("<command-name>")) {
		return false
	}
	var l claudeCommandLine
	if json.Unmarshal(line, &l) != nil || l.Type != "user" {
		return false
	}
	return c.local(strings.Join(rawTexts(l.Message.Content), "\n"), l.UUID, l.ParentUUID, l.PromptID)
}

// Local reports whether a decoded Claude Code line is the echo of a local
// command: a user line holding <command-name> that follows its caveat (its
// parent, or a line of the same prompt), or holds the caveat itself. A
// caveat line is noted for the echo that follows it.
func (c *ClaudeCommands) Local(o map[string]any) bool {
	if t, _ := o["type"].(string); t != "user" {
		return false
	}
	var texts []string
	if msg, ok := o["message"].(map[string]any); ok {
		switch c := msg["content"].(type) {
		case string:
			texts = []string{c}
		case []any:
			for _, b := range c {
				bm, _ := b.(map[string]any)
				if bt, _ := bm["type"].(string); bt == "text" {
					s, _ := bm["text"].(string)
					texts = append(texts, s)
				}
			}
		}
	}
	uuid, _ := o["uuid"].(string)
	parent, _ := o["parentUuid"].(string)
	prompt, _ := o["promptId"].(string)
	return c.local(strings.Join(texts, "\n"), uuid, parent, prompt)
}

func (c *ClaudeCommands) local(text, uuid, parent, prompt string) bool {
	caveat := strings.Contains(text, "<local-command-caveat>")
	if !strings.Contains(text, "<command-name>") {
		if caveat {
			c.caveat, c.prompt = uuid, prompt
		}
		return false
	}
	return caveat || c.caveat != "" && (parent == c.caveat || prompt != "" && prompt == c.prompt)
}

// rawTexts are the texts of a message's content: a string, or its text
// blocks.
func rawTexts(content json.RawMessage) []string {
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
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}
