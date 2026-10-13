// Package settings loads optional client configuration before command dispatch.
package settings

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Models struct {
	Curate   string `yaml:"curate"`
	Judge    string `yaml:"judge"`
	Dream    string `yaml:"dream"`
	Verify   string `yaml:"verify"`
	Fallback string `yaml:"verify_fallback"`
}

type Config struct {
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
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && !required {
		return nil
	}
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
	base, err := filepath.Abs(filepath.Dir(path))
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
	values := map[string]string{"TACIT_STORE": resolve(c.Store), "TACIT_PROJECTS": resolve(c.Projects), "TACIT_INDEX": resolve(c.Index), "TACIT_CONFIG": resolve(c.Keys), "TACIT_LOCAL": resolve(c.Local), "TACIT_OWNER_NAMES": strings.Join(c.OwnerNames, ","), "TACIT_CURATE_MODEL": c.Models.Curate, "TACIT_JUDGE_MODEL": c.Models.Judge, "TACIT_DREAM_MODEL": c.Models.Dream, "TACIT_VERIFY_MODEL": c.Models.Verify, "TACIT_VERIFY_FALLBACK_MODEL": c.Models.Fallback}
	var roots []string
	for _, p := range c.VerifyRoots {
		if p != "" {
			roots = append(roots, resolve(p))
		}
	}
	values["TACIT_VERIFY_ROOTS"] = strings.Join(roots, string(filepath.ListSeparator))
	for key, value := range values {
		if _, set := os.LookupEnv(key); !set && value != "" {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}
