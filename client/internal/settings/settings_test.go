package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func clear(t *testing.T, key string) {
	t.Helper()
	old, set := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if set {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

func TestConfigurationResolvesPathsAndHonorsEnvironment(t *testing.T) {
	for _, k := range []string{"TACIT_STORE", "TACIT_PROJECTS", "TACIT_OWNER_NAMES", "TACIT_VERIFY_ROOTS", "TACIT_DREAM_MODEL"} {
		clear(t, k)
	}
	root := t.TempDir()
	file := filepath.Join(root, "config.yaml")
	os.WriteFile(file, []byte("store: ./facts\nprojects: ./checkouts\nowner_names: [dana]\nverify_roots: [./rules]\nmodels:\n  dream: test-model\n"), 0o600)
	t.Setenv("TACIT_PROJECTS", "environment-wins")
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_STORE") != filepath.Join(root, "facts") || os.Getenv("TACIT_PROJECTS") != "environment-wins" {
		t.Fatal("path resolution or precedence failed")
	}
	if os.Getenv("TACIT_DREAM_MODEL") != "test-model" || os.Getenv("TACIT_OWNER_NAMES") != "dana" {
		t.Fatal("model or owner configuration missing")
	}
	if os.Getenv("TACIT_VERIFY_ROOTS") != filepath.Join(root, "rules") {
		t.Fatal("source root did not resolve")
	}
}

func TestInvalidConfigurationDoesNotApplyValues(t *testing.T) {
	clear(t, "TACIT_STORE")
	p := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(p, []byte("store: ./facts\nunknown_option: true\n"), 0o600)
	if err := Load(p, true); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, set := os.LookupEnv("TACIT_STORE"); set {
		t.Fatal("invalid config applied values")
	}
	if err := Load(p+".missing", true); err == nil {
		t.Fatal("explicit missing config accepted")
	}
	if err := Load(p+".missing", false); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	keys := []string{"TACIT_RUNNER", "TACIT_DREAM_RUNNER", "TACIT_CURATE_RUNNER", "TACIT_JUDGE_RUNNER", "TACIT_VERIFY_RUNNER", "TACIT_MUSE_COMMAND", "TACIT_CODEX_ARGS"}
	for _, key := range keys {
		old, ok := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if ok {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	text := "runner: codex\nrunners:\n  dream: muse\n  judge: agy\ncommands:\n  muse: ./bin/muse\nrunner_args:\n  codex: [--profile, custom]\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_RUNNER") != "codex" || os.Getenv("TACIT_DREAM_RUNNER") != "muse" || os.Getenv("TACIT_JUDGE_RUNNER") != "agy" {
		t.Fatal("runner selection not applied")
	}
	if os.Getenv("TACIT_MUSE_COMMAND") != filepath.Join(dir, "bin", "muse") {
		t.Fatal("relative command not resolved")
	}
	if os.Getenv("TACIT_CODEX_ARGS") != `["--profile","custom"]` {
		t.Fatal("arguments not preserved")
	}
	t.Setenv("TACIT_DREAM_RUNNER", "pi")
	if err := Load(path, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_DREAM_RUNNER") != "pi" {
		t.Fatal("environment did not win")
	}
	if err := os.WriteFile(path, []byte("runner: unknown\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path, true); err == nil {
		t.Fatal("unknown runner accepted")
	}
}
