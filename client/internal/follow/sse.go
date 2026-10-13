// Package follow keeps the local clone current from the server's change
// feed (SSE on /events, see server TacitServer.Feed and SPEC.md "Change
// feed"). The feed carries only "the head moved, here is its seq"; the
// follower answers each burst with one fetch, one fast-forward and one
// reindex, remembers the last seq it synced, and publishes a heartbeat the
// SessionStart hook reads to skip its own network sync.
package follow

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// maxLine bounds one SSE line. A journal row is about 200 bytes; a line
// this long is a broken or hostile peer, and the follower reconnects.
const maxLine = 64 << 10

// ErrLineTooLong ends a stream that sends a line over maxLine.
var ErrLineTooLong = errors.New("sse: line too long")

// Frame is one unit read from the stream: either a comment (the server's
// ": connected head=N" and ": keepalive" lines) or a dispatched event.
type Frame struct {
	Comment string // text after ": " for a comment frame; "" for an event
	IsEvent bool
	ID      string // last event ID in effect when the event was dispatched
	Type    string // "message" unless an event: field set it
	Data    string // data: lines joined with "\n"
}

// Reader parses a text/event-stream (WHATWG HTML, section 9.2) one frame at
// a time. Lines end in "\n" or "\r\n"; a bare "\r" is not a line break
// (the server never sends one). The retry field is ignored: reconnect
// timing is the follower's own backoff.
type Reader struct {
	br     *bufio.Reader
	first  bool
	lastID string
	id     string
	typ    string
	data   []string
	has    bool
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 4096), first: true}
}

// Next returns the next comment or event. An event cut off by the end of
// the stream is dropped, as the spec requires; the error is the stream's.
func (r *Reader) Next() (Frame, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return Frame{}, err
		}
		if r.first {
			r.first = false
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(line) == 0 {
			// Blank line: dispatch what was collected. The last event ID is
			// updated even by a message without data.
			if r.id != "" {
				r.lastID = r.id
			}
			f := Frame{IsEvent: r.has, ID: r.lastID, Type: r.typ, Data: strings.Join(r.data, "\n")}
			r.id, r.typ, r.data, r.has = "", "", nil, false
			if f.IsEvent {
				if f.Type == "" {
					f.Type = "message"
				}
				return f, nil
			}
			continue
		}
		if line[0] == ':' {
			return Frame{Comment: strings.TrimPrefix(string(line[1:]), " ")}, nil
		}
		name, value := string(line), ""
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			name, value = string(line[:i]), strings.TrimPrefix(string(line[i+1:]), " ")
		}
		switch name {
		case "event":
			r.typ = value
		case "data":
			r.data = append(r.data, value)
			r.has = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				r.id = value
			}
		}
	}
}

// readLine returns one line without its terminator, bounded by maxLine.
func (r *Reader) readLine() ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > maxLine {
			return nil, ErrLineTooLong
		}
		switch {
		case err == nil:
			out = bytes.TrimSuffix(out, []byte("\n"))
			return bytes.TrimSuffix(out, []byte("\r")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
