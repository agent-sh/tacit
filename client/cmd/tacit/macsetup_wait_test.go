package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runReal runs the script for real (no --dry-run) against the stubs, with a
// stand-in checkout, a go that "builds" a fake tacit, and the given answer
// from `tacit authcheck`: the enrollment wait must show the server's own
// answer and stop at once when waiting cannot help.
func (s *stubMac) runReal(t *testing.T, authAnswer string, args ...string) (string, error) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.stub(t, "git", `[ "$1" = --version ] && { echo "git version 2.39.5 (Apple Git-154)"; exit 0; }; [ "$1 $2" = "-C `+repo+`" ] && exit 0; exit 1`)
	s.stub(t, "faketacit", `case "$1" in
pubkey) exit 1 ;;
keygen) echo "ssh-ed25519 AAAAfake $2@macbook" ;;
clone) mkdir -p "$HOME/.local/share/tacit/store/.git" ;;
authcheck) exec auth-answer ;;
*) exit 0 ;;
esac`)
	s.stub(t, "go", `[ "$1" = version ] && { echo "go version go1.27.1 darwin/arm64"; exit 0; }
out=""; while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cp "`+filepath.Join(s.bin, "faketacit")+`" "$out"`)
	s.stub(t, "auth-answer", authAnswer)
	s.stub(t, "launchctl", `[ "$1" = print ] && printf '\tstate = running\n'; exit 0`)
	s.stub(t, "scrump", recordsScrump)
	cmd := exec.Command("bash", append([]string{macSetup, "--no-app", "--repo", repo, "--server", "http://100.64.0.10:7447"}, args...)...)
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin", "HOME=" + s.home, "LANG=C"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func TestMacSetupEnrollmentWait(t *testing.T) {
	t.Run("a wrong host stops at once with the server's answer", func(t *testing.T) {
		s := newStubMac(t)
		out, err := s.runReal(t, `echo "stop: 401 writer token rejected: host_mismatch: the server sees this request from another tailnet host"; exit 3`)
		if err == nil {
			t.Fatalf("exit 0:\n%s", out)
		}
		mustContain(t, out, "the server will not take claude@macbook: stop: 401 writer token rejected: host_mismatch")
		mustNotContain(t, out, "s left)", "install claude")
	})
	t.Run("not enrolled yet shows the answer until the deadline", func(t *testing.T) {
		s := newStubMac(t)
		out, err := s.runReal(t, `echo "retry: 401 writer token rejected: unknown_key: claude@macbook is not enrolled yet"; exit 1`, "--wait", "0")
		if err == nil {
			t.Fatalf("exit 0:\n%s", out)
		}
		mustContain(t, out, "has not taken claude@macbook after 0s. Its last answer: retry: 401 writer token rejected: unknown_key")
	})
	t.Run("read-only waits for the reader key, then clones", func(t *testing.T) {
		s := newStubMac(t)
		out, err := s.runReal(t, `echo "ok: the server takes reader@macbook"; exit 0`, "--read-only")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		mustContain(t, out, "the server takes reader@macbook", "keys:      reader@macbook enrolled: yes", "mode:      read-only", "follower:  running")
		mustNotContain(t, out, "claude@macbook")
	})
	t.Run("an enrolled key goes on to the hooks", func(t *testing.T) {
		s := newStubMac(t)
		out, err := s.runReal(t, `echo "ok: the server takes claude@macbook"; exit 0`)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		mustContain(t, out, "the server takes claude@macbook", "keys:      claude@macbook enrolled: yes", "follower:  running")
	})
}
