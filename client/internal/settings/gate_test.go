package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// The gate section becomes TACIT_GATE_*; a provider needs its threshold, a
// timeout stays within 2s, and the extra request fields travel as JSON.
func TestGateSettingsBecomeEnvironment(t *testing.T) {
	keys := []string{"PROVIDER", "MODEL", "ENDPOINT", "REGION", "THRESHOLD", "TIMEOUT", "KEY_FILE", "FIELDS"}
	unset := func() {
		for _, k := range keys {
			clear(t, "TACIT_GATE_"+k)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	unset()
	os.WriteFile(path, []byte("gate:\n  provider: bedrock\n  model: demo-model\n  threshold: 0.3\n  timeout: 1500ms\n  key_file: route.env\n  fields:\n    thinking:\n      type: off\n"), 0o600)
	if err := Load(path, true); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PROVIDER": "bedrock", "MODEL": "demo-model", "THRESHOLD": "0.3", "TIMEOUT": "1500ms",
		"KEY_FILE": filepath.Join(dir, "route.env"), "FIELDS": `{"thinking":{"type":"off"}}`}
	for k, v := range want {
		if got := os.Getenv("TACIT_GATE_" + k); got != v {
			t.Errorf("TACIT_GATE_%s = %q, want %q", k, got, v)
		}
	}
	for _, bad := range []string{
		"gate:\n  provider: bedrock\n  model: demo-model\n",                         // no threshold
		"gate:\n  provider: bedrock\n  threshold: 0.3\n",                            // no model
		"gate:\n  provider: openai\n  model: m\n  threshold: 0.3\n",                 // no endpoint
		"gate:\n  provider: jev\n  threshold: 0.3\n",                                // not a gate provider
		"gate:\n  provider: bedrock\n  model: m\n  threshold: 1.5\n",                // threshold out of range
		"gate:\n  provider: bedrock\n  model: m\n  threshold: 0.3\n  timeout: 3s\n", // past 2s
	} {
		unset()
		os.WriteFile(path, []byte(bad), 0o600)
		if err := Load(path, true); err == nil {
			t.Errorf("accepted %q", bad)
		}
		if os.Getenv("TACIT_GATE_PROVIDER") != "" {
			t.Errorf("an invalid file set the gate: %q", bad)
		}
	}
}
