package harnesses

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The dreamer, the corpus and the reviewer read the top-level sources: the
// same globs as before the one list, in the same order.
func TestTopLevelSourcesAreTheDreamersGlobs(t *testing.T) {
	home := "/home/u"
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "/srv/state.db")
	var got []string
	for _, s := range TopLevel(Sources(home)) {
		got = append(got, s.Harness+" "+s.Glob+s.DB)
	}
	want := []string{
		"claude " + filepath.Join(home, ".claude", "projects", "*", "*.jsonl"),
		"codex " + filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"),
		"codex " + filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"),
		"codex " + filepath.Join(home, ".codex", "archived_sessions", "*.jsonl"),
		"pi " + filepath.Join(home, ".pi", "agent", "sessions", "*", "*.jsonl"),
		"hermes /srv/state.db",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level sources:\n%q\nwant\n%q", got, want)
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "/srv/pi")
	for _, s := range Sources(home) {
		if s.Harness == "pi" && s.Subagent && s.Glob != filepath.Join("/srv/pi", "*", "tasks", "*.jsonl") {
			t.Errorf("pi tasks under the session dir: %s", s.Glob)
		}
	}
}
