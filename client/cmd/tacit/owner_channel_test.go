package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// tacit review is the owner's channel in every build: the page the desktop
// window shows, with every owner write, behind a token that rides only on
// the URL it opens. Without the token neither the page nor a call answers,
// so a client that can reach the port but was not handed the URL (an agent)
// cannot read the token or drive a write.
func TestReviewIsTheOwnersChannel(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("review")
	rs.token = "tok"
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, p := range []string{"/", "/?t=", "/?t=wrong", "/index.html", "/?token=tok"} {
		if code, body := get(p); code != http.StatusForbidden || strings.Contains(body, "tok\"") || strings.Contains(body, "<html") {
			t.Errorf("GET %s: %d %q", p, code, body)
		}
	}
	code, page := get("/?t=tok")
	if code != http.StatusOK || !strings.Contains(page, `content="tok"`) || !strings.Contains(page, "/desk.js") || !strings.Contains(page, `data-view="folders"`) || !strings.Contains(page, "new-ruling") {
		t.Fatalf("the owner's page: %d %.200s", code, page)
	}
	if code, _ := get("/desk.js"); code != http.StatusOK {
		t.Fatalf("assets: %d", code)
	}

	call := func(method, token string, args ...any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(args)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/call/"+method, bytes.NewReader(b))
		if token != "" {
			req.Header.Set("X-Tacit-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	head := func() string { return strings.TrimSpace(git(t, rs.s.Dir, "rev-parse", "HEAD")) }
	before := head()
	for _, tok := range []string{"", "wrong", "tokk"} {
		if code, _ := call("Retire", tok, "a"); code != http.StatusForbidden {
			t.Fatalf("Retire with token %q: %d", tok, code)
		}
	}
	if head() != before {
		t.Fatal("a refused call wrote")
	}
	subject := func() string { return strings.TrimSpace(git(t, rs.s.Dir, "log", "-1", "--format=%s")) }
	ok := func(method string, args ...any) {
		t.Helper()
		code, out := call(method, "tok", args...)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", method, code, out)
		}
	}
	ok("Retire", "a")
	if s := subject(); s != "owner: retire a" {
		t.Fatalf("retire subject %q", s)
	}
	ok("Restore", "a")
	if s := subject(); s != "owner: restore a" {
		t.Fatalf("restore subject %q", s)
	}
	_, det := call("Fact", "tok", "a")
	rev, _ := det["result"].(map[string]any)["rev"].(string)
	ok("Edit", "a", map[string]any{"rev": rev, "description": "a, edited by the owner", "body": "body of a", "scope": "system", "type": "design_decision", "applies_to": map[string]any{"repos": []string{"x"}}})
	if s := subject(); !strings.HasPrefix(s, "owner: edit a") {
		t.Fatalf("edit subject %q", s)
	}
	ok("Undo", "a")
	if s := subject(); !strings.HasPrefix(s, "owner: undo") {
		t.Fatalf("undo subject %q", s)
	}
	ok("NewRuling", map[string]any{"name": "no-friday-deploys", "body": "Never deploy on a Friday.", "scope": "user"})
	if s := subject(); !strings.HasPrefix(s, "owner: new ruling") {
		t.Fatalf("new ruling subject %q", s)
	}

	// The Folders page's actions, once the store holds the format marker.
	seed := filepath.Join(t.TempDir(), "marker")
	git(t, filepath.Dir(seed), "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	os.WriteFile(filepath.Join(seed, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "format")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	ok("FolderSave", folderDraft{Action: "create", Folder: "projects/x", Kind: "repo", Description: "the x service"})
	if s := subject(); s != "owner: folder create projects/x" {
		t.Fatalf("folder subject %q", s)
	}
	ok("FolderSave", folderDraft{Action: "status", Folder: "projects/x", Status: "dormant", Why: "paused for the quarter"})
	ok("FolderSave", folderDraft{Action: "alias", Folder: "projects/x", Alias: "xsvc"})
	ok("FolderSave", folderDraft{Action: "create", Folder: "areas/ticketing", Kind: "area", Description: "tickets", Tools: []string{"mcp__*__Ticketing*"}})
	if s := subject(); s != "owner: folder create areas/ticketing" {
		t.Fatalf("area subject %q", s)
	}
	if b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "projects", "x", "_record.md")); !strings.Contains(string(b), "by: owner") || !strings.Contains(string(b), "xsvc") {
		t.Fatalf("record:\n%s", b)
	}
}

// Label mode serves the review page alone: its marks go to the label file,
// and no owner write is reachable. Its reads need the token too.
func TestReviewLabelModeHasNoOwnerWrites(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("label")
	rs.token = "tok"
	rs.labelFile = filepath.Join(t.TempDir(), "labels.jsonl")
	rs.sampleIDs = map[string]bool{"x": true}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/?t=tok")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<title>tacit review</title>") || strings.Contains(string(b), "desk.js") {
		t.Fatalf("label page: %d %.200s", resp.StatusCode, b)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/call/Retire", strings.NewReader(`["a"]`))
	req.Header.Set("X-Tacit-Token", "tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an owner write in label mode: %v %v", resp, err)
	}
	if resp, _ := http.Get(srv.URL + "/api/items"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("items without the token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/items", nil)
	req.Header.Set("X-Tacit-Token", "tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("items with the token: %v %v", resp, err)
	}
}

// The link that carries the token is printed only to a terminal; through a
// pipe (an agent's shell) the line names the address alone, and with
// --no-open and no terminal the server does not start.
func TestOwnerLinkOnlyOnATerminal(t *testing.T) {
	var b bytes.Buffer
	url := announceOwner(&b, "review", "127.0.0.1:7448", "s3cret", "", false)
	if url != "http://127.0.0.1:7448/?t=s3cret" || strings.Contains(b.String(), "s3cret") || !strings.Contains(b.String(), "http://127.0.0.1:7448/") {
		t.Fatalf("pipe: %q %q", url, b.String())
	}
	b.Reset()
	announceOwner(&b, "review", "127.0.0.1:7448", "s3cret", "", true)
	if !strings.Contains(b.String(), "http://127.0.0.1:7448/?t=s3cret") {
		t.Fatalf("terminal: %q", b.String())
	}
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return false }
	defer func() { stdoutIsTerminal = old }()
	if code := serveOwner("review", "127.0.0.1:0", "s3cret", "", true, http.NotFoundHandler()); code != 2 {
		t.Fatalf("--no-open without a terminal: %d", code)
	}
	if code := serveOwner("review", "0.0.0.0:0", "s3cret", "", false, http.NotFoundHandler()); code != 2 {
		t.Fatalf("a non-loopback address: %d", code)
	}
}

// tacit review --fact <name> (and tacit app --browser --fact): the link the
// terminal shows carries the fact after the token, as a fragment the server
// never sees; through a pipe neither the token nor the link is printed. A
// name the store does not hold still opens the page (which says so), with a
// line on stderr. --fact needs the owner's page: label mode and the desktop
// window refuse it.
func TestOwnerLinkOpensOnANamedFact(t *testing.T) {
	if got := ownerURL("127.0.0.1:7448", "s3cret", "rule"); got != "http://127.0.0.1:7448/?t=s3cret#fact=rule" {
		t.Fatalf("link %q", got)
	}
	if got := ownerURL("127.0.0.1:7448", "s3cret", "a b/c"); got != "http://127.0.0.1:7448/?t=s3cret#fact=a%20b%2Fc" {
		t.Fatalf("an odd name is escaped: %q", got)
	}
	var b bytes.Buffer
	link := announceOwner(&b, "review", "127.0.0.1:7448", "s3cret", "rule", true)
	if link != "http://127.0.0.1:7448/?t=s3cret#fact=rule" || !strings.Contains(b.String(), link) {
		t.Fatalf("terminal: %q %q", link, b.String())
	}
	b.Reset()
	announceOwner(&b, "review", "127.0.0.1:7448", "s3cret", "rule", false)
	if strings.Contains(b.String(), "s3cret") || strings.Contains(b.String(), "#fact") {
		t.Fatalf("pipe: %q", b.String())
	}

	_, clone := ownerFixture(t)
	rs := clone("open")
	for _, name := range []string{"", "rule", "engine-retries", "note"} {
		if note := openFactNote(rs, name); note != "" {
			t.Errorf("%q: %q", name, note)
		}
	}
	if note := openFactNote(rs, "no-such-rule"); !strings.Contains(note, "no fact is named no-such-rule") || !strings.Contains(note, "the page opens and says so") {
		t.Fatalf("unknown fact: %q", note)
	}
	// The page asks Fact for the name; an unknown one is the error the page turns into its message.
	if _, err := rs.factDetail(ctxT(t), "no-such-rule"); err == nil || !strings.HasPrefix(err.Error(), "no fact named no-such-rule") {
		t.Fatalf("Fact of an unknown name: %v", err)
	}

	// The page itself is served the same with or without a fragment: the
	// browser keeps #fact=... and the token rules do not change.
	rs.token = "tok"
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()
	for path, want := range map[string]int{"/?t=tok#fact=rule": http.StatusOK, "/#fact=rule": http.StatusForbidden, "/?t=wrong#fact=rule": http.StatusForbidden} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want || (want == http.StatusOK && !strings.Contains(string(page), `id="top-q"`)) {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
	}

	if code := runApp([]string{"--fact", "rule"}); code != 2 {
		t.Errorf("app --fact without --browser: %d", code)
	}
	if code := runReview([]string{"--fact", "rule", "--label", "sample.json"}); code != 2 {
		t.Errorf("review --fact in label mode: %d", code)
	}
}
