package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/events"
)

// The judge's environment has no Bedrock or AWS variable, and a name the
// strip does not know but that says Bedrock is refused rather than passed.
func TestJudgeEnvNeverBedrock(t *testing.T) {
	env, err := judgeEnv([]string{"PATH=/bin", "HOME=/h", "AWS_REGION=x", "AWS_PROFILE=y", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"ANTHROPIC_BEDROCK_BASE_URL=u", "ANTHROPIC_DEFAULT_OPUS_MODEL=global.x", "ANTHROPIC_MODEL=m"})
	if err != nil || strings.Join(env, " ") != "PATH=/bin HOME=/h" {
		t.Fatalf("stripped: %v %v", env, err)
	}
	if _, err := judgeEnv([]string{"PATH=/bin", "MY_BEDROCK_ROUTE=1"}); err == nil {
		t.Fatal("a Bedrock variable was passed")
	}
}

// A dream pass judges a sample of the settled delivered lines, up to the
// day's cap, logging each verdict with the probability its line had of
// being judged; lines judged before are not judged again; a refusal of the
// subscription counts nothing and falls back to nothing.
func TestDeliveryJudgePass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "3")
	tr := filepath.Join(home, ".claude", "projects", "-demo", "s1.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o755)
	lines := []string{
		`{"type":"user","timestamp":"2026-11-01T10:00:00Z","sessionId":"s1","userType":"external","message":{"role":"user","content":"fix the scheduler lock race"}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"I will take the shard lock first."}]}}`,
	}
	os.WriteFile(tr, []byte(lines[0]+"\n"), 0o644)
	off := int64(len(lines[0]) + 1)
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(lines[1] + "\n")
	f.Close()
	now := time.Now()
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	var cands []logCand
	for i := 0; i < 5; i++ {
		cands = append(cands, logCand{Name: fmt.Sprintf("note-%d", i), Delivered: true, Prov: "agent"})
	}
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a1", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: tr, Transcript: off, Cands: cands}, now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a2", TS: now.UTC().Format(time.RFC3339Nano), Hook: "prompt", Session: "s1", TranscriptPath: tr, Transcript: off,
		Cands: []logCand{{Name: "too-fresh", Delivered: true}}}, now)
	t.Setenv("TACIT_DELIVERY_JUDGE", "on")
	call, redact := judgeCall, judgeRedact
	t.Cleanup(func() { judgeCall, judgeRedact = call, redact })
	judgeRedact = func(ctx context.Context, evs []events.Event) ([]events.Event, error) { return evs, nil }
	var seen []string
	judgeCall = func(ctx context.Context, model, prompt string) (string, error) {
		seen = append(seen, prompt)
		n := strings.Count(prompt, "## Case ")
		var ls []string
		for i := 1; i <= n; i++ {
			ls = append(ls, fmt.Sprintf(`{"i":%d,"relevant":"yes","confused":"no","novel":"unsure"}`, i))
		}
		return `{"lines":[` + strings.Join(ls, ",") + `]}`, nil
	}
	n, err := deliveryJudgePass(context.Background(), nil, now)
	if err != nil || n != 3 {
		t.Fatalf("judged %d: %v", n, err)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "fix the scheduler lock race") || !strings.Contains(seen[0], "take the shard lock first") || strings.Contains(seen[0], "too-fresh") {
		t.Fatalf("prompt: %s", seen)
	}
	var verdicts []judgeVerdict
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "judge" {
			verdicts = append(verdicts, *e.Judge)
		}
	}
	if len(verdicts) != 3 || verdicts[0].P != 0.6 || verdicts[0].Relevant != "yes" || verdicts[0].Block != "00000000000000a1" {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	// The day's cap is spent: nothing more today.
	if n, _ := deliveryJudgePass(context.Background(), nil, now); n != 0 {
		t.Fatalf("past the cap: %d", n)
	}
	// Another day: the two lines left, and a refusal counts nothing.
	judgeCall = func(ctx context.Context, model, prompt string) (string, error) { return "", errJudgeRefused }
	if n, err := deliveryJudgePass(context.Background(), nil, now.Add(24*time.Hour)); n != 0 || err != errJudgeRefused {
		t.Fatalf("refused: %d %v", n, err)
	}
	b, _ := os.ReadFile(judgeStatePath())
	var st judgeState
	json.Unmarshal(b, &st)
	if st.Count != 0 {
		t.Fatalf("a refused pass counted: %+v", st)
	}
	// Off: nothing.
	t.Setenv("TACIT_DELIVERY_JUDGE", "off")
	if n, _ := deliveryJudgePass(context.Background(), nil, now.Add(24*time.Hour)); n != 0 {
		t.Fatal("judged with the judge off")
	}
}

// An answer that leaves a line out, or says something but yes, no or
// unsure, is a failure: nothing of it is kept.
func TestJudgeAnswersAreChecked(t *testing.T) {
	call := judgeCall
	t.Cleanup(func() { judgeCall = call })
	cases := []judgeCase{{Block: "b", Fact: "f1"}, {Block: "b", Fact: "f2"}}
	for _, ans := range []string{`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"}]}`, `{"lines":[{"i":1,"relevant":"maybe","confused":"no","novel":"no"},{"i":2,"relevant":"yes","confused":"no","novel":"no"}]}`, "no json"} {
		judgeCall = func(ctx context.Context, model, prompt string) (string, error) { return ans, nil }
		if _, err := judgeCases(context.Background(), "m", cases); err == nil {
			t.Errorf("accepted %q", ans)
		}
	}
}

// The eval's command redacts every case before the call, and sends nothing
// when redaction fails.
func TestDeliveryJudgeCommandRedacts(t *testing.T) {
	call, red := judgeCall, judgeRedactTexts
	t.Cleanup(func() { judgeCall, judgeRedactTexts = call, red })
	var sent []string
	judgeCall = func(ctx context.Context, model, prompt string) (string, error) {
		sent = append(sent, prompt)
		return `{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"}]}`, nil
	}
	run := func() int {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(`{"block":"b","fact":"f","hook":"prompt","line":"f: SECRET-x","request":"r","next":["SECRET-y"]}` + "\n")
		w.Close()
		in, out := os.Stdin, os.Stdout
		devnull, _ := os.Open(os.DevNull)
		os.Stdin, os.Stdout = r, devnull
		defer func() { os.Stdin, os.Stdout = in, out; r.Close(); devnull.Close() }()
		return runDeliveryJudge(nil)
	}
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = strings.ReplaceAll(s, "SECRET", "[redacted]")
		}
		return out, nil
	}
	if code := run(); code != 0 || len(sent) != 1 || strings.Contains(sent[0], "SECRET") || !strings.Contains(sent[0], "[redacted]-y") {
		t.Fatalf("code %d, sent %q", code, sent)
	}
	sent = nil
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) { return nil, errors.New("no redactor") }
	if code := run(); code == 0 || len(sent) != 0 {
		t.Fatalf("a failed redaction sent %d prompt(s), code %d", len(sent), code)
	}
}
