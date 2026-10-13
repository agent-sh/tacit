package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Codex hooks.json with an unrelated PreToolUse guard and an old tacit
// entry: install keeps the guard, replaces the old entry, and a second run
// changes nothing.
func TestInstallHooksMergesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	start := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/bin/python3 guard.py"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/old/tacit hook prompt"}]}]}}`
	if err := os.WriteFile(path, []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installHooks(path, "/bin/tacit"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := installHooks(path, "/bin/tacit"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("second install changed the file:\n%s\n%s", first, second)
	}
	var got struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hooks["PreToolUse"]) != 1 || !strings.Contains(got.Hooks["PreToolUse"][0].Hooks[0].Command, "guard.py") {
		t.Fatalf("guard lost: %s", first)
	}
	ups := got.Hooks["UserPromptSubmit"]
	if len(ups) != 1 || ups[0].Hooks[0].Command != "/bin/tacit hook prompt" {
		t.Fatalf("prompt hook: %s", first)
	}
	ss := got.Hooks["SessionStart"]
	if len(ss) != 1 || ss[0].Matcher != "startup" || ss[0].Hooks[0].Command != "/bin/tacit hook session-start" {
		t.Fatalf("start hook: %s", first)
	}
}

// The unit that `tacit install follow` writes is the one in deploy/, with the
// running binary's path in place of %h/.local/bin/tacit.
func TestFollowUnitMatchesTheDeployCopy(t *testing.T) {
	want, err := os.ReadFile("../../deploy/systemd/tacit-follow.service")
	if err != nil {
		t.Fatal(err)
	}
	if got := followUnit("%h/.local/bin/tacit", ""); got != string(want) {
		t.Fatalf("deploy copy drifted from followUnit:\n%s\n---\n%s", got, want)
	}
}

// The unit must not keep Go panics (exit 2) from restarting. The one code it
// refuses to restart on is the one runFollow returns for a bad invocation.
func TestFollowUnitPreventsRestartOnlyOnTheConfigExit(t *testing.T) {
	unit := followUnit("/bin/tacit", "")
	if want := fmt.Sprintf("RestartPreventExitStatus=%d\n", exitConfig); !strings.Contains(unit, want) {
		t.Fatalf("unit lacks %q:\n%s", want, unit)
	}
	if strings.Contains(unit, "ExitStatus=2") || exitConfig == 2 {
		t.Fatal("exit 2 is a Go panic; it must restart")
	}
	if got := runFollow([]string{"--no-such-flag"}); got != exitConfig {
		t.Errorf("a bad flag exits %d, want %d", got, exitConfig)
	}
	if got := runFollow([]string{"--as", "Not A Slug"}); got != exitConfig {
		t.Errorf("a bad --as exits %d, want %d", got, exitConfig)
	}
}

func TestInstallFollowWritesTheUnit(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("TACIT_STORE", "")
	path, err := installFollow("/opt/my tools/tacit")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(cfg, "systemd", "user", "tacit-follow.service") {
		t.Fatalf("path %s", path)
	}
	b, _ := os.ReadFile(path)
	text := string(b)
	for _, want := range []string{
		`ExecStart="/opt/my tools/tacit" follow` + "\n",
		"Restart=always\n",
		"RestartPreventExitStatus=78\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Environment=") {
		t.Errorf("no TACIT_STORE was set, yet the unit has an Environment line:\n%s", text)
	}
	again, err := installFollow("/opt/my tools/tacit")
	if err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(again); string(b2) != text {
		t.Fatal("second install changed the unit")
	}
}

// A store chosen with $TACIT_STORE at install time reaches the service.
func TestInstallFollowCarriesTheStorePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TACIT_STORE", "/data/100%/store")
	path, err := installFollow("/bin/tacit")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `Environment="TACIT_STORE=/data/100%%/store"`+"\n") {
		t.Fatalf("unit:\n%s", b)
	}
}

// Reinstalling keeps tacit's entry where it was, so a hook after it keeps
// its position (Codex keys hook trust by position).
func TestInstallHooksKeepsPosition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	start := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/old/tacit hook prompt"}]},{"hooks":[{"type":"command","command":"later.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installHooks(path, "/bin/tacit"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var got struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	ups := got.Hooks["UserPromptSubmit"]
	if len(ups) != 2 || ups[0].Hooks[0].Command != "/bin/tacit hook prompt" || ups[1].Hooks[0].Command != "later.sh" {
		t.Fatalf("order changed: %s", b)
	}
}
