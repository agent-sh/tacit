package modelrun

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestGenericRouteDefaults(t *testing.T) {
	t.Setenv("TACIT_RUNNER", "")
	for role, want := range map[string]Route{
		"curate": {Runner: "claude", Model: "opus"},
		"verify": {Runner: "claude", Model: "opus"},
		"dream":  {Runner: "claude", Model: "opus"},
		"judge":  {Runner: "claude", Model: "haiku"},
	} {
		t.Setenv(roleEnv(role, "RUNNER"), "")
		t.Setenv(roleEnv(role, "MODEL"), "")
		got, err := Resolve(role, Model(role, ""))
		if err != nil || got != want {
			t.Fatalf("%s: %+v %v", role, got, err)
		}
		fb, err := Fallback(role, got)
		if role != "verify" {
			if err != nil || fb != nil {
				t.Fatalf("%s unexpected fallback: %+v %v", role, fb, err)
			}
			continue
		}
		if err != nil || fb == nil {
			t.Fatalf("%s fallback: %v", role, err)
		}
		expected := Route{Runner: "claude", Model: "sonnet"}
		if *fb != expected {
			t.Fatalf("%s fallback: %+v", role, *fb)
		}
	}
}

func TestCrossRunnerFallbackIsBoundedAndReportsCost(t *testing.T) {
	primary := Route{Runner: "claude", Model: "primary-model"}
	fallback := Route{Runner: "codex", Model: "fallback-model"}
	var calls []Route
	call := func(ctx context.Context, role, model, prompt, sid string) (Result, error) {
		r, err := FromContext(ctx, role, model)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, r)
		if len(calls) == 1 {
			return Result{CostUSD: 0.1, CostReported: true}, ProviderFailure(503, "", "provider unavailable")
		}
		return Result{Text: "[]", CostUSD: 0.2, CostReported: true}, nil
	}
	got, err := runPlan(context.Background(), "curate", primary, &fallback, "private prompt", "sid", call)
	if err != nil || len(calls) != 2 || calls[0] != primary || calls[1] != fallback || got.Route != fallback || got.CostUSD < 0.299 || !got.CostReported {
		t.Fatalf("calls %v, result %+v: %v", calls, got, err)
	}
}

func TestAttemptTimeoutLeavesFallbackTime(t *testing.T) {
	t.Setenv("TACIT_DREAM_TIMEOUT", "20ms")
	primary := Route{Runner: "grok", Model: "primary-model"}
	fallback := Route{Runner: "claude", Model: "fallback-model"}
	attempts := 0
	result, err := runPlan(context.Background(), "dream", primary, &fallback, "p", "sid", func(ctx context.Context, role, model, prompt, sid string) (Result, error) {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		if ctx.Err() != nil {
			t.Fatal("fallback inherits expired deadline")
		}
		return Result{Text: "[]"}, nil
	})
	if err != nil || attempts != 2 || result.Route != fallback {
		t.Fatalf("%+v %v attempts %d", result, err, attempts)
	}
}

func TestProtocolFailureAndCallerCancellationDoNotFallback(t *testing.T) {
	for _, failure := range []error{ErrInvalidResult, context.Canceled, errors.New("model did not complete: refusal")} {
		calls := 0
		p := Default("dream")
		f := Default("curate")
		_, err := runPlan(context.Background(), "dream", p, &f, "p", "s", func(context.Context, string, string, string, string) (Result, error) {
			calls++
			return Result{}, failure
		})
		if calls != 1 || err == nil {
			t.Fatalf("%v: calls %d err %v", failure, calls, err)
		}
	}
}

func TestRouteEffortArgsAndCodexStdin(t *testing.T) {
	cases := []struct {
		route Route
		want  []string
	}{
		{Route{Runner: "claude", Effort: "medium"}, []string{"--effort", "medium"}},
		{Route{Runner: "grok", Effort: "xhigh"}, []string{"--reasoning-effort", "xhigh"}},
		{Route{Runner: "codex", Profile: "work", Effort: "high"}, []string{"--profile", "work", "-c", `model_reasoning_effort="high"`}},
	}
	for _, c := range cases {
		got, err := Args(c.route)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%+v: %v %v", c.route, got, err)
		}
	}
	args, err := appendRouteArgs([]string{"exec", "-"}, Route{Runner: "codex", Effort: "high"})
	if err != nil || args[len(args)-1] != "-" {
		t.Fatal(args, err)
	}
}

func TestConfigOverridesAndDisablesFallback(t *testing.T) {
	t.Setenv("TACIT_VERIFY_RUNNER", "codex")
	t.Setenv("TACIT_VERIFY_EFFORT", "xhigh")
	t.Setenv("TACIT_VERIFY_PROFILE", "work")
	r, err := Resolve("verify", Default("verify").Model)
	if err != nil || r.Effort != "xhigh" || r.Profile != "work" {
		t.Fatal(r, err)
	}
	t.Setenv("TACIT_VERIFY_FALLBACK", "null")
	if f, err := Fallback("verify", r); f != nil || err != nil {
		t.Fatal(f, err)
	}
	t.Setenv("TACIT_VERIFY_TIMEOUT", "bad")
	if _, err := Timeout("verify"); err == nil {
		t.Fatal("bad timeout accepted")
	}
	t.Setenv("TACIT_VERIFY_TIMEOUT", "45s")
	if d, err := Timeout("verify"); d != 45*time.Second || err != nil {
		t.Fatal(d, err)
	}
}

func TestProviderDenialsStopFallback(t *testing.T) {
	for _, message := range []string{"HTTP 403 unauthorized", "API Error: 404 unavailable model", "AccessDeniedException", "model refusal: capacity policy"} {
		if CanFallback(errors.New(message)) {
			t.Fatalf("fallback allowed for %q", message)
		}
	}
}
