package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// --agent-windows none: the agent's window (a prompt no person typed) is
// passed over, its cursor moves past it, and the meter and the summary count
// it; the owner's window is dreamed. The flag wins over
// TACIT_DREAM_AGENT_WINDOWS, which the settings set; a bad value stops the
// run before any model call.
func TestDreamAgentWindowsNone(t *testing.T) {
	home, agentT, statePath := dreamHome(t)
	ownerT := filepath.Join(home, ".claude/projects/demo/s2.jsonl")
	os.WriteFile(ownerT, []byte(`{"type":"user","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s2","origin":{"kind":"human"},"promptSource":"typed","turnOrigin":"human","timestamp":"2026-10-24T09:00:00.000Z","message":{"content":"never push to main in this repo"}}`+"\n"+
		`{"type":"assistant","entrypoint":"cli","sessionId":"s2","timestamp":"2026-10-24T09:00:05.000Z","message":{"content":[{"type":"text","text":"Understood, PRs only."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
	st, _ := dream.LoadState(statePath)
	st.Offsets[ownerT] = 0
	dream.SaveState(statePath, st)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	var prompts []string
	dreamRun = func(_ context.Context, _ string, prompt string) (string, float64, int, error) {
		prompts = append(prompts, prompt)
		return `[]`, .01, 10, nil
	}
	t.Setenv("TACIT_DREAM_AGENT_WINDOWS", "bogus")
	if code := runDream(nil); code != 2 || len(prompts) != 0 {
		t.Fatalf("a bad policy: exit %d, %d call(s)", code, len(prompts))
	}
	t.Setenv("TACIT_DREAM_AGENT_WINDOWS", "all")
	if code := runDream([]string{"--agent-windows", "none"}); code != 0 || len(prompts) != 1 || !strings.Contains(prompts[0], "U: never push to main") {
		t.Fatalf("none: exit %d, %d call(s)", code, len(prompts))
	}
	st, _ = dream.LoadState(statePath)
	agentEnd, _ := os.Stat(agentT)
	if st.Offsets[agentT] != agentEnd.Size() {
		t.Fatalf("the passed-over transcript's cursor is %d, not its end %d", st.Offsets[agentT], agentEnd.Size())
	}
	var meter dream.Meter
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(home), "dreamer.log"))
	if json.Unmarshal(b, &meter) != nil || meter.AgentWindows != "none" || meter.AgentPassed != 1 || meter.Windows != 1 || meter.Calls != 1 {
		t.Fatalf("meter %+v", meter)
	}
	if s := agentSummary(meter); !strings.Contains(s, "agent windows none: 1 passed over") {
		t.Fatalf("summary %q", s)
	}
	// A dry run says what it would send, before any cap.
	st, _ = dream.LoadState(statePath)
	st.Offsets[agentT], st.Offsets[ownerT] = 0, 0
	dream.SaveState(statePath, st)
	out := captureStdout(t, func() { runDream([]string{"--dry-run", "--max-calls", "0", "--agent-windows", "none"}) })
	if !strings.Contains(out, "dream: dry run: 1 window(s) to dream, ") || !strings.Contains(out, "agent windows none: 1 passed over") {
		t.Fatalf("dry run:\n%s", out)
	}
	if agentSummary(dream.Meter{}) != "" {
		t.Fatal("the default printed a policy line")
	}
}
