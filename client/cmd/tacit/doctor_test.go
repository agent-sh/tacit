package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDoctorGitVersion(t *testing.T) {
	for version, want := range map[string]bool{"git version 2.28.0": false, "git version 2.29.0": true, "git version 2.50.1.windows.1": true, "git version 3.0.0": true, "oops": false} {
		if gitSupported(version) != want {
			t.Fatalf("%s", version)
		}
	}
}
func TestDoctorInvalidRole(t *testing.T) {
	if code := runDoctor([]string{"--role", "typo"}); code != 2 {
		t.Fatal(code)
	}
}

func TestDoctorMissingRunnerModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("redactor shell fixture")
	}
	stubScrump(t)
	t.Setenv("TACIT_STORE", filepath.Join(t.TempDir(), "no-store"))
	t.Setenv("TACIT_RUNNER", "muse")
	t.Setenv("TACIT_MUSE_COMMAND", filepath.Join(t.TempDir(), "missing"))
	for _, role := range []string{"DREAM", "CURATE", "JUDGE", "VERIFY"} {
		t.Setenv("TACIT_"+role+"_RUNNER", "")
		t.Setenv("TACIT_"+role+"_PROFILE", "")
		t.Setenv("TACIT_"+role+"_FALLBACK", "null")
	}
	if _, failed := doctorChecks(false, ""); failed {
		t.Fatal("missing model runner blocks memory-only install")
	}
	if _, failed := doctorChecks(true, ""); !failed {
		t.Fatal("required model runner accepted")
	}
	if checks, failed := doctorChecks(false, "dream"); !failed || len(checks) != 7 {
		t.Fatal("role selection did not restrict checks", checks, failed)
	}
}

func TestDoctorFindsProcessesOnAnOutdatedBinary(t *testing.T) {
	dir := t.TempDir()
	// Linux: /proc/<pid>/exe of a replaced binary ends in " (deleted)"; one
	// whose file on disk is another inode was replaced too.
	proc := filepath.Join(dir, "proc")
	bin := filepath.Join(dir, "bin", "tacit")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("new"), 0o755)
	old := filepath.Join(dir, "old-tacit")
	os.WriteFile(old, []byte("old"), 0o755)
	mk := func(pid, target, cmdline string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		if err := os.Symlink(target, filepath.Join(proc, pid, "exe")); err != nil {
			t.Skip("symlinks:", err)
		}
		os.WriteFile(filepath.Join(proc, pid, "cmdline"), []byte(cmdline), 0o644)
	}
	mk("101", bin+" (deleted)", "tacit\x00mcp\x00--harness\x00codex\x00")
	mk("102", bin, "tacit\x00hook\x00")                        // current
	mk("103", filepath.Join(dir, "bin", "other"), "other\x00") // not tacit
	mk("104", bin+" (deleted)", "tacit\x00mcp\x00")            // this process
	got := staleProcsLinux(proc, 104)
	if len(got) != 1 || got[0].PID != 101 || got[0].Command != "tacit mcp --harness codex" || !strings.Contains(got[0].Why, "replaced") {
		t.Fatalf("linux: %+v", got)
	}
	// macOS: ps's start time against the binary's modification time.
	now := time.Now()
	os.Chtimes(bin, now, now)
	ps := strings.Join([]string{
		fmt.Sprintf("  201 %s %s", now.Add(-time.Hour).Format("Mon Jan _2 15:04:05 2006"), bin),
		fmt.Sprintf("  202 %s %s", now.Add(time.Minute).Format("Mon Jan _2 15:04:05 2006"), bin),
		fmt.Sprintf("  203 %s %s", now.Add(-time.Hour).Format("Mon Jan _2 15:04:05 2006"), filepath.Join(dir, "gone", "tacit")),
		fmt.Sprintf("  204 %s /usr/bin/ssh", now.Add(-time.Hour).Format("Mon Jan _2 15:04:05 2006")),
	}, "\n")
	got = staleProcsPS(ps, 1, func(pid int) string { return fmt.Sprintf("tacit mcp #%d", pid) })
	if len(got) != 2 || got[0].PID != 201 || got[1].PID != 203 || got[0].Command != "tacit mcp #201" || !strings.Contains(got[1].Why, "gone") {
		t.Fatalf("ps: %+v", got)
	}
}
