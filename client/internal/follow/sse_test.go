package follow

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func frames(t *testing.T, in string) ([]Frame, error) {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	var out []Frame
	for {
		f, err := r.Next()
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

// The exact bytes the server's Feed module writes: the connected comment,
// two journal events in one chunk, a keepalive.
func TestReaderParsesTheServersFrames(t *testing.T) {
	in := ": connected head=7\n\n" +
		"id: 6\nevent: head_moved\ndata: {\"seq\":6,\"ref\":\"refs/heads/main\"}\n\n" +
		"id: 7\nevent: head_moved\ndata: {\"seq\":7}\n\n" +
		": keepalive\n\n"
	got, err := frames(t, in)
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	want := []Frame{
		{Comment: "connected head=7"},
		{IsEvent: true, ID: "6", Type: "head_moved", Data: `{"seq":6,"ref":"refs/heads/main"}`},
		{IsEvent: true, ID: "7", Type: "head_moved", Data: `{"seq":7}`},
		{Comment: "keepalive"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Multi-line data joins with newlines, CRLF endings work, a missing space
// after the colon is fine, a field with no colon has an empty value, the
// default type is "message", and unknown fields (retry) are skipped.
func TestReaderSpecCorners(t *testing.T) {
	in := "\xef\xbb\xbfdata: a\r\ndata:b\r\ndata\r\nretry: 3000\r\nbogus: x\r\n\r\n" +
		"event:x\ndata: only\n\n"
	got, _ := frames(t, in)
	if len(got) != 2 {
		t.Fatalf("frames: %+v", got)
	}
	if got[0].Data != "a\nb\n" || got[0].Type != "message" || !got[0].IsEvent {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Data != "only" || got[1].Type != "x" {
		t.Errorf("second: %+v", got[1])
	}
}

// The last event ID carries over to later events that set none, and an
// event without data dispatches nothing but still moves the ID.
func TestReaderLastEventID(t *testing.T) {
	got, _ := frames(t, "id: 5\ndata: a\n\nid: 6\n\ndata: b\n\n")
	if len(got) != 2 || got[0].ID != "5" || got[1].ID != "6" || got[1].Data != "b" {
		t.Fatalf("frames: %+v", got)
	}
}

// An event cut off by the connection closing is dropped, not half-delivered.
func TestReaderDropsATruncatedEvent(t *testing.T) {
	got, err := frames(t, "id: 1\ndata: ok\n\nid: 2\ndata: cut")
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("frames: %+v", got)
	}
	if err != io.EOF {
		t.Fatalf("err = %v", err)
	}
}

func TestReaderBoundsALine(t *testing.T) {
	_, err := frames(t, "data: "+strings.Repeat("x", maxLine+10)+"\n\n")
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReaderIgnoresAnIDWithNUL(t *testing.T) {
	got, _ := frames(t, "id: 1\x002\ndata: a\n\n")
	if len(got) != 1 || got[0].ID != "" {
		t.Fatalf("frames: %+v", got)
	}
}
