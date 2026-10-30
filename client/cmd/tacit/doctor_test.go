package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TACIT_STATE_DIR", "")
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
	// The dream role's checks and the dream job's backoff line.
	if checks, failed := doctorChecks(false, "dream"); !failed || len(checks) != 9 || checks[len(checks)-2].Name != "dream backoff" {
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

// tacit doctor checks scrump's records mode (--records, scrump 0.2.0), which
// keeps batched texts apart: a scrump without it is a warning (the texts go
// joined by a boundary instead) and names the version to install; one whose
// records answer is malformed fails, as every batched redaction would; and
// a scrump that leaves a quoted value a cut left open (before 0.2.0) fails
// the scrump check.
func TestDoctorChecksScrumpsRecordsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("redactor shell fixture")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", filepath.Join(t.TempDir(), "no-store"))
	t.Setenv("TACIT_RUNNER", "muse")
	t.Setenv("TACIT_MUSE_COMMAND", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("TACIT_DREAM_RUNNER", "")
	t.Setenv("TACIT_DREAM_PROFILE", "")
	t.Setenv("TACIT_DREAM_FALLBACK", "null")
	realPath := os.Getenv("PATH")
	checks := func(scrump string) map[string]doctorCheck {
		t.Helper()
		t.Setenv("PATH", realPath)
		if scrump != "" {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte(scrump), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+realPath)
		}
		got, _ := doctorChecks(false, "dream")
		out := map[string]doctorCheck{}
		for _, c := range got {
			out[c.Name] = c
		}
		return out
	}
	// Before 0.2.0: --records is a usage error, and a value a cut left open
	// (no closing quote) is not masked; a canary after "=" is.
	old := "#!/bin/sh\ncase \"$*\" in *--records*) echo \"error: unexpected argument '--records' found\" >&2; exit 2;; esac\nexec sed 's/=Zq7Vx9/=******/g'\n"
	c := checks(old)
	if c["scrump records"].Status != "warn" || !strings.Contains(c["scrump records"].Detail, "scrump 0.2.0") {
		t.Errorf("no records mode: %+v", c["scrump records"])
	}
	if c["scrump"].Status != "fail" || !strings.Contains(c["scrump"].Detail, "cut left open") {
		t.Errorf("a scrump that leaves a cut-open value: %+v", c["scrump"])
	}
	// Records taken, separators masked: every batched redaction fails.
	broken := "#!/bin/sh\nsed 's/Zq7Vx9/******/g' | tr '\\036' '*'\n"
	if c := checks(broken); c["scrump records"].Status != "fail" || c["scrump"].Status != "ok" {
		t.Errorf("a malformed records answer: %+v / %+v", c["scrump records"], c["scrump"])
	}
	if realScrump() == nil {
		t.Setenv("PATH", realPath)
		if digest.RecordsReady() == nil && digest.OpenValueReady() == nil {
			if c := checks(""); c["scrump records"].Status != "ok" || c["scrump"].Status != "ok" {
				t.Errorf("the real scrump 0.2.0: %+v / %+v", c["scrump records"], c["scrump"])
			}
		}
	}
}
