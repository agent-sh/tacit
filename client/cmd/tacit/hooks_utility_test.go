package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

type utilityTransport func(*http.Request) (*http.Response, error)

func (f utilityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPromptUtilityUsesScopedSummariesAndKeepsOnlyUseful(t *testing.T) {
	if _, err := exec.LookPath("scrump"); err != nil {
		t.Skip("scrump required for the live redaction integration")
	}
	t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "test-placeholder")
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", filepath.Join(t.TempDir(), "audit"))
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "endpoint-useful", Description: "endpoint api requires a schema validation step before release"},
		{Name: "endpoint-topical", Description: "endpoint api is a common networking topic"},
		{Name: "endpoint-foreign", Description: "endpoint api secrets for another repository", AppliesTo: fact.AppliesTo{Repos: []string{"foreign"}}},
	})
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.typesafe.ai/v1/systemone" {
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		var body struct {
			Model     string            `json:"model"`
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type         string            `json:"type"`
				Instructions map[string]string `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "jev-1.13.0" || body.State["task"] != "endpoint api release" {
			t.Errorf("unexpected model or task: %+v", body.State)
		}
		answers := map[string]any{}
		for id, q := range body.Questions {
			memory := q.Instructions["memory"]
			if strings.Contains(memory, "foreign") || strings.Contains(memory, "detail") {
				t.Error("unscoped or body text sent")
			}
			if q.Type != "noul" || q.Criteria["true"] == "" || q.Criteria["false"] == "" {
				t.Error("missing calibrated rubric")
			}
			score := 0.1
			if strings.Contains(memory, "schema validation") {
				score = 0.95
			}
			answers[id] = map[string]any{"type": "noul", "noul": score}
		}
		encoded, _ := json.Marshal(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	})
	got, err := promptHits(context.Background(), ix, search.Situation{Repo: "demo"}, "endpoint api release", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names(got)) != "endpoint-useful" {
		t.Fatalf("want useful only, got %s", names(got))
	}
	if calls != 1 {
		t.Fatalf("want one batched API call, got %d", calls)
	}
}
