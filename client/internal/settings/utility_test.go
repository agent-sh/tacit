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
