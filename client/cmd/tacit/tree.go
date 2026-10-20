package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// runTree implements `tacit tree [--depth N] [--repo R] [--host H] [path]`:
// the folder indexes from path (default: the store root) down depth levels
// (default 3), for this session's scopes.
func runTree(args []string) int {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	depth := fs.Int("depth", tree.DefaultDepth, "levels below the folder to include (0: the folder alone)")
	repo := fs.String("repo", "", "repository whose project folder is in scope (default: the repo of the current directory)")
	host := fs.String("host", store.ShortHost(), "machine whose folder is in scope")
	fs.Parse(args)
	if fs.NArg() > 1 {
		usage()
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "tree: no store")
		return 2
	}
	out, err := treeView(s, fs.Arg(0), *depth, *repo, *host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tree:", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// treeView is the tree tool's answer for the session in the current
// directory: its repository (repoid, with the names delivery knows it by)
// unless repo names one, and host.
func treeView(s *store.Store, p string, depth int, repo, host string) (string, error) {
	var repos []string
	if repo != "" {
		repos = []string{repo}
	} else if cwd, err := os.Getwd(); err == nil {
		home, _ := os.UserHomeDir()
		if r, ok := repoid.Of(cwd, home); ok {
			repos = append([]string{r.Name}, r.Aliases...)
		}
	}
	ro := tree.ReadOptions{Store: s.Dir, Repos: repos, Host: host}
	if overlay.LayerOn() && overlay.Outside(overlay.Dir(), s.Dir) {
		ro.Local = filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir))
	}
	return tree.Read(ro, p, depth)
}

// treeInstruction is the tree tool's line in the MCP server's instructions.
const treeInstruction = "tacit_tree shows what the store holds for this session, folder by folder, from a folder (default the store root) down a depth (default 3)."

var errTreeArgs = errors.New("depth must be between 0 and 8")

// treeToolText answers the MCP tool: the view, or the error as text the agent
// can act on.
func treeToolText(s *store.Store, p string, depth *int) string {
	d := tree.DefaultDepth
	if depth != nil {
		d = *depth
	}
	if d < 0 || d > tree.MaxDepth {
		return "error: " + errTreeArgs.Error()
	}
	out, err := treeView(s, p, d, "", store.ShortHost())
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// addTreeTool registers tacit_tree on the store's MCP server.
func addTreeTool(server *mcp.Server, s *store.Store) {
	type treeArgs struct {
		Path  string `json:"path,omitempty" jsonschema:"store folder: empty for the root, or system, user, projects/<repo>, machines/<host>, derived/<repo>"`
		Depth *int   `json:"depth,omitempty" jsonschema:"levels below the folder to include; default 3, 0 for the folder alone"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_tree",
		Description: "What the store holds for this session, folder by folder: each folder's facts (name, type, one line) and its subfolders with counts. Read a fact in full with tacit_fact.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a treeArgs) (*mcp.CallToolResult, any, error) {
		return text(treeToolText(s, a.Path, a.Depth)), nil, nil
	})
}
