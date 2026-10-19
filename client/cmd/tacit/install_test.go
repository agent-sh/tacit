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

// tacit install claude registers the MCP server in Claude Code's user
// config next to the servers already there, keeps every other member as
// written (order, numbers), backs the file up, changes nothing on a second
// run, and points the start block at the tools; --no-mcp takes the server
// out again and the hook goes back to `tacit fact`.
func TestInstallClaudeRegistersTheMCPServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_SETTINGS", "")
	cfg := filepath.Join(home, ".claude.json")
	start := `{
  "numStartups": 12,
  "firstStartTime": "2026-01-02T03:04:05.000Z",
  "bigNumber": 12345678901234567890,
  "mcpServers": {
    "other": {
      "type": "stdio",
      "command": "node",
      "args": ["/opt/other/server.mjs"],
      "env": {}
    }
  },
  "projects": {"/home/u/repo": {"allowedTools": [], "mcpServers": {}}}
}`
	if err := os.WriteFile(cfg, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	install := func(args ...string) string {
		t.Helper()
		if code := runInstall(append([]string{"claude"}, args...)); code != 0 {
			t.Fatalf("install claude %v exited %d", args, code)
		}
		b, _ := os.ReadFile(cfg)
		return string(b)
	}
	first := install()
	if b, _ := os.ReadFile(cfg + ".tacit-backup"); string(b) != start {
		t.Fatalf("backup is not the file as it was:\n%s", b)
	}
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", st.Mode().Perm())
	}
	var got struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if s := got.MCPServers["tacit"]; s.Type != "stdio" || s.Command != exe || strings.Join(s.Args, " ") != "mcp --harness claude" {
		t.Fatalf("tacit server: %+v", s)
	}
	if got.MCPServers["other"].Command != "node" {
		t.Fatalf("the other server was lost:\n%s", first)
	}
	order := []string{`"numStartups": 12`, `"firstStartTime"`, `"bigNumber": 12345678901234567890`, `"mcpServers"`, `"other"`, `"tacit"`, `"projects"`}
	at := 0
	for _, k := range order {
		i := strings.Index(first[at:], k)
		if i < 0 {
			t.Fatalf("%s missing or out of order:\n%s", k, first)
		}
		at += i
	}
	hooks, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(hooks), "hook session-start --mcp") || !strings.Contains(string(hooks), "hook prompt\"") {
		t.Fatalf("hooks:\n%s", hooks)
	}
	// Idempotent: same bytes, the backup untouched.
	os.WriteFile(cfg+".tacit-backup", []byte("sentinel"), 0o600)
	if again := install(); again != first {
		t.Fatalf("a second install changed the config:\n%s", again)
	}
	if b, _ := os.ReadFile(cfg + ".tacit-backup"); string(b) != "sentinel" {
		t.Fatal("a second install rewrote the backup")
	}
	// Opt out: the server goes, the rest stays.
	off := install("--no-mcp")
	if strings.Contains(off, `"tacit"`) || !strings.Contains(off, `"other"`) || !strings.Contains(off, `"bigNumber": 12345678901234567890`) {
		t.Fatalf("--no-mcp:\n%s", off)
	}
	hooks, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Contains(string(hooks), "--mcp") || strings.Count(string(hooks), "hook session-start") != 1 {
		t.Fatalf("hooks after --no-mcp:\n%s", hooks)
	}
}

// A server named tacit that is not tacit's own is never replaced.
func TestInstallClaudeLeavesAnotherServerNamedTacit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".claude.json")
	start := `{"mcpServers": {"tacit": {"type": "stdio", "command": "python3", "args": ["/opt/notes/server.py"]}}}`
	os.WriteFile(cfg, []byte(start), 0o600)
	if code := runInstall([]string{"claude"}); code != 1 {
		t.Fatalf("install exited %d, want 1", code)
	}
	if b, _ := os.ReadFile(cfg); string(b) != start {
		t.Fatalf("the config changed:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err == nil {
		t.Fatal("hooks pointing at the MCP tools were installed without the server")
	}
}

// No config yet: a new file with the server alone.
func TestInstallClaudeCreatesTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code := runInstall([]string{"claude"}); code != 0 {
		t.Fatalf("install exited %d", code)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil || !strings.Contains(string(b), `"tacit": {`) {
		t.Fatalf("config: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json.tacit-backup")); err == nil {
		t.Fatal("a backup of a file that did not exist")
	}
}

// The start block names the MCP tools for detail when the hook says the
// session has them, and the command otherwise.
func TestSessionStartNamesTheMCPToolWhenInstalled(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}, filler: 400, rules: 2})
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	cwd := filepath.Join(r.projects, "demo")
	payload := func(session string) string {
		return `{"session_id":"` + session + `","cwd":"` + cwd + `","hook_event_name":"SessionStart","source":"startup"}`
	}
	title := func(out string) string { return strings.SplitN(out, "\n", 2)[0] }
	with := title(hookOut(t, payload("s1"), "session-start", "--mcp"))
	if !strings.Contains(with, "`mcp__tacit__tacit_fact` for detail") || strings.Contains(with, "`tacit fact") {
		t.Fatalf("with the server: %q", with)
	}
	without := title(hookOut(t, payload("s2"), "session-start"))
	if !strings.Contains(without, "`tacit fact <name>` for detail") || strings.Contains(without, "mcp__") {
		t.Fatalf("without the server: %q", without)
	}
}
