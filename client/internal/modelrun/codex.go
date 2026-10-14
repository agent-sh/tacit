package modelrun

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// codexProfile preserves the user's provider and model settings while excluding
// hooks, MCP servers, instructions and tool permissions. Auth stays in the real
// CODEX_HOME; no auth files are copied or read by Tacit. The selected profile is
// reduced to provider/model CLI overrides; embedded credentials are refused.
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

	// --ignore-user-config skips named profiles too. Reconstruct the selected
	// provider/model settings as explicit CLI overrides; never put credentials
	// in argv. Native auth still uses the real CODEX_HOME and provider env.
	if err := publicProviderSettings(selected); err != nil {
		return nil, cleanup, err
	}
	var keys []string
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	stdin := len(remaining) > 0 && remaining[len(remaining)-1] == "-"
	if stdin {
		remaining = remaining[:len(remaining)-1]
	}
	var defaults []string
	for _, key := range keys {
		value, err := tomlLiteral(selected[key])
		if err != nil {
			return nil, cleanup, err
		}
		defaults = append(defaults, "-c", key+"="+value)
	}
	for i, arg := range remaining {
		if arg == "exec" {
			withDefaults := append([]string{}, remaining[:i+1]...)
			withDefaults = append(withDefaults, defaults...)
			remaining = append(withDefaults, remaining[i+1:]...)
			break
		}
	}
	if stdin {
		remaining = append(remaining, "-")
	}
	return remaining, cleanup, nil
}

func publicProviderSettings(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			normalized := strings.ToLower(key)
			if normalized == "env_key" || normalized == "env_http_headers" {
				continue
			}
			if secretSettingName(key) || strings.Contains(strings.ReplaceAll(normalized, "_", ""), "headers") {
				return fmt.Errorf("Codex provider credentials must use native auth or environment references")
			}
			if normalized == "base_url" {
				if address, ok := item.(string); ok {
					parsed, err := url.Parse(address)
					if err != nil || parsed.User != nil {
						return fmt.Errorf("Codex provider URL must not contain credentials")
					}
					for queryKey := range parsed.Query() {
						if secretSettingName(queryKey) {
							return fmt.Errorf("Codex provider URL must not contain credentials")
						}
					}
				}
			}
			if err := publicProviderSettings(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := publicProviderSettings(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func secretSettingName(key string) bool {
	n := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
	if n == "modelautocompacttokenlimit" {
		return false
	}
	for _, part := range []string{"token", "secret", "password", "apikey", "accesskey", "cookie"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	switch n {
	case "key", "auth", "authorization", "signature", "sig":
		return true
	}
	return false
}

func tomlLiteral(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return strconv.Quote(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case map[string]any:
		var keys, items []string
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			literal, err := tomlLiteral(v[key])
			if err != nil {
				return "", err
			}
			items = append(items, strconv.Quote(key)+" = "+literal)
		}
		return "{" + strings.Join(items, ", ") + "}", nil
	case []any:
		var items []string
		for _, item := range v {
			literal, err := tomlLiteral(item)
			if err != nil {
				return "", err
			}
			items = append(items, literal)
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	default:
		return "", fmt.Errorf("unsupported Codex provider setting type %T", value)
	}
}
