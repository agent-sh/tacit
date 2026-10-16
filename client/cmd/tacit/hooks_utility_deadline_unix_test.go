//go:build unix

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestUtilityRejectsFIFOKeyAndAuditWithoutBlocking(t *testing.T) {
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "")
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := syscall.Mkfifo(key, 0600); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(dir, "utility-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	if err := syscall.Mkfifo(audit, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_UTILITY_KEY_FILE", key)
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", dir)
	hits := []search.Hit{{Path: "a", Name: "a", Scope: "system", Score: 30}}
	start := time.Now()
	if got := names(utilityPromptSelection(context.Background(), "synthetic query", hits, nil, nil, 20)); got != "a" {
		t.Fatal(got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("special file blocked delivery")
	}
}

func TestUtilitySelectorDeadlineCoversUnresponsiveWorker(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\n/bin/cat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "dummy")
	t.Setenv("TACIT_UTILITY_TIMEOUT", "200ms")
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", "")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
		}
	}()
	http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		defer close(finished)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":0.1}}}`)), Header: make(http.Header)}, nil
	})
	hits := []search.Hit{{Path: "a", Name: "a", Scope: "system", Score: 30}}
	start := time.Now()
	if got := names(utilityPromptSelection(context.Background(), "synthetic query", hits, nil, nil, 20)); got != "a" {
		t.Fatal(got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("worker blocked whole selector")
	}
	select {
	case <-entered:
	default:
		t.Fatal("blocking worker was not exercised")
	}
}
