// Package agentreview is tacit's reviewer (SPEC.md, "The reviewer"): a
// deciding model reads the conversation an agent's note came from and judges
// the note correct, uncertain or wrong; an uncertain one becomes a short
// question the owner answers in the session they are in, and only their own
// transcript turn counts as the answer. The prompt, the digest and the
// verdicts follow the rulebook Claude mod (~/.claude/mods/rulebook), whose
// verdicts tacit honours.
package agentreview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/harnesses"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// Tool is one tool call of an agent message: its name, its input as JSON.
type Tool struct {
	Name  string
	Input string
}

// Msg is one message of a conversation. Role is "user" or "assistant".
// Owner marks a user message the owner typed themself (OwnerTurn); any other
// user message (a side agent's note, a task report, a hook, another session)
// is a note, never their word.
type Msg struct {
	Role  string
	Text  string
	Owner bool
	Tools []Tool
	// At is when the message was written (RFC 3339), when the transcript
	// says: a statement made from an owner's turn is dated by it.
	At string
}

// Conversation is a session read for the reviewer.
type Conversation struct {
	Harness string // claude, codex, pi, hermes
	Session string // the harness's session id
	Path    string // the transcript, or the Hermes store
	Cwd     string // where the session ran, when the transcript says
	Msgs    []Msg
}

// Ref is the conversation's reference as the ledgers write it: <harness>:<id>.
func (c Conversation) Ref() string { return c.Harness + ":" + c.Session }

// ErrNoSession is a session no transcript on this machine holds.
var ErrNoSession = errors.New("no transcript of that session on this machine")

// maxTranscript is how much of a transcript the reviewer reads: its last
// part, where the save is, when it is longer.
const maxTranscript = 64 << 20

// Sources says where each harness keeps its sessions on this machine (the
// dreamer's sources, dream.DefaultSources): Claude Code's, Codex's and pi's
// transcript globs and the Hermes store.
type Sources struct {
	Claude, Codex, Pi []string
	HermesDB          string
}

// DefaultSources are the dreamer's sources under home: the top-level ones of
// harnesses.Sources.
func DefaultSources(home string) Sources {
	var src Sources
	for _, s := range harnesses.TopLevel(harnesses.Sources(home)) {
		switch s.Format {
		case digest.Claude:
			src.Claude = append(src.Claude, s.Glob)
		case digest.Codex:
			src.Codex = append(src.Codex, s.Glob)
		case digest.Pi:
			src.Pi = append(src.Pi, s.Glob)
		case digest.Hermes:
			src.HermesDB = s.DB
		}
	}
	return src
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{3,127}$`)

// ValidSession reports a session id code may record: letters, digits and
// _.:- only, so it never names a path.
func ValidSession(id string) bool { return sessionID.MatchString(id) && !strings.Contains(id, "..") }

// Locate finds the transcript of a harness's session: a Claude Code
// transcript named <id>.jsonl, a Codex rollout or pi session whose name ends
// in <id>.jsonl, or the compressed copy of one (<id>.jsonl.zst). Hermes
// sessions are rows of its store (ReadHermes).
func (src Sources) Locate(harness, id string) (string, digest.Format, bool) {
	if !ValidSession(id) {
		return "", 0, false
	}
	var globs []string
	var f digest.Format
	switch harness {
	case "claude":
		globs, f = src.Claude, digest.Claude
	case "codex":
		globs, f = src.Codex, digest.Codex
	case "pi":
		globs, f = src.Pi, digest.Pi
	default:
		return "", 0, false
	}
	var found []string
	for _, g := range globs {
		dir, pat := filepath.Split(g)
		matches, _ := transcript.Glob(filepath.Join(dir, strings.Replace(pat, "*.jsonl", "*"+id+".jsonl", 1)))
		for _, m := range matches {
			base := transcript.Name(m)
			if base == id || strings.HasSuffix(base, "-"+id) || strings.HasSuffix(base, "_"+id) {
				if fi, err := os.Lstat(m); err == nil && fi.Mode().IsRegular() {
					found = append(found, m)
				}
			}
		}
	}
	if len(found) == 0 {
		return "", 0, false
	}
	sort.Strings(found)
	return found[0], f, true
}

// Load reads the session a note names: its transcript, or its rows of the
// Hermes store.
func (src Sources) Load(ctx context.Context, harness, id string) (Conversation, error) {
	if harness == "hermes" {
		if src.HermesDB == "" {
			return Conversation{}, ErrNoSession
		}
		return ReadHermes(ctx, src.HermesDB, id)
	}
	path, f, ok := src.Locate(harness, id)
	if !ok {
		return Conversation{}, ErrNoSession
	}
	c, err := ReadTranscript(path, f)
	c.Harness, c.Session = harness, id
	return c, err
}

// OwnerTurn is the one place the reviewer decides what in a transcript line
// the owner typed themself, by the decision the dreamer and the owner corpus
// make (digest/who.go): the session must be one a person types into
// (digest.SessionWho on its path and first bytes, ownerSession), and the
// line's turn theirs (digest.ClaudeTyped, digest.ClaudeQueued for a message
// typed mid-turn), read as the corpus reads it (ownerwords.TurnTexts). An
// agent's prompt in a headless session, a subagent's brief, a task
// notification or an agent relaying "the owner says yes" is never their turn.
func OwnerTurn(f digest.Format, ownerSession bool, line []byte) []string {
	if !ownerSession {
		return nil
	}
	return ownerwords.TurnTexts(f, line)
}

// reader is how a transcript's lines are read in order: whether a person
// types into the session, and the owner's turns read across its lines
// (ownerwords.TurnReader, as OwnerTurn reads one).
type reader struct {
	owner bool
	turns *ownerwords.TurnReader
}

// ownerParts is OwnerTurn on a line read in order, with the text another
// agent wrote into that turn of theirs (a side agent's note they answered).
func (r reader) ownerParts(f digest.Format, line []byte) (own, other []string) {
	if !r.owner {
		return nil, nil
	}
	if r.turns == nil {
		return ownerwords.TurnTexts(f, line), nil
	}
	return r.turns.Parts(f, line)
}

// headBytes is how much of a transcript decides whether a person types into it.
const headBytes = 256 << 10

// ReadTranscript reads a Claude Code, Codex or pi transcript into messages.
func ReadTranscript(path string, f digest.Format) (Conversation, error) {
	return ReadTranscriptFrom(path, f, -1)
}

// ReadTranscriptFrom reads a transcript's messages from byte offset on (a
// question's answer is read from what came after it was asked); a negative
// offset reads its last maxTranscript bytes. Whether a person types into the
// session is decided on its first bytes either way. A transcript the harness
// compressed since its path was recorded is read from its compressed copy
// (transcript.Resolve); offsets are in its JSONL bytes either way.
func ReadTranscriptFrom(path string, f digest.Format, offset int64) (Conversation, error) {
	c := Conversation{Path: path}
	size, err := transcript.Size(path)
	if err != nil {
		return c, err
	}
	head := transcript.Head(path, headBytes)
	owner := digest.SessionWho(transcript.Plain(path), head, f).Owner()
	c.Cwd = cwdOf(head)
	from := int64(0)
	switch {
	case offset >= 0:
		from = offset
	case size > maxTranscript:
		from = size - maxTranscript
	}
	from = min(from, size)
	fh, err := transcript.OpenAt(path, from)
	if err != nil {
		return c, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	rd := reader{owner: owner, turns: &ownerwords.TurnReader{}}
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if first {
			first = false
			if !json.Valid(line) {
				continue // the cut through a line of a long transcript
			}
		}
		if c.Cwd == "" {
			c.Cwd = cwdOf(line)
		}
		c.Msgs = append(c.Msgs, lineMsgs(f, rd, line)...)
	}
	return c, sc.Err()
}

var cwdField = regexp.MustCompile(`"cwd":"([^"]*)"`)

func cwdOf(line []byte) string {
	if m := cwdField.FindSubmatch(line); m != nil {
		return string(m[1])
	}
	return ""
}

// lineMsgs is the messages of one transcript line, with the line's time.
func lineMsgs(f digest.Format, owner reader, line []byte) []Msg {
	var ms []Msg
	switch f {
	case digest.Claude:
		ms = claudeMsgs(owner, line)
	case digest.Codex:
		ms = codexMsgs(owner, line)
	case digest.Pi:
		ms = piMsgs(owner, line)
	}
	if len(ms) > 0 {
		if at := lineTime(line); at != "" {
			for i := range ms {
				ms[i].At = at
			}
		}
	}
	return ms
}

// lineTime is a transcript line's own time (ownerwords.LineTime), RFC 3339
// in UTC; "" when it has none.
func lineTime(line []byte) string {
	if t := ownerwords.LineTime(line); !t.IsZero() {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func userMsg(f digest.Format, owner reader, line []byte, texts []string) []Msg {
	if mine, other := owner.ownerParts(f, line); len(mine) > 0 {
		var ms []Msg
		if note := strings.TrimSpace(strings.Join(other, "\n")); note != "" {
			ms = append(ms, Msg{Role: "user", Text: note}) // what they answered: another agent's
		}
		return append(ms, Msg{Role: "user", Owner: true, Text: strings.Join(mine, "\n")})
	}
	t := strings.TrimSpace(strings.Join(texts, "\n"))
	if t == "" {
		return nil
	}
	return []Msg{{Role: "user", Text: t}}
}

func claudeMsgs(owner reader, line []byte) []Msg {
	var o struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if texts, _, _, ok := digest.ClaudeQueuedLine(line); ok {
		// A message delivered while the agent worked: the owner's when they
		// typed it, else a note (a task notification).
		return userMsg(digest.Claude, owner, line, texts)
	}
	if json.Unmarshal(line, &o) != nil {
		return nil
	}
	if o.Type != "user" && o.Type != "assistant" {
		return nil
	}
	var s string
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(o.Message.Content, &s) == nil {
		blocks = append(blocks, struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "text", Text: s})
	} else if json.Unmarshal(o.Message.Content, &blocks) != nil {
		return nil
	}
	if o.Type == "user" {
		var texts []string
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return nil // a tool's output, not a message
			}
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return userMsg(digest.Claude, owner, line, texts)
	}
	m := Msg{Role: "assistant"}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			m.Tools = append(m.Tools, Tool{Name: b.Name, Input: string(b.Input)})
		}
	}
	m.Text = strings.TrimSpace(strings.Join(texts, "\n"))
	if m.Text == "" && len(m.Tools) == 0 {
		return nil
	}
	return []Msg{m}
}

func codexMsgs(owner reader, line []byte) []Msg {
	var o struct {
		Type    string `json:"type"`
		Payload struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Input     json.RawMessage `json:"input"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "response_item" {
		return nil
	}
	p := o.Payload
	switch p.Type {
	case "message":
		var texts []string
		for _, c := range p.Content {
			texts = append(texts, c.Text)
		}
		switch p.Role {
		case "user":
			return userMsg(digest.Codex, owner, line, texts)
		case "assistant":
			t := strings.TrimSpace(strings.Join(texts, "\n"))
			if t == "" || strings.HasPrefix(t, "[Recorded tool history]") {
				return nil
			}
			return []Msg{{Role: "assistant", Text: t}}
		}
	case "function_call", "custom_tool_call":
		in := string(p.Arguments)
		if in == "" {
			in = string(p.Input)
		}
		var str string
		if json.Unmarshal([]byte(in), &str) == nil {
			in = str
		}
		return []Msg{{Role: "assistant", Tools: []Tool{{Name: p.Name, Input: in}}}}
	}
	return nil
}

func piMsgs(owner reader, line []byte) []Msg {
	var o struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "message" {
		return nil
	}
	var s string
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(o.Message.Content, &s) == nil {
		blocks = append(blocks, struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{Type: "text", Text: s})
	} else if json.Unmarshal(o.Message.Content, &blocks) != nil {
		return nil
	}
	switch o.Message.Role {
	case "user":
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return userMsg(digest.Pi, owner, line, texts)
	case "assistant":
		m := Msg{Role: "assistant"}
		var texts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "toolCall":
				m.Tools = append(m.Tools, Tool{Name: b.Name, Input: string(b.Arguments)})
			}
		}
		m.Text = strings.TrimSpace(strings.Join(texts, "\n"))
		if m.Text == "" && len(m.Tools) == 0 {
			return nil
		}
		return []Msg{m}
	}
	return nil
}

// ReadHermes reads a Hermes session from its store (read only). Only a
// session the owner types into (hermes.Session.Owner) holds turns of theirs.
func ReadHermes(ctx context.Context, db, sid string) (Conversation, error) {
	return ReadHermesAfter(ctx, db, sid, 0)
}

// HermesLastID is a Hermes session's newest message id (where a question
// asked now stands).
func HermesLastID(ctx context.Context, db, sid string) (int64, error) {
	d, err := hermes.Open(db)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	ids, err := d.LastIDs(ctx)
	return ids[sid], err
}

// ReadHermesAfter reads a Hermes session's messages after message id after.
func ReadHermesAfter(ctx context.Context, db, sid string, after int64) (Conversation, error) {
	c := Conversation{Harness: "hermes", Session: sid, Path: db}
	if !ValidSession(sid) {
		return c, ErrNoSession
	}
	d, err := hermes.Open(db)
	if err != nil {
		return c, err
	}
	defer d.Close()
	sessions, err := d.Sessions(ctx)
	if err != nil {
		return c, err
	}
	owner, found := false, false
	for _, s := range sessions {
		if s.ID == sid {
			found, owner = true, s.Owner
			c.Cwd = s.Cwd
			if s.RepoRoot != "" {
				c.Cwd = s.RepoRoot
			}
		}
	}
	if !found {
		return c, ErrNoSession
	}
	rows, err := d.Messages(ctx, sid, after, maxTranscript)
	if err != nil {
		return c, err
	}
	for _, r := range rows {
		if r.Copy {
			continue
		}
		at := ""
		if r.Timestamp > 0 {
			at = time.Unix(int64(r.Timestamp), 0).UTC().Format(time.RFC3339)
		}
		n := len(c.Msgs)
		switch r.Role {
		case "user":
			text, injected := digest.HermesOwnerText(r.Content, r.DisplayKind, r.Summary)
			if own, ok := ownerwords.OwnerPart(text); owner && !injected && ok {
				c.Msgs = append(c.Msgs, Msg{Role: "user", Owner: true, Text: own})
			} else if t := strings.TrimSpace(r.Content); t != "" {
				c.Msgs = append(c.Msgs, Msg{Role: "user", Text: t})
			}
		case "assistant":
			m := Msg{Role: "assistant", Text: strings.TrimSpace(r.Content)}
			var calls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if json.Unmarshal([]byte(r.ToolCalls), &calls) == nil {
				for _, cl := range calls {
					m.Tools = append(m.Tools, Tool{Name: cl.Function.Name, Input: cl.Function.Arguments})
				}
			}
			if m.Text != "" || len(m.Tools) > 0 {
				c.Msgs = append(c.Msgs, m)
			}
		}
		for i := n; i < len(c.Msgs); i++ {
			c.Msgs[i].At = at
		}
	}
	return c, nil
}
