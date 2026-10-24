package digest

import (
	"encoding/json"
	"strings"
	"testing"
)

// Real-shaped Codex session_meta lines (fields as Codex 0.16x writes them,
// values synthetic).
func codexMeta(originator, source, threadSource, parent string) string {
	p := `{"timestamp":"2026-10-20T09:00:00.000Z","type":"session_meta","payload":{"session_id":"019a-s","id":"019a-t","timestamp":"2026-10-20T09:00:00.000Z","cwd":"/home/u/projects/shop","originator":"` + originator + `","cli_version":"0.162.1","source":` + source
	if threadSource != "" {
		p += `,"thread_source":"` + threadSource + `"`
	}
	if parent != "" {
		p += `,"parent_thread_id":"` + parent + `","agent_nickname":"Ada","agent_role":"worker","agent_path":"/root/ada","multi_agent_version":2`
	}
	return p + `,"model_provider":"p","base_instructions":{"text":"You are Codex."},"history_mode":"full"}}` + "\n"
}

func TestSessionWhoCodex(t *testing.T) {
	spawn := `{"subagent":{"thread_spawn":{"parent_thread_id":"019a-parent","depth":1,"agent_path":"/root/ada","agent_nickname":"Ada","agent_role":"worker"}}}`
	for _, c := range []struct {
		name string
		head string
		want Who
	}{
		{"terminal session", codexMeta("codex-tui", `"cli"`, "user", ""), Owner},
		{"desktop app, older version without thread_source", codexMeta("Codex Desktop", `"vscode"`, "", ""), Owner},
		// A subagent a parent thread spawned names the person's front end.
		{"subagent with codex-tui originator", codexMeta("codex-tui", spawn, "subagent", "019a-parent"), Agent},
		{"subagent marked only by its source", codexMeta("codex-tui", spawn, "", ""), Agent},
		{"subagent marked only by its parent", codexMeta("Codex Desktop", `"vscode"`, "", "019a-parent"), Agent},
		{"guardian review", codexMeta("Codex Desktop", `{"subagent":{"other":"guardian"}}`, "guardian_review", ""), Agent},
		{"codex exec", codexMeta("codex_exec", `"exec"`, "user", ""), Agent},
		{"exec through a person's front end", codexMeta("codex-tui", `"exec"`, "user", ""), Agent},
		{"an SDK client", codexMeta("codex_sdk_ts", `"vscode"`, "user", ""), Agent},
		{"no session_meta", `{"type":"response_item","payload":{}}` + "\n", Unknown},
	} {
		if got := SessionWho("/home/u/.codex/sessions/2026/10/20/rollout-x.jsonl", []byte(c.head), Codex); got.Who != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
	}
	// A session_meta line longer than the head is read field by field.
	cut := codexMeta("codex-tui", spawn, "subagent", "019a-parent")
	cut = strings.Replace(cut, `"model_provider"`, `"pad":"`+strings.Repeat("x", 300)+`","model_provider"`, 1)[:len(cut)/2+120]
	if got := SessionWho("r.jsonl", []byte(cut), Codex); got.Who != Agent {
		t.Errorf("cut meta line: %+v", got)
	}
}

func TestSessionWhoClaudeAndPi(t *testing.T) {
	cli := `{"type":"user","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s1","message":{"content":"x"}}` + "\n"
	lane := `{"type":"user","entrypoint":"sdk-cli","cwd":"/home/u/projects/shop","sessionId":"s2","message":{"content":"x"}}` + "\n"
	runner := `{"type":"user","entrypoint":"cli","cwd":"/home/u/projects/shop/.local/runners/w1/task-3","message":{"content":"x"}}` + "\n"
	for _, c := range []struct {
		name, path, head string
		f                Format
		want             Who
	}{
		{"terminal session", "/home/u/.claude/projects/-home-u/s1.jsonl", cli, Claude, Owner},
		{"claude -p lane", "/home/u/.claude/projects/-home-u/s2.jsonl", lane, Claude, Agent},
		{"subagent transcript", "/home/u/.claude/projects/-home-u/s1/subagents/agent-a1.jsonl", cli, Claude, Agent},
		{"runner worktree", "/home/u/.claude/projects/-home-u/s3.jsonl", runner, Claude, Agent},
		{"typed pi session", "/home/u/.pi/agent/sessions/--home-u--/t_p1.jsonl", piSession("2026-10-01T10:00:05.000Z"), Pi, Owner},
		{"headless pi session", "/home/u/.pi/agent/sessions/--home-u--/t_p2.jsonl", piSession("2026-10-01T10:00:00.250Z"), Pi, Agent},
		{"pi subagent by path", "/home/u/.pi/agent/sessions/--home-u--/t_p1/tasks/t_p3.jsonl", piSession("2026-10-01T10:00:05.000Z"), Pi, Agent},
		{"pi subagent by header", "/home/u/.pi/agent/sessions/--home-u--/t_p4.jsonl", strings.Replace(piSession("2026-10-01T10:00:05.000Z"), `"cwd":`, `"parentSession":"/home/u/.pi/agent/sessions/--home-u--/t_p1.jsonl","cwd":`, 1), Pi, Agent},
	} {
		if got := SessionWho(c.path, []byte(c.head), c.f); got.Who != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
	}
}

func TestClaudeTyped(t *testing.T) {
	for _, c := range []struct {
		name, line string
		want       bool
	}{
		{"typed", `{"type":"user","entrypoint":"cli","origin":{"kind":"human"},"promptSource":"typed","turnOrigin":"human"}`, true},
		{"queued while busy", `{"type":"user","entrypoint":"cli","origin":{"kind":"human"},"promptSource":"queued","turnOrigin":"human"}`, true},
		{"desktop app through its SDK", `{"type":"user","entrypoint":"claude-desktop","origin":{"kind":"human"},"promptSource":"sdk","turnOrigin":"human"}`, true},
		{"turnOrigin only", `{"type":"user","entrypoint":"claude-desktop","turnOrigin":"human"}`, true},
		{"older version, no marks", `{"type":"user","entrypoint":"cli"}`, true},
		{"task notification", `{"type":"user","entrypoint":"cli","origin":{"kind":"task-notification","producer":"bg"},"promptSource":"system","turnOrigin":"task_notification"}`, false},
		{"task notification without origin", `{"type":"user","entrypoint":"claude-desktop","turnOrigin":"task_notification"}`, false},
		{"scheduled task", `{"type":"user","entrypoint":"cli","promptSource":"system","turnOrigin":"scheduled","scheduledTaskId":"t1"}`, false},
		{"scheduled without the task id", `{"type":"user","entrypoint":"claude-desktop","turnOrigin":"scheduled"}`, false},
		{"teammate", `{"type":"user","entrypoint":"cli","origin":{"kind":"peer","from":"lane-2"},"promptSource":"system","turnOrigin":"peer"}`, false},
		{"plugin", `{"type":"user","entrypoint":"cli","origin":{"kind":"plugin","name":"x"},"promptSource":"system","turnOrigin":"system"}`, false},
		{"claude -p", `{"type":"user","entrypoint":"sdk-cli","promptSource":"sdk","turnOrigin":"sdk"}`, false},
		{"claude -p, older version", `{"type":"user","entrypoint":"sdk-cli"}`, false},
		{"accepted suggestion", `{"type":"user","entrypoint":"cli","origin":{"kind":"human"},"promptSource":"suggestion_accepted","turnOrigin":"human"}`, false},
		{"sidechain", `{"type":"user","entrypoint":"cli","isSidechain":true,"origin":{"kind":"human"}}`, false},
		{"meta", `{"type":"user","entrypoint":"cli","isMeta":true}`, false},
		{"no entrypoint, no marks", `{"type":"user"}`, false},
		{"repository origin", `{"type":"user","entrypoint":"cli","origin":{"host":"github.com"}}`, true},
	} {
		var o map[string]any
		if err := json.Unmarshal([]byte(c.line), &o); err != nil {
			t.Fatal(err)
		}
		if got, why := ClaudeTyped(ClaudeMarksOf(o)); got != c.want {
			t.Errorf("%s: %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}

// The dreamer's digest of each kind of session: the owner's typed turns are
// U:, every other user turn that is not harness text is another agent's
// prompt, P:.
func TestLinesLabelAgentsPrompts(t *testing.T) {
	assistant := `{"type":"assistant","entrypoint":"cli","message":{"content":[{"type":"text","text":"On it."}]}}`
	digestOf := func(path string, lines ...string) []string {
		in := strings.Join(lines, "\n") + "\n"
		return Lines(strings.NewReader(in), Claude, SessionWho(path, []byte(in), Claude).Owner())
	}
	// A `claude -p` lane: the prompt is the lane's driver's.
	got := digestOf("/h/.claude/projects/p/lane.jsonl",
		`{"type":"user","entrypoint":"sdk-cli","promptSource":"sdk","turnOrigin":"sdk","message":{"content":"Owner ruling: ship the dashboard before friday"}}`, assistant)
	if got[0] != "P: Owner ruling: ship the dashboard before friday" {
		t.Errorf("claude -p lane: %q", got)
	}
	// A resumed session of theirs: their turns, then a `claude -p --resume`
	// that an agent drove, then their own turn again.
	got = digestOf("/h/.claude/projects/p/own.jsonl",
		`{"type":"user","entrypoint":"cli","origin":{"kind":"human"},"promptSource":"typed","turnOrigin":"human","message":{"content":"never force push to main"}}`,
		assistant,
		`{"type":"user","entrypoint":"sdk-cli","promptSource":"sdk","turnOrigin":"sdk","message":{"content":"continue the release as the owner asked"}}`,
		`{"type":"user","entrypoint":"cli","isSidechain":true,"message":{"content":"subagent task: audit the payment module"}}`,
		`{"type":"user","entrypoint":"cli","promptSource":"system","turnOrigin":"scheduled","scheduledTaskId":"t1","message":{"content":"check the nightly build"}}`,
		`{"type":"user","entrypoint":"cli","origin":{"kind":"human"},"promptSource":"typed","message":{"content":"and keep the changelog short"}}`,
		assistant)
	want := []string{"U: never force push to main", "A: On it.", "P: continue the release as the owner asked", "P: subagent task: audit the payment module", "P: check the nightly build", "U: and keep the changelog short", "A: On it."}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("resumed session:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A Codex subagent that names the person's front end.
	spawn := `{"subagent":{"thread_spawn":{"parent_thread_id":"019a-parent","depth":1}}}`
	in := codexMeta("codex-tui", spawn, "subagent", "019a-parent") +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"},{"type":"input_text","text":"fix the flaky test the owner flagged"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Fixed."}]}}` + "\n"
	got = Lines(strings.NewReader(in), Codex, SessionWho("r.jsonl", []byte(in), Codex).Owner())
	want = []string{"H: <environment_context>cwd</environment_context>", "P: fix the flaky test the owner flagged", "A: Fixed."}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("codex subagent:\n%s", strings.Join(got, "\n"))
	}
	if !Interesting(got) {
		t.Error("an agent's session with a prompt and an answer is still read")
	}
}
