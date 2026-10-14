package modelrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func CanFallback(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidResult) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Kind == "capacity" || failure.Kind == "unavailable" || failure.Kind == "authentication"
	}
	return false
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
	one := func(r Route, fallback bool) (Result, error) {
		attemptContext := WithRoute(ctx, r)
		if fallback {
			attemptContext = FallbackContext(attemptContext)
		}
		attempt, cancel := context.WithTimeout(attemptContext, timeout)
		defer cancel()
		answer, err := call(attempt, role, r.Model, prompt, sid)
		answer.Route = r
		return answer, err
	}
	answer, err := one(primary, false)
	if err == nil || fallback == nil || *fallback == primary || ctx.Err() != nil || !CanFallback(err) {
		return answer, err
	}
	fmt.Fprintf(os.Stderr, "tacit: %s unavailable (%v); trying %s\n", primary.Label(), err, fallback.Label())
	next, nextErr := one(*fallback, true)
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
