package ownerwords

import (
	"path/filepath"
	"testing"
)

// A Mac keeps the same ~/.claude and ~/.codex layout,
// and its temporary directories are /private/tmp and /var/folders/...: the
// corpus reads the owner's terminal and desktop-app sessions there and skips
// sessions started in those temporary directories.
func TestCorpusOnAMacHome(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-Users-someone-projects-tacit")
	writeFile(t, filepath.Join(proj, "cli.jsonl"), `{"type":"user","entrypoint":"cli","sessionId":"mac1","cwd":"/Users/someone/projects/tacit","message":{"role":"user","content":"keep the mac setup to one script please"}}`+"\n")
	writeFile(t, filepath.Join(proj, "desk.jsonl"), `{"type":"user","entrypoint":"claude-desktop","sessionId":"mac2","cwd":"/Users/someone/projects/tacit","message":{"role":"user","content":"the desktop app counts as my own words too"}}`+"\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-private-var-folders-x-T-run", "tmp.jsonl"), `{"type":"user","entrypoint":"cli","sessionId":"mac3","cwd":"/private/var/folders/x1/abc/T/run","message":{"role":"user","content":"an agent smoke test typed this prompt here"}}`+"\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-private-tmp", "t.jsonl"), `{"type":"user","entrypoint":"cli","sessionId":"mac4","cwd":"/private/tmp","message":{"role":"user","content":"another scratch session in the temp dir"}}`+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"sync the macbook with the workstation","sessionId":"mac1"}`+"\n")
	writeFile(t, filepath.Join(home, "Library", "Application Support", "Claude", "config.json"), "{}\n")

	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"keep the mac setup to one script":       "claude:cli",
		"the desktop app counts as my own words": "claude:desk",
		"sync the macbook with the workstation":  "claude:mac1",
		"an agent smoke test typed this prompt":  "",
		"another scratch session in the temp":    "",
	} {
		m, _ := c.Find(q, "", true)
		if m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
	for _, cwd := range []string{"/private/tmp", "/private/tmp/x", "/var/folders/ab/cd/T/y", "/private/var/folders/ab/cd/T/y"} {
		if !AgentCwd(cwd) {
			t.Errorf("AgentCwd(%q) = false", cwd)
		}
	}
	for _, cwd := range []string{"/Users/someone/projects/tacit", "/Users/someone"} {
		if AgentCwd(cwd) {
			t.Errorf("AgentCwd(%q) = true", cwd)
		}
	}
}
