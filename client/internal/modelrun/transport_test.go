package modelrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// What Codex prints when its stream to the provider fails at the transport
// level: a notice per retry and, once the retries are spent, the failed turn.
const (
	reconnectNotice = `{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion: Transport error: timeout)"}`
	droppedTurn     = `{"type":"turn.failed","error":{"message":"stream disconnected before completion: Transport error: timeout"}}`
)

// A Codex error event is a notice: Codex prints a stream retry there and goes
// on, so the turn's own end decides. A turn that recovered is a result, and a
// notice after its end changes nothing; a failed turn fails with its own
// error, not the first notice; a turn that never ended fails with the last
// notice. Only a transport error behind "stream disconnected before
// completion:" is a dropped stream. Before, the first notice failed the call
// and threw away the answer that followed it.
func TestCodexRetryNoticeIsNotTheTurnsFailure(t *testing.T) {
	recovered := reconnectNotice + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":12}}`
	for name, stream := range map[string]string{"recovered": recovered, "a notice after the turn's end": recovered + "\n" + reconnectNotice} {
		if result, err := parse("codex", []byte(stream)); err != nil || result.Text != "[]" || result.InputTokens != 12 {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
	}
	for name, stream := range map[string]string{"failed turn": reconnectNotice + "\n" + droppedTurn, "no end": reconnectNotice} {
		if _, err := parse("codex", []byte(stream)); !IsTransport(err) || CanFallback(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, stream := range map[string]string{
		"failed for capacity after a retry notice": reconnectNotice + "\n" + `{"type":"turn.failed","error":{"message":"{\"status\":503,\"error\":{\"message\":\"try again\"}}"}}`,
		"no end, the last notice for capacity":     reconnectNotice + "\n" + `{"type":"error","message":"Reconnecting... 2/5 (unexpected status 503 Service Unavailable: try again)"}`,
	} {
		if _, err := parse("codex", []byte(stream)); IsTransport(err) || FailureKind(err) != "capacity" || !CanFallback(err) {
			t.Fatalf("%s: kind %q: %v", name, FailureKind(err), err)
		}
	}
	// Codex puts other errors behind the same "stream disconnected before
	// completion:"; a filtered response fails that way every time.
	filtered := `{"type":"turn.failed","error":{"message":"stream disconnected before completion: Incomplete response returned, reason: content_filter"}}`
	if _, err := parse("codex", []byte(filtered)); err == nil || IsTransport(err) {
		t.Fatalf("a filtered response read as a dropped stream: %v", err)
	}
}

// A dropped stream keeps its kind through the runner's error, and is no
// reason to fall back.
func TestCodexStreamDropIsTransport(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_VERIFY_RUNNER", "codex")
	t.Setenv("TACIT_CODEX_COMMAND", exe)
	t.Setenv("TACIT_TEST_RUNNER", "codex")
	t.Setenv("TACIT_TEST_CAPTURE", filepath.Join(t.TempDir(), "capture"))
	t.Setenv("TACIT_TEST_RESPONSE", reconnectNotice+"\n"+droppedTurn)
	t.Setenv("TACIT_TEST_EXIT", "1")
	_, err = Run(context.Background(), "verify", "", "synthetic prompt", "synthetic-session")
	if !IsTransport(err) || CanFallback(err) || FailureKind(err) != "transport" {
		t.Fatalf("kind %q, fallback %v: %v", FailureKind(err), CanFallback(err), err)
	}
}
