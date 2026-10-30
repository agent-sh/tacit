package modelrun

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// A model call's session is named before the call and listed where the
// dreamer reads the sessions it never dreams.
func TestExcludeList(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TACIT_STATE_DIR", state)
	a, b := NewSessionID(), NewSessionID()
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuid.MatchString(a) || a == b {
		t.Fatalf("session ids %q %q", a, b)
	}
	if len(Excluded()) != 0 {
		t.Fatal("an empty state excludes sessions")
	}
	if err := Exclude(a); err != nil {
		t.Fatal(err)
	}
	if err := Exclude(b, "runner-reported-id"); err != nil {
		t.Fatal(err)
	}
	ex := Excluded()
	if len(ex) != 3 || !ex[a] || !ex[b] || !ex["runner-reported-id"] {
		t.Fatalf("excluded %v", ex)
	}
	if p := ExcludeFile(""); p != filepath.Join(state, "dream-exclude") {
		t.Fatalf("exclude file %s", p)
	}
	fi, err := os.Stat(ExcludeFile(""))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("exclude file mode %v", fi.Mode())
	}
}

// A list that cannot be written is an error, so a caller that must keep its
// session out of the dreamer can stop: here the list is a full device.
func TestExcludeReportsAFailedWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /dev/full")
	}
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	state := t.TempDir()
	t.Setenv("TACIT_STATE_DIR", state)
	if err := os.Symlink("/dev/full", filepath.Join(state, "dream-exclude")); err != nil {
		t.Fatal(err)
	}
	if err := Exclude(NewSessionID()); err == nil {
		t.Fatal("a write to a full device was reported as done")
	}
}
