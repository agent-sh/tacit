package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

func providerIndex(t *testing.T) *search.Index {
	t.Helper()
	return relevanceIndex(t, []fact.Meta{
		{Name: "endpoint-useful", Description: "endpoint api requires a schema validation step before release"},
		{Name: "endpoint-topical", Description: "endpoint api is a common networking topic"},
	})
}

func fakeTransport(t *testing.T, f func(*http.Request, map[string]any) (int, any)) *int {
	t.Helper()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = utilityTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		status, reply := f(r, body)
		encoded, _ := json.Marshal(reply)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	})
	return &calls
}

// The JSON object a model provider is asked for: useful memories score high.
func modelScores(prompt string) string {
	var in struct {
		Memories []struct{ ID, Memory string } `json:"memories"`
	}
	_ = json.Unmarshal([]byte(prompt), &in)
	out := map[string]int{}
	for _, m := range in.Memories {
		out[m.ID] = 5
		if strings.Contains(m.Memory, "schema validation") {
			out[m.ID] = 97
		}
	}
	b, _ := json.Marshal(out)
	return "Scores: " + string(b)
}

func TestUtilityBedrockScoresWithConverse(t *testing.T) {
	stubScrump(t)
	t.Setenv("TACIT_UTILITY_PROVIDER", "bedrock")
	t.Setenv("TACIT_UTILITY_MODEL", "us.amazon.nova-micro-v1:0")
	t.Setenv("TACIT_UTILITY_REGION", "us-west-2")
	t.Setenv("TACIT_UTILITY_THRESHOLD", "0.9")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-placeholder")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-placeholder")
	ix := providerIndex(t)
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		if r.URL.Host != "bedrock-runtime.us-west-2.amazonaws.com" || r.URL.Path != "/model/us.amazon.nova-micro-v1:0/converse" {
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer bedrock-placeholder" {
			t.Errorf("wrong credential sent: %q", r.Header.Get("Authorization"))
		}
		system := body["system"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(system, utilityQuestion) {
			t.Error("rubric missing from the system prompt")
		}
		user := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		return 200, map[string]any{"output": map[string]any{"message": map[string]any{"content": []map[string]string{{"text": modelScores(user)}}}},
			"usage": map[string]int{"inputTokens": 400}}
	})
	got, err := promptHits(context.Background(), ix, search.Situation{Repo: "demo"}, "endpoint api release", nil)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || names(got) != "endpoint-useful" {
		t.Fatalf("calls %d, want useful only, got %s", *calls, names(got))
	}
}

func TestUtilityOpenAICompatibleSendsOnlyItsOwnKey(t *testing.T) {
	stubScrump(t)
	t.Setenv("TACIT_UTILITY_PROVIDER", "openai")
	t.Setenv("TACIT_UTILITY_ENDPOINT", "http://127.0.0.1:11434/v1/")
	t.Setenv("TACIT_UTILITY_MODEL", "local-judge")
	t.Setenv("TACIT_UTILITY_THRESHOLD", "0.9")
	t.Setenv("TACIT_UTILITY_API_KEY", "endpoint-placeholder")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-placeholder")
	t.Setenv("OPENAI_API_KEY", "openai-placeholder")
	ix := providerIndex(t)
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		if r.URL.String() != "http://127.0.0.1:11434/v1/chat/completions" || body["model"] != "local-judge" {
			t.Errorf("unexpected request %s %v", r.URL, body["model"])
		}
		if r.Header.Get("Authorization") != "Bearer endpoint-placeholder" {
			t.Errorf("wrong credential sent: %q", r.Header.Get("Authorization"))
		}
		user := body["messages"].([]any)[1].(map[string]any)["content"].(string)
		return 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": modelScores(user)}}}}
	})
	got, _ := promptHits(context.Background(), ix, search.Situation{Repo: "demo"}, "endpoint api release", nil)
	if *calls != 1 || names(got) != "endpoint-useful" {
		t.Fatalf("calls %d, want useful only, got %s", *calls, names(got))
	}
}

// A self-hosted System One server: its own model name is accepted, and the
// TypeSafe key is never sent to it.
func TestUtilitySystemOneEndpointKeepsTheTypeSafeKeyAtHome(t *testing.T) {
	stubScrump(t)
	t.Setenv("TACIT_UTILITY_PROVIDER", "systemone")
	t.Setenv("TACIT_UTILITY_ENDPOINT", "http://localhost:8000/v1/systemone")
	t.Setenv("TACIT_UTILITY_THRESHOLD", "0.8")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-placeholder")
	ix := providerIndex(t)
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		if r.URL.String() != "http://localhost:8000/v1/systemone" {
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("a credential was sent to a custom endpoint: %q", auth)
		}
		if _, ok := body["model"]; ok {
			t.Error("an unset model must be omitted")
		}
		answers := map[string]any{}
		for id, q := range body["questions"].(map[string]any) {
			score := 0.1
			if strings.Contains(q.(map[string]any)["instructions"].(map[string]any)["memory"].(string), "schema validation") {
				score = 0.9
			}
			answers[id] = map[string]any{"type": "noul", "noul": score}
		}
		return 200, map[string]any{"model": "strands-decider-0.1.0", "answers": answers}
	})
	got, _ := promptHits(context.Background(), ix, search.Situation{Repo: "demo"}, "endpoint api release", nil)
	if *calls != 1 || names(got) != "endpoint-useful" {
		t.Fatalf("calls %d, want useful only, got %s", *calls, names(got))
	}
}

// Misconfiguration or a bad reply never blocks a prompt: the local selector answers.
func TestUtilityProviderFailuresFallBackToLocalSelection(t *testing.T) {
	stubScrump(t)
	hits := []search.Hit{{Path: "a", Name: "a", Scope: "system", Score: 30}}
	cases := map[string]map[string]string{
		"threshold_required":    {"TACIT_UTILITY_PROVIDER": "bedrock", "TACIT_UTILITY_MODEL": "m", "AWS_BEARER_TOKEN_BEDROCK": "k"},
		"invalid_endpoint":      {"TACIT_UTILITY_PROVIDER": "systemone", "TACIT_UTILITY_ENDPOINT": "http://example.com/v1/systemone", "TACIT_UTILITY_THRESHOLD": "0.5"},
		"invalid_region":        {"TACIT_UTILITY_PROVIDER": "bedrock", "TACIT_UTILITY_MODEL": "m", "TACIT_UTILITY_REGION": "evil.example.com/x", "TACIT_UTILITY_THRESHOLD": "0.5", "AWS_BEARER_TOKEN_BEDROCK": "k"},
		"key_unavailable":       {"TACIT_UTILITY_PROVIDER": "bedrock", "TACIT_UTILITY_MODEL": "m", "TACIT_UTILITY_THRESHOLD": "0.5", "AWS_BEARER_TOKEN_BEDROCK": ""},
		"response_invalid":      {"TACIT_UTILITY_PROVIDER": "openai", "TACIT_UTILITY_ENDPOINT": "https://judge.example/v1", "TACIT_UTILITY_MODEL": "m", "TACIT_UTILITY_THRESHOLD": "0.5"},
		"no_model_for_bedrock":  {"TACIT_UTILITY_PROVIDER": "bedrock", "TACIT_UTILITY_THRESHOLD": "0.5", "AWS_BEARER_TOKEN_BEDROCK": "k"},
		"no_endpoint_for_model": {"TACIT_UTILITY_PROVIDER": "openai", "TACIT_UTILITY_MODEL": "m", "TACIT_UTILITY_THRESHOLD": "0.5"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"TACIT_UTILITY_MODEL", "TACIT_UTILITY_ENDPOINT", "TACIT_UTILITY_REGION", "TACIT_UTILITY_THRESHOLD", "TACIT_UTILITY_API_KEY"} {
				t.Setenv(k, "")
			}
			for k, v := range env {
				t.Setenv(k, v)
			}
			fakeTransport(t, func(*http.Request, map[string]any) (int, any) {
				return 200, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "I would rather not say."}}}}
			})
			var status string
			got := utilityPromptSelectionWork(context.Background(), "synthetic query", hits, nil, nil, 20, func(a utilityAudit) { status = a.Status })
			if names(got) != "a" || status == "ok" {
				t.Fatalf("got %s with status %q, want the local selection and a failure status", names(got), status)
			}
		})
	}
}
