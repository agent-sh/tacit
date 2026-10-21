package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

var gitVersionPattern = regexp.MustCompile(`\bgit version (\d+)\.(\d+)`)

func gitSupported(version string) bool {
	m := gitVersionPattern.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 2 || major == 2 && minor >= 29
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	models := fs.Bool("models", false, "require every configured model runner, including fallbacks")
	role := fs.String("role", "", "require one role: dream, curate, judge or verify (and its fallback)")
	asJSON := fs.Bool("json", false, "print machine-readable checks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *role != "" && *role != "dream" && *role != "curate" && *role != "judge" && *role != "verify" {
		fmt.Fprintln(os.Stderr, "doctor: --role takes dream, curate, judge or verify; no positional arguments")
		return 2
	}
	checks, failed := doctorChecks(*models, *role)
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(checks)
	} else {
		for _, check := range checks {
			fmt.Printf("%s: %s: %s\n", check.Status, check.Name, check.Detail)
		}
	}
	if failed {
		return 1
	}
	return 0
}

func doctorChecks(models bool, role string) ([]doctorCheck, bool) {
	var checks []doctorCheck
	failed := false
	add := func(name, status, detail string) {
		checks = append(checks, doctorCheck{name, status, detail})
		failed = failed || status == "fail"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	output, err := exec.CommandContext(ctx, "git", "--version").Output()
	cancel()
	if err != nil || !gitSupported(string(output)) {
		add("git", "fail", "Git 2.29 or newer is required")
	} else {
		add("git", "ok", "version supports local stores")
	}
	if err := digest.RedactionReady(); err != nil {
		add("scrump", "fail", "text profile is unavailable or failed its masking canary")
	} else {
		add("scrump", "ok", "text profile masked its canary")
	}
	roles := []string{"dream", "curate", "judge", "verify"}
	if role != "" {
		roles = []string{role}
	}
	checked := map[string]string{}
	for _, job := range roles {
		primary, err := modelrun.Resolve(job, modelrun.Model(job, modelrun.Default(job).Model))
		if err != nil {
			add(job, "fail", "invalid runner configuration")
			continue
		}
		if _, err := modelrun.Timeout(job); err != nil {
			add(job, "fail", "invalid attempt timeout")
			continue
		}
		fallback, err := modelrun.Fallback(job, primary)
		if err != nil {
			add(job, "fail", "invalid fallback configuration")
			continue
		}
		routes := []modelrun.Route{primary}
		if fallback != nil {
			routes = append(routes, *fallback)
		}
		for i, route := range routes {
			name := job
			if i > 0 {
				name += " fallback"
			}
			if err := modelrun.Preflight(route); err != nil {
				add(name, "fail", "invalid arguments, effort or Codex profile: "+digest.Redact(err.Error()))
				continue
			}
			add(name+" config", "ok", "arguments and profile passed offline validation")
			command := modelrun.Command(route.Runner)
			status, seen := checked[command]
			if !seen {
				executable, err := exec.LookPath(command)
				status = "ok"
				if err != nil {
					status = "missing"
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					cmd := exec.CommandContext(ctx, executable, "--version")
					platform.Prepare(cmd)
					cmd.WaitDelay = 2 * time.Second
					cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
					if cmd.Run() != nil {
						status = "version failed"
					}
					cancel()
				}
				checked[command] = status
			}
			if status != "ok" {
				severity := "warn"
				if models || role != "" {
					severity = "fail"
				}
				add(name+" runner", severity, route.Runner+": "+status)
			} else {
				add(name+" runner", "ok", route.Runner+": executable responds to --version")
			}
		}
	}
	add("tacit", "info", fmt.Sprintf("binary %s, writes store format %d", version, ledger.FormatVersion))
	if s, err := store.Default(); err == nil && s.Exists() {
		switch f, ok, err := ledger.ReadFormat(s.Dir); {
		case err != nil:
			add("store format", "warn", "the marker "+ledger.FormatPath+" cannot be read; folder records are not written")
		case !ok:
			add("store format", "info", "no format marker: folder records are not written until the owner runs `tacit format --set` (once every machine runs a binary of format "+fmt.Sprint(ledger.FormatVersion)+")")
		case !f.Writes():
			add("store format", "warn", fmt.Sprintf("the store is format %d, newer than this binary: update tacit", f.Version))
		default:
			add("store format", "ok", fmt.Sprintf("format %d set %s", f.Version, f.At))
		}
	}
	add("provider", "info", "offline only; credentials, model availability and live completions were not checked")
	return checks, failed
}
