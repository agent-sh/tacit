package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude is a claude binary on PATH that logs the model it was asked
// for and answers by the mode set for that model: "ok" marks every finding
// in the prompt "both", any other mode is a failed call shaped like the
// real claude -p output for that HTTP status.
const fakeClaude = `#!/bin/sh
model=""; prompt=""
while [ $# -gt 0 ]; do
  case "$1" in
    --model) model="$2"; shift ;;
    -p) prompt="$2"; shift ;;
  esac
  shift
done
echo "$model" >> "$FAKE_LOG"
mode="$FAKE_SONNET"
[ "$model" = "$FAKE_PRIMARY" ] && mode="$FAKE_OPUS"
case "$mode" in
  ok|partial*)
    # A pair finding is marked "both", a stale one "keep" (still current).
    mark=both
    case "$prompt" in *'"kind":"stale"'*) mark=keep ;; esac
    vs=""
    for id in $(printf '%s' "$prompt" | grep -oE '"(ID|id)":"[0-9a-f]+"' | cut -d'"' -f4); do
      [ -n "$vs" ] && vs="$vs,"
      vs="$vs{\"id\":\"$id\",\"mark\":\"$mark\",\"evidence\":\"checked\"}"
    done
    case "$mode" in
      ok) echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"total_cost_usd\":0.25,\"result\":\"done\",\"structured_output\":{\"verdicts\":[$vs]}}" ;;
      partial529) echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":true,\"api_error_status\":529,\"total_cost_usd\":0.3,\"result\":\"API Error: 529 overloaded_error\",\"structured_output\":{\"verdicts\":[$vs]}}"; exit 1 ;;
      partialbudget) echo "{\"type\":\"result\",\"subtype\":\"error_max_budget_usd\",\"is_error\":true,\"total_cost_usd\":1.1,\"result\":\"\",\"structured_output\":{\"verdicts\":[$vs]}}"; exit 1 ;;
    esac
    ;;
  529nostatus)
    echo '{"type":"result","subtype":"success","is_error":true,"total_cost_usd":0,"result":"Repeated 529 Overloaded errors"}'
    exit 1 ;;
  403)
    echo '{"type":"result","subtype":"success","is_error":true,"api_error_status":403,"total_cost_usd":0,"result":"API Error: 403 AccessDeniedException: You do not have access to the model with the specified model ID."}'
    exit 1 ;;
  503)
    echo '{"type":"result","subtype":"success","is_error":true,"api_error_status":503,"total_cost_usd":0,"result":"API Error: 503 ServiceUnavailableException: Bedrock is unable to process your request."}'
    exit 1 ;;
  529late)
    echo '{"type":"result","subtype":"success","is_error":true,"api_error_status":529,"total_cost_usd":0.4,"result":"API Error: Repeated 529 Overloaded errors"}'
    exit 1 ;;
  400)
    echo "[claude-code:unrecognized_model] {\"model\":\"$model\"}" >&2
    echo '{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"total_cost_usd":0,"result":"API Error: 400 The provided model identifier is invalid.. Run --model to pick a different model."}'
    exit 1 ;;
  budget)
    echo '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":1.1,"result":""}'
    exit 1 ;;
esac
`

// fallbackFixture puts the fake claude first on PATH, gives the verifier a
// home with the directories it reads, and points the store at a review
// fixture clone. It returns the clone and a reader for the logged models.
func fallbackFixture(t *testing.T, opus, sonnet string) (*reviewServer, func() []string) {
	t.Helper()
	_, clone := reviewFixture(t)
	rs := clone("one")
	return rs, fakeClaudeFor(t, rs.s.Dir, opus, sonnet)
}

// fakeClaudeFor installs the fake claude for a store and returns a reader
// for the models it was asked for.
func fakeClaudeFor(t *testing.T, storeDir, opus, sonnet string) func() []string {
	t.Helper()
	stubScrump(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".config", "agents"), 0o755)
	os.MkdirAll(memoryDir(home), 0o755)
	logf := filepath.Join(t.TempDir(), "models.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", storeDir)
	t.Setenv("FAKE_LOG", logf)
	t.Setenv("FAKE_PRIMARY", verifyModel)
	t.Setenv("FAKE_OPUS", opus)
	t.Setenv("FAKE_SONNET", sonnet)
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	old := providerRetryWait
	providerRetryWait = time.Millisecond
	t.Cleanup(func() { providerRetryWait = old })
	return func() []string {
		b, _ := os.ReadFile(logf)
		return strings.Fields(string(b))
	}
}

// decisionModels reads the model recorded on each agent decision.
func decisionModels(t *testing.T, rs *reviewServer) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	var models []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var d decision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("decision record %q: %v", line, err)
		}
		if d.By == "agent" {
			models = append(models, d.Model)
		}
	}
	return models
}

func defaultRun(t *testing.T) verifyRun {
	return verifyRun{model: verifyModel, fallback: verifyFallbackModel, max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir()}
}

func TestVerifyOpusAnswersWithoutFallback(t *testing.T) {
	rs, models := fallbackFixture(t, "ok", "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r := defaultRun(t)
	n, cost, err := r.run(ctx, rs.s, claudeVerifier)
	if err != nil || n == 0 {
		t.Fatalf("run: n %d cost %v err %v", n, cost, err)
	}
	if got := models(); len(got) != 1 || got[0] != verifyModel {
		t.Fatalf("models called %v, want one Opus call", got)
	}
	ds := decisionModels(t, rs)
	if len(ds) != n {
		t.Fatalf("%d agent decisions for %d verdicts", len(ds), n)
	}
	for _, m := range ds {
		if m != verifyModel {
			t.Fatalf("decision recorded model %q, want %s", m, verifyModel)
		}
	}
	if r.fellBack != 0 || r.byModel[verifyModel] != n || !strings.Contains(r.modelSummary(), verifyModel) {
		t.Fatalf("summary %q fellBack %d", r.modelSummary(), r.fellBack)
	}
}

func TestVerifyOpusWithoutCapacityFallsBackToSonnet(t *testing.T) {
	// partial529: verdicts came back with the error; they are discarded and
	// only Sonnet's land.
	for _, mode := range []string{"503", "529late", "529nostatus", "partial529"} {
		t.Run(mode, func(t *testing.T) {
			rs, models := fallbackFixture(t, mode, "ok")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			r := defaultRun(t)
			n, cost, err := r.run(ctx, rs.s, claudeVerifier)
			if err != nil || n == 0 {
				t.Fatalf("run: n %d err %v", n, err)
			}
			got := models()
			if len(got) < 2 || got[0] != verifyModel || got[len(got)-1] != verifyFallbackModel {
				t.Fatalf("models called %v, want Opus first and Sonnet last", got)
			}
			for _, m := range got[:len(got)-1] {
				if m != verifyModel {
					t.Fatalf("models called %v: Sonnet ran more than once", got)
				}
			}
			for _, m := range decisionModels(t, rs) {
				if m != verifyFallbackModel {
					t.Fatalf("decision recorded model %q, want %s", m, verifyFallbackModel)
				}
			}
			if r.fellBack != 1 || r.byModel[verifyFallbackModel] != n || r.byModel[verifyModel] != 0 || !strings.Contains(r.modelSummary(), "fell back") {
				t.Fatalf("summary %q fellBack %d", r.modelSummary(), r.fellBack)
			}
			// The Opus call that ran before it failed is paid for too.
			if (mode == "529late" || mode == "partial529") && cost < 0.5 {
				t.Fatalf("cost %v leaves out the failed Opus call", cost)
			}
		})
	}
}

func TestVerifyOtherFailuresDoNotFallBack(t *testing.T) {
	// partialbudget: verdicts came back with a non-capacity error; none land.
	for _, mode := range []string{"400", "403", "budget", "partialbudget"} {
		t.Run(mode, func(t *testing.T) {
			rs, models := fallbackFixture(t, mode, "ok")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			r := defaultRun(t)
			n, _, err := r.run(ctx, rs.s, claudeVerifier)
			if err == nil || n != 0 {
				t.Fatalf("a %s failure should fail the batch: n %d err %v", mode, n, err)
			}
			for _, m := range models() {
				if m != verifyModel {
					t.Fatalf("models called %v: fell back on a %s failure", models(), mode)
				}
			}
			if len(decisionModels(t, rs)) != 0 || r.fellBack != 0 {
				t.Fatal("verdicts landed after a failed batch")
			}
		})
	}
}

// --model names one model: a capacity failure fails the batch.
func TestVerifyNamedModelHasNoFallback(t *testing.T) {
	rs, models := fallbackFixture(t, "503", "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r := defaultRun(t)
	r.fallback = ""
	if n, _, err := r.run(ctx, rs.s, claudeVerifier); err == nil || n != 0 {
		t.Fatalf("n %d err %v", n, err)
	}
	for _, m := range models() {
		if m != verifyModel {
			t.Fatalf("models called %v with no fallback set", models())
		}
	}
}

// A stale finding goes through the same fallback, and its decision records
// the model that produced it.
func TestVerifyStaleFindingFallsBackAndRecordsTheModel(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	models := fakeClaudeFor(t, fx.rs.s.Dir, "503", "ok")
	r := defaultRun(t)
	r.tl = fx.b
	n, _, err := r.run(ctx, fx.rs.s, claudeVerifier)
	if err != nil || n != 1 {
		t.Fatalf("run: settled %d err %v", n, err)
	}
	if got := models(); len(got) < 2 || got[0] != verifyModel || got[len(got)-1] != verifyFallbackModel {
		t.Fatalf("models called %v, want Opus first and Sonnet last", got)
	}
	fx.rs.loadFacts()
	found := false
	for _, it := range fx.rs.staleItems() {
		if it.A.Name == "engine-retries" {
			found = true
			if it.Decision == nil || it.Decision.By != "agent" || it.Decision.Action != "current" || it.Decision.Model != verifyFallbackModel {
				t.Fatalf("stale decision %+v", it.Decision)
			}
		}
	}
	if !found || r.fellBack != 1 || r.byModel[verifyFallbackModel] != 1 {
		t.Fatalf("found %v fellBack %d byModel %v", found, r.fellBack, r.byModel)
	}
}

func TestCapacityFailureClassification(t *testing.T) {
	cases := []struct {
		status int
		text   string
		want   bool
	}{
		{529, "API Error: Repeated 529 Overloaded errors", true},
		{503, "ServiceUnavailableException", true},
		{429, "ThrottlingException: Too many requests", true},
		{0, "Opus is experiencing high load, please use /model to switch to Sonnet", true},
		{500, "ModelNotReadyException: model is not ready", true},
		{0, "Repeated 529 Overloaded errors", true},
		// Lost access is not capacity: it fails loudly instead of running on Sonnet.
		{404, "ResourceNotFoundException: model not found in this region", false},
		{403, "AccessDeniedException: You don't have access to the model with the specified model ID.", false},
		{400, "The provided model identifier is invalid.", false},
		{400, "ValidationException: input is too long", false},
		{500, "Internal server error", false},
		{0, "", false},
	}
	for _, c := range cases {
		if got := capacityFailure(c.status, c.text); got != c.want {
			t.Errorf("capacityFailure(%d, %q) = %v, want %v", c.status, c.text, got, c.want)
		}
	}
}
