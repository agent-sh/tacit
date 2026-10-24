package digest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Who typed a session's user turns. This file is the one place that decides
// it, for the dreamer (which labels a turn U: or P:) and for the owner corpus
// (ownerwords: which turns are the owner's words). A turn is the owner's only
// when what its format records proves a person typed it; anything else, an
// agent's prompt, a scheduler's, a harness's, or a turn nothing tells apart,
// is not.
//
// Per session (SessionWho), from the transcript's path and its first bytes:
//
//   - all formats: a working directory only agents use (AgentCwd) is an
//     agent's session;
//   - Claude Code: a subagent's transcript (<session>/subagents/) is an
//     agent's; a session whose lines name no person's entrypoint ("cli",
//     "claude-desktop") is an agent's (`claude -p` is "sdk-cli", an Agent SDK
//     lane "sdk-ts"). Every other session may hold owner turns, and each of
//     its turns is decided on its own line (ClaudeTyped), which needs the
//     line's own marks or entrypoint;
//   - Codex: session_meta names the front end (`originator`) and how the
//     thread was started (`source`, `thread_source`, `parent_thread_id`). A
//     subagent a parent thread spawned reports the person's front end
//     (codex-tui) with `source: {"subagent": ...}`, `thread_source:
//     "subagent"` and the parent's id; `codex exec` is `source: "exec"`. Only
//     a person's front end with none of those is the owner's;
//   - pi: a subagent's session (under <session>/tasks/, or a header naming
//     its parentSession) is an agent's, and so is a session a program started
//     (PiHeadless, by timing: pi records no mode).
//
// Hermes sessions are decided from the session row (hermes.Session.Owner).
type Who string

const (
	Owner   Who = "owner"
	Agent   Who = "agent"
	Unknown Who = "unknown"
)

// Verdict is who a session's user turns come from, and why.
type Verdict struct {
	Who Who
	Why string
}

// Owner reports a session whose turns can be the owner's.
func (v Verdict) Owner() bool { return v.Who == Owner }

var (
	// ClaudeOwnerEntrypoints are the Claude Code entrypoints a person types
	// into: a terminal and the desktop app.
	ClaudeOwnerEntrypoints = map[string]bool{"cli": true, "claude-desktop": true}
	// CodexOwnerOriginators are the Codex front ends a person types into;
	// anything else (codex_exec, an SDK or app-server client) is not them.
	CodexOwnerOriginators = map[string]bool{"Codex Desktop": true, "codex-tui": true, "codex_cli_rs": true, "codex_vscode": true}
	// AgentCwds are where only agents start sessions: a runner's worker
	// checkouts, scratch and temporary directories, Claude Code's background
	// jobs, probe and e2e drivers.
	AgentCwds = []string{"/.local/runners/", "/.claude/jobs/", "/.cache/", "/scratchpad", "/e2e/"}
	// AgentCwdPrefixes are directory prefixes no session of theirs starts in:
	// the temporary directories of Linux and of macOS, where /tmp is
	// /private/tmp and $TMPDIR is under /var/folders.
	AgentCwdPrefixes = []string{"/tmp/", "/var/tmp/", "/private/tmp/", "/private/var/tmp/", "/var/folders/", "/private/var/folders/"}
	// AgentCwdExact are directories only agents start sessions in, as such.
	AgentCwdExact = map[string]bool{"/tmp": true, "/var/tmp": true, "/private/tmp": true, "/private/var/tmp": true}
)

// AgentCwd reports a working directory only agents start sessions in. A
// directory inside the home directory is never taken for a temporary one by
// its prefix, wherever the home is (a test's or a sandbox's home under /tmp).
func AgentCwd(cwd string) bool {
	if cwd == "" {
		return false
	}
	if AgentCwdExact[strings.TrimRight(cwd, "/")] {
		return true
	}
	if !inHome(cwd) {
		for _, p := range AgentCwdPrefixes {
			if strings.HasPrefix(cwd, p) {
				return true
			}
		}
	}
	for _, part := range AgentCwds {
		if strings.Contains(cwd, part) {
			return true
		}
	}
	return false
}

func inHome(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		return false
	}
	home = strings.TrimRight(filepath.ToSlash(home), "/")
	dir = filepath.ToSlash(dir)
	return dir == home || strings.HasPrefix(dir, home+"/")
}

var (
	headCwdRx   = regexp.MustCompile(`"cwd":"([^"]*)"`)
	headEntryRx = regexp.MustCompile(`"entrypoint":"([^"]*)"`)
	// The fields of a Codex session_meta line, for a first line too long to
	// be whole in the head (its instructions run to many kilobytes).
	codexOriginatorRx = regexp.MustCompile(`"originator":"([^"]*)"`)
	codexSourceRx     = regexp.MustCompile(`"source":("[^"]*"|\{"subagent")`)
	codexThreadSrcRx  = regexp.MustCompile(`"thread_source":"([^"]*)"`)
	codexParentRx     = regexp.MustCompile(`"parent_thread_id":"([^"]+)"`)
)

// SessionWho decides who a transcript's user turns come from, from its path
// and its first bytes (the first 256 KiB are enough: every format writes what
// it records about the session at the start).
func SessionWho(path string, head []byte, f Format) Verdict {
	p := filepath.ToSlash(path)
	switch f {
	case Claude:
		return claudeWho(p, head)
	case Codex:
		return codexWho(head)
	case Pi:
		return piWho(p, head)
	}
	return Verdict{Unknown, "no transcript format"}
}

func claudeWho(path string, head []byte) Verdict {
	if strings.Contains(path, "/subagents/") {
		return Verdict{Agent, "claude subagent"}
	}
	if m := headCwdRx.FindSubmatch(head); m != nil && AgentCwd(string(m[1])) {
		return Verdict{Agent, "agent cwd"}
	}
	ms := headEntryRx.FindAllSubmatch(head, -1)
	if ms == nil {
		// Nothing in the head (a first line longer than it, an old version):
		// each line decides on its own marks and entrypoint (ClaudeTyped).
		return Verdict{Owner, "no entrypoint in the head; each turn decides"}
	}
	seen := map[string]bool{}
	var eps []string
	for _, m := range ms {
		e := string(m[1])
		if ClaudeOwnerEntrypoints[e] {
			return Verdict{Owner, "entrypoint " + e}
		}
		if !seen[e] {
			seen[e] = true
			eps = append(eps, e)
		}
	}
	return Verdict{Agent, "entrypoint " + strings.Join(eps, ",")}
}

// CodexMeta is what a Codex session_meta line says about how the thread
// started.
type CodexMeta struct {
	ID           string
	Cwd          string
	Originator   string
	Source       string // "cli", "vscode", "exec", "subagent", ...
	ThreadSource string // "user", "subagent", "guardian_review", ... ("" in older versions)
	Parent       string // parent_thread_id
}

// ReadCodexMeta reads the session_meta line at the start of head; ok is
// false when head does not open with one. A line cut off by the end of head
// is read field by field.
func ReadCodexMeta(head []byte) (CodexMeta, bool) {
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if !bytes.Contains(line, []byte(`"session_meta"`)) {
		return CodexMeta{}, false
	}
	var o struct {
		Type    string `json:"type"`
		Payload struct {
			ID           string          `json:"id"`
			Cwd          string          `json:"cwd"`
			Originator   string          `json:"originator"`
			Source       json.RawMessage `json:"source"`
			ThreadSource string          `json:"thread_source"`
			Parent       string          `json:"parent_thread_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &o) == nil {
		if o.Type != "session_meta" {
			return CodexMeta{}, false
		}
		m := CodexMeta{ID: o.Payload.ID, Cwd: o.Payload.Cwd, Originator: o.Payload.Originator, ThreadSource: o.Payload.ThreadSource, Parent: o.Payload.Parent}
		var s string
		var d map[string]json.RawMessage
		switch {
		case json.Unmarshal(o.Payload.Source, &s) == nil:
			m.Source = s
		case json.Unmarshal(o.Payload.Source, &d) == nil:
			if _, ok := d["subagent"]; ok {
				m.Source = "subagent"
			} else {
				for k := range d {
					m.Source = k
				}
			}
		}
		return m, true
	}
	var m CodexMeta
	get := func(rx *regexp.Regexp) string {
		if s := rx.FindSubmatch(line); s != nil {
			return string(s[1])
		}
		return ""
	}
	m.Originator = get(codexOriginatorRx)
	m.ThreadSource = get(codexThreadSrcRx)
	m.Parent = get(codexParentRx)
	m.Cwd = get(headCwdRx)
	if s := get(codexSourceRx); s == `{"subagent"` {
		m.Source = "subagent"
	} else {
		m.Source = strings.Trim(s, `"`)
	}
	return m, true
}

func codexWho(head []byte) Verdict {
	m, ok := ReadCodexMeta(head)
	if !ok {
		return Verdict{Unknown, "no session_meta"}
	}
	switch {
	case AgentCwd(m.Cwd):
		return Verdict{Agent, "agent cwd"}
	case m.Source == "subagent" || m.ThreadSource == "subagent" || m.Parent != "":
		return Verdict{Agent, "codex subagent"}
	case m.Source == "exec":
		return Verdict{Agent, "codex exec"}
	case m.ThreadSource != "" && m.ThreadSource != "user":
		return Verdict{Agent, "codex thread_source " + m.ThreadSource}
	case m.Originator == "":
		return Verdict{Unknown, "no originator"}
	case !CodexOwnerOriginators[m.Originator]:
		return Verdict{Agent, "originator " + m.Originator}
	}
	return Verdict{Owner, "originator " + m.Originator}
}

func piWho(path string, head []byte) Verdict {
	if strings.Contains(path, "/tasks/") && strings.Contains(path, "/sessions/") {
		return Verdict{Agent, "pi subagent"}
	}
	h, ok := ReadPiHeader(head)
	if !ok {
		return Verdict{Unknown, "no pi header"}
	}
	switch {
	case h.ParentSession != "":
		return Verdict{Agent, "pi subagent"}
	case AgentCwd(h.Cwd):
		return Verdict{Agent, "agent cwd"}
	case PiHeadless(head):
		return Verdict{Agent, "pi headless"}
	}
	return Verdict{Owner, "pi interactive"}
}

// ClaudeMarks are what a Claude Code transcript line records about who wrote
// a user turn. Current versions mark each turn three ways: `origin.kind`
// ("human" is typed by a person; "task-notification", "peer", "plugin" are
// agents' or the harness's), `promptSource` ("typed" and "queued" are a
// person's; "suggestion_accepted" is a suggestion the model wrote and the
// person accepted; "system", "sdk" are not a person's) and `turnOrigin`
// ("human", or "task_notification", "scheduled", "peer", "sdk", "system").
// The desktop app sends a person's turn through the SDK (promptSource "sdk")
// and marks it origin.kind "human". Older versions mark none: the line's
// entrypoint is all there is.
type ClaudeMarks struct {
	Type           string          `json:"type"`
	Entrypoint     string          `json:"entrypoint"`
	IsMeta         bool            `json:"isMeta"`
	IsCompact      bool            `json:"isCompactSummary"`
	TranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	Sidechain      bool            `json:"isSidechain"`
	Origin         json.RawMessage `json:"origin"`
	PromptSource   string          `json:"promptSource"`
	TurnOrigin     string          `json:"turnOrigin"`
	ScheduledTask  string          `json:"scheduledTaskId"`
}

// OriginKind is origin.kind ("" when there is none; a repository origin,
// {"host": ...}, has no kind).
func (m ClaudeMarks) OriginKind() string {
	var o struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(m.Origin, &o)
	return o.Kind
}

// ClaudeMarksOf reads the marks of a decoded transcript line.
func ClaudeMarksOf(o map[string]any) ClaudeMarks {
	s := func(k string) string { v, _ := o[k].(string); return v }
	b := func(k string) bool { v, _ := o[k].(bool); return v }
	m := ClaudeMarks{Type: s("type"), Entrypoint: s("entrypoint"), IsMeta: b("isMeta"), IsCompact: b("isCompactSummary"),
		TranscriptOnly: b("isVisibleInTranscriptOnly"), Sidechain: b("isSidechain"), PromptSource: s("promptSource"),
		TurnOrigin: s("turnOrigin"), ScheduledTask: s("scheduledTaskId")}
	if org, ok := o["origin"].(map[string]any); ok {
		if k, _ := org["kind"].(string); k != "" {
			m.Origin, _ = json.Marshal(map[string]string{"kind": k})
		}
	}
	return m
}

var (
	humanPromptSources = map[string]bool{"typed": true, "queued": true}
)

// ClaudeTyped reports a Claude Code user line a person typed, and why not
// when it is not. Harness text inside a typed turn is the caller's to drop
// (HarnessText). In order:
//   - a meta line, a compaction summary, a transcript-only line and a
//     sidechain (a subagent's turn inside the session) are no one's typing;
//   - a scheduled task's prompt is the scheduler's;
//   - origin.kind, when the line has one, decides: "human" is a person;
//   - else turnOrigin, when the line has one: "human" is a person;
//   - else promptSource, when the line has one: "typed" or "queued";
//   - else (a version that marks nothing) the entrypoint: "cli" or
//     "claude-desktop".
//
// An accepted suggestion is not their words even under origin.kind "human":
// the model wrote it.
func ClaudeTyped(m ClaudeMarks) (bool, string) {
	switch {
	case m.Type != "user":
		return false, "not a user line"
	case m.IsMeta || m.IsCompact || m.TranscriptOnly:
		return false, "harness line"
	case m.Sidechain:
		return false, "sidechain"
	case m.ScheduledTask != "":
		return false, "scheduled task"
	case m.PromptSource == "suggestion_accepted":
		return false, "accepted suggestion"
	}
	if k := m.OriginKind(); k != "" {
		if k == "human" {
			return true, ""
		}
		return false, "origin " + k
	}
	if m.TurnOrigin != "" {
		if m.TurnOrigin == "human" {
			return true, ""
		}
		return false, "turnOrigin " + m.TurnOrigin
	}
	if m.PromptSource != "" {
		if humanPromptSources[m.PromptSource] {
			return true, ""
		}
		return false, "promptSource " + m.PromptSource
	}
	if ClaudeOwnerEntrypoints[m.Entrypoint] {
		return true, ""
	}
	if m.Entrypoint == "" {
		return false, "no entrypoint"
	}
	return false, "entrypoint " + m.Entrypoint
}

// ClaudeQueued reads a Claude Code line that delivers a message queued while
// the agent was working: Claude Code records what the owner types mid-turn
// not as a user line but as an attachment of type queued_command, delivered
// after the tool result it waited for, in order with the rest of the turn.
// ok is false for any other line. texts are the message's text (its prompt,
// a string or text blocks), and typed reports a message a person typed, and
// why not when it is not:
//   - a sidechain's (a subagent's) and a meta attachment are no one's typing;
//   - a command mode other than "prompt" (a task notification) is the
//     harness's;
//   - origin.kind, when it has one, decides: "human" is a person ("peer",
//     "coordinator", "task-notification" are agents' or the harness's);
//   - else (versions before origin was recorded) a line of a person's
//     entrypoint, as ClaudeTyped falls back to it.
//
// Versions from 2.1.296 also mark a person's queued message humanTurn; it
// adds nothing origin.kind does not say. Harness text inside the message is
// the caller's to drop (HarnessText), as for a user line.
func ClaudeQueued(o map[string]any) (texts []string, typed bool, why string, ok bool) {
	if t, _ := o["type"].(string); t != "attachment" {
		return nil, false, "", false
	}
	a, _ := o["attachment"].(map[string]any)
	if at, _ := a["type"].(string); at != "queued_command" {
		return nil, false, "", false
	}
	switch p := a["prompt"].(type) {
	case string:
		texts = []string{p}
	case []any:
		for _, b := range p {
			bm, _ := b.(map[string]any)
			if bt, _ := bm["type"].(string); bt == "text" {
				s, _ := bm["text"].(string)
				texts = append(texts, s)
			}
		}
	}
	side, _ := o["isSidechain"].(bool)
	meta, _ := a["isMeta"].(bool)
	mode, _ := a["commandMode"].(string)
	kind := ""
	if org, isMap := a["origin"].(map[string]any); isMap {
		kind, _ = org["kind"].(string)
	}
	entry, _ := o["entrypoint"].(string)
	switch {
	case side:
		return texts, false, "sidechain", true
	case meta:
		return texts, false, "harness line", true
	case mode != "" && mode != "prompt":
		return texts, false, "commandMode " + mode, true
	case kind == "human":
		return texts, true, "", true
	case kind != "":
		return texts, false, "origin " + kind, true
	case ClaudeOwnerEntrypoints[entry]:
		return texts, true, "", true
	case entry == "":
		return texts, false, "no entrypoint", true
	}
	return texts, false, "entrypoint " + entry, true
}

// ClaudeQueuedLine is ClaudeQueued on a raw transcript line.
func ClaudeQueuedLine(line []byte) (texts []string, typed bool, why string, ok bool) {
	if !bytes.Contains(line, []byte(`"queued_command"`)) {
		return nil, false, "", false
	}
	var o map[string]any
	if json.Unmarshal(line, &o) != nil {
		return nil, false, "", false
	}
	return ClaudeQueued(o)
}
