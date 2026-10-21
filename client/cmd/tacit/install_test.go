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
	ptu := got.Hooks["PreToolUse"]
	if len(ptu) != 2 || !strings.Contains(ptu[0].Hooks[0].Command, "guard.py") {
		t.Fatalf("guard lost: %s", first)
	}
	if ptu[1].Matcher != toolPushTools || ptu[1].Hooks[0].Command != "/bin/tacit hook pre-tool" {
		t.Fatalf("pre-tool hook: %s", first)
	}
	ups := got.Hooks["UserPromptSubmit"]
	if len(ups) != 1 || ups[0].Hooks[0].Command != "/bin/tacit hook prompt" {
		t.Fatalf("prompt hook: %s", first)
	}
	ss := got.Hooks["SessionStart"]
	if len(ss) != 1 || ss[0].Matcher != "startup" || ss[0].Hooks[0].Command != "/bin/tacit hook session-start" {
		t.Fatalf("start hook: %s", first)
	}
	pc := got.Hooks["PreCompact"]
	if len(pc) != 1 || pc[0].Matcher != "" || pc[0].Hooks[0].Command != "/bin/tacit hook pre-compact" {
		t.Fatalf("pre-compact hook: %s", first)
	}
}

// An install of an earlier build of this branch has a second SessionStart
// entry, matcher compact, running `hook session-start --compact`, between
// the startup entry and an unrelated one. A reinstall removes it and keeps
// the others where they are; a rerun changes nothing.
func TestInstallHooksRemovesAStaleCompactEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	start := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"/old/tacit hook session-start"}]},` +
		`{"matcher":"compact","hooks":[{"type":"command","command":"/old/tacit hook session-start --compact"}]},` +
		`{"hooks":[{"type":"command","command":"later.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := range 2 {
		if err := installHooks(path, "/bin/tacit"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if i == 1 && string(b) != string(first) {
			t.Fatalf("a rerun changed the file:\n%s\n---\n%s", first, b)
		}
		first = b
	}
	var got struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	ss := got.Hooks["SessionStart"]
	if len(ss) != 2 || ss[0].Matcher != "startup" || ss[0].Hooks[0].Command != "/bin/tacit hook session-start" ||
		ss[1].Hooks[0].Command != "later.sh" || strings.Contains(string(first), "--compact") {
		t.Fatalf("entries: %s", first)
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
	// --no-mcp installs the hooks and leaves their server alone.
	if code := runInstall([]string{"claude", "--no-mcp"}); code != 0 {
		t.Fatalf("install --no-mcp exited %d, want 0", code)
	}
	if b, _ := os.ReadFile(cfg); string(b) != start {
		t.Fatalf("--no-mcp changed their server:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("--no-mcp installed no hooks: %v", err)
	}
}

// A symlinked config is written through the link, and the link stays.
func TestInstallClaudeWritesThroughASymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "dotfiles", "claude.json")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte(`{"theme": "dark"}`), 0o600)
	link := filepath.Join(home, ".claude.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	if code := runInstall([]string{"claude"}); code != 0 {
		t.Fatalf("install exited %d", code)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v", err)
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), `"tacit"`) || !strings.Contains(string(b), `"theme"`) {
		t.Fatalf("the target was not merged:\n%s", b)
	}
}

// The file changed while the merge was made (Claude Code wrote it): the
// merge is redone on what is there now, so their change is kept.
func TestSetClaudeMCPRedoesTheMergeWhenTheFileChanged(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	os.WriteFile(cfg, []byte(`{"a": 1}`), 0o600)
	calls := 0
	readHook = func() {
		if calls++; calls == 1 {
			os.WriteFile(cfg, []byte(`{"a": 1, "b": 2}`), 0o600)
		}
	}
	defer func() { readHook = func() {} }()
	if _, err := setClaudeMCP(cfg, map[string]any{"type": "stdio", "command": "tacit", "args": []string{"mcp"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), `"b"`) || !strings.Contains(string(b), `"tacit"`) {
		t.Fatalf("a concurrent change was lost:\n%s", b)
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
	title := func(out string) string { return strings.Join(strings.SplitN(out, "\n", 3)[:2], "\n") }
	with := title(hookOut(t, payload("s1"), "session-start", "--mcp"))
	if !strings.Contains(with, "`mcp__tacit__tacit_fact <name>` opens one") || !strings.Contains(with, "run one `mcp__tacit__tacit_search` for it") || strings.Contains(with, "`tacit ") {
		t.Fatalf("with the server: %q", with)
	}
	without := title(hookOut(t, payload("s2"), "session-start"))
	if !strings.Contains(without, "`tacit fact <name>` opens one") || !strings.Contains(without, "run one `tacit search <what you will do>` for it") || strings.Contains(without, "mcp__") {
		t.Fatalf("without the server: %q", without)
	}
	// With the instruction off the title names the tools as before.
	t.Setenv("TACIT_DELIVERY_INSTRUCTION", "off")
	off := hookOut(t, payload("s3"), "session-start", "--mcp")
	if first := strings.SplitN(off, "\n", 2)[0]; !strings.Contains(first, "`mcp__tacit__tacit_fact` for detail, `mcp__tacit__tacit_search` for more") || strings.Contains(off, "tacit holds") {
		t.Fatalf("instruction off: %q", off)
	}
}

// An entry an earlier release wrote under the narrower PreToolUse matcher is
// widened in place (Codex keys hook trust by position); one it shares with
// another hook is left with that hook, and ours moves to its own entry.
func TestInstallWidensTheOldPreToolMatcherInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	old := `{"hooks":{"PreToolUse":[{"matcher":"Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch","hooks":[{"type":"command","command":"/bin/tacit hook pre-tool"}]},{"matcher":"Bash","hooks":[{"type":"command","command":"guard.py"}]}]}}`
	os.WriteFile(path, []byte(old), 0o644)
	if err := installHooksFor(path, "/bin/tacit", "linux"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &got)
	ptu := got.Hooks["PreToolUse"]
	if len(ptu) != 2 || ptu[0].Matcher != toolPushTools || ptu[0].Hooks[0].Command != "/bin/tacit hook pre-tool" || ptu[1].Hooks[0].Command != "guard.py" {
		t.Fatalf("not widened in place: %s", b)
	}
	shared := `{"hooks":{"PreToolUse":[{"matcher":"Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch","hooks":[{"type":"command","command":"guard.py"},{"type":"command","command":"/bin/tacit hook pre-tool"}]}]}}`
	os.WriteFile(path, []byte(shared), 0o644)
	if err := installHooksFor(path, "/bin/tacit", "linux"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	got.Hooks = nil
	json.Unmarshal(b, &got)
	ptu = got.Hooks["PreToolUse"]
	if len(ptu) != 2 || ptu[0].Matcher != "Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch" || len(ptu[0].Hooks) != 1 || ptu[0].Hooks[0].Command != "guard.py" || ptu[1].Matcher != toolPushTools {
		t.Fatalf("a shared entry: %s", b)
	}
}
