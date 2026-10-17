package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
)

func TestWindowsCursorAndFreshStart(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s1.jsonl")
	line := func(s string) string { return s + "\n" }
	initial := line(`{"type":"user","cwd":"/home/u/projects/api","message":{"content":"old turn that must never be replayed"}}`)
	os.WriteFile(f, []byte(initial), 0o644)
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}

	st := &State{Offsets: map[string]int64{}}
	ws, _ := Windows(src, st, true)
	if len(ws) != 0 || st.Offsets[f] != int64(len(initial)) {
		t.Fatalf("fresh start must only record cursors: %v %v", ws, st.Offsets)
	}

	// Append a delta with an owner line, an error and assistant text; plus a
	// trailing partial line that must not be consumed.
	delta := line(`{"type":"user","cwd":"/home/u/projects/api","message":{"content":"commit it"}}`) +
		line(`{"type":"assistant","message":{"content":[{"type":"text","text":"Committing."},{"type":"tool_use","name":"Bash","input":{"command":"git commit"}}]}}`) +
		line(`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"tools/hooks/pre-push: eslint: not found"}]}}`)
	delta += strings.Repeat(" ", minNewBytes) + "\n" // padding so the delta clears the minimum
	partial := `{"type":"user","message":{"content":"unfinished`
	os.WriteFile(f, []byte(initial+delta+partial), 0o644)
	ws, _ = Windows(src, st, false)
	if len(ws) != 1 {
		t.Fatalf("expected one window, got %d", len(ws))
	}
	w := ws[0]
	if w.Repo != "api" {
		t.Fatalf("repo %q", w.Repo)
	}
	joined := strings.Join(w.Lines, "\n")
	if !strings.Contains(joined, "U: commit it") || !strings.Contains(joined, "E: tools/hooks/pre-push: eslint: not found") || strings.Contains(joined, "old turn") || strings.Contains(joined, "unfinished") {
		t.Fatalf("window lines:\n%s", joined)
	}
	if w.End != int64(len(initial)+len(delta)) {
		t.Fatalf("window end should stop before the partial line: %d", w.End)
	}
	if st.Offsets[f] != int64(len(initial)) {
		t.Fatalf("Windows must not advance the cursor for a window with content: %d", st.Offsets[f])
	}
	// The caller advances the cursor after a successful call; then nothing new.
	st.Offsets[f] = w.End
	ws, _ = Windows(src, st, false)
	if len(ws) != 0 {
		t.Fatal("no new bytes should mean no window")
	}
	// A truncated file restarts from its end.
	os.WriteFile(f, []byte("{}\n"), 0o644)
	Windows(src, st, false)
	if st.Offsets[f] != 3 {
		t.Fatalf("shrunk file cursor %d", st.Offsets[f])
	}
}

func TestParseAndToFact(t *testing.T) {
	xs := Parse("```json\n[{\"description\":\"api's tools/hooks/pre-push fails with eslint: not found; prefix PATH=$HOME/.local/node/bin\",\"type\":\"machine_fact\",\"kind\":\"observed\",\"applies_to\":{\"commands\":[\"git push\"],\"repos\":[\"api\"]}},{\"description\":\"\"}]\n```")
	if len(xs) != 1 {
		t.Fatalf("parsed %d", len(xs))
	}
	f := ToFact(xs[0], "api", "host-a", "sess-1")
	if f.Meta.Scope != "machines/host-a" || f.Meta.Type != "machine_fact" || f.Meta.Source.Harness != "dreamer" || f.Meta.Source.Session != "sess-1" || !fact.IsSlug(f.Meta.Name) {
		t.Fatalf("%+v", f.Meta)
	}
	if _, err := fact.Parse("inbox/dreamer@host-a/"+f.Meta.Name+".md", must(f.Encode())); err != nil {
		t.Fatal(err)
	}
	g := ToFact(Extracted{Description: "The owner wants releases batched", Type: "user_standard", Kind: "user"}, "webapp", "host-a", "")
	if g.Meta.Scope != "projects/webapp" || g.Meta.Source.Kind != "user" {
		t.Fatalf("%+v", g.Meta)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestRepoOfCwd(t *testing.T) {
	repos := map[string]bool{"/h/projects/tacit": true, "/h/projects/agent-sh/agnix": true, "/h/projects/atlas": true, "/h/projects/atlas-worktrees/KV-1": true}
	isRepo := func(d string) bool { return repos[d] }
	for cwd, want := range map[string]string{
		"/h/projects/tacit":                                              "tacit",
		"/h/projects/tacit/client":                                       "tacit",
		"/h/projects/tacit/.claude/worktrees/agent-a8d2":                 "tacit",
		"/h/projects/atlas/.local/runners/worker-05/worktrees/task-7-g1": "atlas",
		"/h/projects/atlas-worktrees/KV-1":                               "atlas",
		"/h/projects/agent-sh/agnix":                                     "agnix",
		"/h/projects/agent-sh/agnix/crates/x":                            "agnix",
		"/h":                                                             "",
		"/h/projects":                                                    "",
		"/h/.cache/run42/head":                                           "",
		"/srv/memra":                                                     "memra",
	} {
		if got := RepoOfCwd(cwd, "/h", isRepo); got != want {
			t.Errorf("%s: %q, want %q", cwd, got, want)
		}
	}
	// Checkouts under $TACIT_PROJECTS instead of ~/projects.
	t.Setenv("TACIT_PROJECTS", "/code")
	repos["/code/atlas"] = true
	if got := RepoOfCwd("/code/atlas/src", "/h", isRepo); got != "atlas" {
		t.Errorf("TACIT_PROJECTS: %q, want atlas", got)
	}
	if got := RepoOfCwd("/code", "/h", isRepo); got != "" {
		t.Errorf("TACIT_PROJECTS root: %q, want none", got)
	}
}

func TestParseCheckedDistinguishesEmptyFromFailure(t *testing.T) {
	for _, body := range []string{"[]", "```json\n[]\n```"} {
		if rows, err := ParseChecked(body); err != nil || len(rows) != 0 {
			t.Fatalf("empty result %q: %v %v", body, rows, err)
		}
	}
	for _, body := range []string{"model unavailable", "{}", "[{\"description\":", "[1]", "[null]", "[{}]"} {
		if _, err := ParseChecked(body); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestWindowsPassOverALineLongerThanTheReadCap(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s1.jsonl")
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	// One record longer than maxReadBytes, then an ordinary turn after it.
	long := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxReadBytes+1000) + `"}}` + "\n"
	after := `{"type":"user","cwd":"/home/u/projects/api","message":{"content":"commit it"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Committing."},{"type":"tool_use","name":"Bash","input":{"command":"git commit"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"tools/hooks/pre-push: eslint: not found"}]}}` + "\n" +
		strings.Repeat(" ", minNewBytes) + "\n"
	os.WriteFile(f, []byte(long+after), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	ws, _ := Windows(src, st, false)
	if len(ws) != 0 || st.Offsets[f] != int64(len(long)) || st.Skipped != 1 {
		t.Fatalf("the long line should be passed over: %d windows, cursor %d (want %d), skipped %d", len(ws), st.Offsets[f], len(long), st.Skipped)
	}
	ws, _ = Windows(src, st, false)
	if len(ws) != 1 || !strings.Contains(strings.Join(ws[0].Lines, "\n"), "U: commit it") {
		t.Fatalf("the turn after the long line was not read: %v", ws)
	}
	// A long line still being written (no newline yet) keeps the cursor.
	g := filepath.Join(dir, "s2.jsonl")
	os.WriteFile(g, []byte(`{"type":"user","message":{"content":"`+strings.Repeat("y", maxReadBytes+10)), 0o644)
	st.Offsets[g] = 0
	Windows(src, st, false)
	if st.Offsets[g] != 0 {
		t.Fatalf("an unfinished long line moved the cursor to %d", st.Offsets[g])
	}
}
