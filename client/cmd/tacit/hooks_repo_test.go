package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestActivityRepoFromPromptAndTranscript(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	widget := filepath.Join(home, "workplace", "widget")
	api := filepath.Join(home, "workplace", "api")
	for _, d := range []string{widget, api} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(home, ".claude", "projects", "-h", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(cmds ...string) {
		var b strings.Builder
		for _, c := range cmds {
			b.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + c + `"}}]}}` + "\n")
		}
		if err := os.WriteFile(transcript, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	repo := func(prompt string) string {
		return activityRepo(ctx, hookInput{Cwd: home, Prompt: prompt, TranscriptPath: transcript}, home)
	}

	write("cd "+widget+" && go test ./...", "git -C "+widget+" status", "ls "+widget)
	if got := repo("fix the failing test"); got != "widget" {
		t.Errorf("recent tool calls in widget: %q", got)
	}
	// One typed path alone does not move the session ...
	write("ls " + filepath.Join(home, "Downloads"))
	if got := repo("compare with " + api + "/server.go"); got != "" {
		t.Errorf("one typed path alone decided: %q", got)
	}
	// ... a typed path with a recent tool call there does.
	write("cat " + api + "/go.mod")
	if got := repo("look at " + api + "/server.go"); got != "api" {
		t.Errorf("typed path plus a tool call in api: %q", got)
	}
	// Nothing points at a repo: no guess.
	write("ls " + filepath.Join(home, "Downloads"))
	if got := repo("what time is it"); got != "" {
		t.Errorf("no repo activity: %q", got)
	}
}

func TestReadTailRefusesWhatIsNotTheTranscript(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	outside := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readTail(outside, home); got != "" {
		t.Errorf("a transcript outside home was read")
	}
	link := filepath.Join(home, "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if got := readTail(link, home); got != "" {
		t.Errorf("a symlink out of home was read")
	}
	fifo := filepath.Join(home, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no mkfifo here")
	}
	if got := readTail(fifo, home); got != "" { // must return, not block
		t.Errorf("a FIFO was read")
	}
}

// A session started in a folder that is no repo gets the facts of the repo
// it works in: the paths in its prompt name the checkout.
func TestHookFromHomeDeliversTheRepoTheSessionWorksIn(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}, filler: 400, rules: 5})
	if _, err := r.step(ctxMinute(t)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", r.root)
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	demo := filepath.Join(r.projects, "demo")
	ask := func(session, prompt string) string {
		return hookOut(t, `{"session_id":"`+session+`","cwd":"`+r.root+`","hook_event_name":"UserPromptSubmit","prompt":"`+prompt+`"}`, "prompt")
	}
	// The derived fact (hot spots) joins only for the session's own repo: no
	// repo, no derived line; the paths into demo make demo the repo.
	without := ask("p1", "quokka scheduler zebra rule: also check src/scheduler/worker.go and pool.go")
	if strings.Contains(without, "demo-hot-spots") {
		t.Fatalf("a home session with no path into demo got demo's derived fact:\n%s", without)
	}
	with := ask("p2", "quokka scheduler zebra rule: also check "+demo+"/src/scheduler/worker.go and "+demo+"/src/scheduler/pool.go")
	if !strings.Contains(with, "demo-hot-spots") {
		t.Fatalf("the repo the prompt's paths are in was not used:\n%s", with)
	}
}
