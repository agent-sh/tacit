package modelrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

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
	name := args[len(args)-2]
	path := filepath.Join(home, name+".config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := toml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "operator-model" || got["model_provider"] != "custom" {
		t.Fatal("provider choice lost")
	}
	if _, ok := got["mcp_servers"]; ok {
		t.Fatal("MCP configuration inherited")
	}
	if _, ok := got["features"]; ok {
		t.Fatal("tool configuration inherited")
	}
	if strings.Contains(strings.Join(args, " "), "provider.example") {
		t.Fatal("provider configuration leaked into argv")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("temporary profile survived")
	}
	unchanged, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(unchanged) != config {
		t.Fatal("user configuration changed")
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
			if strings.Contains(strings.Join(args, " "), "work") {
				t.Fatalf("unsanitized profile in argv: %v", args)
			}
			b, err := os.ReadFile(filepath.Join(home, args[len(args)-2]+".config.toml"))
			if err != nil || !strings.Contains(string(b), "chosen") {
				t.Fatalf("profile lost: %v", err)
			}
		})
	}
}
