package main

import (
	"path/filepath"
	"runtime"
	"testing"
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
	if checks, failed := doctorChecks(false, "dream"); !failed || len(checks) != 6 {
		t.Fatal("role selection did not restrict checks", checks, failed)
	}
}
