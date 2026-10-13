package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// urlPw is the password planted in the URL fixture, built at run time so no
// secret-shaped literal sits in the repository.
var urlPw = strings.Repeat("SuperSe", 1) + "cretPw"

// probeHome builds a home directory with one repo and a secret file, and
// points HOME at it so every probe path check runs against it.
func probeHome(t *testing.T) (home, repo string) {
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
	os.MkdirAll(filepath.Join(home, "store"), 0o755)
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "x"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("plainsecretvalue"), 0o600)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "x", "s.txt"), []byte("topsecretvalue123"), 0o600)
	os.WriteFile(filepath.Join(home, ".bash_history"), []byte("historysecret"), 0o600)
	repo = filepath.Join(home, "projects", "r")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello tacit\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".env"), []byte("API_KEY="+strings.Repeat("abcdefghijklmnop", 1)+"\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "sub", "notes.txt"), []byte("db=postgres://u:"+urlPw+"@h/db\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "secrets"), 0o755)
	os.WriteFile(filepath.Join(repo, "secrets", "credentials.json"), []byte("{\"x\":\"credsecret77\"}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "server.pem"), []byte("pemsecret88\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "deploy_ed25519"), []byte("keysecret99\n"), 0o600)
	os.WriteFile(filepath.Join(repo, "Credentials.JSON"), []byte("casesecret66\n"), 0o644)
	for _, n := range []string{"deploy_rsa", "release.jks", "putty.ppk", "ring.gpg", "vault.kdbx", ".vault-token", "app-secrets.yaml", "terraform.tfstate.backup"} {
		os.WriteFile(filepath.Join(repo, n), []byte("namedsecret55 "+n+"\n"), 0o600)
	}
	os.WriteFile(filepath.Join(repo, "blob.bin"), append([]byte("MyAppMasterPassphrase-77\x00\x01\x02"), []byte("\xff\xfe")...), 0o644)
	os.WriteFile(filepath.Join(repo, "tail.bin"), append([]byte(strings.Repeat("text line\n", 1000)), []byte("TailPassphrase-55\x00\x01")...), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return home, repo
}

// Every way a "read" could become a write, code execution, or a look at a
// secret is refused before anything runs.
func TestProbeRefusesWritesCodeAndSecrets(t *testing.T) {
	home, repo := probeHome(t)
	bad := [][]string{
		{"git-log", repo, "-c", "alias.x=!sh"},
		{"git-log", repo, "--output=/tmp/x"},
		{"git-log", repo, "-O", "sh"},
		{"git-log", repo, "--exec-path=/tmp"},
		{"git-grep", repo, "x", "-Osh"},
		{"git-grep", repo, "x", "--open-files-in-pager=sh"},
		{"git-show", repo, "HEAD", "--ext-diff"},
		{"git-show", repo, "--output=/tmp/x"},
		{"git-blame", repo, "/etc/passwd"},
		{"git-blame", repo, ".env"},
		{"git-log", repo, "--", ".env"},
		{"git-log", repo, "--", ":(attr:x)README.md"},
		{"git-log", "/etc"},
		{"git-log", filepath.Join(home, ".ssh")},
		{"grep", filepath.Join(home, ".ssh"), "x"},
		{"grep", filepath.Join(home, ".claude"), "x"},
		{"grep", home, "x"},
		{"find", home, "*"},
		{"ls", home},
		{"ls", filepath.Join(home, ".claude")},
		{"git-show", repo, "HEAD", "--", "../other"},
		{"git-log", repo, "--", "sub/../.env"},
		{"grep", repo, "x", "--include=*"},
		{"find", repo, "$(id)"},
		{"ls", "/etc"},
		{"ls", filepath.Join(home, ".ssh")},
		{"ls", "-la", repo},
		{"gh", "pr", "merge", "5"},
		{"gh", "pr", "view", "5", "--web"},
		{"gh", "pr", "view", "5", "--comments"},
		{"gh", "issue", "view", "5", "-R", "https://evil/x"},
		{"gh", "issue", "view", "5", "-R", "host/o/r"},
		{"gh", "api", "repos"},
		{"gh", "repo", "view", "../.."},
		{"unit", "x; rm"},
		{"units", "extra"},
		{"sh", "-c", "id"},
	}
	for _, args := range bad {
		if _, _, err := buildCmds(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestProbeAcceptsReadOnlyFormsAndHidesSecrets(t *testing.T) {
	requireScrump(t)
	home, repo := probeHome(t)
	good := [][]string{
		{"git-log", repo, "-n", "3", "--grep", "seed", "--", "README.md"},
		{"git-log", repo},
		{"git-show", repo, "HEAD", "--stat"},
		{"git-blame", repo, "README.md", "-L", "1,1"},
		{"git-grep", repo, "tacit", "-i"},
		{"git-ls", repo},
		{"git-tags", repo},
		{"grep", repo, "tacit", "-i"},
		{"find", repo, "*.md"},
		{"ls", repo},
		{"ls", "~/projects"},
		{"gh", "pr", "view", "5", "-R", "avifenesh/tacit"},
		{"gh", "issue", "list", "-R", "avifenesh/tacit", "--state", "closed", "--limit", "5000"},
		{"gh", "repo", "view", "avifenesh/tacit"},
	}
	for _, args := range good {
		cmds, _, err := buildCmds(args)
		if err != nil {
			t.Errorf("refused %q: %v", args, err)
			continue
		}
		flat := strings.Join(cmds[0], " ")
		if args[0] == "gh" && !strings.Contains(flat, "--json") {
			t.Errorf("gh without fixed fields: %s", flat)
		}
		if strings.Contains(flat, "--limit 5000") {
			t.Errorf("limit not capped: %s", flat)
		}
	}
	_ = home
	// Tracked and untracked secret files stay out of git and grep output,
	// whatever paths the model passes, and secret-shaped text in ordinary
	// files is redacted.
	for _, args := range [][]string{
		{"git-ls", repo}, {"git-ls", repo, "."}, {"git-ls", repo, "secrets"},
		{"git-grep", repo, "secret"}, {"git-grep", repo, "API", "--", ".e*"}, {"git-grep", repo, "secret", "--", "secrets"},
		{"grep", repo, "secret"}, {"find", repo, "*"}, {"find", repo, "*.pem"},
		{"git-show", repo, "HEAD"}, {"git-show", repo, "HEAD", "--", "."}, {"git-show", repo, "HEAD", "--", "secrets"},
		{"git-log", repo, "-p", "--", "."},
	} {
		out, _ := probe(t.Context(), args)
		for _, leak := range []string{".env", "abcdefghijklmnop", "credsecret77", "pemsecret88", "credentials.json", "server.pem", "keysecret99", "deploy_ed25519", "casesecret66", "Credentials.JSON", "namedsecret55", "release.jks", ".vault-token", "tfstate"} {
			if strings.Contains(out, leak) {
				t.Errorf("%q showed %s:\n%s", args, leak, out)
				break
			}
		}
	}
	// A blob or tree sha is not a commit.
	blob := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD:.env"))
	if out, err := probe(t.Context(), []string{"git-show", repo, blob}); err == nil || strings.Contains(out, "abcdefghijklmnop") {
		t.Fatalf("blob sha printed a secret: %v\n%s", err, out)
	}
	// grep -l lists file names only; secret-named ones stay out whatever the order.
	if out, _ := probe(t.Context(), []string{"grep", repo, "secret|tacit", "-l"}); strings.Contains(out, "Credentials.JSON") || strings.Contains(out, "server.pem") || !strings.Contains(out, "README.md") {
		t.Fatalf("grep -l listed a secret file or lost README:\n%s", out)
	}
	// A valid unit shows state only: no ExecStart, no journal, no process lines.
	os.MkdirAll(filepath.Join(home, ".config", "systemd", "user"), 0o755)
	if out, _ := probe(t.Context(), []string{"unit", "tacit-curate.service"}); strings.Contains(out, "ExecStart") || strings.Contains(out, "CGroup") || strings.Contains(out, "●") {
		t.Fatalf("unit printed more than state:\n%s", out)
	}
	// Blame from a subdirectory checks the blob it will print, not the root's.
	os.MkdirAll(filepath.Join(repo, "sub2"), 0o755)
	os.WriteFile(filepath.Join(repo, "sub2", "README.md"), []byte("sub\x00\x01bin"), 0o644)
	gitOut(t, repo, "add", "sub2")
	gitOut(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "sub2")
	if out, err := probe(t.Context(), []string{"git-blame", filepath.Join(repo, "sub2"), "README.md"}); err == nil || strings.Contains(out, "bin") {
		t.Fatalf("subdirectory blame printed a binary: %v\n%s", err, out)
	}
	// A tracked binary is not blamed into the output.
	for _, f := range []string{"blob.bin", "tail.bin"} {
		if out, err := probe(t.Context(), []string{"git-blame", repo, f}); err == nil || strings.Contains(out, "Passphrase") {
			t.Fatalf("binary %s blamed: %v\n%s", f, err, out)
		}
	}
	// A working-tree directory symlink that leads outside the roots is never
	// read: grep uses the index and blame uses HEAD. The outside directory
	// holds a file with the tracked name, so a worktree read would find it.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "notes.txt"), []byte("OUTSIDE-REAL-CONTENT\n"), 0o644)
	os.RemoveAll(filepath.Join(repo, "sub"))
	os.Symlink(outside, filepath.Join(repo, "sub"))
	for _, args := range [][]string{{"git-grep", repo, "OUTSIDE"}, {"git-blame", repo, "sub/notes.txt"}, {"grep", repo, "OUTSIDE"}, {"find", repo, "notes.txt"}} {
		if out, _ := probe(t.Context(), args); strings.Contains(out, "OUTSIDE-REAL-CONTENT") {
			t.Errorf("%q followed a symlink out of the roots:\n%s", args, out)
		}
	}
	// A repo whose .git points at a real repository outside the roots is refused.
	outRepo := filepath.Join(outside, "repo")
	os.MkdirAll(outRepo, 0o755)
	os.WriteFile(filepath.Join(outRepo, "x"), []byte("x"), 0o644)
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "out"}} {
		gitOut(t, outRepo, a...)
	}
	gitfile := filepath.Join(home, "projects", "g")
	os.MkdirAll(gitfile, 0o755)
	os.WriteFile(filepath.Join(gitfile, ".git"), []byte("gitdir: "+filepath.Join(outRepo, ".git")+"\n"), 0o644)
	if _, err := probe(t.Context(), []string{"git-log", gitfile}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a gitfile pointing outside the roots: %v", err)
	}
	// Ordinary lines that merely mention a secret file name stay in git output.
	os.WriteFile(filepath.Join(repo, "HOWTO.md"), []byte("copy the file to prod.env and run\n"), 0o644)
	gitOut(t, repo, "add", "HOWTO.md")
	gitOut(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "document how to load prod.env")
	if out, _ := probe(t.Context(), []string{"git-log", repo, "-n", "1"}); !strings.Contains(out, "prod.env") {
		t.Fatalf("git-log dropped a commit that mentions a secret name:\n%s", out)
	}
	// Without a home directory nothing is reachable.
	t.Setenv("HOME", "")
	if _, err := homeDir(repo, true); err == nil {
		t.Fatal("empty HOME was accepted")
	}
	t.Setenv("HOME", home)
	out, err := probe(t.Context(), []string{"grep", repo, "postgres"})
	if err != nil || strings.Contains(out, urlPw) {
		t.Fatalf("URL password not redacted (%v):\n%s", err, out)
	}
	out, _ = probe(t.Context(), []string{"git-log", repo})
	if !strings.Contains(out, "seed") {
		t.Fatalf("git-log lost the commit:\n%s", out)
	}
}

func gitOut(t *testing.T, repo string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// A program that floods output is stopped and the probe still answers.
func TestProbeCapsOutput(t *testing.T) {
	stubScrump(t)
	_, repo := probeHome(t)
	big := strings.Repeat("tacit line\n", 40000)
	os.WriteFile(filepath.Join(repo, "big.txt"), []byte(big), 0o644)
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "big"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := probe(t.Context(), []string{"git-log", repo, "-p"})
	if err != nil || len(out) > probeMax+200 || !strings.Contains(out, "output cut") {
		t.Fatalf("len %d err %v", len(out), err)
	}
}
