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
	"strings"
	"time"
)

func main() {
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
	app := exec.CommandContext(ctx, abs, "app", "--browser", "--no-open", "--addr", "127.0.0.1:0")
	app.Env = env
	out, err := app.StdoutPipe()
	check(err)
	app.Stderr = os.Stderr
	check(app.Start())
	defer func() { app.Process.Kill(); app.Wait() }()
	urls := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			line := s.Text()
			if strings.HasPrefix(line, "tacit app: ") {
				urls <- strings.Fields(line)[2]
				return
			}
		}
		close(urls)
	}()
	var url string
	select {
	case url = <-urls:
	case <-ctx.Done():
		check(fmt.Errorf("app startup exceeded its deadline"))
	}
	if url == "" {
		check(fmt.Errorf("app exited before serving"))
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
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

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
}
