package transcript

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// rollout is a Codex-shaped transcript of about n bytes: a session_meta line,
// then user and assistant messages, varied enough to fill many zstd blocks.
func rollout(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"timestamp":"2026-10-02T09:15:07.120Z","type":"session_meta","payload":{"id":"0c5e7a19-3b2d-4f6e-8a1c-9d0b2e4f6a8c","cwd":"/home/u/projects","originator":"codex-tui","source":"cli"}}` + "\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-10-02T09:%02d:%02d.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"turn %d: keep the cache warm %x"}]}}`+"\n", i/60%60, i%60, i, i*2654435761)
		fmt.Fprintf(&b, `{"timestamp":"2026-10-02T09:%02d:%02d.500Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done %d %x"}]}}`+"\n", i/60%60, i%60, i, i*40503)
	}
	return b.Bytes()
}

func encodeAll(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithWindowSize(2<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(plain, nil)
}

// streamed is a frame with no content size in its header (a streaming
// writer's).
func streamed(t *testing.T, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var h zstd.Header
	if err := h.Decode(out.Bytes()); err != nil || h.HasFCS {
		t.Fatalf("the streamed frame records its size (%v): the case is not tested", err)
	}
	return out.Bytes()
}

func skippable(n int) []byte {
	b := make([]byte, 8+n)
	binary.LittleEndian.PutUint32(b, 0x184D2A5E)
	binary.LittleEndian.PutUint32(b[4:], uint32(n))
	return b
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSizeAndReadsOfCompressedCopies(t *testing.T) {
	dir := t.TempDir()
	plain := rollout(3 << 20)
	half := len(plain) / 2
	cases := map[string][]byte{
		"one frame with its size": encodeAll(t, plain),
		"a frame with no size":    streamed(t, plain),
		// Frames may follow each other, with skippable frames between.
		"two frames and a skippable one": append(append(encodeAll(t, plain[:half]), skippable(13)...), streamed(t, plain[half:])...),
	}
	for name, packed := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name, "rollout-2026-10-02T09-15-07-0c5e7a19.jsonl.zst")
			write(t, path, packed)
			n, err := Size(path)
			if err != nil || n != int64(len(plain)) {
				t.Fatalf("size %d, %v; want %d", n, err, len(plain))
			}
			for _, off := range []int64{0, 1, int64(half) - 7, int64(len(plain)) - 100, int64(len(plain))} {
				r, err := OpenAt(path, off)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				r.Close()
				if err != nil || !bytes.Equal(got, plain[off:]) {
					t.Fatalf("at %d: %d bytes, %v", off, len(got), err)
				}
			}
			if _, err := OpenAt(path, int64(len(plain))+1); err == nil {
				t.Fatal("an offset past the end was read")
			}
			if h := Head(path, 300); !bytes.Equal(h, plain[:300]) {
				t.Fatalf("head %q", h)
			}
		})
	}
}

func TestPlainTranscriptsReadAsBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	plain := rollout(10 << 10)
	write(t, path, plain)
	if n, err := Size(path); err != nil || n != int64(len(plain)) {
		t.Fatalf("size %d %v", n, err)
	}
	r, err := OpenAt(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, _ := io.ReadAll(r); !bytes.Equal(got, plain[100:]) {
		t.Fatal("plain read differs")
	}
}

func TestNamesGlobAndResolve(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "sessions", "2026", "10", "13")
	both := filepath.Join(day, "rollout-a.jsonl")
	only := filepath.Join(day, "rollout-b.jsonl.zst")
	plainOnly := filepath.Join(day, "rollout-c.jsonl")
	write(t, both, []byte("{}\n"))
	write(t, both+Zst, encodeAll(t, []byte("{}\n")))
	write(t, only, encodeAll(t, []byte("{}\n")))
	write(t, plainOnly, []byte("{}\n"))
	write(t, filepath.Join(day, "notes.zst"), []byte("x")) // no transcript
	got, err := Glob(filepath.Join(dir, "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{both: true, only: true, plainOnly: true}
	if len(got) != len(want) {
		t.Fatalf("glob %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("glob %v", got)
		}
	}
	if Plain(only) != filepath.Join(day, "rollout-b.jsonl") || Name(only) != "rollout-b" || Name(plainOnly) != "rollout-c" || !Compressed(only) || Compressed(plainOnly) {
		t.Fatal("names")
	}
	if ok, _ := Match(filepath.Join(dir, "sessions", "*", "*", "*", "*.jsonl"), only); !ok {
		t.Fatal("a compressed copy does not match its glob")
	}
	// A path recorded while the transcript was plain finds its compressed copy.
	if Resolve(Plain(only)) != only || Resolve(plainOnly) != plainOnly || Resolve(both) != both {
		t.Fatal("resolve")
	}
	if b := Head(Plain(only), 10); string(b) != "{}\n" {
		t.Fatalf("head through resolve %q", b)
	}
}
