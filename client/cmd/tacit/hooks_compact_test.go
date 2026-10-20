package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The PreCompact hook starts `tacit dream --session <transcript>` detached
// and returns at once: the harness waits for it before it compacts. The
// stand-in binary takes two seconds; the hook must not.
func TestPreCompactHookStartsTheDreamAndReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	home, transcript, _ := dreamHome(t)
	out := filepath.Join(t.TempDir(), "args")
	fake := filepath.Join(t.TempDir(), "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\nsleep 2\nprintf '%s\\n' \"$*\" > '"+out+"'\n"), 0o700)
	previous := hookExe
	defer func() { hookExe = previous }()
	hookExe = func() (string, error) { return fake, nil }
	env := filepath.Join(home, ".config", "tacit", "env")
	os.MkdirAll(filepath.Dir(env), 0o700)
	os.WriteFile(env, nil, 0o600)

	began := time.Now()
	printed := hookOut(t, `{"session_id":"s1","transcript_path":"`+transcript+`","cwd":"`+home+`","hook_event_name":"PreCompact","trigger":"auto"}`, "pre-compact")
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Fatalf("the hook waited for the dream: %s", took)
	}
	if printed != "" {
		t.Fatalf("the hook printed %q", printed)
	}
	var got []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, _ = os.ReadFile(out); len(got) > 0 {
			break
		}
	}
	if want := "dream --session " + transcript + " --env-file " + env + "\n"; string(got) != want {
		t.Fatalf("started %q, want %q", got, want)
	}
	// No transcript: nothing to start, still exit 0.
	os.Remove(out)
	hookOut(t, `{"session_id":"s1","hook_event_name":"PreCompact"}`, "pre-compact")
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a dream started with no transcript")
	}
}

// After a compaction the start block brings what the session was not given
// yet, and the session's own dreamer notes no curator has split: labelled,
// never a RULE line, at most three, each once. A note of another session, or
// one the curator already split, is not among them; a plain start and a
// compact start each answer only their own source.
func TestCompactStartDeliversTheSessionsOwnNotes(t *testing.T) {
	// Filler, so the repo's name is a rare term the start query ranks by.
	r := newRig(t, rigOpts{repos: []string{"demo"}, filler: 200, rules: 1})
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	host := store.ShortHost()
	note := func(name, session, kind, desc string) {
		writeFactFile(t, r.clone, &fact.Fact{Path: "inbox/dreamer@" + host + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "machine_fact", Scope: "projects/demo",
			Source: fact.Source{Harness: "dreamer", Host: host, Kind: kind, Session: session}, Created: "2026-10-20"}, Body: desc + "\n"})
	}
	note("own-cache-path", "s1", "observed", "The demo fixture cache lives in /srv/demo-cache")
	note("own-port", "s1", "observed", "The demo fixture server listens on port 7781")
	note("own-owner-says", "s1", "user", "Owner: never push the demo fixture branch")
	note("own-fourth", "s1", "observed", "The demo fixture build needs make 4.4")
	note("own-curated", "s1", "observed", "The demo fixture logs go to /var/log/demo")
	note("other-session", "s2", "observed", "The demo fixture uses the blue queue")
	writeFact(t, r.clone, fact.Meta{Name: "demo-logs", Description: "The demo fixture logs go to /var/log/demo", Type: "machine_fact", Scope: "projects/demo", Origin: "own-curated"}, "x")
	r.commitAs("fixture notes")
	cwd := filepath.Join(r.projects, "demo")
	payload := func(source string) string {
		return `{"session_id":"s1","cwd":"` + cwd + `","transcript_path":"/nowhere/s1.jsonl","hook_event_name":"SessionStart","source":"` + source + `"}`
	}
	start := hookOut(t, payload("startup"), "session-start")
	if !strings.Contains(start, "demo-note") || strings.Contains(start, "own-") {
		t.Fatalf("start block:\n%s", start)
	}
	if out := hookOut(t, payload("compact"), "session-start"); out != "" {
		t.Fatalf("the startup entry answered a compaction:\n%s", out)
	}
	if out := hookOut(t, payload("startup"), "session-start", "--compact"); out != "" {
		t.Fatalf("the compact entry answered a start:\n%s", out)
	}
	compact := hookOut(t, payload("compact"), "session-start", "--compact")
	own := 0
	for _, l := range strings.Split(compact, "\n") {
		if strings.Contains(l, "(this session, not yet reviewed)") {
			own++
			if strings.HasPrefix(l, "- RULE") {
				t.Fatalf("an unreviewed note became a rule: %s", l)
			}
		}
		if name, _, ok := strings.Cut(strings.TrimPrefix(l, "- "), ":"); ok && strings.HasPrefix(l, "- ") && strings.Contains(start, "- "+name+":") {
			t.Fatalf("delivered twice in one session: %s", l)
		}
	}
	if own != 3 || strings.Contains(compact, "other-session") || strings.Contains(compact, "own-curated") {
		t.Fatalf("compact block (%d own):\n%s", own, compact)
	}
	again := hookOut(t, payload("compact"), "session-start", "--compact")
	if strings.Count(again, "(this session, not yet reviewed)") != 1 {
		t.Fatalf("the second compaction: the one note left, once:\n%s", again)
	}
}
