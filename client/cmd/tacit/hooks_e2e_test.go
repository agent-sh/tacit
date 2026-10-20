package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookOut runs `tacit hook <args>` the way a harness does: JSON on stdin,
// the block it prints on stdout.
func hookOut(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	in.WriteString(stdin)
	in.Seek(0, 0)
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, wr
	code := runHook(args)
	wr.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	in.Close()
	if code != 0 {
		t.Fatalf("hook exit %d", code)
	}
	b, _ := io.ReadAll(rd)
	return string(b)
}

// The hooks end to end on a store the curator really produced: the facts
// come from the derive step itself, the clone has a real remote, and the
// hook is run through runHook (checkout directory, session file, sync and
// all).
func TestHooksEndToEndOnGeneratedFacts(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}, filler: 400, rules: 5})
	if _, err := r.step(ctxMinute(t)); err != nil {
		t.Fatal(err)
	}
	if r.serverDerived() == "" {
		t.Fatal("no derived facts were pushed")
	}
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	cwd := filepath.Join(r.projects, "demo")
	payload := func(session, event, extra string) string {
		return `{"session_id":"` + session + `","cwd":"` + cwd + `","hook_event_name":"` + event + `"` + extra + `}`
	}

	// SessionStart: the owner's ruling is there, derived facts are not
	start := hookOut(t, payload("start1", "SessionStart", `,"source":"startup"`), "session-start")
	if !strings.Contains(start, "RULE demo-quokka-scheduler-0") {
		t.Fatalf("SessionStart lacks the ruling:\n%s", start)
	}
	if strings.Contains(start, "demo-hot-spots") || strings.Contains(start, "demo-module-map") || strings.Contains(start, "computed from") {
		t.Fatalf("SessionStart carries a derived fact:\n%s", start)
	}
	if n := strings.Count(start, "\n"); n > startLimit+1 {
		t.Fatalf("%d lines, cap %d plus the title:\n%s", n, startLimit, start)
	}

	// A prompt that matches rulings and names a hot file: the curated lines up
	// to the ceiling (the five rules score alike, so all pass the relative
	// threshold), then the one derived line
	p := "quokka scheduler zebra rule: also check src/scheduler/worker.go and pool.go"
	got := hookOut(t, payload("prompt1", "UserPromptSubmit", `,"prompt":"`+p+`"`), "prompt")
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 1+promptCeiling+1 {
		t.Fatalf("expected a title, %d curated lines and 1 derived line:\n%s", promptCeiling, got)
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "- demo-hot-spots (computed from") {
		t.Fatalf("the derived line is not last:\n%s", got)
	}
	for _, l := range lines[1 : 1+promptCeiling] {
		if strings.Contains(l, "computed from") || !strings.Contains(l, "quokka") {
			t.Fatalf("a curated slot was taken or reordered: %q\n%s", l, got)
		}
	}
	// once per session
	if again := hookOut(t, payload("prompt1", "UserPromptSubmit", `,"prompt":"`+p+`"`), "prompt"); strings.TrimSpace(again) != "" {
		t.Fatalf("a repeat delivered facts again:\n%s", again)
	}

	// A prompt that only names the hot file: just the derived line
	got = hookOut(t, `{"session_id":"def456","cwd":"`+cwd+`","prompt":"why does src/scheduler/worker.go change in so many commits"}`, "prompt")
	if !strings.Contains(got, "demo-hot-spots (computed from") || strings.Contains(got, "quokka") {
		t.Fatalf("hot-file prompt:\n%s", got)
	}

	// A prompt about nothing in them: no derived line
	got = hookOut(t, `{"session_id":"ghi789","cwd":"`+cwd+`","prompt":"please summarize the recent changes carefully"}`, "prompt")
	if strings.Contains(got, "computed from") {
		t.Fatalf("an unrelated prompt pulled a derived fact:\n%s", got)
	}
}
