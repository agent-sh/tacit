package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivityRepoFromPromptAndTranscript(t *testing.T) {
	home := t.TempDir()
	widget := filepath.Join(home, "workplace", "widget")
	api := filepath.Join(home, "workplace", "api")
	for _, d := range []string{widget, api} {
		os.MkdirAll(filepath.Join(d, ".git"), 0o755)
	}
	transcript := filepath.Join(home, ".claude", "projects", "-h", "s.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o755)
	write := func(cmds ...string) {
		var b strings.Builder
		for _, c := range cmds {
			b.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + c + `"}}]}}` + "\n")
		}
		os.WriteFile(transcript, []byte(b.String()), 0o600)
	}

	write("cd "+widget+" && go test ./...", "git -C "+widget+" status")
	if got := activityRepo(hookInput{Cwd: home, Prompt: "fix the failing test", TranscriptPath: transcript}, home); got != "widget" {
		t.Errorf("recent tool calls in widget: %q", got)
	}
	// A path the owner typed outweighs one earlier tool call elsewhere.
	write("ls " + widget)
	if got := activityRepo(hookInput{Cwd: home, Prompt: "look at " + api + "/server.go", TranscriptPath: transcript}, home); got != "api" {
		t.Errorf("prompt path in api: %q", got)
	}
	// Nothing points at a repo: no guess.
	write("ls " + filepath.Join(home, "Downloads"))
	if got := activityRepo(hookInput{Cwd: home, Prompt: "what time is it", TranscriptPath: transcript}, home); got != "" {
		t.Errorf("no repo activity: %q", got)
	}
	// A transcript outside home is not read.
	outside := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(outside, []byte(`"cd `+widget+`" "cd `+widget+`"`), 0o600)
	if got := activityRepo(hookInput{Cwd: home, TranscriptPath: outside}, home); got != "" {
		t.Errorf("transcript outside home was read: %q", got)
	}
}
