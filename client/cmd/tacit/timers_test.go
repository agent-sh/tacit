package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// timerLines returns the [Timer] section as KEY=VALUE lines in file order,
// without comments, so a comment that names a directive is not read as one.
func timerLines(text string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			in = line == "[Timer]"
			continue
		}
		if in {
			k, v, _ := strings.Cut(line, "=")
			out = append(out, strings.TrimSpace(k)+"="+strings.TrimSpace(v))
		}
	}
	return out
}

// No timer pairs Persistent= with a monotonic value. Persistent= is for
// OnCalendar= timers: a stamp file lets a run missed while the timer was not
// loaded run at start. systemd loads only the stamp's wall-clock time when a
// timer starts, so after a user manager restart a boot-relative value counted
// as spent and the unit-relative one had no base until its service had run
// once: the timer came back `active (elapsed)` with no next run. Every timer
// the client writes and every one it ships is checked: a timer added to either
// place is named here, so it is checked too.
func TestNoTimerPersistsAMonotonicValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TACIT_STORE", filepath.Join(home, "facts"))
	if _, err := installDreamSystemd(home, "/opt/tacit", "/home/u/.config/tacit/env"); err != nil {
		t.Fatal(err)
	}
	if _, err := installLocalCurate("linux", home, "/opt/tacit", "/home/u/.config/tacit/env"); err != nil {
		t.Fatal(err)
	}
	if code := installCurator("linux", home, "/opt/tacit", nil); code != 0 {
		t.Fatalf("install curate: %d", code)
	}
	for dir, want := range map[string][]string{
		filepath.Join(home, ".config", "systemd", "user"): {"tacit-curate-local.timer", "tacit-curate.timer", "tacit-dream-idle.timer", "tacit-dream.timer"},
		"../../deploy/systemd":                            {"tacit-curate.timer", "tacit-dream-idle.timer", "tacit-dream.timer", "tacit-store-fetch.timer"},
	} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.timer"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range paths {
			names = append(names, filepath.Base(p))
		}
		slices.Sort(names)
		if !slices.Equal(names, want) {
			t.Fatalf("%s holds %v, want %v", dir, names, want)
		}
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			lines := timerLines(string(b))
			if len(lines) == 0 {
				t.Fatalf("%s: no directive in [Timer]:\n%s", p, b)
			}
			var monotonic []string
			persistent := ""
			for _, l := range lines {
				k, v, _ := strings.Cut(l, "=")
				if strings.HasPrefix(k, "On") && (strings.HasSuffix(k, "Sec") || strings.HasSuffix(k, "USec")) {
					monotonic = append(monotonic, l)
				}
				if k == "Persistent" && !slices.Contains([]string{"0", "no", "off", "false"}, strings.ToLower(v)) {
					persistent = l
				}
			}
			if persistent != "" && len(monotonic) > 0 {
				t.Fatalf("%s: %s with %v:\n%s", p, persistent, monotonic, b)
			}
		}
	}
}

// The dreamer's timer and the store mirror's count their first run from when
// the user manager started, with the delays they had. A boot-relative delay is
// in the past at every session restart, which left nothing to wait for.
func TestDreamAndMirrorTimersCountFromTheManagerStart(t *testing.T) {
	for name, want := range map[string][]string{
		"tacit-dream.timer":       {"OnStartupSec=20min", "OnUnitActiveSec=4h"},
		"tacit-store-fetch.timer": {"OnStartupSec=2min", "OnUnitActiveSec=15min"},
	} {
		b, err := os.ReadFile(filepath.Join("../../deploy/systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := timerLines(string(b)); !slices.Equal(got, want) {
			t.Fatalf("%s: %v, want %v:\n%s", name, got, want, b)
		}
	}
}
