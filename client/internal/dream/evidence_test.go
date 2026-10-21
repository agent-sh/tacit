package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// Every window keeps its evidence of work: in its cwd's checkout (not only
// one elsewhere), or in the non-git folder of the projects directory its
// cwd is in.
func TestWindowsKeepWorkForEveryRootAndProjectFolder(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "projects")
	t.Setenv("TACIT_PROJECTS", proj)
	widget := filepath.Join(proj, "widget")
	os.MkdirAll(filepath.Join(widget, ".git"), 0o755)
	os.WriteFile(filepath.Join(widget, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	notes := filepath.Join(proj, "notes-folder", "sub")
	os.MkdirAll(notes, 0o755)
	dir := filepath.Join(home, "transcripts")
	os.MkdirAll(dir, 0o755)
	mk := func(name, cwd, file, tool string) string {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(`{"type":"user","cwd":"`+filepath.ToSlash(cwd)+`","message":{"content":"fix it"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Fixing."},{"type":"tool_use","name":"`+tool+`","input":{"file_path":"`+filepath.ToSlash(file)+`"}}]}}`+"\n"+
			strings.Repeat(" ", minNewBytes)+"\n"), 0o644)
		return f
	}
	a := mk("a.jsonl", widget, filepath.Join(widget, "main.go"), "Edit")
	b := mk("b.jsonl", notes, filepath.Join(notes, "plan.md"), "Write")
	c := mk("c.jsonl", home, filepath.Join(home, "x.md"), "Write")
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	ws, _ := Windows(src, &State{Offsets: map[string]int64{a: 0, b: 0, c: 0}}, false)
	got := map[string]Window{}
	for _, w := range ws {
		got[w.File] = w
	}
	if w := got[a]; w.Work != "edit" || w.WorkRoot != widget || w.WorkFolder != "" {
		t.Errorf("repo window: %+v", w)
	}
	if w := got[b]; w.Work != "edit" || w.WorkFolder != filepath.Join(proj, "notes-folder") || w.WorkRoot != "" {
		t.Errorf("folder window: work %q root %q folder %q", w.Work, w.WorkRoot, w.WorkFolder)
	}
	if w := got[c]; w.WorkRoot != "" || w.WorkFolder != "" {
		t.Errorf("home window: %+v", w)
	}
	if ProjectFolder(filepath.Join(proj, ".cache", "x"), home) != "" || ProjectFolder(proj, home) != "" || ProjectFolder(filepath.Join(base, "elsewhere"), home) != "" {
		t.Error("ProjectFolder outside a project folder")
	}
}

func TestToolUsage(t *testing.T) {
	tools, progs := ToolsOf([]string{
		"T: mcp__tickets__TicketingRead V123",
		"T: Bash FOO=1 sudo /usr/bin/git -C x status && make build | tee log; ./run.sh",
		"T: Read /etc/hosts",
		"U: not a tool",
	})
	if strings.Join(tools, ",") != "Bash,Read,mcp__tickets__TicketingRead" || strings.Join(progs, ",") != "git,make,run.sh,tee" {
		t.Fatalf("tools %v programs %v", tools, progs)
	}
	path := filepath.Join(t.TempDir(), ToolsFile)
	u := LoadToolUsage(path)
	u.Add("s1", "2026-10-20", tools, progs)
	u.Add("old", "2020-01-01", []string{"Bash"}, nil)
	u.Add("", "2026-10-20", tools, nil)
	if err := u.Save(path, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	u = LoadToolUsage(path)
	if len(u.Sessions) != 1 || !u.Used("claude:s1", "mcp__*__Ticketing*") || !u.Used("s1", "Bash") || u.Used("s1", "mcp__*__Paging*") || u.Used("s2", "Bash") {
		t.Fatalf("usage %+v", u.Sessions)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "status") || strings.Contains(string(b), "V123") {
		t.Fatalf("arguments kept:\n%s", b)
	}
}
