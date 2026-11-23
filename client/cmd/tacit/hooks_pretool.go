package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// toolBudget bounds the PreToolUse hook: it runs before every edit and
// shell command, so it answers within this or prints nothing.
const toolBudget = 400 * time.Millisecond

// toolDeadline is what preTool runs under: toolBudget, except in tests that
// check what the hook prints, which widen it so a loaded machine or the race
// detector cannot turn their answer into the empty one.
var toolDeadline = toolBudget

// toolPushTools is the PreToolUse matcher `tacit install` writes: the shell
// tool of both harnesses (a command a rule is keyed to is denied once) and
// tacit's own MCP tools (the store's clickstream). No other call starts the
// hook.
const toolPushTools = "Bash|mcp__.*__tacit_.*"

// legacyToolPushTools are matchers earlier releases wrote; install replaces
// its entry under one of them in place (Codex keys hook trust by position).
var legacyToolPushTools = []string{"Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch", "Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch|mcp__.*"}

// patchFile is a file line of an apply_patch patch.
var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

// toolTargets reads what a tool call is about to change: the files an edit
// writes, or the shell command it runs.
func toolTargets(name string, input json.RawMessage) (paths []string, command string) {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		// Codex passes some tools' input as a plain string (a patch).
		var s string
		if json.Unmarshal(input, &s) == nil {
			in = map[string]any{"command": s}
		}
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if p := str(k); p != "" {
			paths = append(paths, p)
		}
	}
	cmd := str("command")
	if cmd == "" {
		cmd = str("input")
	}
	if m := patchFile.FindAllStringSubmatch(cmd, -1); m != nil {
		for _, x := range m {
			paths = append(paths, strings.TrimSpace(x[1]))
		}
		return paths, ""
	}
	if name == "Bash" || len(paths) == 0 {
		command = cmd
	}
	return paths, command
}

// repoRel is p (absolute, or relative to cwd) relative to the checkout r
// is (repoid's Worktree, symlinks resolved on both sides), and false when
// p is outside it.
func repoRel(r repoid.Repo, cwd, p string) (string, bool) {
	if r.Worktree == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(r.Worktree, repoid.RealPath(filepath.Clean(p)))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// preTool is `tacit hook pre-tool` (Claude Code's and Codex's PreToolUse):
// before a shell command, a deny when a rule, standard or past mistake keyed
// to that command applies (hooks_deny.go); before a call of tacit's own MCP
// tools, nothing but the store's clickstream (logNavigation). It delivers no
// context: the pushes it gave before an edit or a command were read too
// rarely for what they cost (SPEC.md, "Command rules"). It prints nothing on
// any failure or after toolBudget, and nothing in a throwaway harness
// instance (sessionKind's scratch).
func preTool(s *store.Store, in hookInput) {
	_, command := toolTargets(in.ToolName, in.ToolInput)
	// The store's clickstream, after the answer.
	defer logNavigation(s, in, navOf(in, command))
	if strings.HasPrefix(in.ToolName, "mcp__") || strings.TrimSpace(command) == "" || in.SessionID == "" {
		return
	}
	kind := sessionKind(in)
	if kind == "scratch" {
		return
	}
	hookHeadless = kind != ""
	ctx, cancel := context.WithTimeout(context.Background(), toolDeadline)
	defer cancel()
	t := loadDenyTable(time.Now())
	if t == nil || len(t.Facts) == 0 {
		return
	}
	sc := denyScope{host: store.ShortHost()}
	if r, ok := sessionRepo(in.Cwd); ok {
		sc.repos = repoNames(search.Situation{Repo: r.Name, RepoAliases: repoAliases(r)})
	}
	fs := denyFor(t, command, sc, deniedSet(in.SessionID), outcomesFor(outcomeScope(sc.repos)))
	if len(fs) == 0 || ctx.Err() != nil {
		return
	}
	names := make([]string, len(fs))
	hits := make([]search.Hit, len(fs))
	for i, f := range fs {
		names[i] = f.Name
		hits[i] = search.Hit{Name: f.Name, Scope: f.Scope, Type: f.Type, Created: f.Created}
	}
	lines := append([]string(nil), names...)
	for _, f := range fs {
		lines = append(lines, denyKeyLine(f.Key))
	}
	addDenied(in.SessionID, lines)
	beginLog("deny", in, command, sc.repos, nil)
	if dlog != nil {
		dlog.Tool = in.ToolName
	}
	logChose(hits, "deny")
	remember(in.SessionID, hits) // given: in the delivered set, and owed a mark
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": denyReason(fs, hookMCP)}})
	fmt.Println(string(out))
	endLog(hits)
}
