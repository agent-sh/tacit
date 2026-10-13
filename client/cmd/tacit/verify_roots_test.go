package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The verifier reads no rule directory unless TACIT_VERIFY_ROOTS names one.
func TestVerifyRootsComeOnlyFromTheEnvironment(t *testing.T) {
	t.Setenv("TACIT_VERIFY_ROOTS", "")
	if got := verifyRoots(); len(got) != 0 {
		t.Fatalf("roots with nothing set: %v", got)
	}
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("TACIT_VERIFY_ROOTS", strings.Join([]string{a, "", b + "/"}, string(filepath.ListSeparator)))
	if got := verifyRoots(); !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("roots %v, want %v", got, []string{a, b})
	}
	roots, err := probeRoots()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{a, b} {
		w, _ := filepath.EvalSymlinks(want)
		found := false
		for _, r := range roots {
			found = found || r == w
		}
		if !found {
			t.Errorf("probe roots %v lack %s", roots, w)
		}
	}
}

// A fresh curator has a store but may have no Claude auto-memory yet.
func TestVerifierWithoutAutoMemory(t *testing.T) {
	storeDir := t.TempDir()
	models := fakeClaudeFor(t, storeDir, "ok", "ok")
	home, _ := os.UserHomeDir()
	mem := memoryDir(home)
	if err := os.Remove(mem); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_VERIFY_ROOTS", "")
	if _, _, _, err := claudeVerifier(context.Background(), verifyModel, "check", t.TempDir(), 1); err != nil {
		t.Fatalf("fresh curator without auto-memory: %v", err)
	}
	if got := models(); len(got) != 1 {
		t.Fatalf("model calls: %v", got)
	}
	if _, err := os.Stat(mem); !os.IsNotExist(err) {
		t.Fatalf("verifier created auto-memory: %v", err)
	}

	// A misspelled explicit source must still stop the verifier.
	missing := filepath.Join(home, "missing-rules")
	t.Setenv("TACIT_VERIFY_ROOTS", missing)
	if _, _, _, err := claudeVerifier(context.Background(), verifyModel, "check", t.TempDir(), 1); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing configured rules: %v", err)
	}
	if got := models(); len(got) != 1 {
		t.Fatalf("called model after invalid roots: %v", got)
	}
}
