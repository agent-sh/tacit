package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The judge's environment keeps what a process needs and the Bedrock
// route, never a gateway or another config directory; without the route it
// is an error (the judge is skipped there); a refused name the keep list
// would pass is an error too.
func TestJudgeEnvBedrockRoute(t *testing.T) {
	env, err := judgeEnv([]string{"PATH=/bin", "HOME=/h", "AWS_REGION=x", "AWS_BEARER_TOKEN_BEDROCK=t", "CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=global.x", "ANTHROPIC_BASE_URL=https://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k",
		"ANTHROPIC_BEDROCK_BASE_URL=https://gw", "AWS_ENDPOINT_URL=https://gw", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=1", "CLAUDE_CODE_USE_GATEWAY=1",
		"ANTHROPIC_CUSTOM_HEADERS=h", "CLAUDE_CONFIG_DIR=/c", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_PLUGIN_DIRS=/p", "LANG=C.UTF-8"})
	if err != nil || strings.Join(env, " ") != "PATH=/bin HOME=/h AWS_REGION=x AWS_BEARER_TOKEN_BEDROCK=t CLAUDE_CODE_USE_BEDROCK=1 ANTHROPIC_DEFAULT_OPUS_MODEL=global.x LANG=C.UTF-8" {
		t.Fatalf("kept: %v %v", env, err)
	}
	// A gateway's environment (another dreamer's route): no Bedrock, no judge.
	for _, e := range [][]string{
		{"PATH=/bin", "ANTHROPIC_BASE_URL=https://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k"},
		{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=1"},
		{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=0", "AWS_BEARER_TOKEN_BEDROCK=t"},
	} {
		if _, err := judgeEnv(e); !errors.Is(err, errJudgeNoRoute) {
			t.Fatalf("%v: %v", e, err)
		}
	}
	old := judgeEnvKeep
	t.Cleanup(func() { judgeEnvKeep = old })
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_USE_VERTEX", "HTTPS_PROXY",
		"CLAUDE_CONFIG_DIR", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_PLUGIN_DIRS"} {
		judgeEnvKeep = regexp.MustCompile(`^(PATH|` + name + `|CLAUDE_CODE_USE_BEDROCK|AWS_BEARER_TOKEN_BEDROCK)$`)
		if _, err := judgeEnv([]string{"PATH=/bin", name + "=x", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_BEARER_TOKEN_BEDROCK=t"}); err == nil || errors.Is(err, errJudgeNoRoute) {
			t.Fatalf("%s was passed: %v", name, err)
		}
	}
}

// With no Bedrock route the pass draws nothing, sends nothing and logs why.
func TestJudgePassWithoutARoute(t *testing.T) {
	f := newJudgeFixture(t, 2)
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw")
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if n != 0 || !errors.Is(err, errJudgeNoRoute) || len(f.sent) != 0 {
		t.Fatalf("judged %d, %v, sent %d", n, err, len(f.sent))
	}
	skipped := false
	for _, e := range readDeliveryLog(t, f.state) {
		skipped = skipped || e.Hook == "judge-skip" && e.Reason == "no Bedrock route"
		if e.Hook == "judge-pass" {
			t.Fatalf("a pass without a route: %+v", e)
		}
	}
	if !skipped {
		t.Fatal("no judge-skip entry")
	}
	if _, err := os.Stat(judgeStatePath()); err == nil {
		t.Fatal("a pass without a route moved the judge's state")
	}
}

// The judge's call is `claude -p` with the reduced environment, local
// settings only, no MCP server, no tools and no session kept.
func TestJudgeCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_PROFILE", "p")
	for _, k := range []string{"AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "TACIT_DELIVERY_JUDGE_MODEL"} {
		t.Setenv(k, "")
	}
	if judgeModel() != "global.anthropic.claude-fable-5-2" {
		t.Fatalf("default model %q", judgeModel())
	}
	cmd, err := judgeCommand(context.Background(), judgeModel(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_USE_BEDROCK=1") || !slices.Contains(cmd.Env, "AWS_PROFILE=p") {
		t.Fatalf("the route did not reach the call: %v", cmd.Env)
	}
	for _, kv := range cmd.Env {
		if judgeEnvRefused(strings.SplitN(kv, "=", 2)[0]) {
			t.Fatalf("passed %s", kv)
		}
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"claude -p", "--model global.anthropic.claude-fable-5-2", "--tools  ", "--setting-sources local", "--strict-mcp-config", "--no-session-persistence"} {
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
	t.Setenv("USERPROFILE", home) // the home os.UserHomeDir reads on Windows
	f := &judgeFixture{state: filepath.Join(home, "state"), now: time.Now()}
	t.Setenv("TACIT_STATE_DIR", f.state)
	t.Setenv("TACIT_DELIVERY_JUDGE", "on")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test")
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
	// A moment whose transcript no harness of this machine writes: never drawn.
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a6", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: filepath.Join(home, "elsewhere", "x.jsonl"), Transcript: 1,
		Cands: []logCand{{Name: "unreadable-note", Delivered: true}}}, f.now)
	// One that showed nothing is named too.
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a7", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: filepath.Join(home, "elsewhere", "y.jsonl"), Transcript: 1,
		Cands: []logCand{{Name: "not-shown-either"}}}, f.now)
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
	// The rate aims at 0.8 of the cap over the lines of the first pool: 2.4 of 6.
	if len(verdicts) != 3 || !near(verdicts[0].P, 0.4) || verdicts[0].Model != "claude-test-model" || verdicts[0].Prompt != judgePromptID || verdicts[0].Block != "00000000000000a1" {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	if len(passes) != 1 || passes[0].Pool != 6 || passes[0].Drawn != 6 || passes[0].Judged != 3 || passes[0].Waiting != 3 {
		t.Fatalf("pass: %+v", passes)
	}
	if strings.Join(passes[0].UnreadBlocks, ",") != "00000000000000a6,00000000000000a7" || passes[0].From == "" || passes[0].To == "" || strings.Join(passes[0].Answered, ",") != "claude-test-model" {
		t.Fatalf("the pass's window: %+v", passes[0])
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
	if len(verdicts) != 6 || push == nil || !near(push.P, 0.4) {
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
	if len(passes) != 1 || passes[0].Unreadable != 3 || len(passes[0].Unread) != 3 || passes[0].Prompt != judgePromptID || passes[0].Model == "" {
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
	// A moment that cannot be read is answered unreadable and not sent; the
	// rest are judged.
	code, out = run(string(bad) + "\n" + string(m) + "\n")
	if code != 0 || len(f.sent) != 2 || strings.Count(f.sent[1], "## Case ") != 1 ||
		!strings.HasPrefix(out, `{"block":"b2","fact":"g","unreadable":true}`) || !strings.Contains(out, `"fact":"f"`) {
		t.Fatalf("code %d, out %s, sent %d", code, out, len(f.sent))
	}
}

// The model a verdict names is the one the CLI says answered, past its small
// helper model; the alias asked for when the answer does not say.
func TestAnsweredBy(t *testing.T) {
	u := func(names ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for _, n := range names {
			m[n] = json.RawMessage(`{}`)
		}
		return m
	}
	for _, c := range []struct {
		usage map[string]json.RawMessage
		want  string
	}{{u("claude-x-1", "claude-haiku-2"), "claude-x-1"}, {u(), "opus"}, {u("a", "b"), "opus"}} {
		if got := answeredBy(c.usage, "opus"); got != c.want {
			t.Errorf("answeredBy(%v) = %q, want %q", c.usage, got, c.want)
		}
	}
}

// A refusal leaves a waiting line's tries alone; another failure counts
// one, and a line that fails judgeTries times is given up and named.
func TestJudgeTriesAndLost(t *testing.T) {
	f := newJudgeFixture(t, 1)
	fail := errors.New("bad answer")
	var calls int
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		calls++
		if f.refuse {
			return "", "", errJudgeRefused
		}
		return "", "", fail
	}
	f.refuse = true
	for i := 0; i < judgeTries+1; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(i)*time.Minute))
	}
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	if len(st.Pending) != 2 || st.Pending[0].Tries != 0 {
		t.Fatalf("refusals counted tries: %+v", st.Pending)
	}
	f.refuse = false
	for i := 0; i < judgeTries; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(10+i)*time.Minute))
	}
	st = readJudgeState(f.now.UTC().Format("2006-01-02"))
	_, passes := judgeEntries(t, f.state)
	var lost []string
	for _, p := range passes {
		lost = append(lost, p.Lost...)
	}
	var failed []string
	for _, p := range passes {
		failed = append(failed, p.Failed...)
	}
	if len(failed) != 2 {
		t.Fatalf("failed %v", failed)
	}
	if len(st.Pending) != 0 || len(lost) != 2 || calls == 0 {
		t.Fatalf("pending %+v, lost %v", st.Pending, lost)
	}
}

// Past judgePending waiting lines, some are given up at random and named;
// a quiet first day does not draw everything.
func TestJudgeOverflowAndRate(t *testing.T) {
	f := newJudgeFixture(t, 1)
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	for i := 0; i < judgePending+5; i++ {
		st.Pending = append(st.Pending, judgeMoment{Block: "old", Fact: fmt.Sprint(i), Hook: "prompt", Transcript: f.tr, Offset: f.off, P: 0.1})
	}
	st.Count = judgeDaily // nothing judged this pass
	writeJudgeState(st)
	deliveryJudgePass(context.Background(), nil, f.now)
	_, passes := judgeEntries(t, f.state)
	st = readJudgeState(f.now.UTC().Format("2006-01-02"))
	if len(passes) != 1 || passes[0].Dropped != 7 || len(passes[0].Lost) != 7 || len(st.Pending) != judgePending {
		t.Fatalf("pass %+v, pending %d", passes, len(st.Pending))
	}
	// The kept lines' p is thinned by the share kept.
	for _, m := range st.Pending {
		if m.Block == "old" && !near(m.P, 0.1*float64(judgePending)/float64(judgePending+7)) {
			t.Fatalf("p %v", m.P)
		}
	}
	// The pool held 2 lines: the rate is 0.8 of the cap over the cap.
	if !near(passes[0].Rate, 0.8) {
		t.Fatalf("rate %v", passes[0].Rate)
	}
}

// A push whose call the harness writes after the hook ran still has its
// call as its request.
func TestJudgePushCallWrittenAfter(t *testing.T) {
	f := newJudgeFixture(t, 1)
	m := judgeMoment{Block: "b", Fact: "f", Hook: "push", Tool: "Bash", Transcript: f.tr, Offset: f.off + 1, Line: "f: x"}
	// The offset just inside the prompt's line aligns to the next line: the
	// assistant's text and call come after it.
	b, _ := os.ReadFile(f.tr)
	i := strings.Index(string(b), `{"type":"assistant"`)
	m.Offset = int64(i)
	os.WriteFile(f.tr, []byte(strings.Replace(string(b), `{"type":"text","text":"I will take the shard lock first."},`, "", 1)), 0o644)
	c, ok := momentOf(context.Background(), nil, m)
	if !ok || !strings.HasPrefix(c.Request, "the tool call Bash: ") || !strings.Contains(c.Request, "made for the request: fix the scheduler") {
		t.Fatalf("case %+v %v", c, ok)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A .claude in the judge's working directory, whose local settings could
// set a route, stops the call before it runs.
func TestJudgeRefusesLocalSettings(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	os.MkdirAll(filepath.Join(deliveryLogDir(), "judge-cwd", ".claude"), 0o700)
	if _, _, err := judgeCall(context.Background(), "opus", "x"); err == nil || !strings.Contains(err.Error(), ".claude") {
		t.Fatalf("ran over a .claude: %v", err)
	}
}

// Only the route's own errors end a pass without counting against the
// lines; an error of the request counts.
func TestRouteRefusal(t *testing.T) {
	for text, want := range map[string]bool{
		"API Error: 429 Too Many Requests":                      true,
		"API Error: 403 {\"message\":\"x\"}":                    true,
		"API Error: 401 unauthorized":                           true,
		"rate limit reached":                                    true,
		"Overloaded":                                            true,
		"The security token included in the request is expired": true,
		"ExpiredTokenException: token expired":                  true,
		"UnrecognizedClientException":                           true,
		"AccessDeniedException: no access":                      true,
		"API Error: Request rejected (429) · Too many tokens, please wait before trying again.":                           true,
		"API Error: Request rejected (429) · this may be a temporary capacity issue.":                                     true,
		"API Error: Could not load AWS credentials · token expired. Check or refresh your AWS credentials and try again.": true,
		`API Error: 400 {"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}`:  false,
		"max_tokens: 64000 > 32000":                     false,
		"API Error: 500 request_id req_403abc internal": false,
		"took 14030ms":                                  false,
		"invalid token count in response":               false,
	} {
		if got := routeRefusal(text); got != want {
			t.Errorf("routeRefusal(%q) = %v, want %v", text, got, want)
		}
	}
}

// The AWS SDK's config files: none on the bearer route, and a profile
// route whose config names an endpoint refuses the call.
func TestJudgeCommandAWSConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "t")
	t.Setenv("AWS_PROFILE", "")
	cmd, err := judgeCommand(context.Background(), "m", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	empty := func(cmd *exec.Cmd) {
		t.Helper()
		for _, name := range []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"} {
			var f string
			for _, kv := range cmd.Env {
				if v, ok := strings.CutPrefix(kv, name+"="); ok {
					f = v
				}
			}
			if b, err := os.ReadFile(f); f == "" || err != nil || len(b) != 0 {
				t.Fatalf("no empty %s: %q %v", name, f, err)
			}
		}
	}
	empty(cmd)
	// The bearer route reads no AWS file, an endpoint in ~/.aws/config too.
	os.MkdirAll(filepath.Join(home, ".aws"), 0o700)
	os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[profile p]\nendpoint_url = https://gw.example\n"), 0o600)
	if cmd, err = judgeCommand(context.Background(), "m", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	empty(cmd)
	// A profile route: an endpoint_url or a services section in the config
	// or the credentials file refuses the call; a plain profile runs.
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_PROFILE", "p")
	for _, c := range []struct{ file, text string }{
		{"config", "[profile p]\nendpoint_url = https://gw.example\n"},
		{"config", "[profile p]\nservices = gw\n"},
		{"credentials", "[p]\nendpoint_url = https://gw.example\n"},
	} {
		os.Remove(filepath.Join(home, ".aws", "config"))
		os.Remove(filepath.Join(home, ".aws", "credentials"))
		os.WriteFile(filepath.Join(home, ".aws", c.file), []byte(c.text), 0o600)
		if _, err := judgeCommand(context.Background(), "m", t.TempDir()); err == nil {
			t.Fatalf("%s %q was run over", c.file, c.text)
		}
	}
	os.Remove(filepath.Join(home, ".aws", "credentials"))
	os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[profile p]\nregion = us-east-1\n"), 0o600)
	if _, err := judgeCommand(context.Background(), "m", t.TempDir()); err != nil {
		t.Fatalf("a plain profile: %v", err)
	}
	// Windows's home, when HOME is not set.
	win := t.TempDir()
	os.MkdirAll(filepath.Join(win, ".aws"), 0o700)
	os.WriteFile(filepath.Join(win, ".aws", "config"), []byte("[profile p]\nendpoint_url = https://gw.example\n"), 0o600)
	if !awsConfigEndpoint([]string{"USERPROFILE=" + win}) || awsConfigEndpoint([]string{"USERPROFILE=" + t.TempDir()}) {
		t.Fatal("the Windows home was not read")
	}
}

// The CLI's structured status decides first: a 429 in the JSON result is
// the route's, whatever its text; a 400 is the request's.
func TestJudgeOutcome(t *testing.T) {
	fail := errors.New("exit status 1")
	for _, c := range []struct {
		out  string
		want error
	}{
		{`{"is_error":true,"api_error_status":429,"result":"something new"}`, errJudgeRefused},
		{`{"is_error":true,"api_error_status":529,"result":"x"}`, errJudgeRefused},
		{`{"is_error":true,"api_error_status":400,"result":"API Error: 400 prompt is too long: 210000 tokens"}`, nil},
		{`{"is_error":true,"api_error_status":400,"result":"API Error: 400 the request mentions a rate limit"}`, nil},
		{`{"is_error":true,"result":"API Error: Request rejected (429) · Too many tokens, please wait before trying again."}`, errJudgeRefused},
	} {
		_, _, err := judgeOutcome([]byte(c.out), "", fail, "m")
		if c.want != nil && !errors.Is(err, c.want) || c.want == nil && (err == nil || errors.Is(err, errJudgeRefused)) {
			t.Errorf("%s: %v", c.out, err)
		}
	}
	text, by, err := judgeOutcome([]byte(`{"result":"{}","modelUsage":{"claude-x":{}}}`), "", nil, "m")
	if err != nil || text != "{}" || by != "claude-x" {
		t.Fatalf("%q %q %v", text, by, err)
	}
}

// A label the log cannot take: the paid call counts against the day, the
// line waits as a failed try, and is written by a later pass.
func TestJudgeLabelTheLogCannotTake(t *testing.T) {
	f := newJudgeFixture(t, 1)
	day := filepath.Join(f.state, "delivery", f.now.UTC().Format("2006-01-02")+".jsonl")
	b, err := os.ReadFile(day)
	if err != nil {
		t.Fatal(err)
	}
	full := func() {
		if err := os.Truncate(day, deliveryLogDayCap+judgeLogHeadroom); err != nil {
			t.Fatal(err)
		}
	}
	// The pool is read before the file fills: judgeCall fills it.
	call := judgeCall
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		full()
		return call(ctx, model, prompt)
	}
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if n != 0 {
		t.Fatalf("judged %d into a full log (%v)", n, err)
	}
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	if st.Count != 2 || len(st.Pending) != 2 || st.Pending[0].Tries != 1 {
		t.Fatalf("count %d, pending %+v", st.Count, st.Pending)
	}
	// The log has room again: the waiting lines are written.
	judgeCall = call
	os.WriteFile(day, b, 0o600)
	if n, err := deliveryJudgePass(context.Background(), nil, f.now.Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("the waiting labels: %d %v", n, err)
	}
}

// A label the log keeps refusing is given up after judgeTries, named in
// the pass's lost and failed lines.
func TestJudgeLabelTheLogKeepsRefusing(t *testing.T) {
	f := newJudgeFixture(t, 1)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "100")
	write := appendJudgeLabel
	t.Cleanup(func() { appendJudgeLabel = write })
	appendJudgeLabel = func(*logEntry, time.Time) error { return errors.New("the day's delivery log is full") }
	for i := 0; i < judgeTries; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(i)*time.Minute))
	}
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	if len(st.Pending) != 0 || st.Count != 2*judgeTries {
		t.Fatalf("pending %+v, count %d", st.Pending, st.Count)
	}
	_, passes := judgeEntries(t, f.state)
	var failed, lost []string
	for _, p := range passes {
		failed, lost = append(failed, p.Failed...), append(lost, p.Lost...)
	}
	if len(failed) != 2 || len(lost) != 2 {
		t.Fatalf("failed %v, lost %v", failed, lost)
	}
}

// realJudgeCall is the judge's call as built, before any test stands in
// for it.
var realJudgeCall = judgeCall

// judgeFakeCLI is this test binary run as the judge's `claude` (a link to
// it, or a copy, named claude: see newFakeJudgeCLI). It keeps the prompt it
// reads. With a child file it starts a child of its own that holds its
// stdout and stderr (a wrapper's CLI), marks child.started, and the child
// writes child.survived if it is still alive 5 s later. It waits the milliseconds in its sleep file (after
// printing, with a late file), prints the stdout file (stdout.N on its Nth
// call, when there is one) and the stderr file beside it, and exits with
// the code in its exit file. The judge's environment passes no test
// variable, so the name tells.
func judgeFakeCLI() {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") != "claude" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(3)
	}
	dir := filepath.Dir(exe)
	if os.Getenv("TACIT_FAKE_CLI_CHILD") != "" {
		time.Sleep(5 * time.Second)
		os.WriteFile(filepath.Join(dir, "child.survived"), nil, 0o600)
		time.Sleep(15 * time.Second)
		os.Exit(0)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	os.WriteFile(filepath.Join(dir, "prompt"), prompt, 0o600)
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	n++
	os.WriteFile(filepath.Join(dir, "calls"), []byte(strconv.Itoa(n)), 0o600)
	if _, err := os.Stat(filepath.Join(dir, "child")); err == nil {
		child := exec.Command(exe)
		child.Env = append(os.Environ(), "TACIT_FAKE_CLI_CHILD=1")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		os.WriteFile(filepath.Join(dir, "child.started"), nil, 0o600)
	}
	wait := func() {
		if b, err := os.ReadFile(filepath.Join(dir, "sleep")); err == nil {
			ms, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	_, late := os.Stat(filepath.Join(dir, "late"))
	if late != nil {
		wait()
	}
	out, err := os.ReadFile(filepath.Join(dir, "stdout."+strconv.Itoa(n)))
	if err != nil {
		out, _ = os.ReadFile(filepath.Join(dir, "stdout"))
	}
	os.Stdout.Write(out)
	errb, _ := os.ReadFile(filepath.Join(dir, "stderr"))
	os.Stderr.Write(errb)
	if late == nil {
		wait()
	}
	code := 0
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
		code, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	os.Exit(code)
}

// newFakeJudgeCLI puts a claude that answers as told first on PATH, with
// a bearer route for the judge, and returns how to tell it what to print.
// It is a hard link to this test binary where the file system allows one,
// else a copy.
func newFakeJudgeCLI(t *testing.T) (dir string, answer func(stdout, stderr string, exit int)) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	name := filepath.Join(dir, "claude")
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if os.Link(exe, name) != nil {
		b, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, b, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test")
	t.Setenv("AWS_PROFILE", "")
	return dir, func(stdout, stderr string, exit int) {
		os.WriteFile(filepath.Join(dir, "stdout"), []byte(stdout), 0o600)
		os.WriteFile(filepath.Join(dir, "stderr"), []byte(stderr), 0o600)
		os.WriteFile(filepath.Join(dir, "exit"), []byte(strconv.Itoa(exit)), 0o600)
	}
}

// The call reads the CLI's output once it has run: what it printed is the
// answer, and a throttle it printed on stderr is the route's refusal.
func TestJudgeCallReadsTheAnswer(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	dir, answer := newFakeJudgeCLI(t)
	answer(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","result":"{\"lines\":[]}","modelUsage":{"claude-x":{}}}`+"\n", "", 0)
	text, by, err := realJudgeCall(context.Background(), "m", "the cases")
	if err != nil || text != `{"lines":[]}` || by != "claude-x" {
		t.Fatalf("%q %q %v", text, by, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "prompt")); string(b) != "the cases" {
		t.Fatalf("the CLI read %q", b)
	}
	answer("", "API Error: 429 Too many requests", 1)
	if _, _, err := realJudgeCall(context.Background(), "m", "the cases"); !errors.Is(err, errJudgeRefused) {
		t.Fatalf("a throttle on stderr: %v", err)
	}
}

// The CLI's result is read in the forms it prints: its one object, an
// array of events whose last result counts, a result line after other
// output.
func TestJudgeOutcomeForms(t *testing.T) {
	res := `{"type":"result","subtype":"success","is_error":false,"result":"A","modelUsage":{"claude-x":{}}}`
	for _, out := range []string{
		res + "\n",
		`[{"type":"system","subtype":"init"},{"type":"assistant","message":{"content":[]}},` + res + `]`,
		"a notice printed first\n" + res + "\n",
		`{"type":"result","is_error":false,"result":"A","api_error_status":"none","modelUsage":{"claude-x":{}}}`,
	} {
		text, by, err := judgeOutcome([]byte(out), "", nil, "m")
		if err != nil || text != "A" || by != "claude-x" {
			t.Errorf("%s: %q %q %v", out, text, by, err)
		}
	}
}

// An answer is the JSON in it, whatever prose or code fence is around it,
// braces in the prose too.
func TestJudgeAnswerAroundProse(t *testing.T) {
	call := judgeCall
	t.Cleanup(func() { judgeCall = call })
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		return "Here is {my} answer:\n```json\n" + `{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"unsure"}]}` + "\n```\nDone {ok}.", model, nil
	}
	v, err := judgeCases(context.Background(), "m", []judgeCase{{Block: "b", Fact: "f"}})
	if err != nil || len(v) != 1 || v[0].Relevant != "yes" || v[0].Novel != "unsure" {
		t.Fatalf("%+v %v", v, err)
	}
	// An example of the format before the answer is passed over.
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		return `The format is {"lines":[{"i":1,"relevant":"yes|no|unsure","confused":"yes|no|unsure","novel":"yes|no|unsure"}]}, so: {"lines":[{"i":1,"relevant":"no","confused":"no","novel":"no"}]}`, model, nil
	}
	v, err = judgeCases(context.Background(), "m", []judgeCase{{Block: "b", Fact: "f"}})
	if err != nil || len(v) != 1 || v[0].Relevant != "no" {
		t.Fatalf("after an example: %+v %v", v, err)
	}
	// Two answers that disagree are no answer; a repeat is one.
	two := func(second string) {
		judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
			return `{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"}]} On second thought: {"lines":[{"i":1,"relevant":"` + second + `","confused":"no","novel":"no"}]}`, model, nil
		}
		v, err = judgeCases(context.Background(), "m", []judgeCase{{Block: "b", Fact: "f"}})
	}
	if two("no"); err == nil {
		t.Fatalf("two answers that disagree: %+v", v)
	}
	if two("yes"); err != nil || len(v) != 1 {
		t.Fatalf("a repeated answer: %+v %v", v, err)
	}
	// A line answered twice in one answer is no answer.
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		return `{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"},{"i":1,"relevant":"no","confused":"yes","novel":"yes"}]}`, model, nil
	}
	if v, err = judgeCases(context.Background(), "m", []judgeCase{{Block: "b", Fact: "f"}}); err == nil {
		t.Fatalf("a line answered twice: %+v", v)
	}
}

// rawPending is the judge state's waiting lines as written, a missing
// count as 0.
func rawPending(t *testing.T) []map[string]any {
	t.Helper()
	var st struct {
		Pending []map[string]any `json:"pending"`
	}
	b, err := os.ReadFile(judgeStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Pending {
		for _, k := range []string{"tries", "held"} {
			if m[k] == nil {
				m[k] = float64(0)
			}
		}
	}
	return st.Pending
}

// rawJudgePasses is the judge-pass entries of the log as written.
func rawJudgePasses(t *testing.T, state string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(state, "delivery", "*.jsonl"))
	var out []map[string]any
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var e struct {
				Hook string         `json:"hook"`
				Pass map[string]any `json:"judge_pass"`
			}
			if json.Unmarshal([]byte(l), &e) == nil && e.Hook == "judge-pass" {
				out = append(out, e.Pass)
			}
		}
	}
	return out
}

// A pass whose call gives no labels says why in its entry (answer_error),
// as a class and never the answer's text, which its error does not carry
// either. A call that could not start, that the pass's deadline ended or
// that printed no result says nothing about the lines: like a refusal
// (which has no class) it counts no try, and counts as held instead; the
// others count one try. An answer that reads labels every line.
func TestJudgePassNamesWhyAnAnswerFailed(t *testing.T) {
	const mark = "XYZZY-ANSWER-TEXT"
	result := func(text, stop string) string {
		b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "stop_reason": stop, "result": text, "modelUsage": map[string]any{"claude-x": map[string]any{}}})
		return string(b)
	}
	good := result(`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"},{"i":2,"relevant":"no","confused":"no","novel":"no"}]}`, "end_turn")
	for _, c := range []struct {
		name, stdout, stderr string
		exit                 int
		localSettings        bool
		noCLI                bool          // no claude on PATH
		sleep                time.Duration // the CLI's wait, past the pass's deadline when set
		child                bool          // the CLI's child holds its pipes past the deadline
		late                 bool          // the CLI waits after printing
		deadline             time.Duration // the pass's, when the CLI sleeps (2 s by default)
		want                 string        // the class; "refused", "labelled"
		tries, held          int           // each waiting line's after the pass
	}{
		{name: "no output", want: "no-output", held: 1},
		{name: "no output and an exit", stderr: "error: unknown option '--tools'", exit: 1, want: "no-output", held: 1},
		{name: "setup", localSettings: true, stdout: good, want: "setup", held: 1},
		{name: "no claude", noCLI: true, want: "setup", held: 1},
		{name: "deadline", sleep: 30 * time.Second, stdout: good, want: "timeout", held: 1},
		{name: "deadline over a wrapper", sleep: 30 * time.Second, child: true, stdout: good, want: "timeout", held: 1},
		{name: "deadline after an answer", sleep: 30 * time.Second, late: true, deadline: 5 * time.Second, stdout: good, want: "labelled"},
		{name: "deadline after prose", sleep: 30 * time.Second, late: true, deadline: 5 * time.Second, stdout: result("I cannot judge these lines. "+mark, "end_turn"), want: "prose", tries: 1},
		{name: "the error form with a throttle", stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: 429 Too many requests"]}`, exit: 1, want: "refused"},
		{name: "the error form with a connection error", stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: Connection error."]}`, exit: 1, want: "server", held: 1},
		{name: "the error form", stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["something broke"]}`, exit: 1, want: "no-output", held: 1},
		{name: "the model's timeout", stdout: `{"type":"result","is_error":true,"api_error_status":408,"result":"API Error: 408 Model has timed out"}`, exit: 1, want: "server", held: 1},
		{name: "an empty result and a throttle", stdout: result("", "end_turn"), stderr: "API Error: 429 Too many requests", exit: 1, want: "refused"},
		{name: "a notice and a cut-off result", stdout: "a notice\n" + `{"type":"result","is_error":false,"result":"{\"lines\":[{\"i\":1,\"relevant\":\"the rate limit of this ` + mark, exit: 1, want: "no-output", held: 1},
		{name: "connection error", stderr: "API Error: Connection error.", exit: 1, want: "server", held: 1},
		{name: "server error with no status", stdout: `{"type":"result","is_error":true,"result":"API Error: 500 Internal server error"}`, exit: 1, want: "server", held: 1},
		{name: "a model Bedrock serves through a profile", stdout: `{"type":"result","is_error":true,"api_error_status":400,"result":"API Error: 400 Invocation of model ID m with on-demand throughput isn't supported. Retry your request with the ID or ARN of an inference profile that contains this model."}`, exit: 1, want: "setup", held: 1},
		{name: "a request error that names a model", stdout: `{"type":"result","is_error":true,"api_error_status":400,"result":"API Error: 400 the model's tool_use id was not found"}`, exit: 1, want: "call", tries: 1},
		{name: "unknown model", stdout: `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"result":"API Error (m): 400 The provided model identifier is invalid.. Run --model to pick a different model."}`, stderr: "[claude-code:unrecognized_model] {}", exit: 1, want: "setup", held: 1},
		{name: "declined", stdout: result("I can't help with that. "+mark, "refusal"), want: "declined", tries: 1},
		{name: "empty", stdout: result("", "end_turn"), want: "empty", tries: 1},
		{name: "prose", stdout: result("I cannot judge these lines. "+mark, "end_turn"), want: "prose", tries: 1},
		{name: "prose with a brace", stdout: result("I cannot judge {these} lines. "+mark, "end_turn"), want: "prose", tries: 1},
		{name: "cut off", stdout: result(`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"},{"i":2,"relevant":"`+mark, "end_turn"), want: "truncated", tries: 1},
		{name: "cut off after a brace", stdout: result(`Here is {my} answer: {"lines":[{"i":1,"relevant":"`+mark, "end_turn"), want: "truncated", tries: 1},
		{name: "output-token limit", stdout: result("", "max_tokens"), want: "truncated", tries: 1},
		{name: "shape", stdout: result(`{"lines":[{"i":1,"relevant":"`+mark+`","confused":"no","novel":"no"}]}`, "end_turn"), want: "shape", tries: 1},
		{name: "other", stdout: result(`{"lines":"`+mark+`"}`, "end_turn"), want: "other", tries: 1},
		{name: "call", stdout: `{"type":"result","is_error":true,"api_error_status":400,"result":"API Error: 400 prompt is too long"}`, exit: 1, want: "call", tries: 1},
		{name: "exit after an answer", stdout: result(`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"},{"i":2,"relevant":"no","confused":"no","novel":"no"}]}`+"\nNo rate limit or overload here.", "end_turn"), exit: 1, want: "labelled"},
		{name: "exit after an answer, the route's words on stderr", stdout: good, stderr: "API Error: 529 overloaded", exit: 1, want: "labelled"},
		{name: "exit after an answer, a model's words on stderr", stdout: good, stderr: "helper model claude-haiku not found", exit: 1, want: "labelled"},
		{name: "a cut-off result", stdout: `{"type":"result","is_error":false,"result":"{\"lines\":[{\"i\":1,\"relevant\":\"no rate limit here ` + mark, exit: 1, want: "no-output", held: 1},
		{name: "a result with no text", stdout: `{"type":"result","subtype":"success","is_error":false}`, want: "no-output", held: 1},
		{name: "server error", stdout: `{"type":"result","is_error":true,"api_error_status":503,"result":"API Error: 503 temporary capacity issue"}`, exit: 1, want: "server", held: 1},
		{name: "refused on stderr", stderr: "API Error: 429 Too many requests", exit: 1, want: "refused"},
		{name: "refused on stdout", stdout: "API Error: 429 Too many requests\n", exit: 1, want: "refused"},
		{name: "labelled", stdout: good, want: "labelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newJudgeFixture(t, 1)
			dir, answer := newFakeJudgeCLI(t)
			judgeCall = realJudgeCall
			answer(c.stdout, c.stderr, c.exit)
			if c.localSettings {
				os.MkdirAll(filepath.Join(deliveryLogDir(), "judge-cwd", ".claude"), 0o700)
			}
			if c.noCLI {
				t.Setenv("PATH", t.TempDir())
			}
			ctx := context.Background()
			if c.sleep > 0 {
				os.WriteFile(filepath.Join(dir, "sleep"), []byte(strconv.Itoa(int(c.sleep.Milliseconds()))), 0o600)
				d := c.deadline
				if d == 0 {
					d = 2 * time.Second
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
			}
			if c.child {
				os.WriteFile(filepath.Join(dir, "child"), nil, 0o600)
			}
			if c.late {
				os.WriteFile(filepath.Join(dir, "late"), nil, 0o600)
			}
			start := time.Now()
			n, err := deliveryJudgePass(ctx, nil, f.now)
			// The deadline ends the call's whole tree, a child that holds
			// its pipes too: the pass does not wait the child's 20 s.
			if c.sleep > 0 && time.Since(start) > 12*time.Second {
				t.Fatalf("the deadline took %s", time.Since(start))
			}
			if c.child {
				// The child marks itself 5 s in, if the deadline's kill (at 2 s)
				// left it alive.
				time.Sleep(time.Until(start.Add(7 * time.Second)))
				if _, err := os.Stat(filepath.Join(dir, "child.started")); err != nil {
					t.Fatal("the CLI's child never started")
				}
				if _, err := os.Stat(filepath.Join(dir, "child.survived")); err == nil {
					t.Fatal("the CLI's child outlived the deadline")
				}
			}
			passes := rawJudgePasses(t, f.state)
			if len(passes) != 1 {
				t.Fatalf("passes %v", passes)
			}
			p := passes[0]
			switch c.want {
			case "labelled":
				if err != nil || n != 2 || p["answer_error"] != nil {
					t.Fatalf("%d %v %v", n, err, p)
				}
			case "refused":
				if !errors.Is(err, errJudgeRefused) || p["refused"] != true || p["answer_error"] != nil {
					t.Fatalf("%v %v", err, p)
				}
			default:
				if err == nil || n != 0 || p["answer_error"] != c.want {
					t.Fatalf("%d %v %v", n, err, p)
				}
			}
			if err != nil && strings.Contains(err.Error(), mark) {
				t.Fatalf("the error carries the answer's text: %v", err)
			}
			if c.want != "labelled" {
				waiting := rawPending(t)
				if len(waiting) != 2 {
					t.Fatalf("waiting %v", waiting)
				}
				for _, m := range waiting {
					if m["tries"] != float64(c.tries) || m["held"] != float64(c.held) {
						t.Fatalf("waiting %v, want %d tries and %d held each", waiting, c.tries, c.held)
					}
				}
			}
			files, _ := filepath.Glob(filepath.Join(f.state, "delivery", "*"))
			for _, fn := range files {
				if b, _ := os.ReadFile(fn); strings.Contains(string(b), mark) {
					t.Fatalf("%s holds the answer's text", fn)
				}
			}
		})
	}
}

// A line held 12 times (judgeHeldTries) by failures that say nothing about it
// is given up, named in the pass's lost and failed lines; one held less
// still waits.
func TestJudgeHeldLinesAreGivenUp(t *testing.T) {
	const heldTries = 12 // SPEC: two days of the dreamer's 4-hour runs
	f := newJudgeFixture(t, 1)
	_, answer := newFakeJudgeCLI(t)
	judgeCall = realJudgeCall
	answer("", "", 0) // no output: held
	for i := 0; i < heldTries-1; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(i)*time.Minute))
	}
	if w := rawPending(t); len(w) != 2 || w[0]["held"] != float64(heldTries-1) || w[0]["tries"] != float64(0) {
		t.Fatalf("waiting %v", w)
	}
	deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(heldTries-1)*time.Minute))
	passes := rawJudgePasses(t, f.state)
	last := passes[len(passes)-1]
	failed, _ := last["failed"].([]any)
	lost, _ := last["lost"].([]any)
	if w := rawPending(t); len(w) != 0 || len(failed) != 2 || len(lost) != 2 {
		t.Fatalf("waiting %v, last pass %v", w, last)
	}
}

// A held failure in a later batch of a pass leaves the earlier batch's
// labels and holds only its own lines.
func TestJudgeHeldInALaterBatch(t *testing.T) {
	f := newJudgeFixture(t, judgeBatch) // a batch of notes and the push line
	dir, answer := newFakeJudgeCLI(t)
	judgeCall = realJudgeCall
	var ls []string
	for i := 1; i <= judgeBatch; i++ {
		ls = append(ls, fmt.Sprintf(`{"i":%d,"relevant":"yes","confused":"no","novel":"no"}`, i))
	}
	b, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": `{"lines":[` + strings.Join(ls, ",") + `]}`})
	answer("", "", 0) // the second call prints nothing: held
	os.WriteFile(filepath.Join(dir, "stdout.1"), b, 0o600)
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if n != judgeBatch || err == nil {
		t.Fatalf("judged %d, %v", n, err)
	}
	w := rawPending(t)
	if len(w) != 1 || w[0]["held"] != float64(1) || w[0]["tries"] != float64(0) {
		t.Fatalf("waiting %v", w)
	}
}
