package agentreview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// The reviewer finds a Codex session Codex compressed (.jsonl.zst) or
// archived, and a question's answer recorded at an offset of the plain file is
// read from the compressed copy at the same offset.
func TestReviewerReadsCompressedCodexSessions(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "sessions", "2026", "10", "13")
	plain := filepath.Join(day, "rollout-2026-10-13T00-00-32-01a1cccc.jsonl")
	head := `{"timestamp":"2026-10-13T00:00:32.000Z","type":"session_meta","payload":{"id":"01a1cccc","cwd":"/h/projects/tacit","originator":"codex-tui","source":"cli"}}` + "\n" +
		`{"timestamp":"2026-10-13T00:01:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"is the dreamer reading codex?"}]}}` + "\n"
	answer := `{"timestamp":"2026-10-13T00:02:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"yes, keep the hooks under 300 ms"}]}}` + "\n"
	writeLines(t, plain, strings.TrimSuffix(head+answer, "\n"))
	offset := int64(len(head)) // where a question was asked
	enc, _ := zstd.NewWriter(nil)
	b, _ := os.ReadFile(plain)
	os.WriteFile(plain+".zst", enc.EncodeAll(b, nil), 0o600)
	enc.Close()
	os.Remove(plain)

	src := Sources{Codex: []string{filepath.Join(dir, "sessions", "*", "*", "*", "*.jsonl"), filepath.Join(dir, "archived_sessions", "*.jsonl")}}
	p, f, ok := src.Locate("codex", "01a1cccc")
	if !ok || p != plain+".zst" || f != digest.Codex {
		t.Fatalf("locate: %q %v", p, ok)
	}
	// The path recorded with the question is the plain one.
	c, err := ReadTranscriptFrom(plain, digest.Codex, offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Msgs) != 1 || !c.Msgs[0].Owner || !strings.Contains(c.Msgs[0].Text, "keep the hooks under 300 ms") {
		t.Fatalf("answer: %+v", c.Msgs)
	}
	c, err = ReadTranscript(p, digest.Codex)
	if err != nil || len(c.Msgs) != 2 || c.Cwd != "/h/projects/tacit" {
		t.Fatalf("whole: %+v %v", c, err)
	}
	arch := filepath.Join(dir, "archived_sessions", filepath.Base(p))
	os.MkdirAll(filepath.Dir(arch), 0o700)
	os.Rename(p, arch)
	if p, _, ok := src.Locate("codex", "01a1cccc"); !ok || p != arch {
		t.Fatalf("locate archived: %q %v", p, ok)
	}
}
