package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Headless sessions (SPEC.md, "Headless sessions"): a run no person types
// into gets no unsolicited delivery. The start, prompt and compaction blocks
// print nothing there, the PreToolUse hook denies nothing, and the Stop hook
// asks for no mark; tacit's MCP tools stay, so an agent there still pulls
// what it needs. Measured on a real machine: most of the prompt hook's lines
// went to such runs (SDK and `-p` runs, exec runs, subagents, throwaway
// harness instances of test suites), where nobody reads them and nothing
// marks them.

// headlessScan is how much of a transcript's head the kind is read from:
// the first entries carry the entrypoint, the session's metadata and the
// first prompt.
const headlessScan = 64 << 10

// keelSubagentBrief is how a subagent's first prompt starts in Keel
// (agent-sh/keel), whose transcripts follow Claude Code's format without
// the sidechain flag.
const keelSubagentBrief = "You are a subagent of a Keel session"

// sessionKind is why a session is headless ("" when a person works in it):
//
//   - env: TACIT_HEADLESS=1 in the hook's environment (a cron job, a batch
//     script, a test harness that says so); TACIT_HEADLESS=0 makes any run
//     a person's (an A/B's `claude -p` arms, which measure the blocks);
//   - subagent: the harness's agent_id (Claude Code and Codex send it from
//     inside a subagent), the transcript's first entry on a sidechain, or a
//     Keel subagent's brief as the first prompt;
//   - automation: Hermes's agent_context other than primary (cron, subagent);
//   - sdk: a Claude Code transcript whose entrypoint is an SDK one (sdk-cli
//     is `claude -p`, sdk-ts and sdk-py the Agent SDK), or, with no
//     transcript to read, CLAUDE_CODE_ENTRYPOINT saying so;
//   - exec: a Codex transcript of `codex exec`;
//   - scratch: a transcript under the user's cache directory, where only
//     throwaway harness instances (test suites, e2e runs) keep their state.
//
// The answer is kept next to the session's delivered set, so the transcript
// is read once a session.
func sessionKind(in hookInput) string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("TACIT_HEADLESS"))); v {
	case "1", "true", "yes":
		return "env"
	case "0", "false", "no":
		return "" // a harness that runs headless on purpose and wants the blocks (an A/B's arms)
	}
	if in.AgentID != "" {
		return "subagent"
	}
	if in.AgentContext != "" && in.AgentContext != "primary" {
		return "automation"
	}
	cache := ""
	if p := sessionFile(in.SessionID); p != "" {
		cache = p + ".kind"
		if b, err := os.ReadFile(cache); err == nil {
			k := strings.TrimSpace(string(b))
			if k == "interactive" {
				return ""
			}
			return k
		}
	}
	k, final := transcriptKind(in.TranscriptPath)
	if final && cache != "" {
		v := k
		if v == "" {
			v = "interactive"
		}
		if os.MkdirAll(filepath.Dir(cache), 0o700) == nil {
			_ = os.WriteFile(cache, []byte(v+"\n"), 0o600)
		}
	}
	return k
}

// transcriptKind reads the kind from the transcript's head. final is false
// when the head holds no first prompt yet (a session's first hook), so the
// answer is not kept.
func transcriptKind(path string) (kind string, final bool) {
	if path != "" {
		if dir, err := os.UserCacheDir(); err == nil && within(path, dir) {
			return "scratch", true
		}
	}
	head := readHead(path)
	if head == nil {
		if ep := os.Getenv("CLAUDE_CODE_ENTRYPOINT"); strings.HasPrefix(ep, "sdk") {
			return "sdk", false
		}
		return "", false
	}
	first := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		first = head[:i]
	}
	switch {
	case bytes.Contains(first, []byte(`"isSidechain":true`)):
		return "subagent", true
	case bytes.Contains(head, []byte(`"originator":"codex_exec"`)) || bytes.Contains(first, []byte(`"source":"exec"`)):
		return "exec", true
	case bytes.Contains(head, []byte(`"entrypoint":"sdk`)):
		return "sdk", true
	case bytes.Contains(head, []byte(`"content":"`+keelSubagentBrief)) || bytes.Contains(head, []byte(`"text":"`+keelSubagentBrief)):
		return "subagent", true
	}
	// A head with a person's prompt in it decides; one without may still
	// get its first prompt.
	return "", bytes.Contains(head, []byte(`"type":"user"`)) || bytes.Contains(head, []byte(`"role":"user"`))
}

// readHead is the first headlessScan bytes of a regular file, or nil.
func readHead(path string) []byte {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, headlessScan))
	if len(b) == 0 {
		return nil
	}
	return b
}

// within: p is dir or under it.
func within(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
