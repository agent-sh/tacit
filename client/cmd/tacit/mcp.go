package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runMCP serves the store over MCP on stdio: `tacit mcp --harness codex`.
// Tools: tacit_tree, tacit_search, tacit_fact, tacit_add. Every harness that
// speaks MCP gets the same verbs; Claude Code gets them too, next to its hooks.
//
// `tacit mcp --probe-only` serves one tool, tacit_probe, for the verifier:
// read-only commands with validated arguments and no shell in between, so
// a prompt-injected model has nothing to chain a second command onto.
func runMCP(args []string) int {
	harness := "codex"
	probeOnly := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--harness" && i+1 < len(args) {
			harness = args[i+1]
		}
		if args[i] == "--probe-only" {
			probeOnly = true
		}
	}
	if probeOnly {
		return runProbeMCP()
	}
	if !agentHarness(harness) {
		fmt.Fprintln(os.Stderr, "mcp: not a harness an agent may write as:", harness)
		return 2
	}
	s, err := store.Default()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 2
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tacit", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: treeInstruction})
	addTreeTool(server, s)

	type searchArgs struct {
		Query string `json:"query" jsonschema:"what you are about to do or need to know; facts are matched on it"`
		Repo  string `json:"repo,omitempty" jsonschema:"repo name to scope to; default: the repo of the working directory"`
		Limit int    `json:"limit,omitempty" jsonschema:"max facts, default 8"`
		Inbox bool   `json:"inbox,omitempty" jsonschema:"include unreviewed inbox facts"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_search",
		Description: "Search the shared fact store (owner standards, machine facts, past mistakes, design decisions) for this machine and repo. Call it before a risky or unfamiliar step.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if a.Repo == "" {
			cwd, _ := os.Getwd()
			a.Repo = store.RepoName(ctx, cwd)
		}
		if a.Limit <= 0 {
			a.Limit = 8
		}
		ix, err := openIndex(ctx, s)
		if err != nil {
			return nil, nil, err
		}
		defer ix.Close()
		// The agent asked: derived facts (hot spots, module map) are in scope.
		hits, err := ix.Query(ctx, search.Situation{Repo: a.Repo, Host: store.ShortHost(), Query: a.Query, Derived: true}, a.Limit, a.Inbox)
		if err != nil {
			return nil, nil, err
		}
		if len(hits) == 0 {
			return text("no matching facts"), nil, nil
		}
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "## %s%s (%s, %s)\n%s\n\n%s\n\n", h.Name, mcpTag(h), h.Scope, h.Type, h.Description, h.Body)
		}
		return text(b.String()), nil, nil
	})

	type factArgs struct {
		Name string `json:"name" jsonschema:"fact name as shown by tacit_search"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "tacit_fact", Description: "Read one fact in full by name."}, func(ctx context.Context, req *mcp.CallToolRequest, a factArgs) (*mcp.CallToolResult, any, error) {
		p, ok := findFactOrLocal(s.Dir, a.Name)
		if !ok {
			return text("not found: " + a.Name), nil, nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		return text(string(b)), nil, nil
	})

	type addArgs struct {
		Name        string `json:"name" jsonschema:"short slug, e.g. memra-precommit-cargo-path"`
		Description string `json:"description" jsonschema:"one line (max 200 chars) that states the fact itself with the concrete path, command, host or number"`
		Body        string `json:"body,omitempty" jsonschema:"one to three sentences: the fact and the why"`
		Type        string `json:"type" jsonschema:"user_standard | design_decision | past_mistake | machine_fact | code_change | hot_spot | structure | history"`
		Scope       string `json:"scope,omitempty" jsonschema:"projects/<repo> | machines/<host> | user | system; default projects/<current repo>"`
		Kind        string `json:"kind,omitempty" jsonschema:"user if the owner said it, observed if you found it out (default), web if it came from a fetched page"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_add",
		Description: "Record a durable fact the next agent will need: an owner ruling, a machine quirk, a mistake and its fix, a decision. Not for code structure or anything git log already says. Lands in this writer's inbox for the curator; on a read-only clone, in this machine's local notes, which the other harnesses here read and nothing pushes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a addArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if a.Scope == "" {
			cwd, _ := os.Getwd()
			if r := store.RepoName(ctx, cwd); r != "" {
				a.Scope = "projects/" + r
			}
		}
		if a.Body == "" {
			a.Body = a.Description
		}
		rel, err := addFact(ctx, s, harness, fact.Meta{Name: a.Name, Description: a.Description, Type: a.Type, Scope: a.Scope, Source: fact.Source{Kind: a.Kind}}, a.Body)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		return text(strings.Replace(addedLine(rel), "added ", "recorded ", 1)), nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 1
	}
	return 0
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func runProbeMCP() int {
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp: redaction needs scrump with the text profile:", err)
		return 2
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tacit", Version: "0.1.0"}, nil)
	type probeArgs struct {
		Verb string   `json:"verb" jsonschema:"one of git-log git-show git-blame git-grep git-ls git-tags grep find ls gh unit units sys"`
		Args []string `json:"args,omitempty" jsonschema:"the verb's arguments, one per element, as in: git-log <repo> -n 5 --grep fix -- README.md"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_probe",
		Description: "Read-only look at the projects tree, the fact store, GitHub or this machine. Verbs: git-log <repo> [-n N] [--grep P] [--author A] [--since D] [-- path]; git-show <repo> <rev> [--stat] [-- path]; git-blame <repo> <path> [-L a,b]; git-grep <repo> <pattern> [-i] [-- path]; git-ls <repo> [path]; git-tags <repo> [--contains rev]; grep <dir> <pattern> [-i]; find <dir> <name-glob>; ls <path>; gh pr|issue view <n> [-R owner/repo]; gh pr|issue list [-R owner/repo] [--state s] [--search q] [--limit N]; gh repo view <owner/repo>; gh release list [-R owner/repo]; unit <name>; units; sys. Paths must be under ~/projects, the store, ~/.config/agents or the memory dir. Other options are refused.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a probeArgs) (*mcp.CallToolResult, any, error) {
		out, err := probe(ctx, append([]string{a.Verb}, a.Args...))
		if err != nil {
			res := text(strings.TrimSpace(out + "\nprobe: " + err.Error()))
			res.IsError = out == "" // nothing usable came back
			return res, nil, nil
		}
		if strings.TrimSpace(out) == "" {
			out = "(no output)"
		}
		return text(out), nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 1
	}
	return 0
}

// mcpTag marks a tacit_search hit: unreviewed (an inbox note, the store's or
// the local dreamer's) and from this machine's overlay, both when both hold.
func mcpTag(h search.Hit) string {
	tag := ""
	if h.Inbox {
		tag += " [unreviewed]"
	}
	if h.Local {
		tag += " (this machine)"
	}
	return tag
}
