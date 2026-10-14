package modelrun

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func overrides(t *testing.T, args []string) map[string]any {
	t.Helper()
	got := map[string]any{}
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) {
			var value map[string]any
			if err := toml.Unmarshal([]byte(args[i+1]), &value); err != nil {
				t.Fatal(err)
			}
			for k, v := range value {
				got[k] = v
			}
		}
	}
	return got
}

func TestCodexProfileKeepsProviderAndRemovesTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	config := `model="operator-model"
model_provider="custom"
model_reasoning_effort="low"
[model_providers.custom]
name="Custom"
base_url="https://provider.example/v1"
env_key="PROVIDER_API_KEY"
[mcp_servers.unrelated]
command="must-not-start"
[features]
shell_tool=true
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	args, cleanup, err := codexProfile([]string{"exec", "--ignore-user-config", "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got := overrides(t, args)
	if got["model"] != "operator-model" || got["model_provider"] != "custom" {
		t.Fatal("provider choice lost")
	}
	for _, key := range []string{"mcp_servers", "features"} {
		if _, ok := got[key]; ok {
			t.Fatalf("inherited %s", key)
		}
	}
	if args[len(args)-1] != "-" {
		t.Fatal("stdin marker moved")
	}
	unchanged, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(unchanged) != config {
		t.Fatal("user config changed")
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Fatal("temporary config survived")
	}
}

func TestCodexProfileArgumentForms(t *testing.T) {
	for _, form := range [][]string{{"--profile", "work"}, {"-p", "work"}, {"--profile=work"}, {"-pwork"}} {
		t.Run(strings.Join(form, "_"), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[profiles.work]\nmodel=\"chosen\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args, cleanup, err := codexProfile(append(append([]string{"exec"}, form...), "-"))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if got := overrides(t, args)["model"]; got != "chosen" {
				t.Fatal("profile lost", got)
			}
			for _, arg := range args {
				if strings.HasPrefix(arg, "--profile") || arg == "-p" || arg == "work" {
					t.Fatalf("ignored profile in argv: %v", args)
				}
			}
		})
	}
}

func TestCodexProviderSecretsNeverReachArgv(t *testing.T) {
	fake := "synthetic-" + "credential"
	userURL := url.URL{Scheme: "https", Host: "provider.example", User: url.UserPassword("fixture", fake)}
	queryName := strings.Join([]string{"api", "key"}, "-")
	fields := []map[string]any{
		{"experimental_bearer_token": fake},
		{"http_headers": map[string]any{"Authorization": fake}},
		{"base_url": userURL.String()},
		{"query_params": map[string]any{queryName: fake}},
	}
	for _, key := range []string{"password", "key"} {
		fields = append(fields, map[string]any{"base_url": "https://provider.example?" + url.Values{key: []string{fake}}.Encode()})
	}
	for _, field := range fields {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		config := map[string]any{"model_provider": "custom", "model_providers": map[string]any{"custom": field}}
		data, err := toml.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), data, 0600); err != nil {
			t.Fatal(err)
		}
		args, _, err := codexProfile([]string{"exec", "-"})
		if err == nil || len(args) != 0 {
			t.Fatal("embedded credentials accepted")
		}
		if strings.Contains(err.Error(), fake) {
			t.Fatal("credential echoed in error")
		}
	}
}

func TestExplicitCodexOptionsOverrideProfileDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider=\"base\"\nmodel_reasoning_effort=\"medium\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args, cleanup, err := codexProfile([]string{"exec", "-c", `model_provider="operator"`, "-c", `model_reasoning_effort="high"`, "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got := overrides(t, args)
	if got["model_provider"] != "operator" || got["model_reasoning_effort"] != "high" {
		t.Fatal("explicit options lost", got)
	}
}
