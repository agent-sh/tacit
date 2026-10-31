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
// "keep", with evidence. By the call's number (from 1; the calls are counted
// in $FAKE_DIR/calls and each call's findings logged in $FAKE_DIR/sent): in
// $FAKE_DROP its stream drops the way Codex reports it once its retries are
// spent (exit 1), in $FAKE_NOTICE it prints a retry notice and then answers,
// in $FAKE_BAD it answers with no JSON, in $FAKE_EMPTY it answers with no
// evidence, which the review page refuses. "all" is every call.
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
evidence="checked at HEAD"
if on "$FAKE_EMPTY"; then evidence=""; fi
mark=both
case "$prompt" in *'"kind":"stale"'*) mark=keep ;; esac
vs=""
for id in $ids; do
  [ -n "$vs" ] && vs="$vs,"
  vs="$vs{\\\"id\\\":\\\"$id\\\",\\\"mark\\\":\\\"$mark\\\",\\\"evidence\\\":\\\"$evidence\\\"}"
done
printf '%s\n' "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"verdicts\\\":[$vs]}\"}}" '{"type":"turn.completed","usage":{"input_tokens":12}}'
`

// fakeModes are the fake Codex's call numbers for each way it answers.
type fakeModes struct{ drop, notice, bad, empty string }

// codexVerify is the review fixture's five open pairs, and a stale finding on
// each fact named in stale (sent after the pairs, in that order), in a
// sandbox where tacit verify runs on the fake Codex. It returns the server's
// repository, the clone, the verifier's state directory and a reader of the
// findings each call was sent, by call.
func codexVerify(t *testing.T, stale []string, m fakeModes) (bare string, rs *reviewServer, state string, sent func() map[int][]string) {
	t.Helper()
	stubScrump(t)
	bare, clone := reviewFixture(t)
	rs = clone("one")
	if len(stale) > 0 {
		var flags strings.Builder
		for _, name := range stale {
			flags.WriteString("- stale? [[" + name + "]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 x: subject - FOO (gone at HEAD)\n")
		}
		os.WriteFile(filepath.Join(rs.s.Dir, "review", "2026-10-02.md"), []byte(flags.String()), 0o644)
		git(t, rs.s.Dir, "add", "-A")
		git(t, rs.s.Dir, "commit", "-q", "-m", "stale findings")
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
	t.Setenv("FAKE_DROP", m.drop)
	t.Setenv("FAKE_NOTICE", m.notice)
	t.Setenv("FAKE_BAD", m.bad)
	t.Setenv("FAKE_EMPTY", m.empty)
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
// is no failure: that batch's verdict lands too. A mark the page refuses
// counts against its finding and fails no run, with the drop as without it.
// Before, the drop failed the run (exit 1) and the notice failed its batch.
func TestVerifyAStreamDropLosesNothingSettled(t *testing.T) {
	bare, rs, state, sent := codexVerify(t, nil, fakeModes{drop: "1", notice: "2", empty: "3"})
	var code int
	errs := captureStderr(t, func() { captureStdout(t, func() { code = runVerify([]string{"--batch", "1"}) }) })
	calls := sent()
	if len(calls) != 5 {
		t.Fatalf("calls %v, want one finding in each of five", calls)
	}
	for i := 1; i <= 5; i++ {
		if len(calls[i]) != 1 {
			t.Fatalf("calls %v, want one finding in each of five", calls)
		}
	}
	dropped, noticed, refused := calls[1][0], calls[2][0], calls[3][0]
	if code != 0 || !strings.Contains(errs, "stream disconnected before completion") || !strings.Contains(errs, "stay open for the next run") || !strings.Contains(errs, "an agent mark needs evidence") {
		t.Fatalf("exit %d after a stream drop with findings settled:\n%s", code, errs)
	}
	marks := serverMarks(t, bare)
	if marks[dropped] != "" || marks[refused] != "" || marks[noticed] == "" || marks[calls[4][0]] == "" || marks[calls[5][0]] == "" {
		t.Fatalf("server marks %v: want %s (dropped) and %s (refused) open, %s (after its retry notice) and the last two marked", marks, dropped, refused, noticed)
	}
	git(t, rs.s.Dir, "fetch", "-q", "origin")
	if ahead := strings.TrimSpace(git(t, rs.s.Dir, "rev-list", "--count", "origin/main..HEAD")); ahead != "0" {
		t.Fatalf("%s commit(s) of the run are not on the server", ahead)
	}
	r := verifyRun{stateDir: filepath.Join(state, "verify")}
	if tries := r.attempts(); tries[dropped] != 0 || tries[refused] != 1 {
		t.Fatalf("attempts %v: want none for the dropped finding and one for the refused mark", tries)
	}
	t.Setenv("FAKE_DROP", "")
	t.Setenv("FAKE_NOTICE", "")
	t.Setenv("FAKE_EMPTY", "")
	captureStderr(t, func() { captureStdout(t, func() { code = runVerify([]string{"--batch", "1"}) }) })
	if code != 0 || serverMarks(t, bare)[dropped] == "" {
		t.Fatalf("the next run: exit %d, the dropped finding's mark %q", code, serverMarks(t, bare)[dropped])
	}
}

// These runs exit 1, with a drop or without: one that settled nothing, one
// with another error after or before the drop, in the pair batches, the
// stale ones or across the two, and a dry run. A stale batch's push the
// server refuses is the run's error, as a pair batch's is, also after an
// earlier error.
func TestVerifyRunsThatStillFail(t *testing.T) {
	for _, c := range []struct {
		name       string
		stale      []string
		modes      fakeModes
		args       []string
		refuse     bool   // the server refuses every push of the run
		staleCalls []int  // the calls that take the stale findings
		say        string // a line the run prints
	}{
		{name: "nothing settled", modes: fakeModes{drop: "all"}},
		{name: "another failure after a drop", modes: fakeModes{drop: "1", bad: "2"}},
		{name: "another failure before a drop", modes: fakeModes{drop: "2", bad: "1"}},
		// five pair batches, then a stale batch for each stale finding
		{name: "a pair batch's failure before a stale drop", stale: []string{"c"}, modes: fakeModes{bad: "1", drop: "6"}, staleCalls: []int{6}},
		{name: "a stale batch's failure before a stale drop", stale: []string{"c", "d"}, modes: fakeModes{bad: "6", drop: "7"}, staleCalls: []int{6, 7}},
		{name: "a stale batch's failure after a pair drop", stale: []string{"c"}, modes: fakeModes{drop: "1", bad: "6"}, staleCalls: []int{6}},
		{name: "a stale batch's failure after a stale drop", stale: []string{"c", "d"}, modes: fakeModes{drop: "6", bad: "7"}, staleCalls: []int{6, 7}},
		{name: "a stale push refused after a pair failure", stale: []string{"c"}, modes: fakeModes{bad: "1 2 3 4 5"}, refuse: true, staleCalls: []int{6}, say: "the server refused this mark"},
		{name: "dry run", modes: fakeModes{drop: "1"}, args: []string{"--dry-run"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bare, rs, _, sent := codexVerify(t, c.stale, c.modes)
			if c.refuse {
				if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte("#!/bin/sh\necho a synthetic refusal >&2\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var code int
			errs := captureStderr(t, func() { captureStdout(t, func() { code = runVerify(append([]string{"--batch", "1"}, c.args...)) }) })
			lines := strings.Split(strings.TrimSpace(errs), "\n")
			if code != 1 || c.say != "" && (strings.Count(errs, c.say) != 1 || !strings.Contains(lines[len(lines)-1], c.say)) {
				t.Fatalf("exit %d, want 1 and %q once, as the run's error:\n%s", code, c.say, errs)
			}
			calls := sent()
			for _, n := range c.staleCalls {
				if ids := calls[n]; len(ids) != 1 || rs.staleByID(ids[0]) == nil {
					t.Fatalf("call %d was sent %v, not a stale finding", n, ids)
				}
			}
		})
	}
}
