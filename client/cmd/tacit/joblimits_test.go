package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every scheduled job's unit stops a run that outlives the run's own limits,
// so a run that hangs outside all of them no longer holds its job for good
// (a oneshot's TimeoutStartSec is infinite unless set). The bound is above
// what the binary allows: a unit never stops a run the binary would finish.
func TestScheduledJobsHaveARunLimitAboveTheirOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TACIT_STORE", filepath.Join(home, "facts"))
	if _, err := installDreamSystemd(home, "/opt/tacit", "/home/u/.config/tacit/env"); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(home, ".config", "systemd", "user")
	unit := func(name string) string {
		b, err := os.ReadFile(filepath.Join(units, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	timeout := regexp.MustCompile(`(?m)^TimeoutStartSec=(\S+)$`)
	for name, limit := range map[string]time.Duration{
		"tacit-dream.service":      dreamJobLimit,
		"tacit-dream-idle.service": dreamIdleJobLimit,
	} {
		m := timeout.FindStringSubmatch(unit(name))
		if m == nil || m[1] != span(limit, "h", "min") {
			t.Fatalf("%s: TimeoutStartSec %v, want %s:\n%s", name, m, span(limit, "h", "min"), unit(name))
		}
	}
	if m := timeout.FindStringSubmatch(localCurateUnit("/opt/tacit", "/home/u/.config/tacit/env")); m == nil || m[1] != span(curateJobLimit, "h", "min") {
		t.Fatalf("local curate unit: TimeoutStartSec %v", m)
	}
	// The run's own limits, where the code names them (the 20-minute model
	// timeout, the 45-minute --max-runtime, curate's 150-minute budget and its
	// tails are the flags' and the call's defaults).
	if own := dreamLockWait + 20*time.Minute + 45*time.Minute; dreamJobLimit <= own {
		t.Fatalf("dream limit %s is not above its own %s", dreamJobLimit, own)
	}
	if own := 20*time.Minute + 45*time.Minute + quickBudget + 10*time.Minute; dreamIdleJobLimit <= own {
		t.Fatalf("idle limit %s is not above its own %s", dreamIdleJobLimit, own)
	}
	if own := curateLockWait + 150*time.Minute + 30*time.Minute; curateJobLimit <= own {
		t.Fatalf("curate limit %s is under its own %s", curateJobLimit, own)
	}
	// The Windows tasks carry the same bounds. The follower runs for good,
	// and maintain has none: verify has no overall budget.
	for target, want := range map[string]string{"dream": "PT2H30M", "dream-idle": "PT2H30M", "curate": "PT0S", localCurateJob: "PT5H", "follow": "PT0S"} {
		p, err := installWindowsJob(home, `C:\tacit.exe`, target, `C:\env`)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "<ExecutionTimeLimit>"+want+"</ExecutionTimeLimit>") {
			t.Fatalf("%s task: want ExecutionTimeLimit %s:\n%s", target, want, b)
		}
	}
	// The optional mirror's plain git gets the client's transfer limit and a
	// bound under its 15-minute timer.
	b, err := os.ReadFile("../../deploy/systemd/tacit-store-fetch.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "git -c http.lowSpeedLimit=1000 -c http.lowSpeedTime=5 ") || !strings.Contains(string(b), "\nTimeoutStartSec=10min\n") {
		t.Fatalf("mirror unit:\n%s", b)
	}
}
