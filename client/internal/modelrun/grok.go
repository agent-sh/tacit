package modelrun

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Grok persists agent and session state in GROK_HOME. Reusing that state in
// independent headless jobs can stall before sampling. Give each job a fresh
// home, retaining provider settings and references to native auth and policy.
func grokHome(dir string) (string, error) {
	source := os.Getenv("GROK_HOME")
	if source == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		source = filepath.Join(home, ".grok")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	home := filepath.Join(dir, "grok-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		return "", err
	}
	read := func(name string) (map[string]any, error) {
		b, err := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read Grok %s", name)
		}
		config := map[string]any{}
		if toml.Unmarshal(b, &config) != nil {
			return nil, fmt.Errorf("invalid Grok %s", name)
		}
		return config, nil
	}
	config, err := read("config.toml")
	if err != nil {
		return "", err
	}
	selected := map[string]any{}
	for _, key := range []string{"model", "models", "model_providers", "auth", "grok_com_config", "auth_provider", "endpoints"} {
		if value, ok := config[key]; ok {
			selected[key] = value
		}
	}
	if err := grokPublicSettings(selected); err != nil {
		return "", err
	}
	b, err := toml.Marshal(selected)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), b, 0600); err != nil {
		return "", err
	}
	// Retain managed defaults and mandatory hooks. The added policy only
	// tightens: user/project/plugin hooks and every MCP server are excluded.
	managed, err := read("managed_config.toml")
	if err != nil {
		return "", err
	}
	if err := grokPublicSettings(managed); err != nil {
		return "", err
	}
	delete(managed, "allowedMcpServers")
	managed["allowed_mcp_servers"] = []any{}
	delete(managed, "allowManagedHooksOnly")
	managed["allow_managed_hooks_only"] = true
	features, _ := managed["features"].(map[string]any)
	if features == nil {
		features = map[string]any{}
	}
	features["managed_config"] = false
	managed["features"] = features
	mcps, _ := managed["managed_mcps"].(map[string]any)
	if mcps == nil {
		mcps = map[string]any{}
	}
	mcps["enabled"] = false
	mcps["gateway_tools_enabled"] = false
	managed["managed_mcps"] = mcps
	b, err = toml.Marshal(managed)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(home, "managed_config.toml"), b, 0600); err != nil {
		return "", err
	}
	// The CLI reads its existing sign-in itself. Tacit neither reads nor copies
	// credentials. Hard links are the Windows fallback when symlinks are denied.
	names := []string{"auth.json"}
	policies, err := filepath.Glob(filepath.Join(source, "requirements.*"))
	if err != nil {
		return "", err
	}
	for _, policy := range policies {
		names = append(names, filepath.Base(policy))
	}
	// Retain the native settings cache alongside any requirements inputs.
	// Reference that cache when a requirements file exists; keep agent/session
	// registry files fresh so the headless job cannot reuse a stuck identity.
	if len(policies) > 0 {
		names = append(names, "settings_cache.json")
	}
	for _, name := range names {
		from, to := filepath.Join(source, name), filepath.Join(home, name)
		info, err := os.Stat(from)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("Grok %s is not a readable regular file", name)
		}
		if err := os.Symlink(from, to); err != nil {
			if err := os.Link(from, to); err != nil {
				return "", fmt.Errorf("cannot reference Grok %s; put the job cache on the same filesystem or enable symlinks", name)
			}
		}
	}
	return home, nil
}

func grokPublicSettings(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			n := strings.ToLower(key)
			if n == "env_key" || n == "env_http_headers" {
				continue
			}
			publicTokenField := false
			switch n {
			case "auth", "auth_provider", "token_header", "auth_token_ttl", "max_tokens", "max_completion_tokens", "max_output_tokens", "input_tokens", "output_tokens", "compaction_at_tokens", "tokens_per_second", "thinking_budget_tokens", "model_auto_compact_token_limit":
				publicTokenField = true
			}
			if !publicTokenField && secretSettingName(n) {
				// Boolean restrictions and numeric token counts are not
				// credentials. Exact credential fields remain refused below.
				switch item.(type) {
				case bool, int64, float64:
				default:
					return fmt.Errorf("Grok provider credentials must use native auth or environment references")
				}
			}
			credential := strings.NewReplacer("_", "", "-", "").Replace(n)
			if strings.Contains(credential, "credentials") {
				return fmt.Errorf("Grok provider credentials must use native auth or environment references")
			}
			switch credential {
			case "key", "apikey", "accesstoken", "refreshtoken", "token", "secret", "clientsecret", "password", "privatekey", "authorization", "accesskey", "secretaccesskey", "awssecretaccesskey", "extraheaders", "headers", "httpheaders", "deploymentkey":
				return fmt.Errorf("Grok provider credentials must use native auth or environment references")
			}
			if strings.HasSuffix(n, "url") || strings.HasSuffix(n, "origin") || n == "issuer" {
				if address, ok := item.(string); ok {
					u, err := url.Parse(address)
					if err != nil || u.User != nil {
						return fmt.Errorf("Grok provider URLs must not contain credentials")
					}
					for key := range u.Query() {
						if secretSettingName(key) {
							return fmt.Errorf("Grok provider URLs must not contain credentials")
						}
					}
				}
			}
			if err := grokPublicSettings(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := grokPublicSettings(item); err != nil {
				return err
			}
		}
	}
	return nil
}
