package fact

import (
	"bytes"
	"strings"
	"testing"
)

const noteFact = "---\nname: pin\ndescription: the pinned version\ntype: design_decision\nscope: system\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-01\n---\n\nWe pin v0.7.17 of the tool.\n"

func TestAppendNoteOnlyAppends(t *testing.T) {
	line, err := NoteLine("2026-10-21", "v0.7.17", "v0.7.19")
	if err != nil || line != "> superseded 2026-10-21: v0.7.17 -> v0.7.19" {
		t.Fatalf("%q %v", line, err)
	}
	out, err := AppendNote([]byte(noteFact), line)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte(noteFact)) || !strings.HasSuffix(string(out), "\n\n"+line+"\n") {
		t.Fatalf("%q", out)
	}
	second, _ := NoteLine("2026-10-22", "v0.7.19", "v0.8.0")
	out2, err := AppendNote(out, second)
	if err != nil || !bytes.HasPrefix(out2, out) || !strings.HasSuffix(string(out2), line+"\n"+second+"\n") {
		t.Fatalf("a second note: %q %v", out2, err)
	}
	f, err := Parse("system/pin.md", out2)
	if err != nil {
		t.Fatal(err)
	}
	if notes, bad := Notes(f.Body); len(notes) != 2 || len(bad) != 0 {
		t.Fatalf("notes %v bad %v", notes, bad)
	}
	// CRLF files keep their line ending, and a body without a final newline
	// gets one before the note.
	crlf := strings.ReplaceAll(strings.TrimSuffix(noteFact, "\n"), "\n", "\r\n")
	out3, err := AppendNote([]byte(crlf), line)
	if err != nil || !bytes.HasPrefix(out3, []byte(crlf)) || !strings.HasSuffix(string(out3), "\r\n\r\n"+line+"\r\n") {
		t.Fatalf("crlf: %q %v", out3, err)
	}
}

func TestNoteLineRefusesMalformedNotes(t *testing.T) {
	for _, c := range [][3]string{
		{"2026-10-21", "", "x"}, {"2026-10-21", "x", " "}, {"21 Oct", "a", "b"}, {"2026-10-21", "a\nb", "c"}, {"2026-10-21", "same", "same"},
		{"2026-10-21", strings.Repeat("a", noteSide+1), "b"},
	} {
		if _, err := NoteLine(c[0], c[1], c[2]); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
	for _, l := range []string{"> superseded 2026-10-21: -> b", "> superseded 2026-10-21: a ->", "> superseded yesterday: a -> b", "> superseded 2026-10-21 a -> b"} {
		if CheckNote(l) == nil {
			t.Errorf("%q accepted", l)
		}
		if _, err := AppendNote([]byte(noteFact), l); err == nil {
			t.Errorf("appended %q", l)
		}
	}
	if _, bad := Notes("text\n> superseded soon: a -> b\n> quoted line\n"); len(bad) != 1 {
		t.Fatalf("bad %v", bad)
	}
}
