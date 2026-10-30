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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
)

// AgentInjected reports text that sits in a user turn of an owner session but
// holds nothing of theirs: it is all a harness's or another agent's
// (digest.TurnPieces: a notice, a framed message, a side agent's note, an
// orchestrator's role prompt).
func AgentInjected(text string) bool {
	_, ok := OwnerPart(text)
	return !ok
}

// OwnerPart is what of a user turn's text is the owner's (digest.TurnPieces:
// a harness's reminders and frames, a side agent's note and agents' prompts
// left out), one piece per line; ok is false when nothing is.
func OwnerPart(text string) (string, bool) {
	own, _, ok := ownerSplit(text)
	return own, ok
}

// ownerSplit is OwnerPart with the texts of the pieces it left out.
func ownerSplit(text string) (own string, other []string, ok bool) {
	var mine []string
	for _, p := range digest.TurnPieces(text) {
		switch {
		case p.Who == digest.Owner:
			mine = append(mine, p.Text)
		case strings.TrimSpace(p.Text) != "":
			other = append(other, p.Text)
		}
	}
	if len(mine) == 0 {
		return "", other, false
	}
	return strings.Join(mine, "\n"), other, true
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
		b.AddAt("claude-history", ref, pasted.ReplaceAllString(h.Display, " "), JSONTime(h.Timestamp))
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
		b.AddAt("codex-history", ref, h.Text, JSONTime(h.TS))
	})
}

// LoadPastes reads what the owner pasted into their prompts (`pastedContents` of
// ~/.claude/history.jsonl), so the transcripts' copies of them are left out of
// their turns (Add). Pastes under 40 characters are kept: a short paste is
// usually their own words.
func (b *Builder) LoadPastes(path string) error {
	ps, err := readPastes(path)
	b.pastes = append(b.pastes, ps...)
	return err
}

// readPastes reads the pastes of a Claude Code prompt history (LoadPastes),
// each once, in order.
func readPastes(path string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	err := (&Builder{}).jsonLines(path, func(line []byte) {
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
				out = append(out, c)
			}
		}
	})
	return out, err
}

// ClaudePastes are the texts the owner pasted into Claude Code prompts, as the
// corpus reads them (LoadPastes): those of home's ~/.claude/history.jsonl,
// read again only when the file changes; none when there is no history. A
// reader of the owner's turns (TurnReader.Pastes) cuts them from their words
// as the corpus does. They are the history's text, unredacted.
func ClaudePastes(home string) ([]string, error) {
	path := filepath.Join(home, ".claude", "history.jsonl")
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pasteCache.Lock()
	defer pasteCache.Unlock()
	if e, ok := pasteCache.byPath[path]; ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return slices.Clone(e.pastes), nil
	}
	ps, err := readPastes(path)
	if err != nil {
		return nil, err
	}
	if pasteCache.byPath == nil {
		pasteCache.byPath = map[string]pasteEntry{}
	}
	pasteCache.byPath[path] = pasteEntry{size: fi.Size(), mod: fi.ModTime(), pastes: ps}
	return slices.Clone(ps), nil
}

type pasteEntry struct {
	size   int64
	mod    time.Time
	pastes []string
}

var pasteCache struct {
	sync.Mutex
	byPath map[string]pasteEntry
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
			t, injected, pasted := hermesRow(m.Content, m.DisplayKind, m.Summary)
			if injected {
				b.c.Stats.AgentTurns++
				continue
			}
			if pasted {
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

// hermesRow is the text of a Hermes user row that can be the owner's, before
// what in it a harness or another agent wrote is cut (AddAt, TurnReader.Hermes): the
// gateway's frames, notices and attached context cut (digest.HermesOwnerText).
// injected reports a row with nothing of theirs, pasted a pasted table or log
// (PastedBlob).
func hermesRow(content, displayKind string, summary bool) (t string, injected, pasted bool) {
	t, injected = digest.HermesOwnerText(content, displayKind, summary)
	if injected {
		return "", true, false
	}
	if PastedBlob(t) {
		return "", false, true
	}
	return t, false, false
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
