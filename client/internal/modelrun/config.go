// Package modelrun adapts installed agent CLIs to Tacit's model jobs.
package modelrun

import (
	"fmt"
	"os"
	"strings"
)

var Names = []string{"claude", "codex", "hermes", "grok", "pi", "muse", "agy"}

func Name(role string) (string, error) {
	name := strings.TrimSpace(os.Getenv("TACIT_" + strings.ToUpper(role) + "_RUNNER"))
	if name == "" {
		name = strings.TrimSpace(os.Getenv("TACIT_RUNNER"))
	}
	if name == "" {
		name = "claude"
	}
	for _, known := range Names {
		if name == known {
			return name, nil
		}
	}
	return "", fmt.Errorf("unknown %s runner %q; choose %s", role, name, strings.Join(Names, ", "))
}

func Command(name string) string {
	if s := strings.TrimSpace(os.Getenv("TACIT_" + strings.ToUpper(name) + "_COMMAND")); s != "" {
		return s
	}
	return name
}

// Model keeps the existing Claude aliases. Other CLIs select their own
// default when the operator has not supplied a model for this role.
func Model(role, claudeDefault string) string {
	if v := strings.TrimSpace(os.Getenv("TACIT_" + strings.ToUpper(role) + "_MODEL")); v != "" {
		return v
	}
	name, _ := Name(strings.TrimSuffix(role, "_fallback"))
	if name == "claude" {
		return claudeDefault
	}
	return ""
}
