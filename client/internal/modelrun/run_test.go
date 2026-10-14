package modelrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var responses = map[string]string{
	"claude": `{"result":"[]","total_cost_usd":0.01,"usage":{"input_tokens":12}}`,
	"codex":  "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"[]\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":12}}",
	"grok":   `{"text":"[]","stopReason":"end_turn","sessionId":"synthetic","total_cost_usd":0.01}`,
	"pi":     "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"[]\"}]}}\n{\"type\":\"agent_end\"}",
	"muse":   `{"payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"[]"}}`,
	"agy":    `{"event":"result","result":{"status":"SUCCESS","response":"[]","usage":{"input_tokens":12}}}`,
	"hermes": `{"type":"result","exit_code":0,"text":"[]","tokens":{"input":12}}`,
}

func TestMain(m *testing.M) {
	if name := os.Getenv("TACIT_TEST_RUNNER"); name != "" {
		if os.Getenv("TACIT_TEST_HANG") == "1" {
			time.Sleep(time.Minute)
			os.Exit(1)
		}
		input, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		if name == "grok" || name == "muse" || name == "hermes" {
			for i, arg := range os.Args {
				if (arg == "--prompt-file" || arg == "--query-file") && i+1 < len(os.Args) {
					input, _ = os.ReadFile(os.Args[i+1])
				}
			}
		}
		b, _ := json.Marshal(map[string]any{"args": os.Args[1:], "prompt": string(input), "cwd": cwd, "job": os.Getenv("TACIT_MODEL_JOB")})
		_ = os.WriteFile(os.Getenv("TACIT_TEST_CAPTURE"), b, 0600)
		response := responses[name]
		if override := os.Getenv("TACIT_TEST_RESPONSE"); override != "" {
			response = override
		}
		fmt.Println(response)
		if os.Getenv("TACIT_TEST_EXIT") == "1" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestConfiguredRunnersUseTheirProtocolAndKeepPromptsOffArgv(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TACIT_DREAM_RUNNER", name)
			t.Setenv("TACIT_"+strings.ToUpper(name)+"_COMMAND", exe)
			t.Setenv("TACIT_TEST_RUNNER", name)
			capture := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("TACIT_TEST_CAPTURE", capture)
			prompt := strings.Repeat("synthetic transcript ", 4000)
			res, err := Run(context.Background(), "dream", "chosen-model", prompt, "12345678-1234-4234-8234-123456789abc")
			if err != nil || res.Text != "[]" {
				t.Fatalf("%s: %+v %v", name, res, err)
			}
			b, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Args             []string
				Prompt, Cwd, Job string
			}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(got.Args, " "), "synthetic transcript") {
				t.Fatal("prompt leaked into argv")
			}
			if !strings.Contains(got.Prompt, "synthetic transcript") || got.Job != "1" {
				t.Fatal("prompt or recursion guard missing")
			}
			if _, err := os.Stat(got.Cwd); !os.IsNotExist(err) {
				t.Fatalf("job directory survived: %v", err)
			}
			if !strings.Contains(strings.Join(got.Args, " "), "chosen-model") {
				t.Fatal("model override not passed")
			}
		})
	}
}

func TestRunnerDeadline(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("TACIT_DREAM_RUNNER", "codex")
	t.Setenv("TACIT_CODEX_COMMAND", exe)
	t.Setenv("TACIT_TEST_RUNNER", "codex")
	t.Setenv("TACIT_TEST_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, "dream", "", "hello", "")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("deadline failed: %v, elapsed %v", err, time.Since(start))
	}
}

func TestDefaultsAndRoleOverrides(t *testing.T) {
	t.Setenv("TACIT_RUNNER", "")
	t.Setenv("TACIT_DREAM_RUNNER", "")
	t.Setenv("TACIT_DREAM_MODEL", "")
	if name, _ := Name("dream"); name != "claude" || Model("dream", "haiku") != "haiku" {
		t.Fatal("default changed")
	}
	t.Setenv("TACIT_RUNNER", "muse")
	if Model("dream", "haiku") != "" {
		t.Fatal("Claude model leaked into another runner")
	}
	t.Setenv("TACIT_DREAM_RUNNER", "agy")
	if name, _ := Name("dream"); name != "agy" {
		t.Fatal(name)
	}
	t.Setenv("TACIT_DREAM_MODEL", "chosen")
	if Model("dream", "haiku") != "chosen" {
		t.Fatal("model override ignored")
	}
	t.Setenv("TACIT_DREAM_RUNNER", "typo")
	if _, err := Name("dream"); err == nil {
		t.Fatal("unknown runner accepted")
	}
}

func TestParseRejectsIncompleteOrFailedRuns(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"claude", `{"is_error":true,"result":"[]"}`},
		{"grok", `{"text":"[]","stopReason":"max_tokens"}`},
		{"grok", `{"type":"error","message":"capacity 429"}`},
		{"codex", `{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}`},
		{"muse", `{"payload":{"kind":"run_terminal","terminal":"failed","text":"[]"}}`},
		{"agy", `{"event":"result","result":{"status":"ERROR","response":"[]"}}`},
		{"hermes", `{"type":"result","exit_code":1,"text":"[]"}`},
		{"pi", `{"type":"message_end","message":{"role":"assistant","stopReason":"error"}}`},
	} {
		if _, err := parse(c.name, []byte(c.body)); err == nil {
			t.Errorf("%s accepted failure", c.name)
		}
	}
}

// Opt-in native protocol check. Only the synthetic prompt leaves the process.
func TestInstalledRunner(t *testing.T) {
	name := os.Getenv("TACIT_SMOKE_RUNNER")
	if name == "" {
		t.Skip("set TACIT_SMOKE_RUNNER for an installed CLI check")
	}
	t.Setenv("TACIT_DREAM_RUNNER", name)
	duration := 90 * time.Second
	if raw := os.Getenv("TACIT_SMOKE_TIMEOUT"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	sid := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	res, err := Run(ctx, "dream", os.Getenv("TACIT_SMOKE_MODEL"), "Return exactly [] as your final response. Do not call any tools or read any files.", sid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Text) != "[]" {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	t.Logf("%s: completed; input_tokens=%d", name, res.InputTokens)
}

// Opt-in check of the shipped route, including profile and effort. Only a
// synthetic prompt is sent; no store or project data is supplied.
func TestInstalledDefaultRoute(t *testing.T) {
	role := os.Getenv("TACIT_SMOKE_ROUTE")
	if role == "" {
		t.Skip("set TACIT_SMOKE_ROUTE for a native default-route check")
	}
	if role != "curate" && role != "verify" && role != "dream" {
		t.Fatal("unknown smoke role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := Default(role)
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	sid := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	prompt := "Return exactly [] as your final response. Do not call any tools or read any files."
	var res Result
	var err error
	if os.Getenv("TACIT_SMOKE_FALLBACK") == "1" {
		res, err = RunWithFallback(ctx, role, r.Model, prompt, sid)
	} else {
		res, err = Run(WithRoute(ctx, r), role, r.Model, prompt, sid)
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Text) != "[]" {
		t.Fatalf("unexpected reply %q", res.Text)
	}
	t.Logf("completed: %s; input_tokens=%d", res.Route.Label(), res.InputTokens)
}
