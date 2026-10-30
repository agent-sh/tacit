package events

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// claudeInterrupt is the user line Claude Code writes when the person
// interrupts: no mark of who typed it (2.1.28x to 2.1.31x put none on it), in
// a session of its own or, with an agent id, in a subagent's transcript.
func claudeInterrupt(cwd, agent, text string) string {
	side := ""
	if agent != "" {
		side = `"agentId":"` + agent + `",`
	}
	return fmt.Sprintf(`{"parentUuid":"a1","isSidechain":%t,%s"promptId":"p1","type":"user","message":{"role":"user","content":[{"type":"text","text":%q}]},"uuid":"u9","timestamp":"2026-10-20T11:05:00.000Z","userType":"external","entrypoint":"cli","cwd":%q,"sessionId":%q,"version":"2.1.312","gitBranch":"main"}`,
		agent != "", side, text, cwd, claudeSession)
}

// Claude Code's interruption mark is a user line it writes into the
// transcript the person interrupted: their session, a subagent's transcript
// when the subagent was running, a session in an agent's working directory.
// A Codex rollout that holds a Claude Code conversation keeps it as a user
// message: an import into their own session, or one a program wrote
// (originator codex_cli). The mark is the harness's text in every one of
// them; the session's prompt before it keeps its own label.
func TestInterruptionMarkIsHarnessTextInEverySession(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	day := filepath.Join(home, ".codex", "sessions", "2026", "10", "21")
	const mark, forTool = "[Request interrupted by user]", "[Request interrupted by user for tool use]"
	shop, lane := "/home/u/projects/shop", "/home/u/.cache/lanes/shop"
	spawn := `{"subagent":{"thread_spawn":{"parent_thread_id":"019d-own","depth":1,"agent_path":"/root/ada","agent_nickname":"Ada","agent_role":"worker"}}}`
	for _, c := range []struct {
		name string
		path string
		f    digest.Format
		// lines end with the session's prompt and then the mark's line.
		lines []string
		mark  string
		// kind and who are the label of the prompt.
		kind Kind
		who  digest.Who
	}{
		{"claude session of theirs", filepath.Join(project, claudeSession+".jsonl"), digest.Claude, []string{
			cl(0, `"type":"user","uuid":"u0","promptId":"p0",`+typedMarks+`,"message":{"role":"user","content":"rebuild the cache index"}`),
			claudeInterrupt(shop, "", forTool),
		}, forTool, KindOwnerText, digest.Owner},
		{"claude subagent", filepath.Join(project, claudeSession, "subagents", "agent-a7b8c9.jsonl"), digest.Claude, []string{
			fmt.Sprintf(`{"parentUuid":null,"isSidechain":true,"agentId":"a7b8c9","type":"user","message":{"role":"user","content":"Find where the cache index is built."},"uuid":"s0","timestamp":"2026-10-20T11:00:00.000Z","userType":"external","entrypoint":"cli","cwd":%q,"sessionId":%q,"version":"2.1.312"}`, shop, claudeSession),
			claudeInterrupt(shop, "a7b8c9", mark),
		}, mark, KindAgentText, digest.Agent},
		{"claude session in an agent's directory", filepath.Join(home, ".claude", "projects", "-home-u--cache-lanes-shop", "l1.jsonl"), digest.Claude, []string{
			fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"Rebuild the cache index."},"uuid":"l0","timestamp":"2026-10-20T11:00:00.000Z","userType":"external","entrypoint":"cli","cwd":%q,"sessionId":"l1","version":"2.1.312"}`, lane),
			claudeInterrupt(lane, "", forTool),
		}, forTool, KindAgentText, digest.Agent},
		{"codex session of theirs", filepath.Join(day, "rollout-2026-10-21T09-00-00-019d-own.jsonl"), digest.Codex, []string{
			codexMeta("019d-own", `"source":"cli","thread_source":"user",`),
			cxUser(1, "rebuild the cache index"),
			cxUser(2, forTool),
		}, forTool, KindOwnerText, digest.Owner},
		{"codex conversation a program wrote", filepath.Join(day, "rollout-2026-10-21T09-02-00-019d-imp.jsonl"), digest.Codex, []string{
			cx(0, "session_meta", `{"session_id":"019d-imp","id":"019d-imp","timestamp":"2026-10-21T09:00:00.000Z","cwd":"/home/u/projects/shop","originator":"codex_cli","cli_version":"0.155.1","source":"cli","thread_source":"user","model_provider":"p","history_mode":"full"}`),
			cxUser(1, "Rebuild the cache index."),
			cxUser(2, mark),
		}, mark, KindAgentText, digest.Agent},
		{"codex subagent thread", filepath.Join(day, "rollout-2026-10-21T09-04-00-019d-sub.jsonl"), digest.Codex, []string{
			codexMeta("019d-sub", `"source":`+spawn+`,"thread_source":"subagent","parent_thread_id":"019d-own",`),
			cxUser(1, "Find where the cache index is built."),
			cxUser(2, mark),
		}, mark, KindAgentText, digest.Agent},
	} {
		path := write(t, c.path, c.lines...)
		evs, _ := readAll(t, path, c.f, 0)
		got := byLine(t, c.lines, evs)
		if p := got[len(c.lines)-2]; len(p) != 1 || p[0].Kind != c.kind || p[0].Who != c.who {
			t.Errorf("%s: the prompt, want %s by %s:\n%s", c.name, c.kind, c.who, dump(p))
		}
		if m := got[len(c.lines)-1]; len(m) != 1 || m[0].Kind != KindHarnessText || m[0].Who != digest.Harness || m[0].Text != c.mark {
			t.Errorf("%s: the interruption mark, want %s by %s:\n%s", c.name, KindHarnessText, digest.Harness, dump(m))
		}
	}
}
