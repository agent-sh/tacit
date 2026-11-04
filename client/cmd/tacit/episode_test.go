package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/events"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

const episodeSessionID = "5b1e0c3a-1111-4222-8333-444455556666"

// episodeHome is a home with one Claude Code session transcript: the
// owner's request naming a code review, the agent's command (with a secret
// the redactor masks), its output and the agent's reply. The harness gives
// the session id as Claude Code does.
func episodeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", episodeSessionID)
	t.Setenv("TACIT_SESSION", "")
	stubScrump(t)
	head := `"sessionId":"` + episodeSessionID + `","cwd":"/work/demo","entrypoint":"cli"`
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-demo", episodeSessionID+".jsonl"),
		`{"type":"user",`+head+`,"promptSource":"typed","uuid":"u1","timestamp":"2026-11-01T10:00:00Z","message":{"role":"user","content":"deploy the release candidate CR-123456789 to staging"}}`,
		`{"type":"assistant",`+head+`,"uuid":"a1","timestamp":"2026-11-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"./deploy.sh CR-123456789 --token Zq7Vx9"}}]}}`,
		`{"type":"user",`+head+`,"uuid":"u2","timestamp":"2026-11-01T10:00:09Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"deployed CR-123456789 with Zq7Vx9 to staging-2"}]}}`,
		`{"type":"assistant",`+head+`,"uuid":"a2","timestamp":"2026-11-01T10:00:12Z","message":{"role":"assistant","content":[{"type":"text","text":"Deployed to staging-2."}]}}`,
		// An earlier recall of the same words and its answer: no event of what was recalled.
		`{"type":"assistant",`+head+`,"uuid":"a3","timestamp":"2026-11-01T10:05:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__tacit__tacit_episode","input":{"find":"CR-123456789"}}]}}`,
		`{"type":"user",`+head+`,"uuid":"u3","timestamp":"2026-11-01T10:05:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"- 1.0 11-01 10:00 owner owner_text: deploy the release candidate CR-123456789"}]}}`,
	)
	return home
}

// episodeOut runs `tacit episode args...` and returns what it printed and
// its exit code.
func episodeOut(t *testing.T, args ...string) (string, int) {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wr
	code := runEpisode(args)
	wr.Close()
	os.Stdout = old
	b, _ := io.ReadAll(rd)
	return string(b), code
}

var refRx = regexp.MustCompile(`(?m)^[->] (\d+\.\d+) `)

// Find gives one line per event of this session holding the words, newest
// first, each with its ref, who wrote it and its kind; around gives the
// events on each side of a ref; show gives one event whole. The secret in
// the command and its output is masked in every answer.
func TestEpisodeFindAroundShow(t *testing.T) {
	episodeHome(t)
	out, code := episodeOut(t, "find", "CR-123456789")
	if code != 0 || !strings.Contains(out, "3 of 3 event(s) of this session (claude) holding") || strings.Contains(out, "tacit_episode") {
		t.Fatalf("find, the session's own recall left out (%d):\n%s", code, out)
	}
	for _, want := range []string{"owner owner_text", "agent tool_call Bash", "tool_result Bash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("find lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("find showed the secret:\n%s", out)
	}
	refs := refRx.FindAllStringSubmatch(out, -1)
	if len(refs) < 3 {
		t.Fatalf("no refs:\n%s", out)
	}
	newest, oldest := refs[0][1], refs[len(refs)-1][1]
	around, code := episodeOut(t, "--n", "1", "around", oldest)
	if code != 0 || !strings.Contains(around, "> "+oldest+" ") || strings.Count(around, "\n- ") != 1 {
		t.Fatalf("around %s (%d):\n%s", oldest, code, around)
	}
	show, code := episodeOut(t, "show", newest)
	if code != 0 || !strings.Contains(show, "## ") || !strings.Contains(show, "\n~~~\n") || strings.Contains(show, "Zq7Vx9") || !strings.Contains(show, "******") {
		t.Fatalf("show %s (%d):\n%s", newest, code, show)
	}
	if out, code := episodeOut(t, "show", "1.99"); code != 1 || !strings.Contains(out, "no event 1.99") {
		t.Fatalf("an unknown ref (%d):\n%s", code, out)
	}
	// before pages back: only events before the ref.
	older, code := episodeOut(t, "--before", newest, "find", "CR-123456789")
	if code != 0 || strings.Contains(older, "- "+newest+" ") || len(refRx.FindAllString(older, -1)) != len(refs)-1 {
		t.Fatalf("before %s (%d):\n%s", newest, code, older)
	}
}

// Without a redactor nothing of the session is shown, on stdout or stderr.
func TestEpisodeShowsNothingWithoutRedaction(t *testing.T) {
	episodeHome(t)
	t.Setenv("PATH", t.TempDir())
	rd, wr, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = wr
	out, code := episodeOut(t, "find", "CR-123456789")
	wr.Close()
	os.Stderr = old
	errOut, _ := io.ReadAll(rd)
	for _, s := range []string{out, string(errOut)} {
		if code == 0 || strings.Contains(s, "to staging") || strings.Contains(s, "deploy.sh") || strings.Contains(s, "Zq7Vx9") {
			t.Fatalf("without scrump (%d):\n%s\nstderr:\n%s", code, out, errOut)
		}
	}
}

// A session of tacit's own model runs is never read, and a session whose
// transcript is not on this machine is said to be so.
func TestEpisodeRefusesExcludedAndAbsentSessions(t *testing.T) {
	home := episodeHome(t)
	os.MkdirAll(filepath.Dir(modelrun.ExcludeFile(home)), 0o700)
	if err := os.WriteFile(modelrun.ExcludeFile(home), []byte(episodeSessionID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := episodeOut(t, "find", "CR-123456789"); code != 1 || !strings.Contains(out, "tacit's own model runs") {
		t.Fatalf("an excluded session (%d):\n%s", code, out)
	}
	// Part of an excluded id finds its transcript by its name's end: refused too.
	if out, code := episodeOut(t, "--session", episodeSessionID[len(episodeSessionID)-12:], "find", "CR-123456789"); code != 1 || !strings.Contains(out, "tacit's own model runs") {
		t.Fatalf("a part of an excluded id (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "--session", "9f9f9f9f-0000-4000-8000-000000000000", "find", "x"); code != 1 || !strings.Contains(out, "not on this machine") {
		t.Fatalf("an absent session (%d):\n%s", code, out)
	}
}

// A fact leads to the session its owner's quote came from, at the quote's
// words.
func TestEpisodeFactLeadsToItsSession(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	home := episodeHome(t)
	_ = home
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	writeFact(t, r.clone, fact.Meta{Name: "demo-release-candidates-go-to-staging", Description: "release candidates are deployed to staging first",
		Type: "user_standard", Scope: "projects/demo", Source: fact.Source{Kind: "user", OwnerQuote: "deploy the release candidate", OwnerQuoteFrom: "claude:" + episodeSessionID}}, "staging first")
	out, code := episodeOut(t, "--fact", "demo-release-candidates-go-to-staging")
	if code != 0 || !strings.Contains(out, "the session demo-release-candidates-go-to-staging came from") || !strings.Contains(out, "owner owner_text") {
		t.Fatalf("fact (%d):\n%s", code, out)
	}
}

// A curated fact whose origin is a dreamer note (harness dreamer, which names
// no transcript's harness) leads to the note's session, found by the note's
// frontmatter name though its file took a suffix.
func TestEpisodeFactFromADreamerNote(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	note := &fact.Fact{Path: "inbox/dreamer@host-a/staging-first-2.md", Meta: fact.Meta{Name: "staging-first", Description: "release candidates go to staging first",
		Type: "unclassified", Scope: "projects/demo", Created: "2026-11-01", Source: fact.Source{Harness: "dreamer", Host: "host-a", Kind: "observed", Session: episodeSessionID}}, Body: "staging"}
	b, err := note.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(r.clone, "inbox", "dreamer@host-a"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(note.Path)), b, 0o644)
	writeFact(t, r.clone, fact.Meta{Name: "demo-candidates-go-to-staging", Description: "release candidates are deployed to staging first",
		Type: "design_decision", Scope: "projects/demo", Origin: "staging-first"}, "staging first")
	out, code := episodeOut(t, "--fact", "demo-candidates-go-to-staging")
	if code != 0 || !strings.Contains(out, "came from (claude)") {
		t.Fatalf("a dreamer note's session (%d):\n%s", code, out)
	}
}

// A named session is looked up under every harness, the caller's first: a
// Codex rollout from a Claude Code caller.
func TestEpisodeNamedSessionOfAnotherHarness(t *testing.T) {
	home := episodeHome(t)
	id := "019a0000-aaaa-7bbb-8ccc-dddddddddddd"
	writeTranscript(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-00-00-"+id+".jsonl"),
		`{"timestamp":"2026-11-04T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work/demo","originator":"codex_cli_rs","source":"cli"}}`,
		`{"timestamp":"2026-11-04T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"look up ticket TK-555123"}]}}`)
	if out, code := episodeOut(t, "--session", id, "find", "TK-555123"); code != 0 || !strings.Contains(out, "(codex)") {
		t.Fatalf("a Codex session from a Claude Code caller (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "--harness", "claude", "--session", id, "find", "TK-555123"); code != 1 || !strings.Contains(out, "not on this machine") {
		t.Fatalf("a named harness restricts the lookup (%d):\n%s", code, out)
	}
}

// A read past its budget says so: find, that the newer events were not
// searched; around and show, that it stopped before the ref. What a read
// found is redacted on a clock of its own.
func TestEpisodeTimeLimit(t *testing.T) {
	episodeHome(t)
	prev := episodeBudget
	episodeBudget = time.Nanosecond
	t.Cleanup(func() { episodeBudget = prev })
	if out, _ := episodeOut(t, "find", "CR-123456789"); !strings.Contains(out, "the newer events were not searched") {
		t.Fatalf("find past the budget:\n%s", out)
	}
	if out, code := episodeOut(t, "show", "999.0"); code != 1 || !strings.Contains(out, "time limit before 999.0") {
		t.Fatalf("show past the budget (%d):\n%s", code, out)
	}
	cut, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := redactFound(cut, []events.Event{{Text: "token Zq7Vx9"}})
	if err != nil || len(got) != 1 || strings.Contains(got[0].Text, "Zq7Vx9") {
		t.Fatalf("redaction after the read's deadline: %v %+v", err, got)
	}
}

// A rune whose lower case is longer (U+023A) does not throw the clip off.
func TestEpisodeClipKeepsToRunes(t *testing.T) {
	text := strings.Repeat("\u023a", 300) + " needle here"
	if got := clipLine(text, []string{"needle"}, 40); !strings.Contains(got, "needle") {
		t.Fatalf("clip: %q", got)
	}
}
