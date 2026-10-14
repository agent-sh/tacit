package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
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

func TestRoleOptionsAndCrossRunnerFallbackConfig(t *testing.T) {
	for _, key := range []string{"TACIT_CURATE_EFFORT", "TACIT_VERIFY_PROFILE", "TACIT_DREAM_TIMEOUT", "TACIT_VERIFY_FALLBACK", "TACIT_DREAM_FALLBACK"} {
		clear(t, key)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := `efforts:
  curate: medium
profiles:
  verify: astra
timeouts:
  dream: 10m
fallbacks:
  verify:
    runner: claude
    model: claude-opus-5-5
    effort: medium
  dream: null
`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"TACIT_CURATE_EFFORT": "medium", "TACIT_VERIFY_PROFILE": "astra", "TACIT_DREAM_TIMEOUT": "10m", "TACIT_DREAM_FALLBACK": "null"} {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s=%q", key, got)
		}
	}
	if got := os.Getenv("TACIT_VERIFY_FALLBACK"); got != `{"runner":"claude","model":"claude-opus-5-5","effort":"medium"}` {
		t.Fatal(got)
	}
	t.Setenv("TACIT_CURATE_EFFORT", "high")
	if err := Load(p, true); err != nil || os.Getenv("TACIT_CURATE_EFFORT") != "high" {
		t.Fatal(err)
	}
	for _, bad := range []string{"fallbacks: {dream: {runner: unknown}}", "timeouts: {dream: bad}", "efforts: {typo: high}"} {
		if err := os.WriteFile(p, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Load(p, true); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestRoleOptionsCanClearDefaults(t *testing.T) {
	for _, key := range []string{"TACIT_VERIFY_PROFILE", "TACIT_CURATE_EFFORT"} {
		clear(t, key)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("profiles: {verify: ''}\nefforts: {curate: ''}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TACIT_VERIFY_PROFILE", "TACIT_CURATE_EFFORT"} {
		if value, set := os.LookupEnv(key); !set || value != "" {
			t.Fatalf("%s not explicitly cleared", key)
		}
	}
}

func TestLegacyFallbackEnvOverridesFileRoute(t *testing.T) {
	clear(t, "TACIT_VERIFY_FALLBACK")
	t.Setenv("TACIT_VERIFY_FALLBACK_MODEL", "operator-model")
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("fallbacks: {verify: {runner: claude, model: file-model}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("TACIT_VERIFY_FALLBACK"); set {
		t.Fatal("file overrides legacy environment")
	}
}

func TestPublishedDefaultPlanMatchesBuiltInRoutes(t *testing.T) {
	roles := []string{"curate", "verify", "dream", "judge"}
	clear(t, "TACIT_RUNNER")
	for _, role := range roles {
		for _, suffix := range []string{"RUNNER", "MODEL", "EFFORT", "PROFILE", "FALLBACK", "FALLBACK_MODEL"} {
			clear(t, "TACIT_"+strings.ToUpper(role)+"_"+suffix)
		}
	}
	if err := Load("../../../config/default.yaml", true); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		got, err := modelrun.Resolve(role, os.Getenv("TACIT_"+strings.ToUpper(role)+"_MODEL"))
		if err != nil || got != modelrun.Default(role) {
			t.Fatalf("%s: %+v %v", role, got, err)
		}
	}
}
