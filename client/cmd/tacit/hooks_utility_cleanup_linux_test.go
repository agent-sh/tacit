//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestUtilityCleanupExitChild(t *testing.T) {
	if os.Getenv("TACIT_UTILITY_EXIT_TEST") != "1" {
		return
	}
	_ = os.Setenv("PATH", os.Getenv("TACIT_UTILITY_TEST_BIN"))
	utilityPromptSelection(context.Background(), "synthetic request", []search.Hit{{Path: "a", Name: "a", Scope: "system", Score: 30}}, nil, nil, 20)
	os.Exit(0)
}

func TestUtilityCLIExitStopsRedactorDescendants(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "descendant.pid")
	script := "#!/bin/sh\n/bin/sleep 20 &\nprintf '%s' \"$!\" > \"$TACIT_UTILITY_TEST_PIDFILE\"\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "scrump"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUtilityCleanupExitChild$")
	cmd.Env = append(os.Environ(), "TACIT_UTILITY_EXIT_TEST=1", "TACIT_UTILITY_PROVIDER=jev", "TACIT_UTILITY_TIMEOUT=200ms", "TYPESAFE_API_KEY=dummy", "TACIT_UTILITY_AUDIT_DIR=", "TACIT_UTILITY_TEST_PIDFILE="+pidfile, "TACIT_UTILITY_TEST_BIN="+dir)
	if raw, err := cmd.CombinedOutput(); err != nil {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("child CLI: %v %s entries=%v path=%s args=%v", err, raw, entries, cmd.Path, cmd.Args)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal("descendant was not started", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		// A zombie is terminated and awaiting its new parent's reap, not running.
		if os.IsNotExist(err) || (err == nil && strings.Contains(string(stat), ") Z ")) {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("redactor descendant survived CLI exit")
		}
	}
}

func TestUtilitySlowAuditDoesNotDiscardSelection(t *testing.T) {
	selected := []search.Hit{{Path: "chosen", Name: "chosen"}}
	release, finished := make(chan struct{}), make(chan struct{})
	defer func() { close(release); <-finished }()
	start := time.Now()
	got := finishUtilitySelection(selected, utilityAudit{Candidates: []utilityAuditHit{{ID: "chosen"}}}, "synthetic", func(_ string, event utilityAudit) {
		defer close(finished)
		<-release
		if !event.Candidates[0].Selected {
			t.Error("receipt did not reflect delivered selection")
		}
	})
	if names(got) != "chosen" {
		t.Fatal("slow audit discarded selection")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("audit blocked delivery")
	}
}
