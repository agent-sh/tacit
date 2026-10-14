// Package modelrun adapts installed agent CLIs to Tacit's model jobs.
package modelrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var Names = []string{"claude", "codex", "hermes", "grok", "pi", "muse", "agy"}

type Route struct {
	Runner  string `json:"runner" yaml:"runner"`
	Model   string `json:"model,omitempty" yaml:"model"`
	Effort  string `json:"effort,omitempty" yaml:"effort"`
	Profile string `json:"profile,omitempty" yaml:"profile"`
}

func Default(role string) Route {
	switch role {
	case "curate":
		return Route{Runner: "claude", Model: "claude-opus-5-5", Effort: "medium"}
	case "verify":
		return Route{Runner: "codex", Model: "openai.gpt-6-astra", Effort: "high", Profile: "astra"}
	case "dream":
		return Route{Runner: "grok", Model: "grok-4.7", Effort: "xhigh"}
	case "judge":
		return Route{Runner: "grok", Model: "grok-4.7", Effort: "xhigh"}
	}
	return Route{Runner: "claude"}
}

func known(name string) bool {
	for _, n := range Names {
		if name == n {
			return true
		}
	}
	return false
}
func roleEnv(role, suffix string) string { return "TACIT_" + strings.ToUpper(role) + "_" + suffix }
func runnerOverridden(role string) bool {
	return strings.TrimSpace(os.Getenv(roleEnv(role, "RUNNER"))) != "" || strings.TrimSpace(os.Getenv("TACIT_RUNNER")) != ""
}

func Name(role string) (string, error) {
	name := strings.TrimSpace(os.Getenv(roleEnv(role, "RUNNER")))
	if name == "" {
		name = strings.TrimSpace(os.Getenv("TACIT_RUNNER"))
	}
	if name == "" {
		name = Default(role).Runner
	}
	if !known(name) {
		return "", fmt.Errorf("unknown %s runner %q; choose %s", role, name, strings.Join(Names, ", "))
	}
	return name, nil
}

func Command(name string) string {
	if s := strings.TrimSpace(os.Getenv("TACIT_" + strings.ToUpper(name) + "_COMMAND")); s != "" {
		return s
	}
	return name
}

// Explicit runner selection keeps the CLI's own default model, except for the
// legacy Claude aliases. The shipped mixed-runner plan applies when omitted.
func Model(role, claudeDefault string) string {
	if v := strings.TrimSpace(os.Getenv(roleEnv(role, "MODEL"))); v != "" {
		return v
	}
	if strings.HasSuffix(role, "_fallback") {
		primary := strings.TrimSuffix(role, "_fallback")
		if !runnerOverridden(primary) && primary == "verify" {
			return Default("curate").Model
		}
		if name, _ := Name(primary); name == "claude" {
			return claudeDefault
		}
		return ""
	}
	if !runnerOverridden(role) {
		return Default(role).Model
	}
	if name, _ := Name(role); name == "claude" {
		return claudeDefault
	}
	return ""
}

func Resolve(role, model string) (Route, error) {
	name, err := Name(role)
	if err != nil {
		return Route{}, err
	}
	r := Route{Runner: name, Model: model}
	d := Default(role)
	if model == "" && !runnerOverridden(role) {
		r.Model = d.Model
	}
	if !runnerOverridden(role) && r.Model == d.Model {
		r.Effort = d.Effort
		r.Profile = d.Profile
	}
	if v, set := os.LookupEnv(roleEnv(role, "EFFORT")); set {
		r.Effort = strings.TrimSpace(v)
	}
	if v, set := os.LookupEnv(roleEnv(role, "PROFILE")); set {
		r.Profile = strings.TrimSpace(v)
	}
	return r, Validate(r)
}

func Validate(r Route) error {
	if !known(r.Runner) {
		return fmt.Errorf("unknown runner %q", r.Runner)
	}
	if r.Profile != "" && r.Runner != "codex" {
		return fmt.Errorf("profile selection is supported by codex; use runner_args for %s", r.Runner)
	}
	if r.Effort != "" {
		switch r.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		default:
			return fmt.Errorf("unknown effort %q", r.Effort)
		}
	}
	return nil
}

// A JSON null disables fallback. Legacy verifier model overrides still select
// another model in the primary CLI. Default cross-runner fallbacks are one-way
// attempts; a fallback never starts its own fallback.
func Fallback(role string, primary Route) (*Route, error) {
	if raw, set := os.LookupEnv(roleEnv(role, "FALLBACK")); set {
		var r *Route
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("invalid %s fallback JSON: %w", role, err)
		}
		if r != nil {
			if err := Validate(*r); err != nil {
				return nil, err
			}
		}
		return r, nil
	}
	if model, set := os.LookupEnv(roleEnv(role, "FALLBACK_MODEL")); set {
		if strings.TrimSpace(model) == "" {
			return nil, nil
		}
		r := primary
		r.Model = strings.TrimSpace(model)
		return &r, nil
	}
	if runnerOverridden(role) || primary.Model != Default(role).Model {
		return nil, nil
	}
	var r Route
	switch role {
	case "curate":
		r = Default("verify")
	case "verify":
		r = Default("curate")
	case "dream":
		r = Route{Runner: "claude", Model: "haiku"}
	default:
		return nil, nil
	}
	return &r, nil
}

// Reuse the verifier's existing per-attempt bound for all model jobs. A separate
// attempt deadline leaves the caller's overall deadline available for fallback.
func Timeout(role string) (time.Duration, error) {
	raw := os.Getenv(roleEnv(role, "TIMEOUT"))
	if raw == "" {
		return 20 * time.Minute, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s timeout must be a positive duration", role)
	}
	return d, nil
}

func (r Route) Label() string {
	m := r.Model
	if m == "" {
		m = "default"
	}
	s := r.Runner + "/" + m
	if r.Effort != "" {
		s += " (" + r.Effort + ")"
	}
	return s
}

type routeKey struct{}

func WithRoute(ctx context.Context, r Route) context.Context {
	return context.WithValue(ctx, routeKey{}, r)
}
func FromContext(ctx context.Context, role, model string) (Route, error) {
	if r, ok := ctx.Value(routeKey{}).(Route); ok {
		return r, Validate(r)
	}
	return Resolve(role, model)
}
