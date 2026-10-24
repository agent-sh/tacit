package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `tacit install pi` writes the questions extension into pi's agent
// directory with this binary's path, and prints the MCP server as before.
func TestInstallPiWritesTheQuestionsExtension(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if code := runInstall([]string{"pi"}); code != 0 {
		t.Fatalf("install pi: %d", code)
	}
	b, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "extensions", "tacit.ts"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "@TACIT_BIN@") || !strings.Contains(text, `["hook", "questions"]`) || !strings.Contains(text, "before_agent_start") {
		t.Fatalf("extension:\n%s", text)
	}
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi-elsewhere"))
	if code := runInstall([]string{"pi"}); code != 0 {
		t.Fatalf("install pi: %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "pi-elsewhere", "extensions", "tacit.ts")); err != nil {
		t.Fatal(err)
	}
}
