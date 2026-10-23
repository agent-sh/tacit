package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// runTree implements `tacit tree [--depth N] [--repo R] [--host H] [path]`:
// the folder indexes from path (default: the store root) down depth levels
// (default 2 at the root, 3 for a folder; tree.DepthFor), for this session's
// scopes.
func runTree(args []string) int {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	depth := fs.Int("depth", -1, "levels below the folder to include (0: the folder alone; default 2 at the store root, 3 for a folder)")
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
	d := *depth
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "depth" })
	if !explicit {
		d = tree.DepthFor(fs.Arg(0))
	}
	if d < 0 || d > tree.MaxDepth {
		fmt.Fprintln(os.Stderr, "tree:", errTreeArgs)
		return 2
	}
	out, err := treeView(s, fs.Arg(0), d, *repo, *host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tree:", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// treeView is the tree tool's answer for the session in the current
// directory (readSession: its repository under every name delivery knows
// it by, unless repo names one, and host): first how many facts of each
// type the answer's scopes hold (scopeCounts, from the search index; left
// out when the index cannot be opened), then the folder indexes, each fact
// line saying whose it is (treeWhose).
func treeView(s *store.Store, p string, depth int, repo, host string) (string, error) {
	cwd, _ := os.Getwd()
	ro := tree.ReadOptions{Store: s.Dir, Session: readSession(storeRecords(s.Dir), cwd, repo, host)}
	if overlay.LayerOn() && overlay.Outside(overlay.Dir(), s.Dir) {
		ro.Local = filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir))
	}
	out, err := tree.Read(ro, p, depth)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ix, err := openHookIndex(ctx, s)
	if err != nil {
		return out, nil
	}
	defer ix.Close()
	return treeCounts(ctx, ix, ro.Session, p) + treeWhose(ctx, ix, out), nil
}

// treeCounts is scopeCounts for the scopes of a tree answer from folder p:
// the session's scopes from the root, those under a scope root, or the one
// scope p is in. Best effort: "" when the index cannot be read in time.
func treeCounts(ctx context.Context, ix *search.Index, sess tree.Session, p string) string {
	p, _ = tree.Clean(p)
	var scopes []string
	if named := tree.Named(p); named != "" {
		scopes = []string{named}
	} else {
		for _, sc := range sess.Scopes() {
			if p == "" || strings.HasPrefix(sc, p+"/") {
				scopes = append(scopes, sc)
			}
		}
	}
	out, err := scopeCounts(ctx, ix, scopes)
	if err != nil || out == "" {
		return ""
	}
	return out + "\n"
}

// treeLineRx is a fact line of a folder index: "- `name` (type...):".
var treeLineRx = regexp.MustCompile("(?m)^- `([a-z0-9][a-z0-9-]*)` \\(([^)\n]*)\\):")

// treeWhose marks each fact line of a tree answer that is not the owner's
// with whose it is, from the index's code-checked fields (search.Hit's
// Provenance): "agent note", "agent note holding your words", or "agent
// note, claims owner authority without evidence". The folder index files
// stay as they are (they already mark the owner's rulings); a line the
// index does not know is left alone.
func treeWhose(ctx context.Context, ix *search.Index, out string) string {
	var names []string
	for _, m := range treeLineRx.FindAllStringSubmatch(out, -1) {
		names = append(names, m[1])
	}
	hs, err := ix.Named(ctx, names)
	if err != nil || len(hs) == 0 {
		return out
	}
	return treeLineRx.ReplaceAllStringFunc(out, func(l string) string {
		m := treeLineRx.FindStringSubmatch(l)
		h, ok := hs[m[1]]
		if !ok {
			return l
		}
		label := ""
		switch h.Provenance() {
		case "claim":
			label = "agent note, " + search.ClaimMark
		case "words":
			label = "agent note holding your words"
		case "agent":
			label = "agent note"
		}
		if label == "" || strings.Contains(m[2], label) {
			return l
		}
		return "- `" + m[1] + "` (" + m[2] + ", " + label + "):"
	})
}

var errTreeArgs = errors.New("depth must be between 0 and 8")

// treeToolText answers the MCP tool: the view, or the error as text the agent
// can act on.
func treeToolText(s *store.Store, p string, depth *int) string {
	d := tree.DepthFor(p)
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
		Depth *int   `json:"depth,omitempty" jsonschema:"levels below the folder to include; default 2 for the store root and 3 for a folder, 0 for the folder alone"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_tree",
		Description: "What the store holds for this session, folder by folder: how many facts of each type its scopes hold, each folder's facts (name, type, one line) and its subfolders with counts. At depth 0 a project, area or topic folder also shows its overview, every line citing its facts. Another repository's or machine's folder only when named as the path. A scope's full list is tacit_search scope=<scope>; a fact in full, tacit_fact.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a treeArgs) (*mcp.CallToolResult, any, error) {
		return text(treeToolText(s, a.Path, a.Depth)), nil, nil
	})
}
