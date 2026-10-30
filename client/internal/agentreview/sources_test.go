package agentreview

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The reviewer reads the top-level sources of the one list (harnesses.Sources),
// the same globs and store as before it.
func TestDefaultSourcesAreTheTopLevelSources(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "/srv/state.db")
	home := "/home/u"
	want := Sources{
		Claude:   []string{filepath.Join(home, ".claude", "projects", "*", "*.jsonl")},
		Codex:    []string{filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"), filepath.Join(home, ".codex", "archived_sessions", "*.jsonl")},
		Pi:       []string{filepath.Join(home, ".pi", "agent", "sessions", "*", "*.jsonl")},
		HermesDB: "/srv/state.db",
	}
	if got := DefaultSources(home); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSources:\n%+v\nwant\n%+v", got, want)
	}
}
