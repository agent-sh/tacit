package modelrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedCallsKeepReportedAccounting(t *testing.T) {
	cases := []struct {
		runner, answer string
		cost           float64
		tokens         int
		known          bool
	}{
		{"claude", `{"is_error":true,"result":"provider failed","api_error_status":503,"total_cost_usd":0.3,"usage":{"input_tokens":120}}`, .3, 120, true},
		{"grok", `{"type":"error","status":429,"message":"provider failed","total_cost_usd":0.2,"usage":{"input_tokens":40}}`, .2, 40, true},
		{"hermes", `{"type":"result","is_error":true,"total_cost_usd":0.1,"tokens":{"input":30}}`, .1, 30, true},
		{"agy", `{"event":"result","result":{"status":"FAILURE","usage":{"input_tokens":20,"cache_read_tokens":5}}}`, 0, 25, false},
	}
	for _, c := range cases {
		result, err := parse(c.runner, []byte(c.answer))
		if err == nil || result.CostUSD != c.cost || result.InputTokens != c.tokens || result.CostReported != c.known {
			t.Fatalf("%s: %+v %v", c.runner, result, err)
		}
	}
}

func TestFallbackUsesTypedProviderMetadata(t *testing.T) {
	if CanFallback(errors.New(`unknown model "capacity-tuned"`)) {
		t.Fatal("invalid model started a fallback")
	}
	for _, status := range []int{400, 403, 404} {
		if CanFallback(ProviderFailure(status, "", "capacity overloaded unavailable")) {
			t.Fatalf("denial %d retried", status)
		}
	}
	for _, status := range []int{401, 429, 500, 503, 529} {
		err := fmt.Errorf("wrapped: %w", ProviderFailure(status, "", "provider response"))
		if !CanFallback(err) {
			t.Fatalf("provider status %d did not survive wrapping", status)
		}
	}
	result, err := parse("codex", []byte(`{"type":"turn.failed","error":{"message":"{\"status\":503,\"error\":{\"message\":\"try again\"}}"}}`))
	if err == nil || !CanFallback(err) || result.Text != "" {
		t.Fatal(result, err)
	}
}

func TestTraceDistinguishesUnknownAndReportedZero(t *testing.T) {
	trace := &Trace{}
	ctx := WithTrace(context.Background(), trace)
	Record(ctx, "dream", Result{Route: Route{Runner: "grok"}, CostUSD: .3, CostReported: true, InputTokens: 120}, ProviderFailure(503, "", "provider failed"))
	Record(FallbackContext(ctx), "dream", Result{Route: Route{Runner: "muse"}}, nil)
	Record(ctx, "dream", Result{CostReported: true}, nil)
	data, err := json.Marshal(trace)
	if err != nil || !strings.Contains(string(data), `"cost_usd":null`) || !strings.Contains(string(data), `"cost_usd":0`) || trace.UnknownCostCalls != 1 || !trace.Calls[1].Fallback || trace.Calls[0].Failure != "capacity" {
		t.Fatal(string(data), err)
	}
	if !strings.Contains(trace.CostSummary(.3), "unknown for 1") {
		t.Fatal(trace.CostSummary(.3))
	}
}

func TestPreflightRejectsBrokenArgumentsAndProfiles(t *testing.T) {
	t.Setenv("TACIT_MUSE_ARGS", "not JSON")
	if err := Preflight(Route{Runner: "muse"}); err == nil {
		t.Fatal("invalid runner arguments accepted")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := Preflight(Route{Runner: "codex", Profile: "missing"}); err == nil {
		t.Fatal("missing profile accepted")
	}
	if err := Preflight(Route{Runner: "hermes", Effort: "high"}); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

func TestRunnerKeepsTypedFailureAndFailedAccounting(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("TACIT_TEST_RUNNER", "claude")
	t.Setenv("TACIT_DREAM_RUNNER", "claude")
	t.Setenv("TACIT_CLAUDE_COMMAND", exe)
	t.Setenv("TACIT_TEST_CAPTURE", filepath.Join(t.TempDir(), "capture"))
	t.Setenv("TACIT_TEST_RESPONSE", `{"is_error":true,"api_error_status":503,"result":"failed","total_cost_usd":0.3,"usage":{"input_tokens":120}}`)
	t.Setenv("TACIT_TEST_EXIT", "1")
	trace := &Trace{}
	result, err := Run(WithTrace(context.Background(), trace), "dream", "model", "p", "sid")
	if !CanFallback(err) || result.CostUSD != .3 || result.InputTokens != 120 || len(trace.Calls) != 1 || trace.Calls[0].CostUSD == nil || trace.Calls[0].Failure != "capacity" {
		t.Fatal(result, trace, err)
	}
	t.Setenv("TACIT_TEST_RESPONSE", "not JSON")
	t.Setenv("TACIT_TEST_EXIT", "0")
	_, err = Run(context.Background(), "dream", "model", "p", "sid")
	if !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("parser error identity lost: %v", err)
	}
}

func TestUnknownCostAndPiRetries(t *testing.T) {
	for _, cost := range []string{"null", `"unknown"`} {
		r, err := parse("claude", []byte(`{"result":"[]","total_cost_usd":`+cost+`}`))
		if err != nil || r.CostReported {
			t.Fatal(r, err)
		}
	}
	r, err := parse("pi", []byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","usage":{"input":10}}}
{"type":"agent_end","willRetry":true}
{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"[]"}],"usage":{"input":20,"cost":{"total":0.1}}}}
{"type":"agent_end"}`))
	if err != nil || r.CostReported || r.CostUSD != .1 || r.InputTokens != 30 {
		t.Fatal(r, err)
	}
}
