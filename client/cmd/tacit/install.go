package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runInstall wires tacit into a harness. Idempotent: existing tacit
// entries are replaced.
//
//   - claude: SessionStart and UserPromptSubmit hooks in ~/.claude/settings.json.
//   - codex: the same two hooks in ~/.codex/hooks.json (Codex takes the same
//     hook JSON and adds plain stdout as context); Codex asks the owner to
//     trust new hooks once, in /hooks.
//   - hermes: a memory provider plugin in ~/.hermes/plugins/tacit that calls
//     the same two hooks and mirrors Hermes' own memory writes into the inbox.
//   - app: for a binary built with the desktop window. Linux: the desktop
//     entry ~/.local/share/applications/tacit.desktop and its icon. macOS:
//     ~/Applications/tacit.app (macapp.go). Nothing is started.
//   - follow: the job that runs `tacit follow`: a systemd user unit on Linux,
//     a launchd agent on macOS (launchd.go). It writes the file and prints
//     the commands; it never loads or starts it.
//   - dream: the dreamer every 4 h, the same way (a service and a timer on
//     Linux, a launchd agent on macOS), with --env-file naming the file with
//     the variables claude -p needs. Prints the commands; starts nothing.
//   - grok, pi, mcp: Grok ignores what start and prompt hooks print, so these
//     get the MCP server only; the snippet is printed.
func runInstall(args []string) int {
	if len(args) < 1 {
		usage()
	}
	target, rest := args[0], args[1:]
	if target != "dream" && target != "curate" && len(rest) != 0 {
		usage()
	}
	exe, err := os.Executable()
	if err != nil {
		return 2
	}
	home, _ := os.UserHomeDir()
	goos := runtime.GOOS
	switch target {
	case "claude", "codex":
		path := filepath.Join(home, ".claude", "settings.json")
		if target == "codex" {
			path = filepath.Join(home, ".codex", "hooks.json")
		}
		if err := installHooks(path, exe); err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		fmt.Println("installed SessionStart and UserPromptSubmit hooks in", path)
		if target == "codex" {
			fmt.Println("Codex skips new hooks until you trust them: run /hooks in Codex once.")
			fmt.Printf("MCP (~/.codex/config.toml):\n[mcp_servers.tacit]\ncommand = %q\nargs = %s\n", exe, mcpArguments("codex"))
		}
	case "hermes":
		hermesHome := os.Getenv("HERMES_HOME")
		if hermesHome == "" {
			hermesHome = filepath.Join(home, ".hermes")
		}
		dir := filepath.Join(hermesHome, "plugins", "tacit")
		if err := installHermes(dir, exe); err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		fmt.Println("installed the tacit memory provider in", dir)
		fmt.Println("In", filepath.Join(hermesHome, "config.yaml"), "set memory.provider: tacit, add tacit to plugins.enabled, give the MCP server the hermes writer, then restart the gateway:")
		fmt.Printf("  tacit:\n    command: %q\n    args: %s\n", exe, mcpArguments("hermes"))
	case "curate":
		return installCurator(goos, home, exe, rest)
	case "follow":
		if st, err := store.Default(); err == nil && st.Exists() && st.Local(context.Background()) {
			fmt.Println("Local store: no follower is needed")
			return 0
		}
		if goos == "windows" {
			path, err := installWindowsJob(home, exe, "follow", "")
			if err != nil {
				fmt.Fprintln(os.Stderr, "install:", err)
				return 1
			}
			printWindowsJob(path, "follow")
			return 0
		}

		if goos == "darwin" {
			path, err := installLaunchAgent(home, launchdFollowLabel, followPlist(exe, os.Getenv("TACIT_STORE"), home, servicePATH(home, exe, lookPath)))
			if err != nil {
				fmt.Fprintln(os.Stderr, "install:", err)
				return 1
			}
			fmt.Println("wrote", path)
			fmt.Println("start it (and after every reinstall):")
			for _, l := range launchctlLines(launchdFollowLabel, path) {
				fmt.Println("  " + l)
			}
			fmt.Println("log:", filepath.Join(launchdLogDir(home), "follow.log"))
			fmt.Println("needs git 2.29 or newer (the Xcode command line tools have it).")
			fmt.Println("the curator and the verifier are not installed here: they run on the machine that holds the curator key.")
			return 0
		}
		path, err := installFollow(exe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		fmt.Println("wrote", path)
		fmt.Println("start it: systemctl --user daemon-reload && systemctl --user enable --now tacit-follow.service")
		fmt.Println("headless machine (no login session): also run `loginctl enable-linger $USER`, or the service stops at logout.")
		fmt.Println("needs git 2.29 or newer.")
	case "dream":
		return installDream(goos, home, exe, rest)
	case "app":
		if goos == "windows" {
			dir := filepath.Join(home, ".config", "tacit", "tasks")
			os.MkdirAll(dir, 0o700)
			script := "& " + psQuote(exe) + " app --browser"
			if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
				script = "$env:TACIT_SETTINGS=" + psQuote(cfg) + "; " + script
			}
			if st := os.Getenv("TACIT_STORE"); st != "" {
				script = "$env:TACIT_STORE=" + psQuote(st) + "; " + script
			}
			path := filepath.Join(dir, "tacit-app.ps1")
			if err := writeFileMode(path, []byte(script+"\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "install:", err)
				return 1
			}
			fmt.Println("Open the app in PowerShell: &", psQuote(path))
			return 0
		}
		if !desktopBuild {
			tags := "desktop,production,webkit2_41"
			if goos == "darwin" {
				tags = "desktop,production"
			}
			fmt.Fprintln(os.Stderr, "install: this tacit was built without the desktop window; build it with -tags "+tags+" and run that binary's install app")
			return 1
		}
		if goos == "darwin" {
			paths, err := installMacApp(home, exe, os.Getenv("TACIT_STORE"), servicePATH(home, exe, lookPath), appIconICNS, adHocSign)
			for _, p := range paths {
				fmt.Println("wrote", p)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "install:", err)
				return 1
			}
			fmt.Println("tacit is in ~/Applications (Spotlight, Launchpad); open it: open", shellQuote(macAppDir(home)))
			fmt.Println("rerun install app after rebuilding tacit-app: the bundle holds a copy of the binary.")
			return 0
		}
		paths, err := installApp(home, exe, os.Getenv("TACIT_STORE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		for _, p := range paths {
			fmt.Println("wrote", p)
		}
		fmt.Println("tacit is in the applications menu; the entry runs", exe, "app")
	case "grok":
		fmt.Printf("Add to ~/.grok/config.toml:\n[mcp_servers.tacit]\ncommand = %q\nargs = %s\nenabled = true\n", exe, mcpArguments("grok"))
	case "pi", "mcp":
		fmt.Printf("Add this MCP server to %s:\n  command: %q\n  args: %s\n", target, exe, mcpArguments(target))
	default:
		usage()
	}
	return 0
}

// installDream implements `tacit install dream [--env-file F]`.
func installDream(goos, home, exe string, args []string) int {
	fs := flag.NewFlagSet("install dream", flag.ContinueOnError)
	envFile := fs.String("env-file", defaultDreamEnv(goos, home), "KEY=VALUE file with the variables claude -p needs (for example the Bedrock ones); never copied into the job")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	// The dreamer writes: a read-only clone gets no dreamer.
	if s, err := store.Default(); err == nil && s.Exists() && s.ReadOnly(context.Background()) {
		fmt.Fprintln(os.Stderr, "install: no dreamer on a read-only clone:", store.ErrReadOnly)
		return 1
	}
	env, err := filepath.Abs(*envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 2
	}
	var paths []string
	if goos == "windows" {
		var p string
		p, err = installWindowsJob(home, exe, "dream", env)
		paths = []string{p}
	} else if goos == "darwin" {
		var p string
		p, err = installLaunchAgent(home, launchdDreamLabel, dreamPlist(exe, env, os.Getenv("TACIT_STORE"), home, servicePATH(home, exe, lookPath)))
		paths = []string{p}
	} else {
		paths, err = installDreamSystemd(home, exe, env)
	}
	for _, p := range paths {
		if p != "" {
			fmt.Println("wrote", p)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	if _, err := os.Stat(env); err != nil {
		fmt.Println("missing:", env, "- the dreamer fails until it exists. It holds KEY=VALUE lines for claude -p (provider variables, for example CLAUDE_CODE_USE_BEDROCK=1 and AWS_REGION for Bedrock, and TACIT_DREAM_MODEL to name a full model id); keep it chmod 600. It may be empty when claude on this machine is logged in another way.")
	}
	if goos == "windows" {
		printWindowsJob(paths[0], "dream")
	} else if goos == "darwin" {
		fmt.Println("start it (and after every reinstall):")
		for _, l := range launchctlLines(launchdDreamLabel, paths[0]) {
			fmt.Println("  " + l)
		}
		fmt.Println("log:", filepath.Join(launchdLogDir(home), "dream.log"))
	} else {
		fmt.Println("start it: systemctl --user daemon-reload && systemctl --user enable --now tacit-dream.timer")
	}
	local := false
	if st, err := store.Default(); err == nil && st.Exists() {
		local = st.Local(context.Background())
	}
	if !local && !hasKey("dreamer") {
		fmt.Println("no dreamer@" + shortHost(mustHostname()) + " key here yet: each run refuses before any model call until `tacit keygen dreamer` is run and the key enrolled.")
	}
	fmt.Println("the dreamer needs claude and scrump (text profile) on the job's PATH.")
	if local {
		fmt.Println("Local mode: no dreamer enrollment key is needed")
	}
	if local {
		fmt.Println("Use tacit install curate for local curation and verification")
	} else {
		fmt.Println("Use tacit install curate on the machine that holds the curator key")
	}
	return 0
}

// followUnit is the systemd user unit for `tacit follow`. exec is already
// escaped for systemd (see unitQuote). Restart=always because the follower
// is a daemon that should outlive a crash, including a Go panic (exit 2). Only
// exitConfig (78, a bad invocation) is not restarted: it would repeat.
func followUnit(exec, storeEnv string) string {
	env := ""
	if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
		env += "Environment=" + unitQuote("TACIT_SETTINGS="+cfg, true) + "\n"
	}
	if storeEnv != "" {
		env += "Environment=" + unitQuote("TACIT_STORE="+storeEnv, true) + "\n"
	}
	return `[Unit]
Description=tacit follower: sync the store when the server's change feed reports a push

[Service]
` + env + `ExecStart=` + exec + ` follow
Restart=always
RestartSec=5
RestartPreventExitStatus=` + strconv.Itoa(exitConfig) + `

[Install]
WantedBy=default.target
`
}

// unitQuote makes v safe as a systemd word: % doubled, and wrapped in double
// quotes (with \ and " escaped) when it holds a space, tab, quote or
// backslash. force always quotes.
func unitQuote(v string, force bool) string {
	v = strings.ReplaceAll(v, "%", "%%")
	if force || strings.ContainsAny(v, " \t\"\\") {
		v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return v
}

// installFollow writes ~/.config/systemd/user/tacit-follow.service (under
// $XDG_CONFIG_HOME when set) with the running binary's path baked in, and
// returns the path. A store chosen through $TACIT_STORE at install time is
// carried into the unit.
func installFollow(exe string) (string, error) {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cfg = filepath.Join(home, ".config")
	}
	dir := filepath.Join(cfg, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tacit-follow.service")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(followUnit(unitQuote(exe, false), os.Getenv("TACIT_STORE"))), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// installHooks merges tacit's two hooks into a Claude-style hooks file
// (Claude Code's settings.json, Codex's hooks.json), keeping every other
// entry.
func installHooks(path, exe string) error { return installHooksFor(path, exe, runtime.GOOS) }

func installHooksFor(path, exe, goos string) error {
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, sub := range map[string]string{"SessionStart": "session-start", "UserPromptSubmit": "prompt"} {
		handler := map[string]any{"type": "command", "command": hookCommand(exe, sub, goos), "timeout": 15}
		if goos == "windows" && filepath.Base(path) == "settings.json" {
			args := []string{}
			if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
				args = append(args, "--config", cfg)
			}
			args = append(args, "hook", sub)
			handler["command"] = exe
			handler["args"] = args
		}
		entry := map[string]any{"hooks": []any{handler}}
		if event == "SessionStart" {
			entry["matcher"] = "startup" // not on resume, clear or compact
		}
		// Replace our entry where it is: Codex keys hook trust by position,
		// so moving it would make every later hook ask for trust again.
		var kept []any
		placed := false
		if list, ok := hooks[event].([]any); ok {
			for _, e := range list {
				switch {
				case !mentionsTacit(e):
					kept = append(kept, e)
				case !placed:
					kept = append(kept, entry)
					placed = true
				}
			}
		}
		if !placed {
			kept = append(kept, entry)
		}
		hooks[event] = kept
	}
	settings["hooks"] = hooks
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

//go:embed hermes/__init__.py hermes/plugin.yaml
var hermesPlugin embed.FS

// installHermes writes the provider plugin with the binary path baked in, so
// the gateway's PATH does not matter.
func installHermes(dir, exe string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"__init__.py", "plugin.yaml"} {
		b, err := hermesPlugin.ReadFile("hermes/" + name)
		if err != nil {
			return err
		}
		lit, _ := json.Marshal(exe) // a JSON string is a valid Python literal
		text := strings.ReplaceAll(string(b), "@TACIT_BIN@", string(lit))
		cfg, _ := json.Marshal(os.Getenv("TACIT_SETTINGS"))
		st, _ := store.Default()
		storePath := ""
		if st != nil {
			storePath = st.Dir
		}
		location, _ := json.Marshal(storePath)
		text = strings.ReplaceAll(strings.ReplaceAll(text, "@TACIT_SETTINGS@", string(cfg)), "@TACIT_STORE@", string(location))
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// mentionsTacit matches only our own hook command, not any entry that
// happens to contain the word.
func mentionsTacit(entry any) bool {
	b, _ := json.Marshal(entry)
	if strings.Contains(string(b), "tacit hook ") {
		return true
	}
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	hooks, _ := m["hooks"].([]any)
	for _, item := range hooks {
		h, _ := item.(map[string]any)
		cmd, _ := h["command"].(string)
		if i := strings.Index(cmd, "-EncodedCommand "); i >= 0 {
			if decoded, err := decodePowerShell(strings.TrimSpace(cmd[i+len("-EncodedCommand "):])); err == nil {
				cmd = decoded
			}
		}
		hasHook := strings.Contains(cmd, " hook ") || strings.Contains(cmd, "'hook' '")
		if args, ok := h["args"].([]any); ok {
			for _, a := range args {
				hasHook = hasHook || a == "hook"
			}
		}
		if strings.Contains(cmd, "tacit") && hasHook {
			return true
		}
	}
	return false
}

// findFactOrLocal is findFact, then this machine's local overlay (the notes
// harnesses wrote here on a read-only clone), for the read paths only.
func findFactOrLocal(storeDir, name string) (string, bool) {
	if p, ok := findFact(storeDir, name); ok {
		return p, true
	}
	return overlay.Find(overlay.Dir(), name)
}

func findFact(storeDir, name string) (string, bool) {
	var found string
	filepath.WalkDir(storeDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "keys") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == name+".md" {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, found != ""
}

func mcpArguments(harness string) string {
	if harness == "mcp" {
		harness = "codex"
	}
	args := []string{}
	if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
		args = append(args, "--config", cfg)
	}
	args = append(args, "mcp", "--harness", harness)
	b, _ := json.Marshal(args)
	return string(b)
}
