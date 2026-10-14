package modelrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/platform"
)

type Result struct {
	Text         string
	CostUSD      float64
	CostReported bool
	InputTokens  int
	Route        Route
	SessionID    string
}

var costWarnings sync.Map

// Run starts a fresh CLI process in a private, disposable working directory.
// Prompts travel through stdin or a mode-0600 file, avoiding command-line
// length limits and keeping fact text out of process listings.
func Run(ctx context.Context, role, model, prompt, sessionID string) (Result, error) {
	route, err := FromContext(ctx, role, model)
	if err != nil {
		return Result{}, err
	}
	name := route.Runner
	executable, err := exec.LookPath(Command(name))
	if err != nil {
		return Result{}, fmt.Errorf("%s runner %s is unavailable: %w", role, name, err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return Result{}, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Result{}, err
	}
	base := filepath.Join(cache, "tacit", "jobs")
	if err = os.MkdirAll(base, 0700); err != nil {
		return Result{}, err
	}
	dir, err := os.MkdirTemp(base, role+"-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	promptFile := filepath.Join(dir, "prompt.txt")
	if err = os.WriteFile(promptFile, []byte(prompt), 0600); err != nil {
		return Result{}, err
	}
	args, input, err := invocation(name, route.Model, prompt, promptFile, sessionID, dir)
	if err != nil {
		return Result{}, err
	}
	args, err = appendRouteArgs(args, route)
	if err != nil {
		return Result{}, err
	}
	if name == "codex" {
		var cleanup func()
		args, cleanup, err = codexProfile(args)
		if err != nil {
			return Result{}, err
		}
		defer cleanup()
		args, err = appendRouteArgs(args, Route{Runner: name, Effort: route.Effort})
		if err != nil {
			return Result{}, err
		}
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TACIT_MODEL_JOB=1")
	if name == "grok" {
		// Keep inherited agent integrations out of a text-only model job.
		for _, source := range []string{"CLAUDE", "CURSOR", "CODEX"} {
			for _, surface := range []string{"HOOKS", "MCPS", "RULES", "SKILLS", "AGENTS", "SESSIONS"} {
				cmd.Env = append(cmd.Env, "GROK_"+source+"_"+surface+"_ENABLED=0")
			}
		}
		cmd.Env = append(cmd.Env, "GROK_MEMORY=0")
	}
	if name == "hermes" {
		cmd.Env = append(cmd.Env, "HERMES_SAFE_MODE=1", "HERMES_IGNORE_RULES=1")
	}
	cmd.Stdin = strings.NewReader(input)
	platform.Prepare(cmd)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	result, parseErr := parse(name, stdout.Bytes())
	result.Route = route
	if runErr != nil {
		detailText := stderr.String()
		if parseErr != nil {
			detailText += "\n" + parseErr.Error()
		}
		detail := strings.TrimSpace(digest.Redact(detailText))
		if len(detail) > 1200 {
			detail = detail[:1200]
		}
		if ctx.Err() != nil {
			runErr = ctx.Err()
		}
		return result, fmt.Errorf("%s runner: %w: %s", name, runErr, detail)
	}
	if parseErr != nil {
		return result, fmt.Errorf("%s runner output: %s", name, digest.Redact(parseErr.Error()))
	}
	if !result.CostReported {
		if _, seen := costWarnings.LoadOrStore(name, true); !seen {
			fmt.Fprintf(os.Stderr, "tacit: %s does not report cost; totals include reported costs only and cannot enforce a full dollar budget\n", name)
		}
	}
	return result, nil
}

func invocation(name, model, prompt, promptFile, sid, dir string) ([]string, string, error) {
	var args []string
	input := ""
	switch name {
	case "claude":
		args = []string{"-p", "--safe-mode", "--no-session-persistence", "--output-format", "json", "--tools", "", "--max-turns", "1", "--session-id", sid}
		input = prompt
	case "codex":
		args = []string{"--no-daemon", "exec", "--ignore-user-config", "--ignore-rules", "--disable", "shell_tool", "-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`, "-c", `approval_policy="never"`, "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "--json"}
		input = prompt
	case "grok":
		// Grok treats an empty allowlist as its default toolset, and rejects
		// an empty curated profile. Select one known tool, then remove it.
		profile := filepath.Join(dir, "tacit-runner.md")
		definition := "---\nname: tacit-runner\ndescription: Process the provided text.\ntoolConfig:\n  tools:\n    - id: GrokBuild:read_file\ninjectDefaultTools: false\ndiscoverSkills: false\ninheritSkills: false\nagentsMd: false\nmcpInheritance: none\nmcpServers: []\nhooks: {}\n---\nProcess only the provided prompt. Return the requested JSON.\n"
		if err := os.WriteFile(profile, []byte(definition), 0600); err != nil {
			return nil, "", err
		}
		args = []string{"--agent", profile, "--prompt-file", promptFile, "--output-format", "json", "--tools", "read_file", "--disallowed-tools", "read_file", "--deny", "Read", "--deny", "MCPTool", "--no-auto-update", "--no-subagents", "--disable-web-search", "--permission-mode", "dontAsk", "--max-turns", "1"}
	case "pi":
		args = []string{"--print", "--mode", "json", "--no-session", "--no-tools", "--no-mcp", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files"}
		input = prompt
	case "muse":
		args = []string{"exec", "--prompt-file", promptFile, "--json", "--no-session-log", "--disable-write", "--disable-shell", "--disable-web-tools", "--disable-reminders", "--no-foreign-personal-context", "--approval-mode", "never", "--approval-judge", "off", "-c", `run.subagent_delegation_mode="off"`, "-c", "run.toolset=[]"}
	case "agy":
		agentDir := filepath.Join(dir, ".agents", "agents")
		if err := os.MkdirAll(agentDir, 0700); err != nil {
			return nil, "", err
		}
		agent := "---\nname: tacit-runner\ndescription: Process the supplied text and return JSON.\nmainAgent: true\nsubagent: false\ntools: []\ncommandExecutionPolicy: off\nmcpServers: []\nskills: []\nplugins: []\n---\nProcess only the supplied text and return the requested JSON.\n"
		if err := os.WriteFile(filepath.Join(agentDir, "tacit-runner.md"), []byte(agent), 0600); err != nil {
			return nil, "", err
		}
		args = []string{"--agent", "tacit-runner", "--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands", "--print-timeout", "0s"}
		b, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
		input = string(b) + "\n"
	case "hermes":
		args = []string{"--ignore-rules", "chat", "--query-file", promptFile, "--oneshot", "--quiet", "--format", "stream-json", "--toolsets", "context_engine"}
	default:
		return nil, "", fmt.Errorf("unknown runner %q", name)
	}
	if raw := os.Getenv("TACIT_" + strings.ToUpper(name) + "_ARGS"); raw != "" {
		var extra []string
		if err := json.Unmarshal([]byte(raw), &extra); err != nil {
			return nil, "", fmt.Errorf("%s runner arguments must be a JSON array: %w", name, err)
		}
		args = append(args, extra...)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if name == "codex" {
		args = append(args, "-")
	}
	return args, input, nil
}
