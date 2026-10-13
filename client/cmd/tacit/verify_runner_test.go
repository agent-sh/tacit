package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func TestPortableVerifierUsesGuardedEvidence(t *testing.T) {
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
