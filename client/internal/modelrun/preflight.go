package modelrun

import (
	"os"
	"path/filepath"
)

// Preflight checks the effective arguments and selected Codex configuration.
// It never starts a CLI, reads an auth file or sends a model request.
func Preflight(route Route) error {
	if err := Validate(route); err != nil {
		return err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	base := filepath.Join(cache, "tacit", "jobs")
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, "doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	args, _, err := invocation(route.Runner, route.Model, "offline preflight", filepath.Join(dir, "prompt.txt"), "offline", dir)
	if err != nil {
		return err
	}
	args, err = appendRouteArgs(args, route)
	if err != nil {
		return err
	}
	if route.Runner == "grok" {
		_, err := grokHome(dir)
		return err
	}
	if route.Runner == "codex" {
		_, cleanup, err := codexProfile(args)
		defer cleanup()
		return err
	}
	return nil
}
