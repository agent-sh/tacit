// Package settings loads optional client configuration before command dispatch.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/modelrun"

	"go.yaml.in/yaml/v3"
)

type Models struct {
	Curate   string `yaml:"curate"`
	Judge    string `yaml:"judge"`
	Dream    string `yaml:"dream"`
	Verify   string `yaml:"verify"`
	Fallback string `yaml:"verify_fallback"`
}

type Runners struct {
	Dream  string `yaml:"dream"`
	Curate string `yaml:"curate"`
	Judge  string `yaml:"judge"`
	Verify string `yaml:"verify"`
}

type Utility struct {
	Provider  string   `yaml:"provider"`
	Threshold *float64 `yaml:"threshold"`
	Timeout   string   `yaml:"timeout"`
	KeyFile   string   `yaml:"key_file"`
	AuditDir  string   `yaml:"audit_dir"`
}

type Config struct {
	Utility         Utility                    `yaml:"utility"`
	DreamMaxRuntime string                     `yaml:"dream_max_runtime"`
	Efforts         map[string]string          `yaml:"efforts"`
	Profiles        map[string]string          `yaml:"profiles"`
	Timeouts        map[string]string          `yaml:"timeouts"`
	Fallbacks       map[string]*modelrun.Route `yaml:"fallbacks"`
	RunnerArgs      map[string][]string        `yaml:"runner_args"`
	Runner          string                     `yaml:"runner"`
	Runners         Runners                    `yaml:"runners"`
	Commands        map[string]string          `yaml:"commands"`

	Store       string   `yaml:"store"`
	Projects    string   `yaml:"projects"`
	Index       string   `yaml:"index"`
	Keys        string   `yaml:"keys"`
	Local       string   `yaml:"local_overlay"`
	OwnerNames  []string `yaml:"owner_names"`
	VerifyRoots []string `yaml:"verify_roots"`
	Models      Models   `yaml:"models"`
}

func Path() string {
	if p := os.Getenv("TACIT_SETTINGS"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "tacit", "config.yaml")
		}
	}
	return filepath.Join(home, ".config", "tacit", "config.yaml")
}

// Load applies file values only where the environment has no override. Relative
// paths are resolved against the configuration file, not a hook's working dir.
func Load(path string, required bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	source := path
	if info.Mode()&os.ModeSymlink != 0 {
		source, err = filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("read config %s: %w", path, err)
		}
	}
	b, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid config %s: %w", path, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("config must contain one YAML document")
	}
	base, err := filepath.Abs(filepath.Dir(source))
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	resolve := func(p string) string {
		if p == "" {
			return ""
		}
		if p == "~" {
			return home
		}
		if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
			return filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			return filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	explicitEmpty := map[string]bool{}
	if c.DreamMaxRuntime != "" {
		if limit, err := time.ParseDuration(c.DreamMaxRuntime); err != nil || limit < 0 {
			return fmt.Errorf("dream_max_runtime must be a nonnegative duration")
		}
	}
	values := map[string]string{"TACIT_STORE": resolve(c.Store), "TACIT_PROJECTS": resolve(c.Projects), "TACIT_INDEX": resolve(c.Index), "TACIT_CONFIG": resolve(c.Keys), "TACIT_LOCAL": resolve(c.Local), "TACIT_OWNER_NAMES": strings.Join(c.OwnerNames, ","), "TACIT_CURATE_MODEL": c.Models.Curate, "TACIT_JUDGE_MODEL": c.Models.Judge, "TACIT_DREAM_MODEL": c.Models.Dream, "TACIT_VERIFY_MODEL": c.Models.Verify, "TACIT_VERIFY_FALLBACK_MODEL": c.Models.Fallback}
	if c.Utility.Provider != "" && c.Utility.Provider != "jev" {
		return fmt.Errorf("utility provider must be jev or empty")
	}
	if c.Utility.Threshold != nil {
		v := *c.Utility.Threshold
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("utility threshold must be in [0,1]")
		}
		values["TACIT_UTILITY_THRESHOLD"] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	if c.Utility.Timeout != "" {
		v, err := time.ParseDuration(c.Utility.Timeout)
		if err != nil || v <= 0 || v > 2*time.Second {
			return fmt.Errorf("utility timeout must be positive and no more than 2s")
		}
	}
	values["TACIT_UTILITY_PROVIDER"] = c.Utility.Provider
	values["TACIT_UTILITY_TIMEOUT"] = c.Utility.Timeout
	values["TACIT_UTILITY_KEY_FILE"] = resolve(c.Utility.KeyFile)
	values["TACIT_UTILITY_AUDIT_DIR"] = resolve(c.Utility.AuditDir)
	values["TACIT_RUNNER"] = c.Runner
	values["TACIT_DREAM_MAX_RUNTIME"] = c.DreamMaxRuntime
	for role, name := range map[string]string{"DREAM": c.Runners.Dream, "CURATE": c.Runners.Curate, "JUDGE": c.Runners.Judge, "VERIFY": c.Runners.Verify} {
		values["TACIT_"+role+"_RUNNER"] = name
	}

	validRole := func(role string) bool {
		return role == "dream" || role == "curate" || role == "judge" || role == "verify"
	}
	for suffix, entries := range map[string]map[string]string{"EFFORT": c.Efforts, "PROFILE": c.Profiles, "TIMEOUT": c.Timeouts} {
		for role, value := range entries {
			if !validRole(role) {
				return fmt.Errorf("unknown job role %q", role)
			}
			if suffix == "TIMEOUT" {
				if d, err := time.ParseDuration(value); err != nil || d <= 0 {
					return fmt.Errorf("%s timeout must be a positive duration", role)
				}
			}
			key := "TACIT_" + strings.ToUpper(role) + "_" + suffix
			values[key] = value
			explicitEmpty[key] = suffix != "TIMEOUT"
		}
	}
	for role, route := range c.Fallbacks {
		if _, set := os.LookupEnv("TACIT_" + strings.ToUpper(role) + "_FALLBACK_MODEL"); set {
			continue
		}
		if !validRole(role) {
			return fmt.Errorf("unknown fallback role %q", role)
		}
		if route != nil {
			if err := modelrun.Validate(*route); err != nil {
				return fmt.Errorf("%s fallback: %w", role, err)
			}
		}
		b, _ := json.Marshal(route)
		values["TACIT_"+strings.ToUpper(role)+"_FALLBACK"] = string(b)
	}
	validRunner := func(name string) bool {
		switch name {
		case "", "claude", "codex", "hermes", "grok", "pi", "muse", "agy":
			return true
		}
		return false
	}
	for key, value := range values {
		if strings.HasSuffix(key, "_RUNNER") && !validRunner(value) {
			return fmt.Errorf("unknown runner %q in %s", value, key)
		}
	}
	for name, command := range c.Commands {
		if name == "" || !validRunner(name) {
			return fmt.Errorf("unknown runner command %q", name)
		}
		if strings.ContainsAny(command, "/\\") {
			command = resolve(command)
		}
		values["TACIT_"+strings.ToUpper(name)+"_COMMAND"] = command
	}
	for name, args := range c.RunnerArgs {
		if name == "" || !validRunner(name) {
			return fmt.Errorf("unknown runner arguments %q", name)
		}
		b, _ := json.Marshal(args)
		values["TACIT_"+strings.ToUpper(name)+"_ARGS"] = string(b)
	}
	var roots []string
	for _, p := range c.VerifyRoots {
		if p != "" {
			roots = append(roots, resolve(p))
		}
	}
	values["TACIT_VERIFY_ROOTS"] = strings.Join(roots, string(filepath.ListSeparator))
	for key, value := range values {
		if _, set := os.LookupEnv(key); !set && (value != "" || explicitEmpty[key]) {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}
