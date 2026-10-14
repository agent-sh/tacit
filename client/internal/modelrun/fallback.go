package modelrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func CanFallback(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidResult) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "accessdenied") || strings.Contains(text, "refusal") || regexp.MustCompile(`(?i)\b(?:http|status|status code|api error|code)\D{0,12}(?:400|403|404)\b`).MatchString(text) {
		return false
	}
	for _, phrase := range []string{"overloaded", "rate limit", "capacity", "throttl", "too many requests", "serviceunavailableexception", "model not ready", "modelnotready", "high load", "connection refused", "connection reset", "not logged in", "authentication expired", "unauthorized"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return regexp.MustCompile(`(?i)\b(?:http|status|status code|api error|code)\D{0,12}(?:429|500|503|529)\b`).MatchString(text)
}

func RunWithFallback(ctx context.Context, role, model, prompt, sid string) (Result, error) {
	primary, err := Resolve(role, model)
	if err != nil {
		return Result{}, err
	}
	fallback, err := Fallback(role, primary)
	if err != nil {
		return Result{}, err
	}
	return runPlan(ctx, role, primary, fallback, prompt, sid, Run)
}

func runPlan(ctx context.Context, role string, primary Route, fallback *Route, prompt, sid string, call func(context.Context, string, string, string, string) (Result, error)) (Result, error) {
	timeout, err := Timeout(role)
	if err != nil {
		return Result{}, err
	}
	one := func(r Route) (Result, error) {
		attempt, cancel := context.WithTimeout(WithRoute(ctx, r), timeout)
		defer cancel()
		answer, err := call(attempt, role, r.Model, prompt, sid)
		answer.Route = r
		return answer, err
	}
	answer, err := one(primary)
	if err == nil || fallback == nil || *fallback == primary || ctx.Err() != nil || !CanFallback(err) {
		return answer, err
	}
	fmt.Fprintf(os.Stderr, "tacit: %s unavailable (%v); trying %s\n", primary.Label(), err, fallback.Label())
	next, nextErr := one(*fallback)
	next.CostUSD += answer.CostUSD
	next.InputTokens += answer.InputTokens
	next.CostReported = answer.CostReported && next.CostReported
	if nextErr != nil {
		return next, fmt.Errorf("primary %s failed: %v; fallback %s: %w", primary.Label(), err, fallback.Label(), nextErr)
	}
	return next, nil
}

// Role options follow the tool's general runner_args, so a job's effort/profile
// wins without changing the operator's shared profile or CLI configuration.
func Args(r Route) ([]string, error) {
	var args []string
	if r.Profile != "" {
		args = append(args, "--profile", r.Profile)
	}
	if r.Effort != "" {
		switch r.Runner {
		case "claude":
			args = append(args, "--effort", r.Effort)
		case "codex":
			args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", r.Effort))
		case "grok", "muse":
			args = append(args, "--reasoning-effort", r.Effort)
		case "pi":
			args = append(args, "--thinking", r.Effort)
		default:
			return nil, fmt.Errorf("%s does not expose an effort adapter; use runner_args", r.Runner)
		}
	}
	return args, nil
}
func appendRouteArgs(args []string, r Route) ([]string, error) {
	options, err := Args(r)
	if err != nil {
		return nil, err
	}
	stdin := r.Runner == "codex" && len(args) > 0 && args[len(args)-1] == "-"
	if stdin {
		args = args[:len(args)-1]
	}
	args = append(args, options...)
	if stdin {
		args = append(args, "-")
	}
	return args, nil
}
