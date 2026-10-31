package ownerwords

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// A slash command Claude Code ran itself (its <local-command-caveat>) is not
// the owner's words, though they typed it or the desktop app filled it in;
// /goal's condition is theirs.
func TestCorpusSlashCommands(t *testing.T) {
	home := t.TempDir()
	lines := []string{
		`{"parentUuid":"c0","promptId":"p1","type":"user","message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user.</local-command-caveat>"},"isMeta":true,"uuid":"c1","entrypoint":"cli","sessionId":"own"}`,
		`{"parentUuid":"c1","promptId":"p1","type":"user","message":{"role":"user","content":"<command-name>/plugin</command-name>\n            <command-message>plugin</command-message>\n            <command-args>enable example-lint-helper@builtin</command-args>"},"uuid":"c2","entrypoint":"cli","sessionId":"own"}`,
		`{"parentUuid":"c2","promptId":"p1","type":"user","message":{"role":"user","content":"<local-command-stdout>(no content)</local-command-stdout>"},"uuid":"c3","entrypoint":"cli","sessionId":"own"}`,
		`{"parentUuid":"e0","type":"user","message":{"role":"user","content":"<command-name>/goal</command-name>\n            <command-message>goal</command-message>\n            <command-args>keep going until the import is done, then post the summary</command-args>"},"uuid":"e2","entrypoint":"cli","sessionId":"own"}`,
	}
	writeFile(t, filepath.Join(home, ".claude", "projects", "-home-u", "own.jsonl"), strings.Join(lines, "\n")+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Find("keep going until the import is done", "", true); m.Ref != "claude:own" {
		t.Fatalf("/goal's condition: %+v", m)
	}
	if m, ok := c.Find("enable example-lint-helper", "", true); ok {
		t.Fatalf("a local command's arguments are in the corpus: %+v", m)
	}
	r := &TurnReader{}
	var got []string
	for _, l := range lines {
		own, _ := r.Parts(digest.Claude, []byte(l))
		got = append(got, own...)
	}
	if len(got) != 1 || got[0] != "keep going until the import is done, then post the summary" {
		t.Fatalf("turn reader %q", got)
	}
}

// Codex compresses old sessions (.jsonl.zst) and moves archived ones; the
// owner's turns in both are read.
func TestCorpusReadsCompressedCodexSessions(t *testing.T) {
	home := t.TempDir()
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	session := func(id, text string) []byte {
		return enc.EncodeAll([]byte(strings.Join([]string{
			`{"timestamp":"2026-08-01T10:00:00.000Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/home/u","originator":"codex-tui","source":"cli"}}`,
			`{"timestamp":"2026-08-01T10:01:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`,
		}, "\n")+"\n"), nil)
	}
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "08", "01", "rollout-2026-08-01T10-00-00-01a0aaaa.jsonl.zst"), string(session("01a0aaaa", "never push to the release branch on friday")))
	writeFile(t, filepath.Join(home, ".codex", "archived_sessions", "rollout-2026-07-01T10-00-00-019fbbbb.jsonl.zst"), string(session("019fbbbb", "the bench box stays off at night")))
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"never push to the release branch on friday": "codex:01a0aaaa",
		"the bench box stays off at night":           "codex:019fbbbb",
	} {
		if m, _ := c.Find(q, "", true); m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
}
