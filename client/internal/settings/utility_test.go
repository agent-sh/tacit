package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUtilityConfigurationPathsOverridesAndValidation(t *testing.T) {
	keys := []string{"TACIT_UTILITY_PROVIDER", "TACIT_UTILITY_THRESHOLD", "TACIT_UTILITY_TIMEOUT", "TACIT_UTILITY_KEY_FILE", "TACIT_UTILITY_AUDIT_DIR"}
	for _, key := range keys {
		clear(t, key)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	raw := "utility:\n  provider: jev\n  threshold: 0.7\n  timeout: 1s\n  key_file: ./key.env\n  audit_dir: ./audit\n"
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_UTILITY_PROVIDER") != "jev" || os.Getenv("TACIT_UTILITY_THRESHOLD") != "0.7" || os.Getenv("TACIT_UTILITY_KEY_FILE") != filepath.Join(dir, "key.env") || os.Getenv("TACIT_UTILITY_AUDIT_DIR") != filepath.Join(dir, "audit") {
		t.Fatal("utility config not resolved")
	}
	t.Setenv("TACIT_UTILITY_THRESHOLD", "0.8")
	if err := Load(file, true); err != nil || os.Getenv("TACIT_UTILITY_THRESHOLD") != "0.8" {
		t.Fatal("environment override lost")
	}
	for _, bad := range []string{"utility: {provider: unknown}", "utility: {threshold: -0.1}", "utility: {threshold: .nan}", "utility: {threshold: 1.1}", "utility: {timeout: 3s}", "utility: {timeout: 0s}", "utility: {api_key: secret}"} {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Load(file, true); err == nil {
			t.Fatalf("invalid settings accepted: %s", bad)
		}
	}
}

func TestUtilityProvidersNeedTheirFieldsAndAThreshold(t *testing.T) {
	for _, key := range []string{"TACIT_UTILITY_PROVIDER", "TACIT_UTILITY_MODEL", "TACIT_UTILITY_ENDPOINT", "TACIT_UTILITY_REGION", "TACIT_UTILITY_THRESHOLD"} {
		clear(t, key)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	good := "utility:\n  provider: bedrock\n  model: us.amazon.nova-micro-v1:0\n  region: us-west-2\n  threshold: 0.95\n"
	if err := os.WriteFile(file, []byte(good), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_UTILITY_PROVIDER") != "bedrock" || os.Getenv("TACIT_UTILITY_MODEL") != "us.amazon.nova-micro-v1:0" || os.Getenv("TACIT_UTILITY_REGION") != "us-west-2" {
		t.Fatal("bedrock settings not exported")
	}
	for _, bad := range []string{
		"utility: {provider: bedrock, threshold: 0.9}",                                  // no model
		"utility: {provider: bedrock, model: m}",                                        // no threshold
		"utility: {provider: systemone, threshold: 0.8}",                                // no endpoint
		"utility: {provider: openai, endpoint: 'https://x.example/v1', threshold: 0.8}", // no model
		"utility: {provider: jev, model: other}",                                        // jev is pinned
		"utility: {provider: jev, endpoint: 'https://x.example'}",                       // jev is pinned
	} {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Load(file, true); err == nil {
			t.Fatalf("invalid settings accepted: %s", bad)
		}
	}
}
