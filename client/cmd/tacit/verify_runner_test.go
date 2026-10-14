package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func TestPortableVerifierUsesGuardedEvidence(t *testing.T) {
	stubScrump(t)
	root := t.TempDir()
	project := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	source := filepath.Join(project, "config.go")
	if err := os.WriteFile(source, []byte("const maxPeers = 64\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(project, "credentials.json")
	if err := os.WriteFile(secret, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	call := func(ctx context.Context, prompt string) (modelrun.Result, error) {
		calls++
		if calls == 1 {
			b, _ := json.Marshal(map[string]any{"probes": [][]string{{"read", source}, {"read", secret}, {"read", filepath.Join(root, "outside.txt")}}})
			return modelrun.Result{Text: string(b), CostUSD: 0.01}, nil
		}
		if !strings.Contains(prompt, "maxPeers = 64") || !strings.Contains(prompt, "REFUSED") || strings.Contains(prompt, "private fixture") {
			t.Fatal("evidence escaped its boundary")
		}
		return modelrun.Result{Text: `{"verdicts":[{"id":"one","mark":"keep","evidence":"config.go:1 sets maxPeers to 64"}]}`, CostUSD: 0.01}, nil
	}
	vs, _, cost, err := portableVerifier(context.Background(), "model", "Check the fact", 4, call)
	if err != nil || len(vs) != 1 || calls != 2 || cost != 0.02 {
		t.Fatalf("verdicts=%v calls=%d cost=%v err=%v", vs, calls, cost, err)
	}
}

func TestPortableVerifierRejectsMissingVerdicts(t *testing.T) {
	_, _, _, err := portableVerifier(context.Background(), "model", "p", 4, func(context.Context, string) (modelrun.Result, error) { return modelrun.Result{Text: `{}`}, nil })
	if err == nil {
		t.Fatal("unstructured success accepted")
	}
}

func TestVerifierFallsBackAcrossRunnersAndRecordsRoute(t *testing.T) {
	primary := modelrun.Route{Runner: "codex", Model: "primary-model", Effort: "high"}
	fallback := modelrun.Route{Runner: "claude", Model: "fallback-model", Effort: "medium"}
	r := verifyRun{model: primary.Model, fallback: fallback.Model, route: &primary, fallbackRoute: &fallback, cwd: t.TempDir(), stateDir: t.TempDir()}
	var routes []modelrun.Route
	call := func(ctx context.Context, model, prompt, cwd string, budget float64) ([]verifyVerdict, string, float64, error) {
		route, err := modelrun.FromContext(ctx, "verify", model)
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, route)
		if len(routes) == 1 {
			return nil, "primary failed", 0.1, &capacityError{model, fmt.Errorf("HTTP 503")}
		}
		return []verifyVerdict{{ID: "one", Mark: "both", Evidence: "checked"}}, "fallback verdict", 0.2, nil
	}
	verdicts, label, cost, _, err := r.callBatch(context.Background(), call, "prompt")
	if err != nil || len(verdicts) != 1 || len(routes) != 2 || routes[0] != primary || routes[1] != fallback || label != fallback.Label() || cost < 0.299 {
		t.Fatalf("%+v %q %f %v routes=%+v", verdicts, label, cost, err, routes)
	}
}

func TestNativeVerifierKeepsEligibleFailureTypes(t *testing.T) {
	for _, status := range []string{"401", "502", "504"} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			models := fakeClaudeFor(t, dir, status, "ok")
			primary := modelrun.Route{Runner: "claude", Model: verifyModel}
			fallback := modelrun.Route{Runner: "claude", Model: verifyFallbackModel}
			r := verifyRun{model: verifyModel, fallback: verifyFallbackModel, route: &primary, fallbackRoute: &fallback, cwd: dir, stateDir: t.TempDir(), budget: 4}
			trace := &modelrun.Trace{}
			_, label, cost, _, err := r.callBatch(modelrun.WithTrace(context.Background(), trace), claudeVerifier, "check")
			if err != nil || label != fallback.Label() || len(models()) != 2 || cost < .349 || len(trace.Calls) != 2 || !trace.Calls[1].Fallback || trace.Calls[0].CostUSD == nil {
				t.Fatalf("native status %s failed fallback: %s %f %v %+v", status, label, cost, err, trace)
			}
		})
	}
}

func TestNativeVerifierUnknownCostIsNotZero(t *testing.T) {
	dir := t.TempDir()
	fakeClaudeFor(t, dir, "unknowncost", "ok")
	trace := &modelrun.Trace{}
	_, _, cost, err := claudeVerifier(modelrun.WithTrace(context.Background(), trace), verifyModel, "check", dir, 4)
	if err != nil || cost != 0 || len(trace.Calls) != 1 || trace.Calls[0].CostUSD != nil || trace.UnknownCostCalls != 1 {
		t.Fatal(trace, cost, err)
	}
}
