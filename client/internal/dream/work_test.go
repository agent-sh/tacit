package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

func TestWorkInReadsEditsAndCommitsPerHarness(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(base, "scratch", "tool")
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)
	home := filepath.Join(base, "home")
	for _, c := range []struct {
		lines []string
		cwd   string
		want  string
	}{
		{[]string{"T: Edit " + filepath.Join(wt, "src", "a.go")}, wt, "edit"},                          // Claude Code
		{[]string{"T: Write " + filepath.Join(wt, "NEW.md")}, filepath.Join(wt, "src"), "edit"},        // a new file
		{[]string{"T: edit src/a.go"}, wt, "edit"},                                                     // pi, relative to the cwd
		{[]string{"T: write_file " + filepath.Join(wt, "x.txt")}, wt, "edit"},                          // Hermes
		{[]string{"T: apply_patch *** Begin Patch *** Update File: src/a.go @@ -1 +1 @@"}, wt, "edit"}, // Codex
		{[]string{"T: exec_command apply_patch *** Begin Patch *** Add File: src/b.go +x"}, wt, "edit"},
		{[]string{"T: Bash git add -A && git commit -m fix"}, wt, "commit"},
		{[]string{"T: shell git -c user.name=x commit -qm y"}, wt, "commit"},
		{[]string{"T: terminal git -C " + wt + " commit -m y"}, base, "commit"},
		{[]string{"T: Edit " + filepath.Join(base, "elsewhere", "a.go")}, wt, ""}, // outside the checkout
		{[]string{"T: Read " + filepath.Join(wt, "src", "a.go")}, wt, ""},         // a read is no work
		{[]string{"T: Grep git commit"}, wt, ""},                                  // not a shell tool
		{[]string{"T: Bash cd /elsewhere && git commit -m x"}, wt, ""},            // ran somewhere else
		{[]string{"T: Bash git log --grep 'git commit'"}, wt, ""},
		{[]string{"T: Bash git commit -m x"}, base, ""}, // the cwd is not in the checkout
		{[]string{"U: commit it", "A: Committed with git commit."}, wt, ""},
		{[]string{"T: edit src/a.go"}, "", ""}, // a relative path without a cwd
	} {
		if got := WorkIn(c.lines, wt, c.cwd, home); got != c.want {
			t.Errorf("WorkIn(%q, cwd %s) = %q, want %q", c.lines, c.cwd, got, c.want)
		}
	}
}

// A session whose cwd is a checkout outside home is filed under it only once
// two sessions edited or committed there; until then the window's repo is
// what its paths say, and a session that only read there is no evidence.
func TestWindowsElsewhereWaitForWorkFromTwoSessions(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	scratch := filepath.Join(base, "srv", "memra")
	os.MkdirAll(filepath.Join(scratch, ".git"), 0o755)
	os.WriteFile(filepath.Join(scratch, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	dir := filepath.Join(home, "transcripts")
	os.MkdirAll(dir, 0o755)
	write := func(name, tool string) string {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(`{"type":"user","cwd":"`+filepath.ToSlash(scratch)+`","message":{"content":"fix the loader"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Fixing."},{"type":"tool_use","name":"`+tool+`","input":{"file_path":"`+filepath.ToSlash(filepath.Join(scratch, "lib", "load.rs"))+`"}}]}}`+"\n"+
			strings.Repeat(" ", minNewBytes)+"\n"), 0o644)
		return f
	}
	a, read := write("a.jsonl", "Edit"), write("r.jsonl", "Read")
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	st := &State{Offsets: map[string]int64{a: 0, read: 0}}
	ws, _ := Windows(src, st, false)
	work := map[string]map[string]bool{}
	AcceptPending(ws, work)
	if len(ws) != 2 {
		t.Fatalf("%d windows", len(ws))
	}
	for _, w := range ws {
		if w.Repo != "" || w.Pending.Name != "memra" {
			t.Fatalf("one session's work named the repo: %+v", w)
		}
		if (w.File == a) != (w.Work == "edit") {
			t.Fatalf("evidence %q for %s", w.Work, w.File)
		}
	}
	b := write("b.jsonl", "Write")
	st.Offsets = map[string]int64{a: 0, read: 0, b: 0}
	ws, _ = Windows(src, st, false)
	AcceptPending(ws, map[string]map[string]bool{})
	for _, w := range ws {
		if w.Repo != "memra" || w.RepoRoot != scratch || w.Fallback != "" {
			t.Fatalf("two sessions' work: %+v", w)
		}
	}
}
