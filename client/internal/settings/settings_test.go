package settings

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
  verify: work
timeouts:
  dream: 10m
fallbacks:
  verify:
    runner: claude
    model: fallback-model
    effort: medium
  dream: null
`
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"TACIT_CURATE_EFFORT": "medium", "TACIT_VERIFY_PROFILE": "work", "TACIT_DREAM_TIMEOUT": "10m", "TACIT_DREAM_FALLBACK": "null"} {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s=%q", key, got)
		}
	}
	if got := os.Getenv("TACIT_VERIFY_FALLBACK"); got != `{"runner":"claude","model":"fallback-model","effort":"medium"}` {
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

func TestPublicExampleMatchesGenericDefaults(t *testing.T) {
	roles := []string{"curate", "verify", "dream", "judge"}
	clear(t, "TACIT_RUNNER")
	for _, role := range roles {
		for _, suffix := range []string{"RUNNER", "MODEL", "EFFORT", "PROFILE", "FALLBACK", "FALLBACK_MODEL"} {
			clear(t, "TACIT_"+strings.ToUpper(role)+"_"+suffix)
		}
	}
	if err := Load("../../../config/example.yaml", true); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		got, err := modelrun.Resolve(role, os.Getenv("TACIT_"+strings.ToUpper(role)+"_MODEL"))
		if err != nil || got != modelrun.Default(role) {
			t.Fatalf("%s: %+v %v", role, got, err)
		}
	}
}

func TestUserConfigLinkLoadsPrivateSettingsFromAnyDirectory(t *testing.T) {
	for _, key := range []string{"TACIT_SETTINGS", "TACIT_STORE", "TACIT_RUNNER", "TACIT_JUDGE_RUNNER", "TACIT_JUDGE_MODEL", "TACIT_JUDGE_EFFORT"} {
		clear(t, key)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	local := filepath.Join(t.TempDir(), "config", "local.yaml")
	if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("store: ./facts\nrunners: {judge: muse}\nmodels: {judge: chosen-model}\nefforts: {judge: high}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	user := Path()
	if err := os.MkdirAll(filepath.Dir(user), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(local, user); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(t.TempDir())
	if err := Load(Path(), false); err != nil {
		t.Fatal(err)
	}
	route, err := modelrun.Resolve("judge", modelrun.Model("judge", "opus"))
	if err != nil || route.Runner != "muse" || route.Model != "chosen-model" || route.Effort != "high" {
		t.Fatal("private route not loaded", err)
	}
	target, err := filepath.EvalSymlinks(local)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TACIT_STORE"); got != filepath.Join(filepath.Dir(target), "facts") {
		t.Fatal("relative path did not resolve against link target")
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if err := Load(Path(), false); err == nil {
		t.Fatal("dangling configured link silently selected defaults")
	}
}

func TestPrivateConfigIsIgnoredByRepositoryRule(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "--no-index", "-v", "config/local.yaml")
	cmd.Dir = "../../.."
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), ".gitignore:") {
		t.Fatalf("private config lacks repository ignore rule: %v", err)
	}
}

func TestRegularConfigPreservesSymlinkedParentPath(t *testing.T) {
	clear(t, "TACIT_STORE")
	root := t.TempDir()
	real := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "config.yaml"), []byte("store: ./facts\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(filepath.Join(alias, "config.yaml"), true); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TACIT_STORE"); got != filepath.Join(alias, "facts") {
		t.Fatal("regular config path spelling changed")
	}
}

func TestDreamRuntimeSetting(t *testing.T) {
	clear(t, "TACIT_DREAM_MAX_RUNTIME")
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("dream_max_runtime: 2h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_DREAM_MAX_RUNTIME") != "2h" {
		t.Fatal("runtime setting not applied")
	}
	for _, value := range []string{"-1m", "tomorrow"} {
		os.WriteFile(file, []byte("dream_max_runtime: "+value+"\n"), 0600)
		if err := Load(file, true); err == nil {
			t.Fatal("invalid runtime accepted", value)
		}
	}
	os.WriteFile(file, []byte("dream_max_runtime: 0\n"), 0600)
	t.Setenv("TACIT_DREAM_MAX_RUNTIME", "3h")
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_DREAM_MAX_RUNTIME") != "3h" {
		t.Fatal("config replaced environment override")
	}
}

func TestStaleWorkersSetting(t *testing.T) {
	clear(t, "TACIT_STALE_WORKERS")
	file := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(file, []byte("stale_workers: 1\n"), 0600)
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_STALE_WORKERS") != "1" {
		t.Fatal("stale_workers not applied")
	}
	os.WriteFile(file, []byte("stale_workers: -1\n"), 0600)
	if err := Load(file, true); err == nil {
		t.Fatal("negative stale_workers accepted")
	}
}

func TestHermesStoreSetting(t *testing.T) {
	clear(t, "TACIT_HERMES_DB")
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	os.WriteFile(file, []byte("hermes_db: off\n"), 0o600)
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_HERMES_DB") != "off" {
		t.Fatalf("hermes_db off: %q", os.Getenv("TACIT_HERMES_DB"))
	}
	os.Unsetenv("TACIT_HERMES_DB")
	os.WriteFile(file, []byte("hermes_db: ./h/state.db\n"), 0o600)
	if err := Load(file, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_HERMES_DB") != filepath.Join(dir, "h", "state.db") {
		t.Fatalf("hermes_db path: %q", os.Getenv("TACIT_HERMES_DB"))
	}
}

// local_layer sets TACIT_LOCAL_LAYER, the environment wins, and a value
// that is neither on nor off fails before any command work.
func TestLocalLayerSetting(t *testing.T) {
	clear(t, "TACIT_LOCAL_LAYER")
	root := t.TempDir()
	file := filepath.Join(root, "config.yaml")
	os.WriteFile(file, []byte("local_layer: on\n"), 0o600)
	if err := Load(file, true); err != nil || os.Getenv("TACIT_LOCAL_LAYER") != "on" {
		t.Fatalf("local_layer: on gives %q, %v", os.Getenv("TACIT_LOCAL_LAYER"), err)
	}
	t.Setenv("TACIT_LOCAL_LAYER", "off")
	if err := Load(file, true); err != nil || os.Getenv("TACIT_LOCAL_LAYER") != "off" {
		t.Fatalf("the environment did not win: %q, %v", os.Getenv("TACIT_LOCAL_LAYER"), err)
	}
	clear(t, "TACIT_LOCAL_LAYER")
	os.WriteFile(file, []byte("local_layer: sometimes\n"), 0o600)
	if err := Load(file, true); err == nil || os.Getenv("TACIT_LOCAL_LAYER") != "" {
		t.Fatalf("an unknown local_layer value loaded: %v", err)
	}
}

// memory_md sets TACIT_MEMORY_MD (on by default, unset), and a value that
// is neither on nor off fails.
func TestMemoryMDSetting(t *testing.T) {
	clear(t, "TACIT_MEMORY_MD")
	file := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(file, []byte("memory_md: off\n"), 0o600)
	if err := Load(file, true); err != nil || os.Getenv("TACIT_MEMORY_MD") != "off" {
		t.Fatalf("memory_md: off gives %q, %v", os.Getenv("TACIT_MEMORY_MD"), err)
	}
	clear(t, "TACIT_MEMORY_MD")
	os.WriteFile(file, []byte("memory_md: later\n"), 0o600)
	if err := Load(file, true); err == nil || os.Getenv("TACIT_MEMORY_MD") != "" {
		t.Fatalf("an unknown memory_md value loaded: %v", err)
	}
}

func TestDeliverySettingsBecomeEnvironment(t *testing.T) {
	for _, k := range []string{"TACIT_DELIVERY_TOOL_PUSH", "TACIT_DELIVERY_START_CEILING", "TACIT_DELIVERY_PROMPT_RELATIVE", "TACIT_DELIVERY_REPO_START"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("delivery:\n  tool_push: off\n  start_ceiling: 4\n  prompt_relative: 0.5\n"), 0o644)
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TACIT_DELIVERY_TOOL_PUSH") != "off" || os.Getenv("TACIT_DELIVERY_START_CEILING") != "4" || os.Getenv("TACIT_DELIVERY_PROMPT_RELATIVE") != "0.5" {
		t.Fatalf("env: %q %q %q", os.Getenv("TACIT_DELIVERY_TOOL_PUSH"), os.Getenv("TACIT_DELIVERY_START_CEILING"), os.Getenv("TACIT_DELIVERY_PROMPT_RELATIVE"))
	}
	if _, set := os.LookupEnv("TACIT_DELIVERY_REPO_START"); set {
		t.Fatal("an unset switch was set")
	}
	for _, bad := range []string{"delivery:\n  tool_push: maybe\n", "delivery:\n  start_ceiling: 0\n", "delivery:\n  prompt_relative: 2\n"} {
		os.WriteFile(p, []byte(bad), 0o644)
		if err := Load(p, true); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
