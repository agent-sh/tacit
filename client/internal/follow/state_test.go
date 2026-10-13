package follow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Current is what the SessionStart hook asks before it skips its own sync.
// Every doubt answers false.
func TestCurrentAnswersFalseOnAnyDoubt(t *testing.T) {
	now := time.Now()
	good := State{PID: os.Getpid(), Store: "/s", Seq: 5, Connected: true, AliveAt: now.Add(-10 * time.Second), VerifiedAt: now.Add(-2 * time.Minute)}
	cases := []struct {
		name string
		mod  func(*State)
		want bool
	}{
		{"healthy", func(*State) {}, true},
		{"just inside the bound", func(s *State) { s.AliveAt = now.Add(-FreshFor + time.Second) }, true},
		{"heartbeat too old", func(s *State) { s.AliveAt = now.Add(-FreshFor - time.Second) }, false},
		{"heartbeat from the future", func(s *State) { s.AliveAt = now.Add(time.Minute) }, false},
		{"never stamped", func(s *State) { s.AliveAt = time.Time{} }, false},
		{"verified just inside the bound", func(s *State) { s.VerifiedAt = now.Add(-VerifiedFor + time.Second) }, true},
		{"last check against the server too old", func(s *State) { s.VerifiedAt = now.Add(-VerifiedFor - time.Second) }, false},
		{"never checked against the server", func(s *State) { s.VerifiedAt = time.Time{} }, false},
		{"check stamp from the future", func(s *State) { s.VerifiedAt = now.Add(time.Minute) }, false},
		{"stream down", func(s *State) { s.Connected = false }, false},
		{"push announced, not merged", func(s *State) { s.Behind = true }, false},
		{"another clone", func(s *State) { s.Store = "/other" }, false},
		{"cleanly stopped", func(s *State) { s.PID = 0 }, false},
		{"process gone", func(s *State) { s.PID = 1 << 30 }, false},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "follow.json")
		st := good
		c.mod(&st)
		if err := Save(path, st); err != nil {
			t.Fatal(err)
		}
		if got := Current(path, "/s", now); got != c.want {
			t.Errorf("%s: Current = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.json")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lock(path)
	var running *ErrRunning
	if !errors.As(err, &running) || running.PID != os.Getpid() {
		t.Fatalf("second Lock: %v, want ErrRunning for pid %d", err, os.Getpid())
	}
	release()
	again, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	again()
}

func TestStatePathIsKeyedByStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_FOLLOW_STATE", "")
	def := filepath.Join(home, ".local", "share", "tacit", "store")
	dir := filepath.Join(home, ".local", "state", "tacit")
	if got := StatePath(def); got != filepath.Join(dir, "follow.json") {
		t.Errorf("default store: %s", got)
	}
	a, b := StatePath("/data/a"), StatePath("/data/b")
	if a == b || a == filepath.Join(dir, "follow.json") || filepath.Dir(a) != dir {
		t.Errorf("stores share or misplace state: %s %s", a, b)
	}
	if StatePath("/data/a/") != a {
		t.Error("the same store, written differently, got another file")
	}
	t.Setenv("TACIT_FOLLOW_STATE", "/x/y.json")
	if got := StatePath("/data/a"); got != "/x/y.json" {
		t.Errorf("override ignored: %s", got)
	}
}

func TestCurrentWithoutAFollower(t *testing.T) {
	dir := t.TempDir()
	if Current(filepath.Join(dir, "missing.json"), "/s", time.Now()) {
		t.Error("missing file")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if Current(bad, "/s", time.Now()) {
		t.Error("corrupt file")
	}
}
