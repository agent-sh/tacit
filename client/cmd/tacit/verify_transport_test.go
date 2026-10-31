package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCodex stands in for the Codex CLI on the verifier's portable protocol.
// It answers every finding in its prompt, a pair "both" and a stale finding
// "keep". By the call's number (from 1; the calls are counted in
// $FAKE_DIR/calls and each call's findings logged in $FAKE_DIR/sent): in
// $FAKE_DROP its stream drops the way Codex reports it once its retries are
// spent (exit 1), in $FAKE_NOTICE it prints a retry notice and then answers,
// in $FAKE_BAD it answers with no JSON. "all" is every call.
const fakeCodex = `#!/bin/sh
prompt=$(cat)
n=$(( $(cat "$FAKE_DIR/calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$FAKE_DIR/calls"
ids=$(printf '%s' "$prompt" | grep -oE '"(ID|id)":"[0-9a-f]+"' | cut -d'"' -f4 | tr '\n' ' ')
echo "$n $ids" >> "$FAKE_DIR/sent"
on() { case " $1 " in *" $n "*|" all ") return 0 ;; esac; return 1; }
notice='{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion: Transport error: timeout)"}'
if on "$FAKE_DROP"; then
  printf '%s\n' "$notice" '{"type":"turn.failed","error":{"message":"stream disconnected before completion: Transport error: timeout"}}'
  exit 1
fi
if on "$FAKE_BAD"; then
  printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"no verdicts here"}}' '{"type":"turn.completed","usage":{"input_tokens":12}}'
  exit 0
fi
if on "$FAKE_NOTICE"; then printf '%s\n' "$notice"; fi
mark=both
case "$prompt" in *'"kind":"stale"'*) mark=keep ;; esac
vs=""
for id in $ids; do
  [ -n "$vs" ] && vs="$vs,"
  vs="$vs{\\\"id\\\":\\\"$id\\\",\\\"mark\\\":\\\"$mark\\\",\\\"evidence\\\":\\\"checked at HEAD\\\"}"
done
printf '%s\n' "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"verdicts\\\":[$vs]}\"}}" '{"type":"turn.completed","usage":{"input_tokens":12}}'
`

// codexVerify is the review fixture's five open pairs (with stale, and a
// stale finding on c, sent after them) in a sandbox where tacit verify runs
// on the fake Codex. drop, notice and bad set its modes. It returns the
// server's repository, the clone, the verifier's state directory and a reader
// of the findings each call was sent, by call.
func codexVerify(t *testing.T, stale bool, drop, notice, bad string) (bare string, rs *reviewServer, state string, sent func() map[int][]string) {
	t.Helper()
	stubScrump(t)
	bare, clone := reviewFixture(t)
	rs = clone("one")
	if stale {
		os.WriteFile(filepath.Join(rs.s.Dir, "review", "2026-10-02.md"), []byte("- stale? [[c]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 x: subject - FOO (gone at HEAD)\n"), 0o644)
		git(t, rs.s.Dir, "add", "-A")
		git(t, rs.s.Dir, "commit", "-q", "-m", "stale c")
		git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	}
	state, _ = sandboxVerify(t, rs.s.Dir)
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_VERIFY_RUNNER", "codex")
	t.Setenv("TACIT_CODEX_COMMAND", bin)
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_DROP", drop)
	t.Setenv("FAKE_NOTICE", notice)
	t.Setenv("FAKE_BAD", bad)
	return bare, rs, state, func() map[int][]string {
		b, _ := os.ReadFile(filepath.Join(dir, "sent"))
		calls := map[int][]string{}
		for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				calls[i+1] = f[1:]
			}
		}
		return calls
	}
}

// serverMarks reads the agent decisions on the server's main, by finding.
func serverMarks(t *testing.T, bare string) map[string]string {
	t.Helper()
	marks := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(git(t, bare, "show", "main:review/decisions.jsonl")), "\n") {
		var d decision
		if json.Unmarshal([]byte(line), &d) == nil && d.By == "agent" {
			marks[d.ID] = d.Action
		}
	}
	return marks
}

// One batch's runner stream drops while the others settle their findings,
// as when a Codex verifier's stream to the provider timed out: each batch's
// marks land on the server as it ends, so the drop loses nothing settled,
// and the run exits 0. The dropped finding stays open, its attempt not
// counted, and the next run settles it. A retry notice Codex recovered from
// is no failure: that batch's verdicts land too. Before, the drop failed the
// run (exit 1) and the notice failed its batch.
func TestVerifyAStreamDropLosesNothingSettled(t *testing.T) {
	bare, rs, state, sent := codexVerify(t, false, "1", "2", "")
	var code int
	errs := captureStderr(t, func() { captureStdout(t, func() { code = runVerify([]string{"--batch", "1"}) }) })
	calls := sent()
	if len(calls) != 5 || len(calls[1]) != 1 || len(calls[2]) != 1 {
		t.Fatalf("calls %v, want one finding in each of five", calls)
	}
	dropped, noticed := calls[1][0], calls[2][0]
	if code != 0 || !strings.Contains(errs, "stream disconnected before completion") || !strings.Contains(errs, "stay open for the next run") {
		t.Fatalf("exit %d after a stream drop with findings settled:\n%s", code, errs)
	}
	marks := serverMarks(t, bare)
	if _, ok := marks[dropped]; ok || marks[noticed] == "" || len(marks) == 0 {
		t.Fatalf("server marks %v: want %s open and %s, after its retry notice, marked", marks, dropped, noticed)
	}
	git(t, rs.s.Dir, "fetch", "-q", "origin")
	if ahead := strings.TrimSpace(git(t, rs.s.Dir, "rev-list", "--count", "origin/main..HEAD")); ahead != "0" {
		t.Fatalf("%s commit(s) of the run are not on the server", ahead)
	}
	r := verifyRun{stateDir: filepath.Join(state, "verify")}
	if n := r.attempts()[dropped]; n != 0 {
		t.Fatalf("the dropped finding's attempt counted: %d", n)
	}
	t.Setenv("FAKE_DROP", "")
	t.Setenv("FAKE_NOTICE", "")
	captureStderr(t, func() { captureStdout(t, func() { code = runVerify([]string{"--batch", "1"}) }) })
	if code != 0 || serverMarks(t, bare)[dropped] == "" {
		t.Fatalf("the next run: exit %d, the dropped finding's mark %q", code, serverMarks(t, bare)[dropped])
	}
}

// A drop lets the run pass only when something settled and nothing else
// failed: a run with nothing settled, a run with another failure before or
// after the drop, also one in a pair batch before a stale batch's drop, and
// a dry run still exit 1.
func TestVerifyAStreamDropStillFailsTheRunOtherwise(t *testing.T) {
	for _, c := range []struct {
		name             string
		stale            bool
		drop, bad        string
		args             []string
		droppedStaleCall int
	}{
		{name: "nothing settled", drop: "all"},
		{name: "another failure after a drop", drop: "1", bad: "2"},
		{name: "another failure before a drop", drop: "2", bad: "1"},
		// five pair batches, then the stale finding's
		{name: "another failure before a stale drop", stale: true, drop: "6", bad: "1", droppedStaleCall: 6},
		{name: "dry run", drop: "1", args: []string{"--dry-run"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, rs, _, sent := codexVerify(t, c.stale, c.drop, "", c.bad)
			var code int
			errs := captureStderr(t, func() { captureStdout(t, func() { code = runVerify(append([]string{"--batch", "1"}, c.args...)) }) })
			if code != 1 {
				t.Fatalf("exit %d, want 1:\n%s", code, errs)
			}
			if c.droppedStaleCall > 0 {
				ids := sent()[c.droppedStaleCall]
				if len(ids) != 1 || rs.staleByID(ids[0]) == nil {
					t.Fatalf("call %d was sent %v, not the stale finding", c.droppedStaleCall, ids)
				}
			}
		})
	}
}
