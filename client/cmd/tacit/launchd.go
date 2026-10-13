package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// macOS runs tacit's background jobs as launchd user agents, the way Linux
// runs them as systemd user units. Same jobs, same binary, same semantics:
//
//   - ai.tacit.follow: `tacit follow --launchd`, started at load and kept
//     alive. KeepAlive restarts it after any failing exit (SuccessfulExit
//     false) and any crash, every ThrottleInterval (the unit's RestartSec).
//     launchd cannot exempt one exit code the way RestartPreventExitStatus=78
//     does, so --launchd turns a bad invocation into exit 0, which is not
//     restarted.
//   - ai.tacit.dream: `tacit dream --env-file F` every 4 h (StartInterval; a
//     run missed while the Mac sleeps runs on wake). launchd has no
//     EnvironmentFile, so the dreamer reads the file itself; no secret is in
//     the plist.
//
// Both run with ProcessType Background (the system's throttled CPU and I/O
// class), a nice value and low-priority I/O, and log to ~/Library/Logs/tacit/.
// The curator and the verifier stay on the curator's machine: no agent is
// written for them.
const (
	launchdFollowLabel = "ai.tacit.follow"
	launchdDreamLabel  = "ai.tacit.dream"
	dreamInterval      = 4 * 60 * 60 // seconds, the timer's OnUnitActiveSec=4h
	followThrottle     = 5           // seconds, the unit's RestartSec=5
)

// launchAgentsDir is ~/Library/LaunchAgents.
func launchAgentsDir(home string) string { return filepath.Join(home, "Library", "LaunchAgents") }

// launchdLogDir is ~/Library/Logs/tacit. launchd does not create it.
func launchdLogDir(home string) string { return filepath.Join(home, "Library", "Logs", "tacit") }

// servicePATH is the PATH a background job gets. launchd starts agents with
// /usr/bin:/bin:/usr/sbin:/sbin only, and the jobs run git, and the dreamer
// also scrump and claude: the directories where those were found at install
// time come first, after the binary's own directory and ~/.local/bin.
func servicePATH(home, exe string, look func(string) (string, error)) string {
	dirs := []string{filepath.Dir(exe), filepath.Join(home, ".local", "bin")}
	for _, tool := range []string{"git", "scrump", "claude"} {
		if p, err := look(tool); err == nil && filepath.IsAbs(p) {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	dirs = append(dirs, filepath.Join(home, ".cargo", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, ":")
}

// plist builds an XML property list. Keys are written in the order given.
type plist struct{ b bytes.Buffer }

func newPlist() *plist {
	p := &plist{}
	p.b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	return p
}

func plistEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (p *plist) key(indent, k string) {
	p.b.WriteString(indent + "<key>" + plistEscape(k) + "</key>\n")
}

func (p *plist) str(k, v string) {
	p.key("\t", k)
	p.b.WriteString("\t<string>" + plistEscape(v) + "</string>\n")
}

func (p *plist) boolean(k string, v bool) {
	p.key("\t", k)
	if v {
		p.b.WriteString("\t<true/>\n")
	} else {
		p.b.WriteString("\t<false/>\n")
	}
}

func (p *plist) integer(k string, v int) {
	p.key("\t", k)
	p.b.WriteString("\t<integer>" + strconv.Itoa(v) + "</integer>\n")
}

func (p *plist) array(k string, vs ...string) {
	p.key("\t", k)
	p.b.WriteString("\t<array>\n")
	for _, v := range vs {
		p.b.WriteString("\t\t<string>" + plistEscape(v) + "</string>\n")
	}
	p.b.WriteString("\t</array>\n")
}

// dict writes a dictionary of strings, in the order of kv (key, value, ...).
func (p *plist) dict(k string, kv ...string) {
	p.key("\t", k)
	p.b.WriteString("\t<dict>\n")
	for i := 0; i+1 < len(kv); i += 2 {
		p.key("\t\t", kv[i])
		p.b.WriteString("\t\t<string>" + plistEscape(kv[i+1]) + "</string>\n")
	}
	p.b.WriteString("\t</dict>\n")
}

// raw writes a key with a value already in plist XML.
func (p *plist) raw(k, value string) {
	p.key("\t", k)
	p.b.WriteString(value)
}

func (p *plist) String() string { return p.b.String() + "</dict>\n</plist>\n" }

// jobEnv is the environment of a background job: PATH, and TACIT_STORE when
// a store other than the default was chosen at install time.
func jobEnv(path, storeEnv string) []string {
	kv := []string{"PATH", path}
	if storeEnv != "" {
		kv = append(kv, "TACIT_STORE", storeEnv)
	}
	if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
		kv = append(kv, "TACIT_SETTINGS", cfg)
	}
	return kv
}

// followPlist is ~/Library/LaunchAgents/ai.tacit.follow.plist.
func followPlist(exe, storeEnv, home, path string) string {
	p := newPlist()
	p.str("Label", launchdFollowLabel)
	log := filepath.Join(launchdLogDir(home), "follow.log")
	// The follower writes its own capped log (--log); launchd's file gets only
	// what happens before that opens and a panic, so it stays small unrotated.
	p.array("ProgramArguments", exe, "follow", "--launchd", "--log", log)
	p.dict("EnvironmentVariables", jobEnv(path, storeEnv)...)
	p.boolean("RunAtLoad", true)
	p.raw("KeepAlive", "\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t\t<key>Crashed</key>\n\t\t<true/>\n\t</dict>\n")
	p.integer("ThrottleInterval", followThrottle)
	p.str("ProcessType", "Background")
	p.integer("Nice", 10)
	p.boolean("LowPriorityIO", true)
	errLog := filepath.Join(launchdLogDir(home), "follow.err.log")
	p.str("StandardOutPath", errLog)
	p.str("StandardErrorPath", errLog)
	return p.String()
}

// dreamPlist is ~/Library/LaunchAgents/ai.tacit.dream.plist.
func dreamPlist(exe, envFile, storeEnv, home, path string) string {
	p := newPlist()
	p.str("Label", launchdDreamLabel)
	p.array("ProgramArguments", exe, "dream", "--env-file", envFile)
	p.dict("EnvironmentVariables", jobEnv(path, storeEnv)...)
	p.integer("StartInterval", dreamInterval)
	p.boolean("RunAtLoad", false)
	p.str("ProcessType", "Background")
	p.integer("Nice", 19)
	p.boolean("LowPriorityIO", true)
	log := filepath.Join(launchdLogDir(home), "dream.log")
	p.str("StandardOutPath", log)
	p.str("StandardErrorPath", log)
	return p.String()
}

// writeFileMode writes data to path with mode, through a temporary file and a rename.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// installLaunchAgent writes ~/Library/LaunchAgents/<label>.plist and the log
// directory, and returns the plist's path. It loads nothing.
func installLaunchAgent(home, label, content string) (string, error) {
	if err := os.MkdirAll(launchdLogDir(home), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(launchAgentsDir(home), label+".plist")
	return path, writeFileMode(path, []byte(content), 0o644)
}

// launchctlLines are the commands that (re)load an agent: bootout first, so
// a second install replaces the running job instead of failing.
func launchctlLines(label, path string) []string {
	return []string{
		"launchctl bootout gui/$(id -u)/" + label + " 2>/dev/null; launchctl bootstrap gui/$(id -u) " + shellQuote(path),
		"check it: launchctl print gui/$(id -u)/" + label + " | grep -E 'state|last exit'",
	}
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dreamUnit is the systemd user service for `tacit dream` on Linux; exec is
// already escaped for systemd, envFile is the KEY=VALUE environment file
// (systemd specifiers such as %h expand in it).
func dreamUnit(exec, envFile string) string {
	extra := ""
	for _, key := range []string{"TACIT_STORE", "TACIT_SETTINGS"} {
		if value := os.Getenv(key); value != "" {
			extra += "Environment=" + unitQuote(key+"="+value, true) + "\n"
		}
	}
	return `[Unit]
Description=tacit dreamer: transcript deltas to inbox facts

[Service]
EnvironmentFile=` + envFile + `
Type=oneshot
` + extra + `ExecStart=` + exec + ` dream
`
}

// dreamTimer runs the dreamer every 4 hours.
const dreamTimer = `[Unit]
Description=tacit dreamer every 4 hours

[Timer]
OnBootSec=20min
OnUnitActiveSec=4h
Persistent=true

[Install]
WantedBy=timers.target
`

// defaultDreamEnv is the KEY=VALUE file with what claude -p needs (provider
// variables such as the Bedrock ones, and the TACIT_*_MODEL overrides):
// ~/.config/tacit/env on every OS.
func defaultDreamEnv(_, home string) string {
	return filepath.Join(home, ".config", "tacit", "env")
}

// systemdUserDir is ~/.config/systemd/user (under $XDG_CONFIG_HOME when set).
func systemdUserDir(home string) string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "systemd", "user")
}

// installDreamSystemd writes tacit-dream.service and tacit-dream.timer.
func installDreamSystemd(home, exe, envFile string) ([]string, error) {
	dir := systemdUserDir(home)
	files := []struct{ name, data string }{
		{"tacit-dream.service", dreamUnit(unitQuote(exe, false), unitQuote(envFile, false))},
		{"tacit-dream.timer", dreamTimer},
	}
	var out []string
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := writeFileMode(p, []byte(f.data), 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}

// lookPath is exec.LookPath; a variable so tests see a fixed PATH.
var lookPath = exec.LookPath
