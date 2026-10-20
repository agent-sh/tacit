package main

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsHooksKeepSpacesAndReinstallWithoutDuplicates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	t.Setenv("TACIT_SETTINGS", `C:\Users\Dana O'Brien\tacit config.yaml`)
	exe := `C:\Users\Dana O'Brien\My Tools\tacit.exe`
	for range 2 {
		if err := installHooksFor(p, exe, "windows"); err != nil {
			t.Fatal(err)
		}
	}
	var root map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &root)
	hooks := root["hooks"].(map[string]any)
	for event, want := range map[string]int{"SessionStart": 2, "UserPromptSubmit": 1, "PreCompact": 1} {
		entries := hooks[event].([]any)
		if len(entries) != want {
			t.Fatalf("%s entries: %d", event, len(entries))
		}
		h := entries[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		cmd := h["command"].(string)
		encoded := strings.Split(cmd, "-EncodedCommand ")[1]
		script, err := decodePowerShell(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(script, psQuote(exe)) || !strings.Contains(script, psQuote(os.Getenv("TACIT_SETTINGS"))) || !strings.Contains(script, "exit $LASTEXITCODE") {
			t.Fatalf("Windows command did not preserve arguments: %s", script)
		}
	}
}

func TestWindowsTasksUseUserRegistrationAndPortableArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TACIT_STORE", `C:\Tacit Data\facts`)
	for _, target := range []string{"follow", "dream", "curate", "dream-idle"} {
		p, err := installWindowsJob(home, `C:\My Tools\tacit.exe`, target, `C:\Provider Env\config.env`)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		var task struct {
			Actions struct {
				Exec struct{ Command, Arguments string }
			}
		}
		if err := xml.Unmarshal(b, &task); err != nil {
			t.Fatal(err)
		}
		if task.Actions.Exec.Command != "powershell.exe" || !strings.Contains(string(b), "InteractiveToken") {
			t.Fatal("task lost current-user execution")
		}
		args := strings.Split(task.Actions.Exec.Arguments, "-EncodedCommand ")
		script, err := decodePowerShell(args[1])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(script, psQuote(`C:\My Tools\tacit.exe`)) || !strings.Contains(script, "$env:TACIT_STORE") {
			t.Fatalf("task command: %s", script)
		}
		if target == "curate" && !strings.Contains(script, "maintain --env-file") {
			t.Fatal("curator does not run verification sequence")
		}
		if target == "dream-idle" && (!strings.Contains(string(b), "<Interval>PT10M</Interval>") ||
			!strings.Contains(script, "dream --idle 20m --env-file 'C:\\Provider Env\\config.env'; if ($LASTEXITCODE -eq 0) { & 'C:\\My Tools\\tacit.exe' curate --quick --min-interval 30m --env-file 'C:\\Provider Env\\config.env' }; exit $LASTEXITCODE")) {
			t.Fatalf("idle task: %s", script)
		}
	}
}

func TestCuratorInstallOnMacUsesItsOwnAgentAndLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TACIT_STORE", filepath.Join(home, "facts")) // not this machine's own store
	if code := installCurator("darwin", home, "/opt/tacit", nil); code != 0 {
		t.Fatalf("install: %d", code)
	}
	b, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "ai.tacit.curate.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ai.tacit.curate", "<string>maintain</string>", "<integer>86400</integer>", "curate.log"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestWindowsClaudeUsesExecutableArguments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	for range 2 {
		if err := installHooksFor(p, `C:\My Tools\tacit.exe`, "windows"); err != nil {
			t.Fatal(err)
		}
	}
	var root map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &root)
	hooks := root["hooks"].(map[string]any)
	e := hooks["SessionStart"].([]any)
	if len(e) != 2 {
		t.Fatal("duplicate Windows Claude hooks")
	}
	h := e[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if h["command"] != `C:\My Tools\tacit.exe` {
		t.Fatal("command was shell quoted")
	}
	args := h["args"].([]any)
	if args[len(args)-2] != "hook" || args[len(args)-1] != "session-start" {
		t.Fatal("hook arguments missing")
	}
}

func TestConfiguredEntrypointsKeepTheSelectedConfigAndStore(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "my config.yaml")
	facts := filepath.Join(root, "my facts")
	t.Setenv("TACIT_SETTINGS", cfg)
	t.Setenv("TACIT_STORE", facts)
	dream := dreamUnit("/opt/tacit", filepath.Join(root, "provider.env"))
	if !strings.Contains(dream, "TACIT_SETTINGS=") || !strings.Contains(dream, cfg) || !strings.Contains(dream, facts) {
		t.Fatal("dream job lost its selected config/store")
	}
	entry := desktopEntry("/opt/tacit-app", facts)
	if !strings.Contains(entry, "TACIT_SETTINGS=") || !strings.Contains(entry, cfg) {
		t.Fatal("desktop app lost its config")
	}
	for _, h := range []string{"codex", "hermes", "grok", "pi"} {
		var args []string
		if err := json.Unmarshal([]byte(mcpArguments(h)), &args); err != nil {
			t.Fatal(err)
		}
		if len(args) != 5 || args[0] != "--config" || args[1] != cfg || args[4] != h {
			t.Fatalf("%s MCP args: %v", h, args)
		}
	}
	dir := filepath.Join(root, "hermes")
	if err := installHermes(dir, "/opt/tacit"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "__init__.py"))
	if !strings.Contains(string(b), cfg) || !strings.Contains(string(b), facts) || strings.Contains(string(b), "@TACIT_") {
		t.Fatal("Hermes lost resolved store or config")
	}
}

func TestCuratorInstallHonorsXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TACIT_STORE", filepath.Join(home, "facts")) // not this machine's own store
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if code := installCurator("linux", home, "/opt/tacit", nil); code != 0 {
		t.Fatalf("install: %d", code)
	}
	if _, err := os.Stat(filepath.Join(xdg, "systemd", "user", "tacit-curate.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "tacit-curate.service")); !os.IsNotExist(err) {
		t.Fatal("curator wrote outside XDG config")
	}
}

func TestHermesRecallAndMirroredWritesUseConfiguredStore(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is needed for the Hermes adapter check")
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "config.yaml")
	facts := filepath.Join(root, "facts")
	t.Setenv("TACIT_SETTINGS", cfg)
	t.Setenv("TACIT_STORE", facts)
	os.MkdirAll(filepath.Join(facts, ".git"), 0o700)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "plugin")
	if err := installHermes(dir, exe); err != nil {
		t.Fatal(err)
	}
	script := `import importlib.util, sys, types
agent=types.ModuleType("agent");agent.__path__=[]
api=types.ModuleType("agent.memory_provider")
api.MemoryProvider=type("MemoryProvider",(),{})
api.RecallStatus=lambda **kw:None
api.is_trivial_prompt=lambda p:False
sys.modules["agent"]=agent;sys.modules["agent.memory_provider"]=api
spec=importlib.util.spec_from_file_location("adapter",sys.argv[1])
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
assert str(m._STORE)==sys.argv[3]
assert m.TacitProvider().is_available()
calls=[]
m.subprocess.run=lambda argv,**kw:(calls.append(argv) or types.SimpleNamespace(stdout="",stderr="",returncode=0))
class Thread:
 def __init__(self,target,**kw):self.target=target
 def start(self):self.target()
m.threading.Thread=Thread
m._run(["hook","prompt"],{},1)
m.TacitProvider().on_memory_write("add","memory","Remember the review order before merging public changes.")
assert len(calls)==2
for argv in calls:assert argv[1:3]==["--config",sys.argv[2]],argv
assert calls[0][3:]==["hook","prompt"]
assert calls[1][3]=="add"
print("PASS: Hermes recall and mirrored writes share the selected config")
`
	cmd := exec.Command(python, "-I", "-c", script, filepath.Join(dir, "__init__.py"), cfg, facts)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "TACIT_STORE=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Hermes adapter: %v\n%s", err, out)
	}
}

func TestDreamInstallQuotesProviderFilePath(t *testing.T) {
	home := t.TempDir()
	env := filepath.Join(home, "provider config.env")
	paths, err := installDreamSystemd(home, "/opt/my tools/tacit", env)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(paths[0])
	if !strings.Contains(string(b), "EnvironmentFile="+unitQuote(env, false)+"\n") {
		t.Fatal("provider file with spaces is not quoted")
	}
}

func TestLocalFollowerDoesNotRestartUnderExistingJobDefinitions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TACIT_STORE", filepath.Join(root, "facts"))
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	if code := runFollowCode(nil); code != exitConfig {
		t.Fatalf("systemd restart-prevention status: %d", code)
	}
	if code := runFollow([]string{"--launchd"}); code != 0 {
		t.Fatalf("launchd successful stop status: %d", code)
	}
}

// Register-ScheduledTask receives a Unicode BSTR, so a UTF-8 declaration
// conflicts with the native parser even though Go can decode the file.
func TestWindowsTaskXMLCanBePassedAsUnicodeString(t *testing.T) {
	path, err := installWindowsJob(t.TempDir(), `C:\Tools\tacit.exe`, "dream", `C:\config.env`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `encoding="UTF-8"`) {
		t.Fatal("task XML forces UTF-8 on a Unicode registration string")
	}
}
