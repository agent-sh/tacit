package dream

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// codexRollout is a Codex owner session as Codex writes it (session_meta of a
// person's front end, then response items), with turns typed by the owner.
func codexRollout(id string, turns ...string) string {
	var b strings.Builder
	b.WriteString(`{"timestamp":"2026-10-12T21:00:32.366Z","type":"session_meta","payload":{"session_id":"` + id + `","id":"` + id + `","timestamp":"2026-10-12T21:00:32.201Z","cwd":"/home/u","originator":"codex-tui","cli_version":"0.162.1","source":"cli","thread_source":"user"}}` + "\n")
	for i, t := range turns {
		b.WriteString(codexTurn(i, t))
	}
	return b.String()
}

func codexTurn(i int, text string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-12T21:%02d:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, i%60, text) + "\n" +
		fmt.Sprintf(`{"timestamp":"2026-10-12T21:%02d:30.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"On it. %s"}]}}`, i%60, strings.Repeat("z", minNewBytes)) + "\n"
}

// compress rolls a transcript into its .jsonl.zst copy and removes it, as
// Codex does.
func compress(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	out := path + ".zst"
	if err := os.WriteFile(out, enc.EncodeAll(b, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	enc.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return out
}

func codexSources(home string) []Source {
	return []Source{
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "archived_sessions", "*.jsonl"), Format: digest.Codex},
	}
}

func joined(ws []Window) string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Lines...)
	}
	return strings.Join(out, "\n")
}

// A cursor holds when Codex compresses the session (.jsonl to .jsonl.zst)
// and when it archives it: the key stays the plain path, the offset is in
// the JSONL bytes, and the next window starts where the last one ended.
func TestCompressedSessionKeepsItsCursor(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	home := t.TempDir()
	day := filepath.Join(home, ".codex", "sessions", "2026", "10", "13")
	os.MkdirAll(day, 0o755)
	name := "rollout-2026-10-13T00-00-32-01a131ea-c641-7a43-9ec9-a2dbedef807b"
	plain := filepath.Join(day, name+".jsonl")
	os.WriteFile(plain, []byte(codexRollout("01a131ea", "old turn, recorded before the dreamer")), 0o644)
	srcs := codexSources(home)
	st := &State{Offsets: map[string]int64{}}
	if ws, _ := Windows(srcs, st, true); len(ws) != 0 {
		t.Fatal("a first run read history")
	}
	at := st.Offsets[plain]
	if at == 0 {
		t.Fatalf("no cursor: %v", st.Offsets)
	}

	// The session goes on, then Codex compresses it.
	f, _ := os.OpenFile(plain, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(codexTurn(1, "ship the hotfix from the release branch only"))
	f.Close()
	packed := compress(t, plain)
	ws, _ := Windows(srcs, st, false)
	if len(ws) != 1 || ws[0].File != plain || ws[0].Start != at || ws[0].Session != name {
		t.Fatalf("after compression: %d windows %+v", len(ws), ws)
	}
	text := joined(ws)
	if !strings.Contains(text, "U: ship the hotfix from the release branch only") || strings.Contains(text, "old turn") {
		t.Fatalf("lines:\n%s", text)
	}
	size, _ := os.Stat(packed)
	if p := st.Packed[packed]; p.Bytes != size.Size() || p.Size != ws[0].End {
		t.Fatalf("the copy's size is not kept: %+v (window ends at %d)", p, ws[0].End)
	}
	st.Offsets[ws[0].File] = ws[0].End
	if ws, _ := Windows(srcs, st, false); len(ws) != 0 {
		t.Fatalf("a compressed copy read again: %d windows (file %d bytes)", len(ws), size.Size())
	}

	// Archived: Codex moves the file; the cursor follows the session.
	arch := filepath.Join(home, ".codex", "archived_sessions", name+".jsonl.zst")
	os.MkdirAll(filepath.Dir(arch), 0o755)
	if err := os.Rename(packed, arch); err != nil {
		t.Fatal(err)
	}
	end := st.Offsets[plain]
	if ws, _ := Windows(srcs, st, false); len(ws) != 0 {
		t.Fatalf("an archived session was read again: %d windows", len(ws))
	}
	if _, ok := st.Offsets[plain]; ok || st.Offsets[filepath.Join(filepath.Dir(arch), name+".jsonl")] != end {
		t.Fatalf("the cursor did not follow the session: %v", st.Offsets)
	}
	if _, ok := st.Packed[packed]; ok || st.Packed[arch].Size != end {
		t.Fatalf("sizes kept: %v", st.Packed)
	}
}

// A dreamer that read only plain transcripts (no compressed root in its
// state) records the compressed copies it never read at their end: Codex's
// history is not replayed. Once the root is recorded, a compressed session it
// has never seen is read from its start.
func TestCompressedRootStartsAtItsEnd(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	home := t.TempDir()
	day := filepath.Join(home, ".codex", "sessions", "2026", "07", "01")
	os.MkdirAll(day, 0o755)
	srcs := codexSources(home)
	old := filepath.Join(day, "rollout-2026-07-01T10-00-00-01aaaaaa.jsonl")
	os.WriteFile(old, []byte(codexRollout("01aaaaaa", "july history the old dreamer never read")), 0o644)
	compress(t, old)
	// The state of a dreamer before v0.9.11: its roots, no compressed one.
	st := &State{Offsets: map[string]int64{"other": 1}}
	for _, s := range srcs {
		st.Sources = append(st.Sources, s.root())
	}
	if ws, _ := Windows(srcs, st, false); len(ws) != 0 {
		t.Fatalf("history replayed: %s", joined(ws))
	}
	if st.Offsets[old] == 0 || !slices.Contains(st.Sources, srcs[0].packedRoot()) {
		t.Fatalf("not recorded at its end: %v %v", st.Offsets, st.Sources)
	}

	newer := filepath.Join(day, "rollout-2026-07-01T11-00-00-01bbbbbb.jsonl")
	os.WriteFile(newer, []byte(codexRollout("01bbbbbb", "keep the canary at one percent")), 0o644)
	compress(t, newer)
	ws, _ := Windows(srcs, st, false)
	if len(ws) != 1 || ws[0].File != newer || ws[0].Start != 0 || !strings.Contains(joined(ws), "U: keep the canary at one percent") {
		t.Fatalf("a new compressed session: %d windows\n%s", len(ws), joined(ws))
	}
}
