package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The judge's environment keeps only what a process needs to find its tools
// and the subscription's login: no Bedrock or AWS route, no gateway, no other
// config directory; a refused name that the keep list would pass is an error.
func TestJudgeEnvNeverBedrock(t *testing.T) {
	env, err := judgeEnv([]string{"PATH=/bin", "HOME=/h", "AWS_REGION=x", "AWS_BEARER_TOKEN_BEDROCK=t", "CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_BASE_URL=https://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k", "ANTHROPIC_CUSTOM_HEADERS=h", "CLAUDE_CONFIG_DIR=/c",
		"CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_PLUGIN_DIRS=/p", "ANTHROPIC_MODEL=m", "LANG=C.UTF-8"})
	if err != nil || strings.Join(env, " ") != "PATH=/bin HOME=/h LANG=C.UTF-8" {
		t.Fatalf("kept: %v %v", env, err)
	}
	old := judgeEnvKeep
	t.Cleanup(func() { judgeEnvKeep = old })
	judgeEnvKeep = regexp.MustCompile(`^(PATH|AWS_REGION)$`)
	if _, err := judgeEnv([]string{"PATH=/bin", "AWS_REGION=x"}); err == nil {
		t.Fatal("a route variable was passed")
	}
}

// The judge's call is `claude -p` with the reduced environment, local
// settings only, no MCP server, no tools and no session kept.
func TestJudgeCommand(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_PROFILE", "p")
	cmd, err := judgeCommand(context.Background(), "opus", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if judgeEnvRefuse.MatchString(strings.SplitN(kv, "=", 2)[0]) {
			t.Fatalf("passed %s", kv)
		}
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"claude -p", "--model opus", "--tools  ", "--setting-sources local", "--strict-mcp-config", "--no-session-persistence"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
}

type judgeFixture struct {
	state, tr string
	off       int64
	now       time.Time
	sent      []string
	refuse    bool
}

// newJudgeFixture writes a transcript (the prompt written after the
// block's moment, as the hook can run first; a push's call before its
// moment) and a delivery log of a prompt block of n notes, a push block, a
// start block and a fresh block.
func newJudgeFixture(t *testing.T, n int) *judgeFixture {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := &judgeFixture{state: filepath.Join(home, "state"), now: time.Now()}
	t.Setenv("TACIT_STATE_DIR", f.state)
	t.Setenv("TACIT_DELIVERY_JUDGE", "on")
	f.tr = filepath.Join(home, ".claude", "projects", "-demo", "s1.jsonl")
	os.MkdirAll(filepath.Dir(f.tr), 0o755)
	first := `{"type":"user","timestamp":"2026-11-01T09:00:00Z","sessionId":"s1","userType":"external","message":{"role":"user","content":"an earlier request"}}` + "\n"
	rest := `{"type":"user","timestamp":"2026-11-01T10:00:00Z","sessionId":"s1","userType":"external","message":{"role":"user","content":"fix the scheduler lock race SECRET-a"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"I will take the shard lock first."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./sched"}}]}}` + "\n"
	os.WriteFile(f.tr, []byte(first+rest), 0o644)
	f.off = int64(len(first))
	pushOff := int64(len(first + rest))
	os.WriteFile(f.tr, []byte(first+rest+`{"type":"user","timestamp":"2026-11-01T10:02:00Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok sched"}]}}`+"\n"), 0o644)
	old := f.now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	var cands []logCand
	for i := 0; i < n; i++ {
		cands = append(cands, logCand{Name: fmt.Sprintf("note-%d", i), Delivered: true, Prov: "agent"})
	}
	cands = append(cands, logCand{Name: "not-shown", Delivered: false})
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a1", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off, Cands: cands}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a2", TS: old, Hook: "push", Tool: "Bash", Session: "s1", TranscriptPath: f.tr, Transcript: pushOff,
		Cands: []logCand{{Name: "push-note", Delivered: true}}}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a3", TS: old, Hook: "start", Session: "s1", TranscriptPath: f.tr, Transcript: 0,
		Cands: []logCand{{Name: "start-note", Delivered: true}}}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a4", TS: f.now.UTC().Format(time.RFC3339Nano), Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off,
		Cands: []logCand{{Name: "too-fresh", Delivered: true}}}, f.now)
	call, red, rnd := judgeCall, judgeRedactTexts, judgeRand
	t.Cleanup(func() { judgeCall, judgeRedactTexts, judgeRand = call, red, rnd })
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = strings.ReplaceAll(s, "SECRET", "[redacted]")
		}
		return out, nil
	}
	judgeRand = func() float64 { return 0 } // every line drawn
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		if f.refuse {
			return "", "", errJudgeRefused
		}
		f.sent = append(f.sent, prompt)
		var ls []string
		for i := 1; i <= strings.Count(prompt, "## Case "); i++ {
			ls = append(ls, fmt.Sprintf(`{"i":%d,"relevant":"yes","confused":"no","novel":"unsure"}`, i))
		}
		return `{"lines":[` + strings.Join(ls, ",") + `]}`, "claude-test-model", nil
	}
	return f
}

func judgeEntries(t *testing.T, state string) (verdicts []judgeVerdict, passes []judgePassLog) {
	for _, e := range readDeliveryLog(t, state) {
		switch {
		case e.Hook == "judge" && e.Judge != nil:
			verdicts = append(verdicts, *e.Judge)
		case e.Hook == "judge-pass" && e.JudgePass != nil:
			passes = append(passes, *e.JudgePass)
		}
	}
	return
}

// A pass draws each settled prompt or push line once, at the day's rate,
// judges the drawn lines up to the cap with each verdict's draw probability
// and the model that answered, and leaves the rest waiting; start lines,
// lines not shown and fresh lines are not drawn; every text is redacted.
func TestDeliveryJudgePass(t *testing.T) {
	f := newJudgeFixture(t, 5)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "3")
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if err != nil || n != 3 {
		t.Fatalf("judged %d: %v", n, err)
	}
	p := f.sent[0]
	if len(f.sent) != 1 || !strings.Contains(p, "Request: fix the scheduler lock race [redacted]-a") || strings.Contains(p, "SECRET") ||
		!strings.Contains(p, "take the shard lock first") || strings.Contains(p, "too-fresh") || strings.Contains(p, "start-note") || strings.Contains(p, "not-shown") {
		t.Fatalf("prompt: %s", p)
	}
	verdicts, passes := judgeEntries(t, f.state)
	// The rate is the cap over the lines of the first pool: 3 of 6.
	if len(verdicts) != 3 || verdicts[0].P != 0.5 || verdicts[0].Model != "claude-test-model" || verdicts[0].Prompt != judgePromptID || verdicts[0].Block != "00000000000000a1" {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	if len(passes) != 1 || passes[0].Pool != 6 || passes[0].Drawn != 6 || passes[0].Judged != 3 || passes[0].Waiting != 3 {
		t.Fatalf("pass: %+v", passes)
	}
	// The cap is spent: nothing more today, and no line is drawn twice.
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(time.Minute)); n != 0 {
		t.Fatalf("past the cap: %d", n)
	}
	// Another day: a refusal keeps the waiting lines and counts nothing.
	f.refuse = true
	if n, err := deliveryJudgePass(context.Background(), nil, f.now.Add(24*time.Hour)); n != 0 || !errors.Is(err, errJudgeRefused) {
		t.Fatalf("refused: %d %v", n, err)
	}
	f.refuse = false
	n, err = deliveryJudgePass(context.Background(), nil, f.now.Add(25*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("the waiting lines: %d %v", n, err)
	}
	verdicts, _ = judgeEntries(t, f.state)
	var push *judgeVerdict
	seen := map[string]int{}
	for i, v := range verdicts {
		seen[v.Block+"/"+v.Fact]++
		if v.Fact == "push-note" {
			push = &verdicts[i]
		}
	}
	if len(verdicts) != 6 || push == nil || push.P != 0.5 {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("%s judged %d times", k, c)
		}
	}
	last := f.sent[len(f.sent)-1]
	if !strings.Contains(last, "At: a tool call about to run\nRequest: the tool call Bash: ") || !strings.Contains(last, "go test ./sched") || !strings.Contains(last, "made for the request: fix the scheduler") {
		t.Fatalf("the push's case: %s", last)
	}
	// Off: nothing.
	t.Setenv("TACIT_DELIVERY_JUDGE", "off")
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a5", TS: f.now.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano), Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off,
		Cands: []logCand{{Name: "later", Delivered: true}}}, f.now.Add(24*time.Hour))
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(26*time.Hour)); n != 0 {
		t.Fatal("judged with the judge off")
	}
}

// A line the draw passes over is never drawn again, and a failed redaction
// sends nothing and counts the line unreadable.
func TestJudgeDrawsOnceAndRedactsFirst(t *testing.T) {
	f := newJudgeFixture(t, 2)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "1") // a rate of 1 in 3
	judgeRand = func() float64 { return 0.99 }  // nothing drawn
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now); n != 0 {
		t.Fatalf("judged %d", n)
	}
	judgeRand = func() float64 { return 0 }
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(time.Minute)); n != 0 || len(f.sent) != 0 {
		t.Fatalf("a line had a second draw: %d", n)
	}
	f2 := newJudgeFixture(t, 2)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "36")
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) { return nil, errors.New("no redactor") }
	if n, _ := deliveryJudgePass(context.Background(), nil, f2.now); n != 0 || len(f2.sent) != 0 {
		t.Fatalf("sent without redaction: %d %q", n, f2.sent)
	}
	_, passes := judgeEntries(t, f2.state)
	if len(passes) != 1 || passes[0].Unreadable != 3 {
		t.Fatalf("pass: %+v", passes)
	}
}

// An answer that leaves a line out, or says something but yes, no or
// unsure, is a failure: nothing of it is kept.
func TestJudgeAnswersAreChecked(t *testing.T) {
	call := judgeCall
	t.Cleanup(func() { judgeCall = call })
	cases := []judgeCase{{Block: "b", Fact: "f1"}, {Block: "b", Fact: "f2"}}
	for _, ans := range []string{`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"}]}`, `{"lines":[{"i":1,"relevant":"maybe","confused":"no","novel":"no"},{"i":2,"relevant":"yes","confused":"no","novel":"no"}]}`, "no json"} {
		judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) { return ans, model, nil }
		if _, err := judgeCases(context.Background(), "m", cases); err == nil {
			t.Errorf("accepted %q", ans)
		}
	}
}

// The eval's command builds each case from its moment as the dream pass
// does, with the line as delivered, redacts it, and sends nothing when a
// moment fails.
func TestDeliveryJudgeCommand(t *testing.T) {
	f := newJudgeFixture(t, 1)
	run := func(in string) (int, string) {
		r, w, _ := os.Pipe()
		w.WriteString(in)
		w.Close()
		out := filepath.Join(t.TempDir(), "out")
		of, _ := os.Create(out)
		stdin, stdout := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = r, of
		code := runDeliveryJudge(nil)
		os.Stdin, os.Stdout = stdin, stdout
		r.Close()
		of.Close()
		b, _ := os.ReadFile(out)
		return code, string(b)
	}
	m, _ := json.Marshal(judgeMoment{Block: "b1", Fact: "f", Hook: "prompt", Transcript: f.tr, Offset: f.off, Line: "f: SECRET-x as delivered"})
	code, out := run(string(m) + "\n")
	if code != 0 || len(f.sent) != 1 || strings.Contains(f.sent[0], "SECRET") || !strings.Contains(f.sent[0], "Line: f: [redacted]-x as delivered") ||
		!strings.Contains(f.sent[0], "Request: fix the scheduler lock race") || !strings.Contains(out, `"fact":"f"`) {
		t.Fatalf("code %d, out %s, sent %q", code, out, f.sent)
	}
	bad, _ := json.Marshal(judgeMoment{Block: "b2", Fact: "g", Hook: "prompt", Transcript: filepath.Join(t.TempDir(), "gone.jsonl")})
	if code, _ := run(string(m) + "\n" + string(bad) + "\n"); code == 0 || len(f.sent) != 1 {
		t.Fatalf("a failed moment still sent: code %d, %d prompt(s)", code, len(f.sent))
	}
}
