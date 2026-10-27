package digest

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// Harness is who wrote a piece of a turn that a harness put there (a
// reminder, a notification, a command echo, a summary, a frame): no one's
// words.
const Harness Who = "harness"

// Piece is a part of a user turn's text and who wrote it: Owner (the person
// whose turn it is), Agent (another agent's text inside it) or Harness.
type Piece struct {
	Who  Who
	Text string
	// Item is the question a Codex answer replies to (the reply's
	// questionItemId), "" for any other piece: an answer that is one of the
	// options its question offered is the model's words (CodexAsked).
	Item string
}

var (
	// systemReminder is a whole block Claude Code wraps its own notes in.
	systemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	// sideNote opens a note a side agent offers: Claude Code delivers it as
	// a turn of the person's (origin.kind "human", promptSource "typed"),
	// the note's body quoted line by line with "> ", and what the person
	// typed in reply below it. A copy pasted from the terminal starts with
	// its prompt mark, "❯ ", and keeps the quote's indent.
	sideNote = regexp.MustCompile(`^\s*(?:❯\s*)?Here is a note offered by a side agent:`)
	// questionReply is Codex's reply to the questions an agent asked the
	// person (request_user_input): a JSON list of {answer, question,
	// questionItemId}.
	questionReply = regexp.MustCompile(`(?s)^\s*<send_user_message_question_reply>\s*(.*?)\s*</send_user_message_question_reply>\s*$`)
	commandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// InjectedNotices open turns a harness or a front end wrote into a person's
// session: Claude Code's summary of a compacted conversation, a front end's
// status prompts, scheduled automations, an interruption mark, and a Relay
// hand-off of a conversation to another harness (which then holds the
// transcript as one user message).
var InjectedNotices = []string{
	"This session is being continued from a previous conversation",
	"Continue the current task",
	"Status check so I can identify which conversation",
	"Automation:",
	"[Request interrupted",
	"The app was quit while you were working",
	"The user moved this existing Relay conversation to you",
}

// TurnPieces splits the text of a user turn into who wrote each part. It is
// the one rule for what in a turn's text is the person's, for the dreamer's
// digest (U:, P:, H:) and the owner corpus alike; whether the turn is a
// person's at all is decided first, from the line (ClaudeTyped,
// ClaudeQueued, SessionWho). In order:
//   - every whole <system-reminder> block is the harness's: Claude Code puts
//     one before (or after) a message the person typed (a background task
//     they started, the scratch folder of a session with no project), and
//     the rest is decided on its own;
//   - a note a side agent offers (sideNote): its header and the quoted lines
//     that follow it are that agent's; what the person typed below the quote
//     is theirs;
//   - a slash command's echo is the harness's, its <command-args> the
//     person's, unless Claude Code ran the command itself (its
//     <local-command-caveat>; ClaudeCommands, which the line readers ask);
//   - a Codex question reply holds the person's answers, each in its answer
//     field; the questions in it are the asking agent's, already in its
//     turn;
//   - any other text that opens with a tag (Claude Code's and Codex's
//     frames, a delegation, a task notification, an attached file), a
//     harness's own heading (HarnessText) or one of InjectedNotices is the
//     harness's;
//   - an orchestrator's role prompt ("You are manager-01 ...", "You are a
//     worker for ..."): an agent's; the person writes in lower case, and no
//     turn of theirs opens that way;
//   - the rest is the person's.
func TurnPieces(text string) []Piece {
	var out []Piece
	rest := systemReminder.ReplaceAllStringFunc(text, func(b string) string {
		out = append(out, Piece{Who: Harness, Text: b})
		return "\n"
	})
	return append(out, pieces(rest)...)
}

// pieces is TurnPieces on text with no reminder block left.
func pieces(text string) []Piece {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil
	}
	if sideNote.MatchString(t) {
		ls := strings.Split(t, "\n")
		j := 1
		for j < len(ls) && (strings.TrimSpace(ls[j]) == "" || strings.HasPrefix(strings.TrimLeft(ls[j], " \t"), ">")) {
			j++
		}
		out := []Piece{{Who: Agent, Text: strings.TrimSpace(strings.Join(ls[:j], "\n"))}}
		return append(out, pieces(strings.Join(ls[j:], "\n"))...)
	}
	if m := questionReply.FindStringSubmatch(t); m != nil {
		var items []struct {
			Answer   string `json:"answer"`
			Question string `json:"question"`
			Item     string `json:"questionItemId"`
		}
		if json.Unmarshal([]byte(m[1]), &items) != nil {
			return []Piece{{Who: Harness, Text: t}}
		}
		var out []Piece
		for _, it := range items {
			if q := strings.TrimSpace(it.Question); q != "" {
				out = append(out, Piece{Who: Harness, Text: q})
			}
			if a := strings.TrimSpace(it.Answer); a != "" {
				out = append(out, Piece{Who: Owner, Text: a, Item: it.Item})
			}
		}
		return out
	}
	if strings.HasPrefix(t, "<") || claudeHarness.MatchString(t) || codexHarness.MatchString(t) {
		// A local command's echo with its caveat (ClaudeCommands) is the
		// harness's whole.
		if m := commandArgs.FindStringSubmatch(t); m != nil && strings.Contains(t, "<command-name>") && !strings.Contains(t, "<local-command-caveat>") {
			out := []Piece{{Who: Harness, Text: strings.TrimSpace(commandArgs.ReplaceAllString(t, ""))}}
			if a := strings.TrimSpace(m[1]); a != "" {
				out = append(out, pieces(a)...)
			}
			return out
		}
		return []Piece{{Who: Harness, Text: t}}
	}
	for _, p := range InjectedNotices {
		if strings.HasPrefix(t, p) {
			return []Piece{{Who: Harness, Text: t}}
		}
	}
	if strings.HasPrefix(t, "You are ") {
		return []Piece{{Who: Agent, Text: t}}
	}
	return []Piece{{Who: Owner, Text: t}}
}

// OwnerPieces are the texts of ps that are the person's.
func OwnerPieces(ps []Piece) []string {
	var out []string
	for _, p := range ps {
		if p.Who == Owner {
			out = append(out, p.Text)
		}
	}
	return out
}

// CodexAsked is what the questions a Codex agent asked the person offered:
// a request_user_input call lists options for each question, and the reply
// names the question each answer is for (questionItemId: the tool, the
// call's id and the question's index). An answer that is one of its
// question's options is a choice the person made among the model's words,
// not words of theirs, as ClaudeTyped holds an accepted suggestion. Keyed by
// call id and index ("call_x#0").
type CodexAsked map[string][]string

// Note records the options of a Codex request_user_input call on a decoded
// transcript line; any other line is left alone.
func (a CodexAsked) Note(o map[string]any) {
	if a == nil || o["type"] != "response_item" {
		return
	}
	p, _ := o["payload"].(map[string]any)
	name, _ := p["name"].(string)
	if p["type"] != "function_call" || !strings.HasPrefix(name, "request_user_input") {
		return
	}
	id, _ := p["call_id"].(string)
	args, _ := p["arguments"].(string)
	var in struct {
		Questions []struct {
			Options []json.RawMessage `json:"options"`
		} `json:"questions"`
	}
	if id == "" || json.Unmarshal([]byte(args), &in) != nil {
		return
	}
	for i, q := range in.Questions {
		var opts []string
		for _, raw := range q.Options {
			var s string
			var lab struct {
				Label string `json:"label"`
			}
			switch {
			case json.Unmarshal(raw, &s) == nil:
			case json.Unmarshal(raw, &lab) == nil:
				s = lab.Label
			}
			if s = strings.TrimSpace(s); s != "" {
				opts = append(opts, s)
			}
		}
		a[fmt.Sprintf("%s#%d", id, i)] = opts
	}
}

// NoteLine is Note on a raw transcript line.
func (a CodexAsked) NoteLine(line []byte) {
	if !bytes.Contains(line, []byte(`"request_user_input`)) || !bytes.Contains(line, []byte(`"function_call"`)) {
		return
	}
	var o map[string]any
	if json.Unmarshal(line, &o) == nil {
		a.Note(o)
	}
}

// Offered reports an answer that is one of the options the question it
// replies to (item, a questionItemId) offered.
func (a CodexAsked) Offered(item, answer string) bool {
	var id []any
	if a == nil || json.Unmarshal([]byte(item), &id) != nil || len(id) < 3 {
		return false
	}
	call, _ := id[1].(string)
	idx, ok := id[2].(float64)
	if !ok {
		return false
	}
	ans := strings.Join(strings.Fields(answer), " ")
	for _, o := range a[fmt.Sprintf("%s#%d", call, int(idx))] {
		if strings.EqualFold(strings.Join(strings.Fields(o), " "), ans) {
			return true
		}
	}
	return false
}

// Chosen is ps with each answer that is an option its question offered
// (Offered) made the harness's: the person chose it, the model wrote it.
func (a CodexAsked) Chosen(ps []Piece) []Piece {
	for i, p := range ps {
		if p.Who == Owner && p.Item != "" && a.Offered(p.Item, p.Text) {
			ps[i].Who, ps[i].Text = Harness, "the person chose the option offered: "+p.Text
		}
	}
	return ps
}
