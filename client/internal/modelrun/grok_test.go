package modelrun

import (
	"context"
	"encoding/json"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"testing"
)

func TestGrokJobsDoNotReuseNativeRegistry(t *testing.T) {
	source := t.TempDir()
	t.Setenv("GROK_HOME", source)
	os.WriteFile(filepath.Join(source, "agent_id"), []byte("stale-agent"), 0600)
	exe, _ := os.Executable()
	t.Setenv("TACIT_GROK_COMMAND", exe)
	t.Setenv("TACIT_TEST_RUNNER", "grok")
	t.Setenv("TACIT_TEST_STALE_GROK", "1")
	t.Setenv("TACIT_DREAM_RUNNER", "grok")
	capture := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("TACIT_TEST_CAPTURE", capture)
	var previous string
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), "dream", "chosen-model", "synthetic prompt", "sid"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(capture)
		var got struct {
			GrokHome string `json:"grok_home"`
		}
		json.Unmarshal(b, &got)
		if got.GrokHome == source || got.GrokHome == "" || got.GrokHome == previous {
			t.Fatal("native registry reused", got)
		}
		if _, err := os.Stat(got.GrokHome); !os.IsNotExist(err) {
			t.Fatal("disposable home survived")
		}
		previous = got.GrokHome
	}
}

func TestGrokHomeKeepsProviderAndPolicyWithoutUserIntegrations(t *testing.T) {
	source := t.TempDir()
	t.Setenv("GROK_HOME", source)
	os.WriteFile(filepath.Join(source, "config.toml"), []byte(`[models]
default="my-model"
[model.my-model]
model="provider-model"
base_url="https://provider.example/v1"
env_key="PROVIDER_KEY"
model_provider="fixture-provider"
[model.my-model.env_http_headers]
Authorization="PROVIDER_AUTH"
[model_providers.fixture-provider]
name="Fixture"
base_url="https://named-provider.example/v1"
wire_api="responses"
env_key="PROVIDER_KEY"
[auth]
disable_api_key_auth=true
[hooks]
SessionStart=[]
[mcp_servers.private]
command="private-server"
[plugins]
enabled=["private-plugin"]
`), 0600)
	os.WriteFile(filepath.Join(source, "managed_config.toml"), []byte(`[models]
allowed_models=["my-model"]
[long_reasoning_reminder]
tokens=1000
[memory_v2]
daily_token_cap=1000
[[hooks.SessionStart]]
hooks=[{type="command",command="mandatory-audit"}]
`), 0600)
	os.WriteFile(filepath.Join(source, "requirements.toml"), []byte(`[models]
allowed_models=["my-model"]
`), 0600)
	os.WriteFile(filepath.Join(source, "settings_cache.json"), []byte("native-policy-cache"), 0600)
	auth := filepath.Join(source, "auth.json")
	os.WriteFile(auth, []byte("native-fixture"), 0600)
	home, err := grokHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg, policy map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	toml.Unmarshal(b, &cfg)
	b, _ = os.ReadFile(filepath.Join(home, "managed_config.toml"))
	toml.Unmarshal(b, &policy)
	if cfg["model"] == nil || cfg["models"] == nil || cfg["model_providers"] == nil || cfg["mcp_servers"] != nil || cfg["hooks"] != nil || cfg["plugins"] != nil {
		t.Fatal("provider lost or integrations inherited", cfg)
	}
	if cfg["auth"].(map[string]any)["disable_api_key_auth"] != true {
		t.Fatal("authentication restriction lost")
	}
	if policy["long_reasoning_reminder"].(map[string]any)["tokens"] != int64(1000) || policy["memory_v2"].(map[string]any)["daily_token_cap"] != int64(1000) {
		t.Fatal("token controls lost")
	}
	if len(policy["allowed_mcp_servers"].([]any)) != 0 || policy["allow_managed_hooks_only"] != true || policy["hooks"] == nil {
		t.Fatal("policy loosened", policy)
	}
	a, _ := os.Stat(auth)
	linked, _ := os.Stat(filepath.Join(home, "auth.json"))
	if !os.SameFile(a, linked) {
		t.Fatal("native credentials were copied")
	}
	original, _ := os.Stat(filepath.Join(source, "requirements.toml"))
	ref, _ := os.Stat(filepath.Join(home, "requirements.toml"))
	if !os.SameFile(original, ref) {
		t.Fatal("native policy was not retained")
	}
	cache, _ := os.Stat(filepath.Join(source, "settings_cache.json"))
	linkedCache, _ := os.Stat(filepath.Join(home, "settings_cache.json"))
	if !os.SameFile(cache, linkedCache) {
		t.Fatal("signed policy cache was not retained")
	}
}

func TestGrokHomeRefusesEmbeddedProviderCredentials(t *testing.T) {
	source := t.TempDir()
	t.Setenv("GROK_HOME", source)
	os.WriteFile(filepath.Join(source, "config.toml"), []byte(`[model.example]
api_key="synthetic-credential"
`), 0600)
	if _, err := grokHome(t.TempDir()); err == nil {
		t.Fatal("embedded credential copied")
	}
}

func TestGrokHomeRejectsInlineEndpointCredentials(t *testing.T) {
	for _, field := range []string{"trace_upload_credentials", "deployment_key"} {
		source := t.TempDir()
		t.Setenv("GROK_HOME", source)
		os.WriteFile(filepath.Join(source, "config.toml"), []byte("[endpoints]\n"+field+"=\"synthetic-private-value\"\n"), 0600)
		if _, err := grokHome(t.TempDir()); err == nil {
			t.Fatalf("copied %s", field)
		}
	}
	source := t.TempDir()
	t.Setenv("GROK_HOME", source)
	os.WriteFile(filepath.Join(source, "config.toml"), []byte(`[model_providers.fixture]
experimental_bearer_token="synthetic-private-value"
`), 0600)
	if _, err := grokHome(t.TempDir()); err == nil {
		t.Fatal("copied named-provider credentials")
	}
}
