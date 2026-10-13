package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the same stdio command emitted by the Muse and agy installers.
func memorySmoke(binary string, env []string, harness string, writes bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.Command(binary, "mcp", "--harness", harness)
	cmd.Env = env
	client := mcp.NewClient(&mcp.Implementation{Name: "tacit-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	call := func(name string, args map[string]any, want string) error {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err
		}
		var text strings.Builder
		for _, content := range res.Content {
			if c, ok := content.(*mcp.TextContent); ok {
				text.WriteString(c.Text)
			}
		}
		if res.IsError || !strings.Contains(text.String(), want) {
			return fmt.Errorf("%s %s: %s", harness, name, text.String())
		}
		return nil
	}
	if err := call("tacit_search", map[string]any{"query": "review public change merging", "repo": "demo"}, "review-first"); err != nil {
		return err
	}
	if err := call("tacit_fact", map[string]any{"name": "review-first"}, "Review every public change"); err != nil {
		return err
	}
	if writes {
		name := harness + "-memory-smoke"
		if err := call("tacit_add", map[string]any{"name": name, "description": "The demo uses the memory smoke fixture.", "type": "machine_fact", "scope": "projects/demo", "kind": "observed"}, "recorded"); err != nil {
			return err
		}
		if err := call("tacit_fact", map[string]any{"name": name}, "harness: "+harness); err != nil {
			return err
		}
	}
	fmt.Printf("PASS: %s memory MCP search, fact%s\n", harness, map[bool]string{true: ", add", false: ""}[writes])
	return nil
}
