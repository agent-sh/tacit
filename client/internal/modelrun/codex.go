package modelrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// codexProfile preserves the user's provider and model settings while excluding
// hooks, MCP servers, instructions and tool permissions. Auth stays in the real
// CODEX_HOME; no auth files are copied or read by Tacit. The explicit profile is
// mode 0600 and removed after the child finishes, including failed runs.
func codexProfile(args []string) ([]string, func(), error) {
	cleanup := func() {}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, cleanup, err
		}
		home = filepath.Join(user, ".codex")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, cleanup, err
	}
	config := map[string]any{}
	read := func(path string, missingOK bool) (map[string]any, error) {
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) && missingOK {
			return map[string]any{}, nil
		}
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := toml.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("invalid Codex configuration at %s", path)
		}
		return out, nil
	}
	config, err = read(filepath.Join(home, "config.toml"), true)
	if err != nil {
		return nil, cleanup, err
	}
	profile, _ := config["profile"].(string)
	var remaining []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--profile=") {
			profile = strings.TrimPrefix(args[i], "--profile=")
			if profile == "" {
				return nil, cleanup, fmt.Errorf("Codex profile argument needs a value")
			}
			continue
		}
		if strings.HasPrefix(args[i], "-p") && !strings.HasPrefix(args[i], "--") && len(args[i]) > 2 {
			profile = strings.TrimPrefix(args[i][2:], "=")
			if profile == "" {
				return nil, cleanup, fmt.Errorf("Codex profile argument needs a value")
			}
			continue
		}
		if args[i] == "--profile" || args[i] == "-p" {
			if i+1 >= len(args) {
				return nil, cleanup, fmt.Errorf("Codex profile argument needs a value")
			}
			i++
			profile = args[i]
			continue
		}
		remaining = append(remaining, args[i])
	}
	if profile != "" {
		if strings.ContainsAny(profile, "/\\") || profile == "." || profile == ".." {
			return nil, cleanup, fmt.Errorf("Codex profile must be a name")
		}
		overlay, err := read(filepath.Join(home, profile+".config.toml"), true)
		if err != nil {
			return nil, cleanup, err
		}
		if len(overlay) == 0 {
			if profiles, ok := config["profiles"].(map[string]any); ok {
				overlay, _ = profiles[profile].(map[string]any)
			}
		}
		if len(overlay) == 0 {
			return nil, cleanup, fmt.Errorf("Codex profile %q was not found", profile)
		}
		for k, v := range overlay {
			config[k] = v
		}
	}
	selected := map[string]any{}
	for _, key := range []string{"model", "model_provider", "model_providers", "model_catalog_json", "model_reasoning_effort", "model_reasoning_summary", "model_verbosity", "model_context_window", "model_auto_compact_token_limit", "service_tier", "preferred_auth_method", "forced_login_method", "forced_chatgpt_workspace_id"} {
		if value, ok := config[key]; ok {
			selected[key] = value
		}
	}
	if len(selected) == 0 {
		return remaining, cleanup, nil
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, cleanup, err
	}
	f, err := os.CreateTemp(home, "tacit-job-*.config.toml")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	b, err := toml.Marshal(selected)
	if err == nil {
		_, err = f.Write(b)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("write isolated Codex profile: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(f.Name()), ".config.toml")
	// Keep stdin's '-' positional marker last.
	if len(remaining) > 0 && remaining[len(remaining)-1] == "-" {
		remaining = remaining[:len(remaining)-1]
	}
	return append(remaining, "--profile", name, "-"), cleanup, nil
}
