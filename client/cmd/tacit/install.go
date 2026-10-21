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
	"slices"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runInstall wires tacit into a harness. Idempotent: existing tacit
// entries are replaced.
//
//   - claude: SessionStart, UserPromptSubmit, PreToolUse and PreCompact hooks in ~/.claude/settings.json,
//     and the MCP server (tacit_tree, tacit_search, tacit_fact, tacit_add) in the user
//     scope of ~/.claude.json; --no-mcp installs the hooks only and removes
//     the server.
//   - codex: the same hooks in ~/.codex/hooks.json (Codex takes the same
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
//     the selected runner’s provider variables; and the idle job every 10
//     minutes (dream --idle 20m, then curate --quick). Prints the commands;
//     starts nothing.
//   - grok, pi, mcp: Grok ignores what start and prompt hooks print, so these
//     get the MCP server only; the snippet is printed.
func runInstall(args []string) int {
	if len(args) < 1 {
		usage()
	}
	target, rest := args[0], args[1:]
	if target != "dream" && target != "curate" && target != "claude" && len(rest) != 0 {
		usage()
	}
	exe, err := os.Executable()
	if err != nil {
		return 2
	}
	home, _ := os.UserHomeDir()
	goos := runtime.GOOS
	switch target {
	case "claude":
		return installClaude(home, exe, rest)
	case "codex":
		path := filepath.Join(home, ".codex", "hooks.json")
		if err := installHooks(path, exe); err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		fmt.Println("installed SessionStart, UserPromptSubmit, PreToolUse and PreCompact hooks in", path)
		fmt.Println("Codex skips new hooks until you trust them: run /hooks in Codex once.")
		fmt.Printf("MCP (~/.codex/config.toml):\n[mcp_servers.tacit]\ncommand = %q\nargs = %s\n", exe, mcpArguments("codex"))
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
	case "muse":
		var args []string
		_ = json.Unmarshal([]byte(mcpArguments("muse")), &args)
		b, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"tacit": map[string]any{"command": exe, "args": args}}}, "", "  ")
		fmt.Println("Merge into Muse's existing settings.json (keep its schema_version and other settings):")
		fmt.Println(string(b))
	case "agy":
		var args []string
		_ = json.Unmarshal([]byte(mcpArguments("agy")), &args)
		quote := shellQuote
		if goos == "windows" {
			quote = psQuote
		}
		parts := []string{"agy", "mcp", "add", "tacit", "--", quote(exe)}
		for _, arg := range args {
			parts = append(parts, quote(arg))
		}
		fmt.Println("Register Tacit's memory tools with agy:")
		fmt.Println(strings.Join(parts, " "))

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
	envFile := fs.String("env-file", defaultDreamEnv(goos, home), "KEY=VALUE file with the selected runner’s provider variables (for example the Bedrock ones); never copied into the job")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	// The dreamer writes: a read-only clone gets no dreamer, unless its local
	// layer is on: then the dreamer and a daily curate work on this machine's
	// overlay (local_layer.go), and the jobs carry the setting.
	localInstall := false
	if s, err := store.Default(); err == nil && s.Exists() && s.ReadOnly(context.Background()) {
		if !overlay.LayerOn() {
			fmt.Fprintln(os.Stderr, "install: no dreamer on a read-only clone:", store.ErrReadOnly)
			return 1
		}
		if !overlay.Outside(overlay.Dir(), s.Dir) {
			fmt.Fprintln(os.Stderr, "install:", overlay.ErrInside)
			return 1
		}
		localInstall = true
	}
	env, err := filepath.Abs(*envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 2
	}
	var paths []string
	if goos == "windows" {
		for _, job := range []string{"dream", "dream-idle"} {
			var p string
			if p, err = installWindowsJob(home, exe, job, env); err != nil {
				break
			}
			paths = append(paths, p)
		}
	} else if goos == "darwin" {
		path := servicePATH(home, exe, lookPath)
		for _, job := range []struct{ label, text string }{
			{launchdDreamLabel, dreamPlist(exe, env, os.Getenv("TACIT_STORE"), home, path)},
			{launchdIdleLabel, dreamIdlePlist(exe, env, os.Getenv("TACIT_STORE"), home, path)},
		} {
			var p string
			if p, err = installLaunchAgent(home, job.label, job.text); err != nil {
				break
			}
			paths = append(paths, p)
		}
	} else {
		paths, err = installDreamSystemd(home, exe, env)
	}
	if localInstall && err == nil {
		var more []string
		more, err = installLocalCurate(goos, home, exe, env)
		paths = append(paths, more...)
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
		fmt.Println("missing:", env, "- the dreamer fails until it exists. It holds KEY=VALUE lines for the selected runner (provider variables, for example CLAUDE_CODE_USE_BEDROCK=1 and AWS_REGION for Bedrock, and TACIT_DREAM_MODEL to name a full model id); keep it chmod 600. It may be empty when the selected runner is already logged in.")
	}
	if goos == "windows" {
		printWindowsJob(paths[0], "dream")
		printWindowsJob(paths[1], "dream-idle")
		if localInstall {
			printWindowsJob(paths[2], localCurateJob)
		}
	} else if goos == "darwin" {
		fmt.Println("start them (and after every reinstall):")
		labels := []string{launchdDreamLabel, launchdIdleLabel}
		if localInstall {
			labels = append(labels, launchdLocalCurateLabel)
		}
		for i, label := range labels {
			for _, l := range launchctlLines(label, paths[i]) {
				fmt.Println("  " + l)
			}
		}
		logs := []string{filepath.Join(launchdLogDir(home), "dream.log"), filepath.Join(launchdLogDir(home), "dream-idle.log")}
		if localInstall {
			logs = append(logs, filepath.Join(launchdLogDir(home), "curate-local.log"))
		}
		fmt.Println("logs:", strings.Join(logs, " "))
	} else if localInstall {
		fmt.Println("start them: systemctl --user daemon-reload && systemctl --user enable --now tacit-dream.timer tacit-dream-idle.timer tacit-curate-local.timer")
	} else {
		fmt.Println("start them: systemctl --user daemon-reload && systemctl --user enable --now tacit-dream.timer tacit-dream-idle.timer")
	}
	if localInstall {
		fmt.Println("local layer: this read-only clone dreams and curates its own sessions into " + overlay.Dir() + ", which never leaves this machine; nothing is written to the clone or pushed. The jobs carry the setting (TACIT_LOCAL_LAYER, TACIT_LOCAL): to turn the layer off, unload and remove them.")
		fmt.Println("the dreamer and the curator need their selected runners and scrump (text profile) on the job's PATH.")
		return 0
	}
	fmt.Println("the idle job dreams every 10 minutes the sessions quiet for 20 minutes or just compacted, then runs a quick curate (on the machine with the curator key; elsewhere it does nothing).")
	local := false
	if st, err := store.Default(); err == nil && st.Exists() {
		local = st.Local(context.Background())
	}
	if !local && !hasKey("dreamer") {
		fmt.Println("no dreamer@" + shortHost(mustHostname()) + " key here yet: each run refuses before any model call until `tacit keygen dreamer` is run and the key enrolled.")
	}
	fmt.Println("the dreamer needs its selected runner and scrump (text profile) on the job's PATH.")
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

// installClaude implements `tacit install claude [--no-mcp]`: the two hooks,
// and tacit's MCP server in Claude Code's user scope, so a session can search
// the store and read a fact in full. The start block then names the tool for
// detail (`hook session-start --mcp`). The tools' definitions ride in every
// session's prompt; --no-mcp leaves them out (docs/client.md has the cost).
func installClaude(home, exe string, args []string) int {
	fs := flag.NewFlagSet("install claude", flag.ContinueOnError)
	noMCP := fs.Bool("no-mcp", false, "install the hooks only, and remove tacit's MCP server from Claude Code's user config")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	cfg := claudeUserConfig(home)
	var entry map[string]any
	if !*noMCP {
		entry = claudeMCPEntry(exe)
	}
	// The server first: when it cannot be written, the hooks keep pointing
	// at `tacit fact`.
	changed, err := setClaudeMCP(cfg, entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	path := filepath.Join(home, ".claude", "settings.json")
	var extra []string
	if !*noMCP {
		extra = []string{"--mcp"}
	}
	if err := installHooksWith(path, exe, runtime.GOOS, extra); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Println("installed SessionStart, UserPromptSubmit, PreToolUse and PreCompact hooks in", path)
	switch {
	case *noMCP && changed:
		fmt.Println("removed the tacit MCP server from", cfg, "(the file as it was:", claudeConfigBackup(cfg)+")")
	case *noMCP:
		fmt.Println("no tacit MCP server registered (--no-mcp)")
	case changed:
		fmt.Println("registered the tacit MCP server (tacit_tree, tacit_search, tacit_fact, tacit_add) for Claude Code in", cfg, "(user scope; the file as it was:", claudeConfigBackup(cfg)+")")
	default:
		fmt.Println("the tacit MCP server is already registered in", cfg)
	}
	if !*noMCP {
		fmt.Println("new sessions get it; `claude mcp list` shows it. Hooks only: tacit install claude --no-mcp")
	}
	return 0
}

// installHooks merges tacit's two hooks into a Claude-style hooks file
// (Claude Code's settings.json, Codex's hooks.json), keeping every other
// entry.
func installHooks(path, exe string) error { return installHooksFor(path, exe, runtime.GOOS) }

func installHooksFor(path, exe, goos string) error { return installHooksWith(path, exe, goos, nil) }

// installHooksWith is installHooksFor with extra arguments for the
// SessionStart hook (--mcp: the harness has tacit's MCP server).
//
// The entries: SessionStart for a real start (matcher startup: not on
// resume, clear or compact; after a compaction the summary carries what the
// session was given); UserPromptSubmit; PreToolUse for the edit and shell
// tools (toolPushTools); PreCompact, which starts the dream
// of the session about to compact, so its facts reach the store for other
// and later sessions. Claude Code and Codex 0.162 take the same events and
// input. A tacit entry under a matcher none of these has (the compact entry
// an earlier build of this branch wrote) is removed.
func installHooksWith(path, exe, goos string, startArgs []string) error {
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
	type spec struct {
		matcher, sub string
		extra        []string
	}
	events := map[string][]spec{
		"SessionStart":     {{"startup", "session-start", startArgs}},
		"UserPromptSubmit": {{"", "prompt", nil}},
		"PreCompact":       {{"", "pre-compact", nil}},
		// PreToolUse: before an edit or a shell command, the one rule,
		// decision or past mistake whose applies_to names that file or
		// command (preTool). It never blocks the call.
		"PreToolUse": {{toolPushTools, "pre-tool", nil}},
	}
	for event, specs := range events {
		handlers := make([]map[string]any, len(specs))
		for i, sp := range specs {
			handler := map[string]any{"type": "command", "command": hookCommand(exe, sp.sub, goos, sp.extra...), "timeout": 15}
			if goos == "windows" && filepath.Base(path) == "settings.json" {
				args := []string{}
				if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
					args = append(args, "--config", cfg)
				}
				args = append(args, "hook", sp.sub)
				args = append(args, sp.extra...)
				handler["command"] = exe
				handler["args"] = args
			}
			handlers[i] = handler
		}
		// Replace our entry where it is: Codex keys hook trust by position,
		// so moving it would make every later hook ask for trust again. Our
		// entries are told apart by their matcher.
		var kept []any
		placed := make([]bool, len(specs))
		if list, ok := hooks[event].([]any); ok {
			for _, e := range list {
				m, ok := e.(map[string]any)
				if !ok {
					kept = append(kept, e)
					continue
				}
				children, ok := m["hooks"].([]any)
				if !ok {
					kept = append(kept, e)
					continue
				}
				matcher, _ := m["matcher"].(string)
				var remaining []any
				for _, child := range children {
					if !mentionsTacit(map[string]any{"hooks": []any{child}}) {
						remaining = append(remaining, child)
						continue
					}
					for i, sp := range specs {
						if !placed[i] && (sp.matcher == matcher || (sp.sub == "pre-tool" && len(children) == 1 && slices.Contains(legacyToolPushTools, matcher))) {
							remaining = append(remaining, handlers[i])
							placed[i] = true
							if sp.matcher != matcher {
								m["matcher"] = sp.matcher // an earlier release's matcher, widened in place (an entry of ours alone)
							}
							break
						}
					}
				}
				if len(remaining) > 0 {
					m["hooks"] = remaining
					kept = append(kept, m)
				}
			}
		}
		for i, sp := range specs {
			if placed[i] {
				continue
			}
			entry := map[string]any{"hooks": []any{handlers[i]}}
			if sp.matcher != "" {
				entry["matcher"] = sp.matcher
			}
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
