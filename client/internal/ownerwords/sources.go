package ownerwords

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
)

// agentInjected are user turns that an agent or a harness wrote into one of
// the owner's sessions: Claude Code's summary of a compacted conversation,
// a front end's status prompts, and scheduled automations.
var agentInjected = []string{
	"This session is being continued from a previous conversation",
	"Continue the current task",
	"Status check so I can identify which conversation",
	"Automation:",
	"[Request interrupted",
	"The app was quit while you were working",
}

// AgentInjected reports text that sits in a user turn of an owner session but
// was written by an agent or a harness: the notices above, a framed message
// (harnessFrame), and an orchestrator's role prompt ("You are manager-01 ...",
// "You are a worker for ..."; the owner writes in lower case, and no turn of theirs on
// this machine opens that way).
func AgentInjected(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range agentInjected {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return strings.HasPrefix(t, "You are ") || harnessFrame(t)
}

// pasted is Claude Code's placeholder for a paste in the prompt history; the
// pasted text itself is often an agent's output, so it is not their words.
var pasted = regexp.MustCompile(`\[Pasted text #\d+[^\]]*\]|\[Image #\d+[^\]]*\]`)

// AddClaudeHistory adds the prompts the owner typed into Claude Code
// (~/.claude/history.jsonl: one line per prompt, `display` and `sessionId`).
// Only interactive sessions write it, and it outlives the transcripts, which
// Claude Code deletes after a few weeks. Pastes are left out, and so is a
// prompt of a session its transcript shows is an agent's (AddTranscripts runs
// first) or whose project is a directory only agents use.
func (b *Builder) AddClaudeHistory(path string) error {
	return b.jsonLines(path, func(line []byte) {
		var h struct {
			Display   string          `json:"display"`
			SessionID string          `json:"sessionId"`
			Project   string          `json:"project"`
			Timestamp json.RawMessage `json:"timestamp"`
		}
		if json.Unmarshal(line, &h) != nil || h.Display == "" {
			return
		}
		if AgentCwd(h.Project) || (h.SessionID != "" && b.agentRefs["claude:"+h.SessionID]) {
			b.c.Stats.AgentTurns++
			return // typed into an agent's session (a lane a program drives through a terminal)
		}
		if strings.HasPrefix(strings.TrimSpace(h.Display), "!") {
			return // a shell command the owner ran, not words to an agent
		}
		ref := "claude:history"
		if h.SessionID != "" {
			ref = "claude:" + h.SessionID
		}
		b.AddAt("claude-history", ref, pasted.ReplaceAllString(h.Display, " "), jsonTime(h.Timestamp))
	})
}

// AddCodexHistory adds the prompts the owner typed into Codex
// (~/.codex/history.jsonl: `session_id`, `text`), less those of a session its
// transcript shows is an agent's (a subagent, an exec run).
func (b *Builder) AddCodexHistory(path string) error {
	return b.jsonLines(path, func(line []byte) {
		var h struct {
			SessionID string          `json:"session_id"`
			Text      string          `json:"text"`
			TS        json.RawMessage `json:"ts"`
		}
		if json.Unmarshal(line, &h) != nil || h.Text == "" {
			return
		}
		if h.SessionID != "" && b.agentRefs["codex:"+h.SessionID] {
			b.c.Stats.AgentTurns++
			return
		}
		ref := "codex:history"
		if h.SessionID != "" {
			ref = "codex:" + h.SessionID
		}
		b.AddAt("codex-history", ref, h.Text, jsonTime(h.TS))
	})
}

// LoadPastes reads what the owner pasted into their prompts (`pastedContents` of
// ~/.claude/history.jsonl), so the transcripts' copies of them are left out of
// their turns (Add). Pastes under 40 characters are kept: a short paste is
// usually their own words.
func (b *Builder) LoadPastes(path string) error {
	seen := map[string]bool{}
	return b.jsonLines(path, func(line []byte) {
		var h struct {
			Pasted map[string]struct {
				Content string `json:"content"`
			} `json:"pastedContents"`
		}
		if json.Unmarshal(line, &h) != nil {
			return
		}
		for _, p := range h.Pasted {
			if c := strings.TrimSpace(p.Content); len(c) >= 40 && !seen[c] {
				seen[c] = true
				b.pastes = append(b.pastes, c)
			}
		}
	})
}

func (b *Builder) jsonLines(path string, fn func([]byte)) error {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

// AddHermes adds what the owner wrote in the owner sessions of a Hermes store
// (hermes.Session.Owner), under "hermes:<session>": each user row's own words
// (digest.HermesOwnerText: frames, attached context and pastes cut), less the
// rows Hermes wrote again when it compacted a conversation, a pasted table or
// log (PastedBlob) and, in Add, a turn that opens with a tag (as piUserTurns).
// A store that cannot be read is an error, as a transcript is.
func (b *Builder) AddHermes(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := hermes.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	sessions, err := db.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if !s.Owner {
			b.c.Stats.AgentSessions++
			continue
		}
		ms, err := db.UserMessages(ctx, s.ID)
		if err != nil {
			return fmt.Errorf("session %s: %w", s.ID, err)
		}
		for _, m := range ms {
			if m.Copy {
				continue
			}
			t, injected := digest.HermesOwnerText(m.Content, m.DisplayKind, m.Summary)
			if injected {
				b.c.Stats.AgentTurns++
				continue
			}
			if PastedBlob(t) {
				continue // a pasted table or log, as for pi (Add drops a tag-framed turn)
			}
			var at time.Time
			if m.Timestamp > 0 {
				at = time.Unix(int64(m.Timestamp), 0)
			}
			b.AddAt("hermes", "hermes:"+s.ID, t, at)
		}
	}
	return nil
}

// LoadAll builds the corpus from every source on this machine: owner sessions
// in the Claude Code, Codex and pi transcripts and in the Hermes store
// (hermes.DefaultDB), the session digests under digests (when not empty) and
// both prompt histories.
func LoadAll(home, digests string) (*Corpus, error) {
	b := NewBuilder()
	if err := b.LoadPastes(filepath.Join(home, ".claude", "history.jsonl")); err != nil {
		return nil, err
	}
	if err := b.AddTranscripts(DefaultSources(home)); err != nil {
		return nil, err
	}
	if db := hermes.DefaultDB(home); db != "" {
		if err := b.AddHermes(db); err != nil {
			return nil, fmt.Errorf("hermes %s: %w", db, err)
		}
	}
	if digests != "" {
		if err := b.AddDigests(digests); err != nil {
			return nil, err
		}
	}
	if err := b.AddClaudeHistory(filepath.Join(home, ".claude", "history.jsonl")); err != nil {
		return nil, err
	}
	if err := b.AddCodexHistory(filepath.Join(home, ".codex", "history.jsonl")); err != nil {
		return nil, err
	}
	return b.Build(), nil
}
