package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Golden files: TACIT_UPDATE_GOLDEN=1 go test ./cmd/tacit -run Golden rewrites them.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "macos", name)
	if os.Getenv("TACIT_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s drifted (TACIT_UPDATE_GOLDEN=1 rewrites it):\n%s\n---\n%s", path, got, want)
	}
}

// macLook is what LookPath finds on a typical Apple Silicon Mac.
func macLook(tool string) (string, error) {
	switch tool {
	case "git":
		return "/usr/bin/git", nil
	case "scrump":
		return "/Users/someone/.local/bin/scrump", nil
	case "claude":
		return "/opt/homebrew/bin/claude", nil
	}
	return "", errors.New("not found")
}

const (
	macHome = "/Users/someone"
	macExe  = "/Users/someone/.local/bin/tacit"
)

func TestServicePATHOrderAndDedup(t *testing.T) {
	got := servicePATH(macHome, macExe, macLook)
	want := "/Users/someone/.local/bin:/usr/bin:/opt/homebrew/bin:/Users/someone/.cargo/bin:/usr/local/bin:/bin:/usr/sbin:/sbin"
	if got != want {
		t.Fatalf("PATH\n got %s\nwant %s", got, want)
	}
	none := servicePATH(macHome, "/opt/t/tacit", func(string) (string, error) { return "", errors.New("no") })
	if !strings.HasPrefix(none, "/opt/t:/Users/someone/.local/bin:/Users/someone/.cargo/bin:/opt/homebrew/bin:") {
		t.Fatalf("PATH without tools: %s", none)
	}
}

func TestFollowPlistGolden(t *testing.T) {
	golden(t, "ai.tacit.follow.plist", followPlist(macExe, "", macHome, servicePATH(macHome, macExe, macLook)))
}

func TestDreamPlistGolden(t *testing.T) {
	golden(t, "ai.tacit.dream.plist", dreamPlist(macExe, "/Users/someone/.config/tacit/env", "", macHome, servicePATH(macHome, macExe, macLook)))
}

func TestInfoPlistGolden(t *testing.T) {
	golden(t, "Info.plist", macInfoPlist("", servicePATH(macHome, filepath.Join(macHome, "Applications", "tacit.app", "Contents", "MacOS", "tacit-app"), macLook)))
}

// readPlist parses a property list the way macOS does (Python's plistlib,
// a strict reader of Apple's format, stands in for plutil -lint on Linux),
// after a well-formedness check with encoding/xml.
func readPlist(t *testing.T, text string) map[string]any {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = true
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, text)
		}
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; plist structure not checked")
	}
	cmd := exec.Command(py, "-I", "-c", `import json, plistlib, sys
print(json.dumps(plistlib.loads(sys.stdin.buffer.read(), fmt=plistlib.FMT_XML)))`)
	cmd.Stdin = strings.NewReader(text)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("plistlib refused it: %v\n%s\n%s", err, stderr.String(), text)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFollowPlistIsAValidKeptAliveAgent(t *testing.T) {
	m := readPlist(t, followPlist("/Users/a b/bin/tacit & co", "/Users/a b/store<1>", macHome, "/usr/bin:/bin"))
	if m["Label"] != "ai.tacit.follow" || m["RunAtLoad"] != true || m["ProcessType"] != "Background" || m["LowPriorityIO"] != true {
		t.Fatalf("plist: %v", m)
	}
	args, _ := m["ProgramArguments"].([]any)
	if len(args) != 5 || args[0] != "/Users/a b/bin/tacit & co" || args[1] != "follow" || args[2] != "--launchd" || args[3] != "--log" || args[4] != "/Users/someone/Library/Logs/tacit/follow.log" {
		t.Fatalf("ProgramArguments %v", args)
	}
	ka, _ := m["KeepAlive"].(map[string]any)
	// Restart on any failing exit and any crash, never after exit 0, which is
	// what --launchd makes of the config exit 78.
	if ka["SuccessfulExit"] != false || ka["Crashed"] != true || len(ka) != 2 {
		t.Fatalf("KeepAlive %v", ka)
	}
	if m["ThrottleInterval"] != float64(followThrottle) {
		t.Fatalf("ThrottleInterval %v", m["ThrottleInterval"])
	}
	env, _ := m["EnvironmentVariables"].(map[string]any)
	if env["TACIT_STORE"] != "/Users/a b/store<1>" || env["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("env %v", env)
	}
	if m["StandardErrorPath"] != "/Users/someone/Library/Logs/tacit/follow.err.log" {
		t.Fatalf("log %v", m["StandardErrorPath"])
	}
}

func TestDreamPlistRunsEveryFourHoursWithTheEnvFile(t *testing.T) {
	m := readPlist(t, dreamPlist(macExe, "/Users/someone/.config/tacit/env", "", macHome, "/usr/bin"))
	if m["Label"] != "ai.tacit.dream" || m["StartInterval"] != float64(4*60*60) || m["RunAtLoad"] != false {
		t.Fatalf("plist: %v", m)
	}
	if _, ok := m["KeepAlive"]; ok {
		t.Fatal("the dreamer is a periodic job, not kept alive")
	}
	args, _ := m["ProgramArguments"].([]any)
	if len(args) != 4 || args[1] != "dream" || args[2] != "--env-file" || args[3] != "/Users/someone/.config/tacit/env" {
		t.Fatalf("ProgramArguments %v", args)
	}
	env, _ := m["EnvironmentVariables"].(map[string]any)
	if _, ok := env["TACIT_STORE"]; ok || len(env) != 1 {
		t.Fatalf("env %v", env)
	}
}

func TestInfoPlistNamesTheBundle(t *testing.T) {
	m := readPlist(t, macInfoPlist("/Users/someone/other store", "/usr/bin"))
	for k, want := range map[string]any{
		"CFBundleExecutable":  "tacit-app",
		"CFBundleIconFile":    "tacit",
		"CFBundleIdentifier":  "ai.tacit.app",
		"CFBundlePackageType": "APPL",
		"CFBundleName":        "tacit",
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	env, _ := m["LSEnvironment"].(map[string]any)
	if env["TACIT_STORE"] != "/Users/someone/other store" || env["PATH"] != "/usr/bin" {
		t.Fatalf("LSEnvironment %v", env)
	}
}

// launchd cannot exempt one exit code from KeepAlive: under --launchd a bad
// invocation exits 0 (not restarted); without it, 78 as before.
func TestFollowUnderLaunchdDoesNotLoopOnABadInvocation(t *testing.T) {
	if got := runFollow([]string{"--launchd", "--no-such-flag"}); got != 0 {
		t.Errorf("--launchd with a bad flag exits %d, want 0", got)
	}
	if got := runFollow([]string{"--no-such-flag", "--launchd"}); got != 0 {
		t.Errorf("a bad flag before --launchd exits %d, want 0", got)
	}
	if got := runFollow([]string{"--launchd", "--as", "Not A Slug"}); got != 0 {
		t.Errorf("--launchd with a bad --as exits %d, want 0", got)
	}
	if got := runFollow([]string{"--no-such-flag"}); got != exitConfig {
		t.Errorf("a bad flag exits %d, want %d", got, exitConfig)
	}
}

func TestInstallLaunchAgentWritesPlistAndLogDir(t *testing.T) {
	home := t.TempDir()
	text := followPlist(macExe, "", home, "/usr/bin")
	path, err := installLaunchAgent(home, launchdFollowLabel, text)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "Library", "LaunchAgents", "ai.tacit.follow.plist") {
		t.Fatalf("path %s", path)
	}
	if b, _ := os.ReadFile(path); string(b) != text {
		t.Fatal("plist content")
	}
	if st, err := os.Stat(filepath.Join(home, "Library", "Logs", "tacit")); err != nil || !st.IsDir() {
		t.Fatal("log directory not created")
	}
	if _, err := installLaunchAgent(home, launchdFollowLabel, text); err != nil {
		t.Fatal("second install:", err)
	}
	if lines := launchctlLines(launchdFollowLabel, "/Users/a b/x.plist"); !strings.Contains(lines[0], "bootout gui/$(id -u)/ai.tacit.follow") || !strings.HasSuffix(lines[0], "bootstrap gui/$(id -u) '/Users/a b/x.plist'") {
		t.Fatalf("launchctl line %q", lines[0])
	}
}

// The dreamer's unit and timer that `tacit install dream` writes on Linux are
// the ones in deploy/.
func TestDreamUnitMatchesTheDeployCopy(t *testing.T) {
	svc, err := os.ReadFile("../../deploy/systemd/tacit-dream.service")
	if err != nil {
		t.Fatal(err)
	}
	if got := dreamUnit("%h/.local/bin/tacit", "%h/.config/tacit/env"); got != string(svc) {
		t.Fatalf("deploy copy drifted from dreamUnit:\n%s\n---\n%s", got, svc)
	}
	timer, err := os.ReadFile("../../deploy/systemd/tacit-dream.timer")
	if err != nil {
		t.Fatal(err)
	}
	if dreamTimer != string(timer) {
		t.Fatalf("deploy copy drifted from dreamTimer:\n%s\n---\n%s", dreamTimer, timer)
	}
}

func TestInstallDreamSystemdWritesBoth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	paths, err := installDreamSystemd(home, "/opt/my tools/tacit", "/home/u/.config/tacit/env")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != filepath.Join(home, ".config", "systemd", "user", "tacit-dream.service") {
		t.Fatalf("paths %v", paths)
	}
	b, _ := os.ReadFile(paths[0])
	if !strings.Contains(string(b), `ExecStart="/opt/my tools/tacit" dream`+"\n") || !strings.Contains(string(b), "EnvironmentFile=/home/u/.config/tacit/env\n") {
		t.Fatalf("unit:\n%s", b)
	}
}

func TestDefaultDreamEnv(t *testing.T) {
	if got := defaultDreamEnv("darwin", macHome); got != "/Users/someone/.config/tacit/env" {
		t.Errorf("darwin: %s", got)
	}
	if got := defaultDreamEnv("linux", "/home/u"); got != "/home/u/.config/tacit/env" {
		t.Errorf("linux: %s", got)
	}
}

// The env file sets what claude -p needs; a bad line is named by number and
// its value never reaches the error.
func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("# bedrock\n\nexport TACIT_T_A=1\nTACIT_T_B=\"two words\"\r\nTACIT_T_C='x=y'\nTACIT_T_D=\n"), 0o600)
	for _, k := range []string{"TACIT_T_A", "TACIT_T_B", "TACIT_T_C", "TACIT_T_D"} {
		t.Setenv(k, "unset")
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"TACIT_T_A": "1", "TACIT_T_B": "two words", "TACIT_T_C": "x=y", "TACIT_T_D": ""} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	os.WriteFile(path, []byte("OK=1\nsecret-value-here\n"), 0o600)
	err := loadEnvFile(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "secret-value-here") {
		t.Fatalf("bad line: %v", err)
	}
	if err := loadEnvFile(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("a missing file loads")
	}
}

// On launchd the env file is the only place a dream model can be set: its
// TACIT_DREAM_MODEL line wins over the flag default read before the file,
// and an explicit --model wins over the file.
func TestDreamModelFromTheEnvFile(t *testing.T) {
	t.Setenv("TACIT_DREAM_MODEL", "")
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("TACIT_DREAM_MODEL=provider.haiku-id\n"), 0o600)
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := dreamModel("haiku", false); got != "provider.haiku-id" {
		t.Errorf("default flag: %q", got)
	}
	if got := dreamModel("opus", true); got != "opus" {
		t.Errorf("explicit --model: %q", got)
	}
}

func TestInstallMacAppWritesTheBundle(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(t.TempDir(), "tacit-app")
	os.WriteFile(exe, []byte("binary"), 0o755)
	var signed []string
	sign := func(app string) error { signed = append(signed, app); return nil }
	paths, err := installMacApp(home, exe, "", "/usr/bin", []byte("icns"), sign)
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "Applications", "tacit.app")
	want := []string{
		filepath.Join(app, "Contents", "Info.plist"),
		filepath.Join(app, "Contents", "Resources", "tacit.icns"),
		filepath.Join(app, "Contents", "MacOS", "tacit-app"),
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paths %v", paths)
	}
	if st, err := os.Stat(want[2]); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("executable: %v %v", st, err)
	}
	if b, _ := os.ReadFile(want[2]); string(b) != "binary" {
		t.Fatal("binary not copied")
	}
	if b, _ := os.ReadFile(want[0]); string(b) != macInfoPlist("", "/usr/bin") {
		t.Fatal("Info.plist")
	}
	if len(signed) != 1 || signed[0] != app {
		t.Fatalf("signed %v", signed)
	}
	// Run from inside the bundle: the copy is not copied onto itself.
	if _, err := installMacApp(home, want[2], "", "/usr/bin", []byte("icns"), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(want[2]); string(b) != "binary" {
		t.Fatal("reinstall from the bundle broke the binary")
	}
	if _, err := installMacApp(home, exe, "", "/usr/bin", nil, nil); err == nil {
		t.Fatal("a build without the icon installs")
	}
}

// The committed icon is an icns container that holds the 1024 px image.
func TestCommittedICNS(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("macos", "tacit.icns"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || string(b[:4]) != "icns" || int(b[4])<<24|int(b[5])<<16|int(b[6])<<8|int(b[7]) != len(b) {
		t.Fatal("not an icns container of its stated length")
	}
	if !bytes.Contains(b, []byte("ic10")) || !bytes.Contains(b, []byte("ic07")) {
		t.Fatal("icns lacks the 1024 or 128 px image")
	}
}

func TestLaunchedFromBundle(t *testing.T) {
	if !launchedFromBundle("/Users/someone/Applications/tacit.app/Contents/MacOS/tacit-app") {
		t.Error("bundle path not recognized")
	}
	if launchedFromBundle("/Users/someone/.local/bin/tacit-app") || launchedFromBundle("/home/u/.local/bin/tacit") {
		t.Error("a plain binary reads as a bundle")
	}
}

func TestLoadEnvFileWindowsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.env")
	t.Setenv("TACIT_BOM_TEST", "unset")
	os.WriteFile(path, []byte("\xef\xbb\xbfTACIT_BOM_TEST=works\r\n"), 0o600)
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TACIT_BOM_TEST"); got != "works" {
		t.Fatalf("BOM-prefixed key was not read: %q", got)
	}
}
