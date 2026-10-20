package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// install dream on a read-only clone with the local layer writes the dream,
// the idle and the daily local curate jobs, each carrying the layer.
func TestInstallDreamOnTheLocalLayer(t *testing.T) {
	r := newLocalRig(t)
	env := filepath.Join(r.root, "env")
	os.WriteFile(env, nil, 0o600)
	home := filepath.Join(r.root, "mac")
	if code := installDream("darwin", home, "/opt/tacit/tacit", []string{"--env-file", env}); code != 0 {
		t.Fatalf("install dream exit %d", code)
	}
	agents := filepath.Join(home, "Library", "LaunchAgents")
	for _, label := range []string{launchdDreamLabel, launchdIdleLabel, launchdLocalCurateLabel} {
		b, err := os.ReadFile(filepath.Join(agents, label+".plist"))
		if err != nil {
			t.Fatal(err)
		}
		m := readPlist(t, string(b))
		envs, _ := m["EnvironmentVariables"].(map[string]any)
		if envs["TACIT_LOCAL_LAYER"] != "on" || envs["TACIT_LOCAL"] != r.local {
			t.Fatalf("%s carries %v", label, envs)
		}
		if label == launchdLocalCurateLabel {
			args, _ := json.Marshal(m["ProgramArguments"])
			if string(args) != `["/opt/tacit/tacit","curate","--env-file","`+env+`"]` || m["StartInterval"] != float64(localCurateInterval) {
				t.Fatalf("local curate job: %s %v", args, m["StartInterval"])
			}
		}
	}
	linux := filepath.Join(r.root, "linux")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(linux, ".config"))
	if code := installDream("linux", linux, "/opt/tacit/tacit", []string{"--env-file", env}); code != 0 {
		t.Fatalf("linux install exit %d", code)
	}
	units := filepath.Join(linux, ".config", "systemd", "user")
	var names []string
	entries, _ := os.ReadDir(units)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, " ") != "tacit-curate-local.service tacit-curate-local.timer tacit-dream-idle.service tacit-dream-idle.timer tacit-dream.service tacit-dream.timer" {
		t.Fatalf("units: %v", names)
	}
	svc, _ := os.ReadFile(filepath.Join(units, "tacit-curate-local.service"))
	if !strings.Contains(string(svc), "ExecStart=/opt/tacit/tacit curate\n") || !strings.Contains(string(svc), "Environment=\"TACIT_LOCAL_LAYER=on\"\n") {
		t.Fatalf("tacit-curate-local.service:\n%s", svc)
	}
	win := filepath.Join(r.root, "win")
	if code := installDream("windows", win, `C:\tacit\tacit.exe`, []string{"--env-file", env}); code != 0 {
		t.Fatalf("windows install exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(win, ".config", "tacit", "tasks", "tacit-curate-local.xml")); err != nil {
		t.Fatal(err)
	}
	r.untouched()
}
