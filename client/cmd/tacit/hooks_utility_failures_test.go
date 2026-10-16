package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestUtilityRankCapDerivedAndNoBackfill(t *testing.T) {
	hits := []search.Hit{{Path: "a", Name: "a"}, {Path: "b", Name: "b"}, {Path: "c", Name: "c"}, {Path: "d", Name: "d"}}
	derived := []search.Hit{{Path: "e", Name: "e", Scope: "derived/demo"}, {Path: "f", Name: "f", Scope: "derived/demo"}}
	values := map[string]float64{"a": 1, "b": .9, "c": .8, "d": .75, "e": .1, "f": 1}
	got := utilityRank(hits, derived, map[string]bool{"a": true}, values, .7)
	if names(got) != "b,c" {
		t.Fatalf("backfilled or used second derived: %s", names(got))
	}
	values["e"] = .7
	if got := names(utilityRank(hits, derived, nil, values, .7)); got != "a,b,c,e" {
		t.Fatal(got)
	}
}

func TestUtilityRejectsInvalidRepliesAndRedirects(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	cases := []struct {
		name, body string
		status     int
	}{
		{"auth", "sensitive response must not be logged", 401},
		{"redirect", "", 302},
		{"malformed", "not json", 200},
		{"wrong-model", `{"model":"other","answers":{"candidate_0":{"type":"noul","noul":0.9}}}`, 200},
		{"missing", `{"model":"jev-1.13.0","answers":{}}`, 200},
		{"null", `{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":null}}}`, 200},
		{"range", `{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":2}}}`, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{"Location": []string{"https://not-the-provider.example"}}}, nil
			})
			_, err := requestUtility(context.Background(), "dummy", []byte(`{}`), 1)
			if err == nil || strings.Contains(err.Error(), "sensitive") || calls != 1 {
				t.Fatalf("bad response accepted or followed: %v calls=%d", err, calls)
			}
		})
	}
}

func TestUtilityDeadlineAndFallback(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := requestUtility(ctx, "dummy", []byte(`{}`), 1); err == nil {
		t.Fatal("deadline ignored")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline not bounded")
	}
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("TACIT_UTILITY_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", t.TempDir())
	hits := []search.Hit{{Path: "valid", Name: "valid", Scope: "system", Score: 30}}
	if got := names(utilityPromptSelection(context.Background(), "synthetic query", hits, nil, nil, 20)); got != "valid" {
		t.Fatalf("missing key broke local fallback: %s", got)
	}
}

func TestUtilityRedactionFailureMakesNoRequest(t *testing.T) {
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "dummy")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", t.TempDir())
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = utilityTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unredacted request sent"); return nil, nil })
	hits := []search.Hit{{Path: "valid", Name: "valid", Scope: "system", Score: 30}}
	if got := names(utilityPromptSelection(context.Background(), "synthetic query", hits, nil, nil, 20)); got != "valid" {
		t.Fatal(got)
	}
}

func TestUtilityRedactsBeforeEgressAndLogging(t *testing.T) {
	if _, err := exec.LookPath("scrump"); err != nil {
		t.Skip("scrump required for the live redaction integration")
	}
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "dummy")
	dir := t.TempDir()
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", dir)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := "Zq7Vx9" + "Lm2Kp4"
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), secret) {
			t.Error("secret left machine")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":0.9}}}`)), Header: make(http.Header)}, nil
	})
	hits := []search.Hit{{Path: "valid", Name: "valid", Description: "deployment password=" + secret, Scope: "system", Score: 30}}
	utilityPromptSelection(context.Background(), "deployment password="+secret, hits, nil, nil, 20)
	if calls != 1 {
		t.Fatal("redaction path was not exercised")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret logged")
		}
	}
}

func TestUtilityKeyAndPrivateAudit(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	dir := t.TempDir()
	key := filepath.Join(dir, "key.env")
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("OTHER=ignore\nexport TYPESAFE_API_KEY='dummy' # comment\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := utilityKey(key); got != "dummy" || err != nil {
		t.Fatalf("key parser: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("private audit requires Unix ownership and no-follow checks")
	}
	old := filepath.Join(dir, "utility-2001-01-01.jsonl")
	os.WriteFile(old, []byte("old"), 0600)
	unrelated := filepath.Join(dir, "keep.txt")
	os.WriteFile(unrelated, []byte("keep"), 0600)
	appendUtilityAudit(dir, utilityAudit{Model: utilityModel, Status: "ok"})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old receipts retained")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrelated file deleted")
	}
	file := filepath.Join(dir, "utility-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("audit permissions")
	}
	raw, _ := os.ReadFile(file)
	var event utilityAudit
	if json.Unmarshal(raw, &event) != nil || event.Model != utilityModel {
		t.Fatal("invalid receipt")
	}
}
