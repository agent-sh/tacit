package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileLockExcludesOtherHandlesAndReleases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease")
	a, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := TryLock(a); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b); !errors.Is(err, ErrBusy) {
		t.Fatalf("second handle: %v", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := Unlock(b); err != nil {
		t.Fatal(err)
	}
}

func TestProcessLiveness(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("current process is not alive")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("invalid process id is alive")
	}
}
