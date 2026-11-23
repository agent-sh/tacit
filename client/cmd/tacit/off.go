package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The off switch (SPEC.md, "Off switch"): while a file offPaths names
// exists, tacit does nothing on its own. Every hook exits 0 with no output,
// the model jobs (dream, curate, maintain, verify, rotation, the delivery
// judge) exit 0 with one line, and every tool of the MCP server answers that
// tacit is off. The file's content is the reason. Removing the file turns
// tacit on again; nothing else changes while it is off.

// offPaths are $XDG_CONFIG_HOME/tacit/off and ~/.config/tacit/off: both,
// since a job a timer or launch agent starts may not carry the variable a
// shell sets, and every process must see the same switch.
func offPaths() []string {
	return offPathsFrom()
}

// offHome is the home the second path is under; the tests point it away
// from the machine's own, so a real off file does not stop the suite.
var offHome = os.UserHomeDir

func offPathsFrom() []string {
	var out []string
	if cfg := os.Getenv("XDG_CONFIG_HOME"); cfg != "" {
		out = append(out, filepath.Join(cfg, "tacit", "off"))
	}
	if home, err := offHome(); err == nil {
		if p := filepath.Join(home, ".config", "tacit", "off"); len(out) == 0 || out[0] != p {
			out = append(out, p)
		}
	}
	return out
}

// tacitOff reports whether an off file exists, its path, and its trimmed
// content as the reason ("no reason given" for an empty or unreadable
// file). One or two stats when tacit is on.
func tacitOff() (reason string, off bool) {
	reason, _, off = offState()
	return reason, off
}

func offState() (reason, path string, off bool) {
	for _, p := range offPaths() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		b, _ := os.ReadFile(p)
		reason = strings.Join(strings.Fields(string(b)), " ")
		if r := []rune(reason); len(r) > 300 {
			reason = string(r[:300])
		}
		if reason == "" {
			reason = "no reason given"
		}
		return reason, p, true
	}
	return "", "", false
}

// offJobs are the commands the off switch stops: the model jobs, the
// timer jobs that run them, and `tacit add`, which the Hermes provider runs
// on each memory write.
var offJobs = map[string]bool{"dream": true, "curate": true, "maintain": true, "verify": true, "rotation": true, "delivery-judge": true, "add": true}

// offLine is the one line a stopped job prints.
func offLine(reason, path string) string {
	return fmt.Sprintf("tacit is off: %s (%s)", reason, path)
}

// jobOff prints the line and reports true when cmd is a job the off switch
// stops and tacit is off.
func jobOff(cmd string) bool {
	if !offJobs[cmd] {
		return false
	}
	reason, path, off := offState()
	if off {
		fmt.Println(cmd + ": " + offLine(reason, path))
	}
	return off
}

// offMiddleware answers every tools/call with "tacit is off: <reason>"
// while the off file exists, checked at each call, so a file made while the
// server runs takes effect at the next call.
func offMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if reason, off := tacitOff(); off {
				return text("tacit is off: " + reason), nil
			}
		}
		return next(ctx, method, req)
	}
}
