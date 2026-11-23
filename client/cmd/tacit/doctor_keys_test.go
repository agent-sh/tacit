package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctor fails when this machine runs a role's job and that writer cannot
// write here, the case of a host name change (every key under the old
// name), and names the keys it holds under other names; a machine that runs
// no job, or whose store needs no key, gets no failure.
func TestDoctorKeyChecks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	keys := filepath.Join(home, "keys")
	touch(t, filepath.Join(keys, "dreamer@oldbox.key"))
	touch(t, filepath.Join(keys, "curator@oldbox.key"))
	noKey := func(writer string) error { return errors.New("no " + writer + "@newbox key on this machine") }
	canWrite := func(string) error { return nil }

	if c := keyChecks(home, "linux", keys, "newbox", noKey); len(c) != 0 {
		t.Fatalf("a machine that runs no job and holds no harness key: %+v", c)
	}
	wants := filepath.Join(home, ".config", "systemd", "user", "timers.target.wants")
	touch(t, filepath.Join(wants, "tacit-dream-idle.timer"))
	c := keyChecks(home, "linux", keys, "newbox", noKey)
	if len(c) != 1 || c[0].Name != "dreamer key" || c[0].Status != "fail" ||
		!strings.Contains(c[0].Detail, "dreamer@newbox") || !strings.Contains(c[0].Detail, "dreamer@oldbox") || !strings.Contains(c[0].Detail, "host name change") {
		t.Fatalf("dream timer, no key for this host name: %+v", c)
	}
	if c := keyChecks(home, "linux", keys, "newbox", canWrite); len(c) != 1 || c[0].Status != "ok" {
		t.Fatalf("dream timer, a store that takes the write: %+v", c)
	}
	touch(t, filepath.Join(wants, "tacit-curate.timer"))
	if c := keyChecks(home, "linux", keys, "newbox", noKey); len(c) != 2 || c[1].Name != "curator key" || c[1].Status != "fail" {
		t.Fatalf("curate timer: %+v", c)
	}
	// A unit file not enabled runs nothing.
	os.RemoveAll(wants)
	touch(t, filepath.Join(home, ".config", "systemd", "user", "tacit-dream.timer"))
	if c := keyChecks(home, "linux", keys, "newbox", noKey); len(c) != 0 {
		t.Fatalf("a disabled timer: %+v", c)
	}
	// macOS: the launch agent's plist.
	touch(t, filepath.Join(home, "Library", "LaunchAgents", "ai.tacit.dream.plist"))
	if c := keyChecks(home, "darwin", keys, "newbox", noKey); len(c) != 1 || c[0].Name != "dreamer key" || c[0].Status != "fail" {
		t.Fatalf("launchd dream agent: %+v", c)
	}

	// A harness key under another host name only fails; with this host's
	// key, or a store that needs none, it does not.
	touch(t, filepath.Join(keys, "claude@oldbox.key"))
	c = keyChecks(home, "linux", keys, "newbox", noKey)
	if len(c) != 1 || c[0].Name != "claude key" || c[0].Status != "fail" || !strings.Contains(c[0].Detail, "claude@oldbox") {
		t.Fatalf("claude key under the old host name: %+v", c)
	}
	if c := keyChecks(home, "linux", keys, "newbox", canWrite); len(c) != 0 {
		t.Fatalf("a store that needs no key: %+v", c)
	}
	touch(t, filepath.Join(keys, "claude@newbox.key"))
	if c := keyChecks(home, "linux", keys, "newbox", noKey); len(c) != 0 {
		t.Fatalf("this host's claude key: %+v", c)
	}
}
