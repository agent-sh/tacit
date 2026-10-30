// Command smoke exercises the standalone CLI and editable browser API on any OS.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// openerNames are the commands the client runs to open its owner page in a
// browser (cmd/tacit openURL): xdg-open, open (macOS), rundll32.exe (Windows).
var openerNames = map[string]bool{"xdg-open": true, "open": true, "rundll32": true}

func main() {
	// Run as the browser opener: record the page's link (its last argument)
	// for the smoke run that put this copy first on PATH, and open nothing.
	if openerNames[strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")] {
		if f := os.Getenv("TACIT_SMOKE_LINK_FILE"); f != "" && len(os.Args) > 1 {
			check(os.WriteFile(f, []byte(os.Args[len(os.Args)-1]), 0o600))
		}
		return
	}
	binary := flag.String("binary", "tacit", "client to exercise")
	redactor := flag.Bool("with-redactor", false, "also exercise agent note redaction with installed scrump")
	flag.Parse()
	abs, err := filepath.Abs(*binary)
	check(err)
	cache, err := os.UserCacheDir()
	check(err)
	parent := filepath.Join(cache, "tacit")
	check(os.MkdirAll(parent, 0o700))
	root, err := os.MkdirTemp(parent, "smoke-")
	check(err)
	defer os.RemoveAll(root)
	cleanup = append(cleanup, func() { os.RemoveAll(root) })
	cfg := filepath.Join(root, "config.yaml")
	// Quoted forward slashes work in YAML on Windows and Unix alike.
	store := filepath.Join(root, "My Facts")
	data := fmt.Sprintf("store: %q\nindex: %q\nlocal_overlay: %q\n", filepath.ToSlash(store), filepath.ToSlash(filepath.Join(root, "index")), filepath.ToSlash(filepath.Join(root, "overlay")))
	check(os.WriteFile(cfg, []byte(data), 0o600))
	env := append(os.Environ(), "TACIT_SETTINGS="+cfg, "TACIT_STORE="+store, "TACIT_INDEX="+filepath.Join(root, "index"), "TACIT_LOCAL="+filepath.Join(root, "overlay"))
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, abs, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			check(fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, out))
		}
		return string(out)
	}
	// The offline doctor must return valid check records on every platform.
	// Without scrump it reports an unmet dependency, so exit 1 is expected.
	doctorCtx, doctorCancel := context.WithTimeout(context.Background(), time.Minute)
	doctor := exec.CommandContext(doctorCtx, abs, "doctor", "--json")
	doctor.Env = env
	doctorOut, doctorErr := doctor.Output()
	doctorCancel()
	if doctorErr != nil {
		if exit, ok := doctorErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			check(doctorErr)
		}
	}
	var checks []struct{ Name, Status string }
	check(json.Unmarshal(doctorOut, &checks))
	gitOK, offline := false, false
	for _, check := range checks {
		gitOK = gitOK || check.Name == "git" && check.Status == "ok"
		offline = offline || check.Name == "provider" && check.Status == "info"
	}
	if !gitOK || !offline {
		check(fmt.Errorf("doctor omitted Git or offline provider checks"))
	}
	fmt.Println("PASS: offline doctor JSON and Git preflight")
	run("init")
	if *redactor {
		secret := "ghp_" + strings.Repeat("a", 40)
		run("add", "--harness", "claude", "--scope", "projects/demo", "--desc", "The demo note masks "+secret, "masked-note")
		found := false
		filepath.WalkDir(filepath.Join(store, "inbox"), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				if strings.Contains(string(b), secret) {
					check(fmt.Errorf("agent note leaked synthetic credential"))
				}
				found = true
			}
			return nil
		})
		if !found {
			check(fmt.Errorf("agent note was not written"))
		}
	}
	run("init")
	run("sync")
	run("lint")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The page's link carries its token and is printed only to a terminal;
	// the app hands it to the browser opener. This smoke run is that opener:
	// a copy of itself first on PATH under the opener's name records it.
	self, err := os.Executable()
	check(err)
	openers := filepath.Join(root, "opener")
	check(os.MkdirAll(openers, 0o700))
	name := map[string]string{"windows": "rundll32.exe", "darwin": "open"}[runtime.GOOS]
	if name == "" {
		name = "xdg-open"
	}
	selfBin, err := os.ReadFile(self)
	check(err)
	check(os.WriteFile(filepath.Join(openers, name), selfBin, 0o700))
	linkFile := filepath.Join(root, "link")
	app := exec.CommandContext(ctx, abs, "app", "--browser", "--addr", "127.0.0.1:0")
	app.Env = append(append([]string{}, env...), "PATH="+openers+string(os.PathListSeparator)+os.Getenv("PATH"), "TACIT_SMOKE_LINK_FILE="+linkFile)
	out, err := app.StdoutPipe()
	check(err)
	app.Stderr = os.Stderr
	check(app.Start())
	defer func() { app.Process.Kill(); app.Wait() }()
	cleanup = append(cleanup, func() { app.Process.Kill(); app.Wait() })
	lines := make(chan string, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		s := bufio.NewScanner(out)
		for s.Scan() {
			if line := s.Text(); strings.HasPrefix(line, "tacit app: ") {
				lines <- line
			}
		}
	}()
	var line string
	select {
	case line = <-lines:
	case <-exited:
		check(fmt.Errorf("app exited before serving"))
	case <-ctx.Done():
		check(fmt.Errorf("app startup exceeded its deadline"))
	}
	url := strings.Fields(line)[2]
	var link string
	for link == "" && ctx.Err() == nil {
		if b, err := os.ReadFile(linkFile); err == nil && len(b) > 0 {
			link = string(b)
			break
		}
		select {
		case <-exited:
			check(fmt.Errorf("app exited before its browser opener ran"))
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(link, url+"?") {
		check(fmt.Errorf("the browser opener got %q, want the page's link at %s", link, url))
	}
	// A pipe never sees the token: not on the line the app printed.
	if token := link[strings.Index(link, "=")+1:]; token == "" || strings.Contains(line, token) {
		check(fmt.Errorf("app printed its page's token to a pipe: %s", line))
	}
	client := &http.Client{Timeout: 30 * time.Second}
	// Without the token the page refuses.
	if resp, err := client.Get(url); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			check(fmt.Errorf("the page answered %d without its token", resp.StatusCode))
		}
	}
	resp, err := client.Get(link)
	check(err)
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(err)
	match := regexp.MustCompile(`name="tacit-token" content="([^"]+)"`).FindSubmatch(b)
	if len(match) != 2 {
		check(fmt.Errorf("app page has no token"))
	}
	post := func(body string) []byte {
		req, err := http.NewRequest(http.MethodPost, url+"api/call/NewRuling", strings.NewReader(body))
		check(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tacit-Token", string(match[1]))
		r, err := client.Do(req)
		check(err)
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		check(err)
		if r.StatusCode != 200 {
			check(fmt.Errorf("owner API answered %d", r.StatusCode))
		}
		return b
	}
	result := post(`[{"name":"review-first","body":"Review every public change before merging it.","scope":"projects/demo"}]`)
	var saved map[string]any
	check(json.Unmarshal(result, &saved))
	if saved["error"] != nil {
		check(fmt.Errorf("owner write: %v", saved["error"]))
	}
	search := run("search", "--repo", "demo", "--host", "smoke", "--json", "review public change merging")
	if !strings.Contains(search, "review-first") {
		check(fmt.Errorf("new ruling was not searchable"))
	}
	for _, harness := range []string{"muse", "agy"} {
		check(memorySmoke(abs, env, harness, *redactor))
	}
	run("lint")
	run("install", "follow")
	follow := exec.Command(abs, "follow")
	follow.Env = env
	if err := follow.Run(); err == nil {
		check(fmt.Errorf("local follower returned a restarting success status"))
	} else if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 78 {
		check(fmt.Errorf("local follower did not return its non-restarting config status: %v", err))
	}
	fmt.Println("PASS: local init, config path with spaces, sync, owner browser write, search, lint, no remote feed")
}

// cleanup runs before a failed check exits, newest first: os.Exit skips the
// deferred calls, which would leave the app running and its directory behind.
var cleanup []func()

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		os.Exit(1)
	}
}
