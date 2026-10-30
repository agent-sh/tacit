// Package harnesses says where each harness keeps its sessions on this
// machine: Claude Code's transcripts and its subagents', Codex's sessions and
// the ones it archived, pi's sessions and their tasks, and the Hermes store.
// It is the one list: the dreamer, the owner corpus and the reviewer read its
// top-level sources (TopLevel), the session events all of them
// (internal/events).
package harnesses

import (
	"path/filepath"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
)

// Source is one place a harness keeps sessions: a glob of plain transcripts
// (transcript.Glob finds their compressed copies with them), or the Hermes
// store.
type Source struct {
	// Harness is the harness's name: claude, codex, pi or hermes.
	Harness string
	Format  digest.Format
	// Glob matches the plain transcripts, ending in *.jsonl; empty for the
	// Hermes store.
	Glob string
	// DB is the Hermes store (Format digest.Hermes), read in place of Glob.
	DB string
	// Subagent marks a glob of subagents' transcripts, one level below a
	// session: Claude Code's <session>/subagents/, pi's <session>/tasks/.
	// Who wrote their turns is decided as for any session
	// (digest.SessionWho): an agent.
	Subagent bool
}

// Sources are this machine's session sources under home, in this order:
// Claude Code's top-level transcripts and its subagents'; Codex's sessions by
// day, the ones directly under a sessions folder (an import), and the ones it
// archived; pi's sessions (digest.PiSessionGlob, pi's own variables) and their
// tasks; and the Hermes store when there is one (hermes.DefaultDB, which
// $TACIT_HERMES_DB names or turns off).
func Sources(home string) []Source {
	pi := digest.PiSessionGlob(home)
	srcs := []Source{
		{Harness: "claude", Format: digest.Claude, Glob: filepath.Join(home, ".claude", "projects", "*", "*.jsonl")},
		{Harness: "claude", Format: digest.Claude, Glob: filepath.Join(home, ".claude", "projects", "*", "*", "subagents", "*.jsonl"), Subagent: true},
		{Harness: "codex", Format: digest.Codex, Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl")},
		{Harness: "codex", Format: digest.Codex, Glob: filepath.Join(home, ".codex", "sessions", "*", "*.jsonl")},
		{Harness: "codex", Format: digest.Codex, Glob: filepath.Join(home, ".codex", "archived_sessions", "*.jsonl")},
		{Harness: "pi", Format: digest.Pi, Glob: pi},
		// A task's session is in a folder named after its parent's file, next
		// to it: <sessions>/<cwd>/<session>/tasks/, or <dir>/<session>/tasks/
		// under $PI_CODING_AGENT_SESSION_DIR.
		{Harness: "pi", Format: digest.Pi, Glob: filepath.Join(filepath.Dir(pi), "*", "tasks", "*.jsonl"), Subagent: true},
	}
	if db := hermes.DefaultDB(home); db != "" {
		srcs = append(srcs, Source{Harness: "hermes", Format: digest.Hermes, DB: db})
	}
	return srcs
}

// TopLevel is srcs less the globs of subagents' transcripts, in order: the
// sources the dreamer, the owner corpus and the reviewer read.
func TopLevel(srcs []Source) []Source {
	var out []Source
	for _, s := range srcs {
		if !s.Subagent {
			out = append(out, s)
		}
	}
	return out
}
