package digest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// PiSessionGlob matches pi's top-level sessions: $PI_CODING_AGENT_SESSION_DIR
// holds them directly; otherwise the sessions directory of
// $PI_CODING_AGENT_DIR, else of ~/.pi/agent, holds a directory per working
// directory. These are pi's own variables, read the way pi reads them (a
// leading ~ is the home directory), so the dreamer needs them in its
// environment when pi has them in its own.
func PiSessionGlob(home string) string {
	tilde := func(p string) string {
		if p == "~" {
			return home
		}
		if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	if d := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
		return filepath.Join(tilde(d), "*.jsonl")
	}
	agent := filepath.Join(home, ".pi", "agent")
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		agent = tilde(d)
	}
	return filepath.Join(agent, "sessions", "*", "*.jsonl")
}

// pi (github.com/earendil-works/pi) writes one JSONL file per session under
// ~/.pi/agent/sessions/--<cwd>--/<time>_<id>.jsonl (PiSessionGlob): a header
// line ({"type":"session","id":...,"cwd":...,"version":...}), then entries. A
// "message" entry holds an AgentMessage by role: "user", "assistant" (text,
// thinking and toolCall blocks, errorMessage when the provider failed),
// "toolResult" (isError), "bashExecution" (a !command the person ran),
// "custom" (an extension's message) and summaries of compacted or abandoned
// branches. "custom_message" entries are text an extension put into the
// context. Subagents write their sessions one level down, with the parent in
// the header (<session>/tasks/<time>_<id>.jsonl, "parentSession"): the glob
// does not reach them, and SessionWho takes either mark for an agent's.
var piHarness = regexp.MustCompile(`^\s*<[A-Za-z][\w:-]*[\s>/]`) // an attached file (<file name=...>), a skill or template expansion

// PiHeadlessGap is how soon after pi set a session up its first user turn
// arrives when a program sent it. pi records no mode: `pi -p`, an RPC or SDK
// driver and an interactive session write the same file. A program sends its
// prompt as soon as the session is set up; a person reads the screen and
// types. Measured 2026-10-20 on 131 sessions (32 on a Mac, 99 on a Linux
// workstation): 84 headless first turns came at most 0.18 s after pi's last
// startup entry, 47 typed ones 1.3 s and more (the fastest a two-letter "hi").
const PiHeadlessGap = time.Second

// piStartup are the entries pi itself writes when it sets a session up: the
// header, the model, the thinking level and the name. An extension's entries
// do not count: one may write when the prompt is submitted.
var piStartup = map[string]bool{"session": true, "model_change": true, "thinking_level_change": true, "session_info": true}

// PiHeadless reports a pi session a program started, from its first bytes
// (the header through the first user turn): the first user turn came less
// than PiHeadlessGap after pi's last startup entry. A person who starts pi
// with the prompt on the command line (`pi "fix it"`) looks the same and is
// taken for a program; the error is on the safe side. false when no user turn
// is in head.
func PiHeadless(head []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var prev time.Time
	for sc.Scan() {
		var o struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, o.Timestamp)
		if err != nil {
			continue
		}
		switch {
		case o.Type == "message" && o.Message.Role == "user":
			return !prev.IsZero() && at.Sub(prev) < PiHeadlessGap
		case piStartup[o.Type]:
			prev = at
		}
	}
	return false
}

// PiHeader is the first line of a pi session: its id, where it started and,
// for a subagent's session, the session that spawned it.
type PiHeader struct {
	ID            string `json:"id"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
}

// ReadPiHeader parses the header line at the start of head; ok is false when
// head does not open with one.
func ReadPiHeader(head []byte) (PiHeader, bool) {
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	var h struct {
		PiHeader
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &h) != nil || h.Type != "session" {
		return PiHeader{}, false
	}
	return h.PiHeader, true
}

// piLine digests one pi entry; owner is the session's verdict (piWho).
func piLine(o map[string]any, owner bool) []string {
	switch o["type"] {
	case "custom_message":
		if txt := blocksText(o["content"]); strings.TrimSpace(txt) != "" {
			return []string{"H: " + clip(txt, hMax)}
		}
		return nil
	case "message":
	default:
		return nil
	}
	m, _ := o["message"].(map[string]any)
	role, _ := m["role"].(string)
	var out []string
	switch role {
	case "user":
		txt := piText(m["content"])
		if strings.TrimSpace(txt) == "" {
			break
		}
		out = append(out, userLines(Pi, txt, owner, nil)...)
	case "assistant":
		cs, _ := m["content"].([]any)
		for _, b := range cs {
			bm, _ := b.(map[string]any)
			switch bm["type"] {
			case "text":
				if txt, _ := bm["text"].(string); strings.TrimSpace(txt) != "" {
					out = append(out, "A: "+clip(txt, aMax))
				}
			case "toolCall":
				name, _ := bm["name"].(string)
				out = append(out, toolLine(name, bm["arguments"]))
			}
		}
		if e, _ := m["errorMessage"].(string); strings.TrimSpace(e) != "" {
			out = append(out, "E: "+clip(e, eMax))
		}
	case "toolResult":
		if isErr, _ := m["isError"].(bool); isErr {
			out = append(out, "E: "+clip(piText(m["content"]), eMax))
		}
	case "bashExecution":
		cmd, _ := m["command"].(string)
		out = append(out, toolLine("bash", map[string]any{"command": cmd}))
		if code, ok := m["exitCode"].(float64); ok && code != 0 {
			s, _ := m["output"].(string)
			out = append(out, "E: "+clip(s, eMax))
		}
	case "custom", "compactionSummary", "branchSummary":
		txt := piText(m["content"])
		if s, _ := m["summary"].(string); s != "" {
			txt = s
		}
		if strings.TrimSpace(txt) != "" {
			out = append(out, "H: "+clip(txt, hMax))
		}
	}
	return out
}

// piText is the text of a pi content: a string, or the text blocks of a list
// (images left out).
func piText(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	cs, _ := c.([]any)
	var parts []string
	for _, b := range cs {
		bm, _ := b.(map[string]any)
		if bm["type"] == "text" {
			if s, _ := bm["text"].(string); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " ")
}
