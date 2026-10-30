package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/mac-setup.sh cannot run for real off a Mac. Its --dry-run runs
// every check and prints every change instead of making it; here it runs on
// Linux against stubs of the macOS tools (uname says Darwin, curl answers the
// health check, launchctl must never be called) with an empty HOME, which
// must stay empty.

const macSetup = "../../../scripts/mac-setup.sh"

type stubMac struct {
	bin, home string
	env       []string // more environment for the script, KEY=VALUE
}

func newStubMac(t *testing.T) *stubMac {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	s := &stubMac{bin: t.TempDir(), home: t.TempDir()}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	s.stub(t, "uname", `case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; *) echo Darwin ;; esac`)
	s.stub(t, "hostname", `echo MacBook.local`)
	s.stub(t, "scutil", `[ "$2" = LocalHostName ] && { echo MacBook; exit 0; }; exit 1`)
	s.stub(t, "xcode-select", `echo /Library/Developer/CommandLineTools`)
	s.stub(t, "go", `[ "$1" = version ] && { echo "go version go1.27.1 darwin/arm64"; exit 0; }; echo "go must not run: $*" >&2; exit 9`)
	s.stub(t, "gh", `echo "gh must not run: $*" >&2; exit 9`)
	// The tailnet answers /healthz with 200; the public path (*.ts.net, Funnel)
	// asks a key on every route, so 401.
	s.stub(t, "curl", `case "$*" in *ts.net/healthz*) echo 401; exit 0 ;; *"/healthz"*) echo 200; exit 0 ;; esac; exit 9`)
	s.stub(t, "launchctl", `echo "launchctl must not run: $*" >&2; exit 9`)
	s.stub(t, "cargo", `echo "cargo must not run: $*" >&2; exit 9`)
	s.stub(t, "git", `[ "$1" = --version ] && { echo "git version 2.39.5 (Apple Git-154)"; exit 0; }; exec `+realGit+` "$@"`)
	return s
}

func (s *stubMac) stub(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (s *stubMac) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{macSetup, "--dry-run"}, args...)...)
	cmd.Env = append([]string{"PATH=" + s.bin + ":/usr/bin:/bin", "HOME=" + s.home, "LANG=C"}, s.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// homeEntries lists what the run left in HOME, other than what the test put there.
func homeEntries(t *testing.T, home string, keep ...string) []string {
	t.Helper()
	var got []string
	filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == home {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		for _, k := range keep {
			if rel == k || strings.HasPrefix(rel, k+string(filepath.Separator)) {
				return nil
			}
		}
		got = append(got, rel)
		return nil
	})
	return got
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	missing := false
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q", w)
			missing = true
		}
	}
	if missing {
		t.Logf("output:\n%s", out)
	}
}

func mustNotContain(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("output has %q", b)
		}
	}
}

// recordsScrump stands in for scrump 0.2.0: the text profile's canary
// masked at the same length, and records mode keeping the issue's repro
// apart (its second record's password masked).
const recordsScrump = `sed -e 's/=.*/=************/' -e 's/Zq7Vx9Lm2Kp4/************/'`

// A Mac that already has a scrump with the text profile but no records mode
// (the old pin) rebuilds at the pinned commit on a rerun.
func TestMacSetupRebuildsAScrumpWithoutRecordsMode(t *testing.T) {
	s := newStubMac(t)
	s.stub(t, "scrump", `case "$*" in *--records*) echo "error: unexpected argument '--records' found" >&2; exit 2;; esac
sed 's/=.*/=************/'`)
	s.stub(t, "cargo", `[ "$1" = --version ] && echo cargo; exit 0`)
	out, err := s.run(t, "--server", "http://mac-reachable.example:7447/", "--enroll-ssh", "root@203.0.113.10")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, "+ cargo install --locked --root "+s.home+"/.local --git https://github.com/avifenesh/scrump --rev b2b3178c412ee5d99aaf36233d3952c2d4e796ca scrump-cli")
	mustNotContain(t, out, "records mode works")
}

func TestMacSetupWriterDryRun(t *testing.T) {
	s := newStubMac(t)
	// scrump with the text profile: masks the value at the same length.
	s.stub(t, "scrump", recordsScrump)
	out, err := s.run(t, "--server", "http://mac-reachable.example:7447/", "--enroll-ssh", "root@203.0.113.10")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bin := s.home + "/.local/bin"
	mustContain(t, out,
		"scrump with the text profile and records mode works",
		"+ go -C ", " build -o "+bin+"/tacit ./cmd/tacit",
		"+ env CGO_ENABLED=1 CGO_LDFLAGS=-framework UniformTypeIdentifiers go -C ", "build -tags desktop,production -o "+bin+"/tacit-app ./cmd/tacit",
		"+ "+bin+"/tacit keygen claude",
		`ssh root@203.0.113.10 "/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll(\"claude@macbook\", \"ssh-ed25519 <public-key> claude@macbook\")'"`,
		"+ "+bin+"/tacit clone http://mac-reachable.example:7447/git claude",
		"until the server takes claude@macbook (tacit authcheck claude)",
		"+ "+bin+"/tacit install claude",
		"+ "+bin+"/tacit install follow",
		"+ launchctl bootstrap gui/",
		"/Library/LaunchAgents/ai.tacit.follow.plist",
		"+ "+bin+"/tacit-app install app",
		"dreamer:   not installed (opt-in: --with-dream)",
		"curator, verifier: the curator's machine only",
		"no fixed HostName",
	)
	mustNotContain(t, out, "install dream", "install codex", "clone --read-only", "must not run", "cargo install")
	if left := homeEntries(t, s.home); len(left) != 0 {
		t.Fatalf("a dry run wrote %v", left)
	}
}

// The Mac that cannot join the tailnet: the public path (from $TACIT_SERVER), one
// reader key enrolled and waited for, a read-only clone signed as reader,
// hooks, follower and the view-only app, and never a dreamer.
func TestMacSetupReadOnlyDryRun(t *testing.T) {
	s := newStubMac(t)
	if err := os.MkdirAll(filepath.Join(s.home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Read-only still redacts: the agents' notes go to the local overlay.
	s.stub(t, "scrump", recordsScrump)
	s.env = []string{"TACIT_SERVER=https://tacit.example.ts.net"}
	out, err := s.run(t, "--read-only")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bin := s.home + "/.local/bin"
	mustContain(t, out,
		"checking the server at https://tacit.example.ts.net",
		"Enroll this Mac's keys: run on the server (public keys only):",
		"scrump with the text profile and records mode works",
		"+ "+bin+"/tacit keygen reader",
		`  /opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll("reader@macbook", "ssh-ed25519 <public-key> reader@macbook")'`,
		"until the server takes reader@macbook (tacit authcheck reader)",
		"+ "+bin+"/tacit clone --read-only --as reader https://tacit.example.ts.net/git",
		"+ "+bin+"/tacit install claude",
		"+ "+bin+"/tacit install codex",
		"+ "+bin+"/tacit install follow",
		"/Library/LaunchAgents/ai.tacit.follow.plist",
		"+ "+bin+"/tacit-app install app",
		"mode:      read-only",
		"keys:      reader@macbook enrolled: not checked (dry run)",
		"dreamer:   never on a read-only Mac",
		"(view only: no curator key here; edit from the curator's machine)",
	)
	mustNotContain(t, out, "ssh ", "keygen claude", "keygen codex", "keygen dreamer", "install dream", "clone https", "cargo", "must not run")
	if left := homeEntries(t, s.home, ".codex"); len(left) != 0 {
		t.Fatalf("a dry run wrote %v", left)
	}
}

// An existing read-only clone of the tailnet without the reader key (the
// first Mac setup) is replaced by one of the public path.
func TestMacSetupReadOnlyReplacesAnOldReadOnlyClone(t *testing.T) {
	s := newStubMac(t)
	s.stub(t, "scrump", recordsScrump)
	store := filepath.Join(s.home, ".local/share/tacit/store")
	if out, err := exec.Command("git", "init", "-q", store).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, kv := range [][]string{{"remote.origin.url", "http://100.64.0.10:7447/git"}, {"tacit.readonly", "true"}} {
		if out, err := exec.Command("git", "-C", store, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	out, err := s.run(t, "--read-only", "--server", "https://tacit.example.ts.net")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out,
		"re-cloning the read-only clone from https://tacit.example.ts.net/git with the reader key",
		"+ rm -rf "+store,
		"clone --read-only --as reader https://tacit.example.ts.net/git",
	)
}

func TestMacSetupDreamAndCodexDryRun(t *testing.T) {
	s := newStubMac(t)
	if err := os.MkdirAll(filepath.Join(s.home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No scrump: it is built from main with cargo.
	s.stub(t, "cargo", `[ "$1" = --version ] && echo cargo; exit 0`)
	s.env = []string{"TACIT_ENROLL_SSH=root@203.0.113.10"}
	out, err := s.run(t, "--server", "http://100.64.0.10:7447", "--with-dream", "--no-app", "--env-file", "/Users/someone/bedrock.env")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bin := s.home + "/.local/bin"
	mustContain(t, out,
		"+ cargo install --locked --root "+s.home+"/.local --git https://github.com/avifenesh/scrump --rev b2b3178c412ee5d99aaf36233d3952c2d4e796ca scrump-cli",
		"+ "+bin+"/tacit keygen codex",
		"+ "+bin+"/tacit keygen dreamer",
		"enroll(\\\"dreamer@macbook\\\"",
		"+ "+bin+"/tacit install codex",
		"+ "+bin+"/tacit install dream --env-file /Users/someone/bedrock.env",
		"/Library/LaunchAgents/ai.tacit.dream.plist",
		"/Users/someone/bedrock.env is missing",
	)
	mustNotContain(t, out, "tacit-app", "must not run")
	if left := homeEntries(t, s.home, ".codex"); len(left) != 0 {
		t.Fatalf("a dry run wrote %v", left)
	}
}

func TestMacSetupStopsEarly(t *testing.T) {
	for _, c := range []struct {
		name, stub, body string
		args             []string
		want             string
	}{
		{"no server named", "", "", []string{}, "name the server: --server URL"},
		{"server unreachable", "curl", `exit 7`, nil, "cannot reach http://100.64.0.10:7447/healthz"},
		{"not a Mac", "uname", `echo Linux`, nil, "this script is for macOS"},
		{"old git", "git", `echo "git version 2.20.1"`, nil, "too old"},
		{"no command line tools", "xcode-select", `exit 2`, nil, "xcode-select --install"},
		{"dream with read-only", "", "", []string{"--server", "https://tacit.example.ts.net", "--read-only", "--with-dream"}, "cannot go with --read-only"},
		{"writer mode on the public path", "", "", []string{"--server", "https://tacit.example.ts.net"}, "it is the public path, which only reads; rerun with --read-only"},
		{"public path unreachable", "curl", `echo 000; exit 7`, []string{"--server", "https://tacit.example.ts.net", "--read-only"}, "needs Tailscale Funnel on the server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStubMac(t)
			s.stub(t, "scrump", recordsScrump)
			if c.stub != "" {
				s.stub(t, c.stub, c.body)
			}
			args := c.args
			if args == nil {
				args = []string{"--server", "http://100.64.0.10:7447"}
			}
			out, err := s.run(t, args...)
			if err == nil {
				t.Fatalf("exit 0:\n%s", out)
			}
			mustContain(t, out, c.want)
			mustNotContain(t, out, "+ ")
		})
	}
}

func TestMacSetupShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	if out, err := exec.Command(sc, "-x", macSetup, "../../../scripts/make-icns.sh").CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}
