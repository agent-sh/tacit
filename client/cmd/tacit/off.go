package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The off switch (SPEC.md, "Off switch"): while the file offPath names
// exists, tacit does nothing on its own. Every hook exits 0 with no output,
// the model jobs (dream, curate, maintain, verify, rotation, the delivery
// judge) exit 0 with one line, and every tool of the MCP server answers that
// tacit is off. The file's content is the reason. Removing the file turns
// tacit on again; nothing else changes while it is off.

// offPath is $XDG_CONFIG_HOME/tacit/off, ~/.config/tacit/off without it.
func offPath() string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "tacit", "off")
}

// tacitOff reports whether the off file exists, and its trimmed content as
// the reason ("no reason given" for an empty or unreadable file). One stat
// when tacit is on.
func tacitOff() (reason string, off bool) {
	p := offPath()
	if p == "" {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	b, _ := os.ReadFile(p)
	reason = strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(reason); len(r) > 300 {
		reason = string(r[:300])
	}
	if reason == "" {
		reason = "no reason given"
	}
	return reason, true
}

// offJobs are the commands the off switch stops: the model jobs, the
// timer jobs that run them, and `tacit add`, which the Hermes provider runs
// on each memory write.
var offJobs = map[string]bool{"dream": true, "curate": true, "maintain": true, "verify": true, "rotation": true, "delivery-judge": true, "add": true}

// offLine is the one line a stopped job prints.
func offLine(reason string) string {
	return fmt.Sprintf("tacit is off: %s (%s)", reason, offPath())
}

// jobOff prints the line and reports true when cmd is a job the off switch
// stops and tacit is off.
func jobOff(cmd string) bool {
	if !offJobs[cmd] {
		return false
	}
	reason, off := tacitOff()
	if off {
		fmt.Println(cmd + ": " + offLine(reason))
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
