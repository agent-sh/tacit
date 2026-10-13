// Package digest compacts Claude Code and Codex transcript JSONL into the
// line form the dreamer reads: U: owner text, H: harness-injected text,
// A: assistant text, T: tool call, E: tool error. Raw tool output is
// dropped and secrets are redacted. It is the Go port of eval/mine/digest.py,
// which the truth set was mined with, so the dreamer sees what was measured.
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
	cmd.Stdin = strings.NewReader(s)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
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
)

// Lines digests JSONL from r in the given format. Only complete lines are
// consumed; the caller tracks byte offsets (see Delta).
func Lines(r io.Reader, f Format) []string {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var o map[string]any
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		if f == Claude {
			out = append(out, claudeLine(o)...)
		} else {
			out = append(out, codexLine(o)...)
		}
	}
	return out
}

// capFor is the final width of a digest line by its prefix.
func capFor(line string) int {
	switch {
	case strings.HasPrefix(line, "U: "):
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
	if f == Claude {
		return claudeHarness.MatchString(txt)
	}
	return codexHarness.MatchString(txt)
}

func claudeLine(o map[string]any) []string {
	t, _ := o["type"].(string)
	if t != "user" && t != "assistant" {
		return nil
	}
	msg, _ := o["message"].(map[string]any)
	content := msg["content"]
	meta, _ := o["isMeta"].(bool)
	var out []string
	userText := func(txt string) {
		if strings.TrimSpace(txt) == "" {
			return
		}
		if meta || claudeHarness.MatchString(txt) {
			out = append(out, "H: "+clip(txt, hMax))
		} else {
			out = append(out, "U: "+clip(txt, uMax))
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

func codexLine(o map[string]any) []string {
	if o["type"] != "response_item" {
		return nil
	}
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
				if codexHarness.MatchString(txt) {
					out = append(out, "H: "+clip(txt, hMax))
				} else {
					out = append(out, "U: "+clip(txt, uMax))
				}
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
// at least one owner line or tool error, and assistant text.
func Interesting(lines []string) bool {
	var u, e, a int
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "U: "):
			u++
		case strings.HasPrefix(l, "E: "):
			e++
		case strings.HasPrefix(l, "A: "):
			a++
		}
	}
	return (u+e) >= 1 && a >= 1
}
