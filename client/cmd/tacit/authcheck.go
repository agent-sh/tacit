package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Exit codes of `tacit authcheck`.
const (
	authOK    = 0 // the server takes this writer
	authRetry = 1 // not yet: the key is unknown (enrollment pending) or the server is unreachable
	authStop  = 3 // waiting will not help: wrong host, bad token, not a tailnet peer, no key here
)

// runAuthcheck implements `tacit authcheck <harness> [--url BASE]`: ask the
// server whether it takes <harness>@<host> as a writer, the way `git push`
// first asks (GET info/refs for receive-pack with the writer's token), and
// print its answer in one line. Nothing is sent and the token is never
// printed. scripts/mac-setup.sh waits on it while the key is enrolled.
//
// A reader key never pushes, so for `tacit authcheck reader` the question is
// the one `git fetch` asks first (info/refs for upload-pack with the token),
// which is also what the server's public path answers.
func runAuthcheck(args []string) int {
	if len(args) < 1 || !fact.IsSlug(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: tacit authcheck <harness> [--url BASE]")
		return 2
	}
	harness, base := args[0], ""
	if len(args) == 3 && args[1] == "--url" {
		base = args[2]
	} else if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tacit authcheck <harness> [--url BASE]")
		return 2
	}
	id := harness + "@" + shortHost(mustHostname())
	dir, err := auth.Dir()
	if err != nil {
		fmt.Println("stop:", err)
		return authStop
	}
	key, err := auth.Load(dir, id)
	if err != nil {
		fmt.Printf("stop: no key %s here; run `tacit keygen %s`\n", id, harness)
		return authStop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if base == "" {
		s, err := store.Default()
		if err != nil || !s.Exists() {
			fmt.Println("stop: no store; pass --url")
			return authStop
		}
		if base, err = s.RemoteURL(ctx); err != nil || base == "" {
			fmt.Println("stop: cannot read the clone's origin")
			return authStop
		}
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/git")
	service := "git-receive-pack"
	if harness == "reader" {
		service = "git-upload-pack"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/git/info/refs?service="+service, nil)
	if err != nil {
		fmt.Println("stop:", err)
		return authStop
	}
	req.SetBasicAuth(id, key.Token(time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("retry: cannot reach the server:", err)
		return authRetry
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	return authAnswer(id, resp.StatusCode, strings.TrimSpace(line))
}

// publicRefusal is the one 401 body of the server's public path
// (TacitServer.Auth.Plug.public_401/0).
const publicRefusal = "the public path needs a reader key token"

// authAnswer prints what the server's answer means and returns the exit code.
func authAnswer(id string, status int, body string) int {
	if len(body) > 200 {
		body = body[:200]
	}
	switch {
	case status == http.StatusOK:
		fmt.Println("ok: the server takes", id)
		return authOK
	case status == http.StatusUnauthorized && strings.Contains(body, publicRefusal):
		// The public path gives every refusal the same body, so it cannot say
		// "not enrolled yet". That is the case a waiting setup is in, so wait;
		// the caller's deadline ends it if the clock or the key is the problem.
		fmt.Printf("retry: %d %s: %s is not taken yet (not enrolled, or this machine's clock is off; the public path does not say which)\n", status, body, id)
		return authRetry
	case status == http.StatusUnauthorized && strings.Contains(body, "unknown_key"):
		fmt.Printf("retry: %d %s: %s is not enrolled yet; run the enroll line on the server\n", status, body, id)
		return authRetry
	case status == http.StatusUnauthorized && strings.Contains(body, "host_mismatch"):
		fmt.Printf("stop: %d %s: the server sees this request from another tailnet host than %s; the key's host must be this machine's tailnet name\n", status, body, id)
		return authStop
	case status == http.StatusUnauthorized && (strings.Contains(body, "unknown_harness") || strings.Contains(body, "bad_keyid")):
		fmt.Printf("stop: %d %s: the server does not take %s as a writer name\n", status, body, id)
		return authStop
	case status == http.StatusUnauthorized && strings.Contains(body, "bad_signature"):
		fmt.Printf("stop: %d %s: %s is enrolled with another key; enroll this machine's line (tacit pubkey) in its place\n", status, body, id)
		return authStop
	case status == http.StatusUnauthorized:
		fmt.Printf("stop: %d %s: the token was refused; check this machine's clock (expired) and that the enrolled line is this key's (tacit pubkey)\n", status, body)
		return authStop
	case status == http.StatusTooManyRequests:
		fmt.Printf("retry: %d %s: the server is holding off this address after failed requests; it lifts within a minute\n", status, body)
		return authRetry
	case status == http.StatusForbidden && strings.Contains(body, "public path is read-only"):
		fmt.Printf("stop: %d %s: %s is a writer, and this URL is the server's public path, which only reads; use a reader key here (tacit authcheck reader)\n", status, body, id)
		return authStop
	case status == http.StatusForbidden:
		fmt.Printf("stop: %d %s: the server does not take requests from this network address\n", status, body)
		return authStop
	case status >= 500:
		fmt.Printf("retry: %d %s: the server is not answering right now\n", status, body)
		return authRetry
	}
	fmt.Printf("stop: %d %s\n", status, body)
	return authStop
}
