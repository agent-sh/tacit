package events

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/harnesses"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// Files finds every session the sources name: Claude Code's subagents, pi's
// tasks, and a compressed Codex rollout by its one path.
func TestFilesFindSubagentsTasksAndCompressedRollouts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "") // restored after the test
	os.Unsetenv("TACIT_HERMES_DB")  // unset: the store under home, when there is one
	project := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	top := write(t, filepath.Join(project, "s1.jsonl"), `{}`)
	sub := write(t, filepath.Join(project, "s1", "subagents", "agent-a1.jsonl"), `{}`)
	write(t, filepath.Join(project, "s1", "subagents", "agent-a1.meta.json"), `{}`)
	rollout := compress(t, write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-x.jsonl"), `{}`))
	archived := write(t, filepath.Join(home, ".codex", "archived_sessions", "rollout-y.jsonl"), `{}`)
	piDir := filepath.Join(home, ".pi", "agent", "sessions", "--home-u--")
	pi := write(t, filepath.Join(piDir, "t_p1.jsonl"), `{}`)
	task := write(t, filepath.Join(piDir, "t_p1", "tasks", "t_p2.jsonl"), `{}`)

	srcs := harnesses.Sources(home)
	found := map[string]bool{}
	for _, s := range srcs {
		files, err := Files(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			found[f] = true
			if (f == sub || f == task) != s.Subagent {
				t.Errorf("%s from a source marked subagent=%v", f, s.Subagent)
			}
		}
	}
	for _, f := range []string{top, sub, rollout, archived, pi, task} {
		if !found[f] {
			t.Errorf("not found: %s", f)
		}
	}
	if len(found) != 6 {
		t.Errorf("found %d files: %v", len(found), found)
	}
	for _, s := range srcs {
		if s.Format == digest.Hermes {
			t.Fatalf("a Hermes source with no store: %+v", s)
		}
	}

	// The Hermes store, when there is one, is the last source.
	db := filepath.Join(home, ".hermes", "state.db")
	os.MkdirAll(filepath.Dir(db), 0o700)
	hermestest.Create(t, db)
	srcs = harnesses.Sources(home)
	if last := srcs[len(srcs)-1]; last.Format != digest.Hermes || last.DB != db || last.Harness != "hermes" {
		t.Fatalf("hermes source: %+v", last)
	}
	if files, err := Files(srcs[len(srcs)-1]); err != nil || files != nil {
		t.Fatalf("files of the store: %v %v", files, err)
	}
}
