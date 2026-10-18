package modelrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeStructuredOutputWinsMalformedText(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"result":            "[{\"description\":\"Use \\`cmd\\`\"}]",
		"structured_output": map[string]any{"facts": []any{map[string]any{"description": "Use `cmd` without escapes."}}},
		"total_cost_usd":    0.1,
		"usage":             map[string]any{"input_tokens": 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := parse("claude", payload)
	var got struct {
		Facts []struct{ Description string }
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(result.Text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 || got.Facts[0].Description != "Use `cmd` without escapes." || result.CostUSD != 0.1 || result.InputTokens != 12 {
		t.Fatalf("structured output or accounting lost: %+v %+v", result, got)
	}
}

func TestSchemaAdapterProtocolAndMissingOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	schema := "{\"type\":\"object\",\"properties\":{\"facts\":{\"type\":\"array\"}},\"required\":[\"facts\"]}"
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TACIT_DREAM_RUNNER", name)
			t.Setenv("TACIT_"+strings.ToUpper(name)+"_COMMAND", exe)
			t.Setenv("TACIT_TEST_RUNNER", name)
			capture := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("TACIT_TEST_CAPTURE", capture)
			if name == "claude" {
				t.Setenv("TACIT_TEST_RESPONSE", "{\"result\":\"done\",\"structured_output\":{\"facts\":[]},\"total_cost_usd\":0.1}")
			}
			result, err := Run(WithJSONSchema(context.Background(), schema), "dream", "model", "synthetic", "sid")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var got struct{ Args []string }
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			hasSchema := false
			turns := ""
			for i := 0; i+1 < len(got.Args); i++ {
				if got.Args[i] == "--json-schema" {
					hasSchema = got.Args[i+1] == schema
				}
				if got.Args[i] == "--max-turns" {
					turns = got.Args[i+1]
				}
			}
			if name == "claude" && (!hasSchema || turns != "2" || !result.Structured) {
				t.Fatalf("schema protocol missing: %+v %+v", got, result)
			}
			if name != "claude" && (hasSchema || result.Structured || result.Text != "[]") {
				t.Fatalf("other runner changed: %+v %+v", got, result)
			}
			if name == "claude" {
				t.Setenv("TACIT_TEST_RESPONSE", responses["claude"])
				result, err = Run(WithJSONSchema(context.Background(), schema), "dream", "model", "synthetic", "sid")
				if !errors.Is(err, ErrInvalidResult) || result.InputTokens != 12 || result.CostUSD != 0.01 {
					t.Fatalf("missing schema result accepted or accounting lost: %+v %v", result, err)
				}
			}
		})
	}
}
