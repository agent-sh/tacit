// Package digest compacts Claude Code, Codex and pi transcript JSONL, and
// Hermes session rows, into the line form the dreamer reads: U: what the
// owner typed, P: a prompt another agent or a program wrote (who.go decides
// which), H: harness-injected text, A: assistant text, T: tool call, E: tool
// error.
// Raw tool output is dropped and secrets are redacted. The Claude and Codex
// readers are the Go port of eval/mine/digest.py, which the truth set was
// mined with, so the dreamer sees what was measured.
package digest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
)

const (
	uMax, aMax, eMax, tMax, hMax = 700, 300, 240, 140, 200
	// spare is kept on every line until Finish redacts, so a secret that
	// straddles a cap is still whole when scrump sees it; the final cut
	// happens after masking.
	spare = 512
)

var (
	claudeHarness = regexp.MustCompile(`^\s*<(task-notification|local-command-stdout|local-command-caveat|command-name|command-message|command-args|system-reminder|agent-message|bash-input|bash-stdout|bash-stderr)\b`)
	codexHarness  = regexp.MustCompile(`^\s*(>>>|Host notice|<environment_context>|# AGENTS\.md|\[Recorded tool history\]|<turn_aborted>|<permissions|<collaboration)`)
	errRx         = regexp.MustCompile(`(?i)\b(error|failed|not found|no such|denied|traceback|panic|exited with code [1-9]\d*|exit status [1-9]\d*)\b`)
	ws            = regexp.MustCompile(`\s+`)
)

// Redact masks secret-shaped text with scrump's text profile, the one
// redactor for every tool on this machine (`scrump --profile text`). Each
// hit is replaced by "*" of the same length. If scrump is missing or fails,
// the text is withheld rather than passed through: under-redaction is a
// leak into a store that is pushed to a server. The detail goes to stderr,
// never into the returned text.
func Redact(s string) string {
	out, err := RedactErr(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "redaction failed:", err)
		return Unavailable
	}
	return out
}

// Unavailable is what Redact returns in place of text it could not redact.
const Unavailable = "[redaction unavailable]"

// RedactErr is Redact for callers that must not proceed on failure.
func RedactErr(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	return redactWithScrump(s)
}

// RedactAll redacts several texts in one scrump call. Texts are joined
// with a record separator that masking never touches; a changed count is
// a failure.
func RedactAll(texts []string) ([]string, error) {
	return RedactAllContext(context.Background(), texts)
}

// RedactAllContext is RedactAll that stops when ctx ends: a caller that gives
// up (a cancelled request) does not wait for scrump.
func RedactAllContext(ctx context.Context, texts []string) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Masks never overwrite a newline, so a separator wrapped in newlines
	// always survives; a bare control character could be eaten by a value
	// that runs to the end of its record.
	const sep = "\n\x1e\n"
	for _, t := range texts {
		if strings.Contains(t, "\x1e") {
			return nil, errors.New("text holds a record separator")
		}
	}
	out, err := redactWithScrumpCtx(ctx, strings.Join(texts, sep))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, sep)
	if len(parts) != len(texts) {
		// A hit ran across records (a key block whose END sits in a later
		// record). Halve until every part comes back whole.
		return bisectCtx(ctx, texts, sep)
	}
	return parts, nil
}

// bisect redacts texts joined by sep, splitting the batch in halves while
// the record count comes back changed. A single record is always exact.
func bisect(texts []string, sep string) ([]string, error) {
	return bisectCtx(context.Background(), texts, sep)
}

func bisectCtx(ctx context.Context, texts []string, sep string) ([]string, error) {
	if len(texts) == 1 {
		r, err := redactWithScrumpCtx(ctx, texts[0])
		if err != nil {
			return nil, err
		}
		return []string{r}, nil
	}
	out, err := redactWithScrumpCtx(ctx, strings.Join(texts, sep))
	if err != nil {
		return nil, err
	}
	if parts := strings.Split(out, sep); len(parts) == len(texts) {
		return parts, nil
	}
	mid := len(texts) / 2
	left, err := bisectCtx(ctx, texts[:mid], sep)
	if err != nil {
		return nil, err
	}
	right, err := bisectCtx(ctx, texts[mid:], sep)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

// RedactionReady proves scrump with the text profile works: a canary
// secret must come back masked at the same length.
func RedactionReady() error {
	// Built from pieces so no secret-shaped literal sits in the source.
	in := "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"
	out, err := redactWithScrump(in)
	if err != nil {
		return err
	}
	if len(out) != len(in) || !strings.Contains(out, "****") || strings.Contains(out, "Zq7Vx9") {
		return errors.New("scrump is installed but its text profile did not mask a canary; rebuild scrump from main")
	}
	return nil
}

func redactWithScrump(s string) (string, error) {
	return redactWithScrumpCtx(context.Background(), s)
}

func redactWithScrumpCtx(ctx context.Context, s string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "scrump", "--profile", "text", "scrub", "-", "-o", "-")
	platform.Prepare(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdin = strings.NewReader(s)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// A wrapper may exit while its children still hold stdout. WaitDelay
		// releases the pipes; cancel the group as well so those children stop.
		if cmd.Process != nil && cmd.Cancel != nil {
			_ = cmd.Cancel()
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("scrump: %w", ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New("scrump: " + msg)
	}
	if stdout.Len() != len(s) {
		// Masks keep the length; anything else is not scrump's text profile.
		return "", fmt.Errorf("scrump changed the length of the text (%d -> %d)", len(s), stdout.Len())
	}
	return stdout.String(), nil
}

// clip collapses whitespace and truncates to the cap plus spare. Finish
// redacts and makes the final cut, so a secret cut by the cap is still
// whole when it is masked.
func clip(s string, n int) string {
	n += spare
	s = strings.TrimSpace(ws.ReplaceAllString(s, " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Format is a transcript dialect.
type Format int

const (
	Claude Format = iota
	Codex
	// Pi is pi's session JSONL (see pi.go).
	Pi
	// Hermes is Hermes's session store, a SQLite database, not JSONL: its rows
	// are digested by HermesLines, and Lines reads nothing in this format.
	Hermes
)

// Lines digests JSONL from r in the given format. owner is the session's
// verdict (SessionWho(...).Owner()): in a session that is not the owner's,
// every user turn that is not harness text is another agent's prompt (P:),
// never the owner's (U:). In a Claude Code owner session each turn is decided
// again on its own line (ClaudeTyped). Only complete lines are consumed; the
// caller tracks byte offsets (see Delta).
func Lines(r io.Reader, f Format, owner bool) []string {
	out, _ := LinesAt(r, f, owner)
	return out
}

// LinesAt is Lines with, for each line, when the entry it came from was
// written (its "timestamp", which Claude Code, Codex and pi record on every
// entry); the zero time when the entry records none.
func LinesAt(r io.Reader, f Format, owner bool) ([]string, []time.Time) {
	var out []string
	var at []time.Time
	// The options a Codex agent's questions offered, for the answers that
	// follow (a window that starts after the question has none of them).
	asked := CodexAsked{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var o map[string]any
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var ls []string
		switch f {
		case Claude:
			ls = claudeLine(o, owner)
		case Codex:
			ls = codexLine(o, owner, asked)
		case Pi:
			ls = piLine(o, owner)
		}
		if len(ls) == 0 {
			continue
		}
		var t time.Time
		if s, ok := o["timestamp"].(string); ok {
			t, _ = time.Parse(time.RFC3339Nano, s)
		}
		out = append(out, ls...)
		for range ls {
			at = append(at, t)
		}
	}
	return out, at
}

// userLines are the digest lines of a user turn's text. In a turn the
// person typed (typed) it is split by who wrote each part (TurnPieces, with
// a Codex answer that is an option its question offered the model's,
// asked): their words U:, an agent's text inside their turn P:, the
// harness's H:. Any other turn is another agent's prompt, P:, or harness
// text, H:, whole.
func userLines(f Format, txt string, typed bool, asked CodexAsked) []string {
	if strings.TrimSpace(txt) == "" {
		return nil
	}
	if !typed {
		if HarnessText(f, txt) {
			return []string{"H: " + clip(txt, hMax)}
		}
		return []string{"P: " + clip(txt, uMax)}
	}
	var out []string
	for _, p := range asked.Chosen(TurnPieces(txt)) {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		switch p.Who {
		case Harness:
			out = append(out, "H: "+clip(p.Text, hMax))
		case Owner:
			out = append(out, "U: "+clip(p.Text, uMax))
		default:
			out = append(out, "P: "+clip(p.Text, uMax))
		}
	}
	return out
}

// capFor is the final width of a digest line by its prefix.
func capFor(line string) int {
	switch {
	case strings.HasPrefix(line, "U: "), strings.HasPrefix(line, "P: "):
		return uMax
	case strings.HasPrefix(line, "A: "):
		return aMax
	case strings.HasPrefix(line, "E: "):
		return eMax
	case strings.HasPrefix(line, "T: "):
		return tMax
	case strings.HasPrefix(line, "H: "):
		return hMax
	}
	return uMax
}

// Finish redacts a digest in one scrump call and cuts every line to its
// cap. Lines are the unredacted output of Lines; nothing from them may
// reach a model before Finish. An error means the window must not be used
// and its cursor must not move.
func Finish(lines []string) ([]string, error) {
	if len(lines) == 0 {
		return lines, nil
	}
	for _, l := range lines {
		if strings.Contains(l, "\n") {
			return nil, errors.New("digest line holds a newline")
		}
	}
	out, err := redactWithScrump(strings.Join(lines, "\n"))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\n")
	if len(parts) != len(lines) {
		// A hit ran across lines; halve until the structure holds.
		parts, err = bisect(lines, "\n")
		if err != nil {
			return nil, err
		}
	}
	for i, l := range parts {
		if r := []rune(l); len(r) > Width(l) {
			parts[i] = string(r[:Width(l)-1]) + "…"
		}
	}
	return parts, nil
}

// Width is the final width of a digest line in runes: its cap plus the
// prefix, except T lines whose cap already counts the prefix. Window sizes
// are computed on it, not on the uncut line.
func Width(line string) int {
	if strings.HasPrefix(line, "T: ") {
		return tMax
	}
	return capFor(line) + 3
}

// HarnessText reports text a harness injected into a user turn (a
// notification, a command echo, a reminder): not the owner's words.
func HarnessText(f Format, txt string) bool {
	switch f {
	case Claude:
		return claudeHarness.MatchString(txt)
	case Pi:
		return piHarness.MatchString(txt)
	case Hermes:
		_, injected := HermesOwnerText(txt, "", false)
		return injected
	}
	return codexHarness.MatchString(txt)
}

func claudeLine(o map[string]any, owner bool) []string {
	t, _ := o["type"].(string)
	if texts, typed, _, ok := ClaudeQueued(o); ok {
		// A message typed while the agent worked: the owner's (U:) in their
		// session, an agent's prompt (P:) in an agent's; a queued task
		// notification or another agent's message is left out, as before.
		if !typed {
			return nil
		}
		var out []string
		for _, txt := range texts {
			out = append(out, userLines(Claude, txt, owner, nil)...)
		}
		return out
	}
	if t != "user" && t != "assistant" {
		return nil
	}
	msg, _ := o["message"].(map[string]any)
	content := msg["content"]
	meta, _ := o["isMeta"].(bool)
	// The summary Claude Code writes as a user turn after a compaction is the
	// harness's text, not the owner's words.
	summary, _ := o["isCompactSummary"].(bool)
	meta = meta || summary
	typed := false
	if t == "user" && owner {
		typed, _ = ClaudeTyped(ClaudeMarksOf(o))
	}
	var out []string
	userText := func(txt string) {
		switch {
		case strings.TrimSpace(txt) == "":
		case meta:
			out = append(out, "H: "+clip(txt, hMax))
		default:
			out = append(out, userLines(Claude, txt, typed, nil)...)
		}
	}
	switch c := content.(type) {
	case string:
		if t == "user" {
			userText(c)
		}
	case []any:
		for _, b := range c {
			bm, _ := b.(map[string]any)
			bt, _ := bm["type"].(string)
			switch {
			case t == "user" && bt == "text":
				txt, _ := bm["text"].(string)
				userText(txt)
			case t == "user" && bt == "tool_result":
				if isErr, _ := bm["is_error"].(bool); isErr {
					out = append(out, "E: "+clip(blocksText(bm["content"]), eMax))
				}
			case t == "assistant" && bt == "text":
				txt, _ := bm["text"].(string)
				if strings.TrimSpace(txt) != "" {
					out = append(out, "A: "+clip(txt, aMax))
				}
			case t == "assistant" && bt == "tool_use":
				name, _ := bm["name"].(string)
				out = append(out, toolLine(name, bm["input"]))
			}
		}
	}
	return out
}

func codexLine(o map[string]any, owner bool, asked CodexAsked) []string {
	if o["type"] != "response_item" {
		return nil
	}
	asked.Note(o)
	p, _ := o["payload"].(map[string]any)
	pt, _ := p["type"].(string)
	var out []string
	switch pt {
	case "message":
		role, _ := p["role"].(string)
		cs, _ := p["content"].([]any)
		for _, b := range cs {
			bm, _ := b.(map[string]any)
			txt, _ := bm["text"].(string)
			if strings.TrimSpace(txt) == "" {
				continue
			}
			switch role {
			case "user":
				// A session imported from another harness (a Relay hand-off of
				// a Claude Code conversation) holds that harness's frames too:
				// one rule for every format (TurnPieces).
				out = append(out, userLines(Codex, txt, owner, asked)...)
			case "assistant":
				if !strings.HasPrefix(txt, "[Recorded tool history]") {
					out = append(out, "A: "+clip(txt, aMax))
				}
			}
		}
	case "function_call", "custom_tool_call":
		name, _ := p["name"].(string)
		var args any = p["arguments"]
		if args == nil {
			args = p["input"]
		}
		if s, ok := args.(string); ok {
			var m map[string]any
			if json.Unmarshal([]byte(s), &m) == nil {
				args = m
			} else {
				args = map[string]any{"cmd": s}
			}
		}
		out = append(out, toolLine(name, args))
	case "function_call_output", "custom_tool_call_output":
		s := blocksText(p["output"])
		head := s
		if len(head) > 400 {
			head = head[:400]
		}
		if errRx.MatchString(head) {
			out = append(out, "E: "+clip(s, eMax))
		}
	}
	return out
}

func toolLine(name string, input any) string {
	m, _ := input.(map[string]any)
	key := ""
	for _, k := range []string{"file_path", "path", "command", "cmd", "pattern", "query", "url"} {
		if v, ok := m[k].(string); ok && v != "" {
			key = v
			break
		}
	}
	return clip("T: "+name+" "+key, tMax)
}

func blocksText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, x := range v {
			parts = append(parts, blocksText(x))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		for _, k := range []string{"text", "content", "output"} {
			if s, ok := v[k].(string); ok {
				return s
			}
			if v[k] != nil {
				return blocksText(v[k])
			}
		}
	}
	return ""
}

// Interesting reports whether a digest has enough substance for the dreamer:
// at least one owner line, agent prompt or tool error, and assistant text.
// An agent's session is read like the owner's: what its agent found out is
// agents' notes all the same.
func Interesting(lines []string) bool {
	var u, e, a int
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "U: "), strings.HasPrefix(l, "P: "):
			u++
		case strings.HasPrefix(l, "E: "):
			e++
		case strings.HasPrefix(l, "A: "):
			a++
		}
	}
	return (u+e) >= 1 && a >= 1
}
