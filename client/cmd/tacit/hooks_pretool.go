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

// toolPushTools is the PreToolUse matcher `tacit install` writes: the edit
// tools of Claude Code, Codex's apply_patch, and the shell tool of both.
const toolPushTools = "Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch"

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
// before an edit or a shell command, at most metaCeiling of this repo's
// rules, standards, decisions and past mistakes whose applies_to paths
// cover a file the call writes, or whose command the call runs, each once
// per session (the session's delivered set, shared with the other hooks).
// It answers as JSON additionalContext, the only PreToolUse output both
// harnesses give the model, never blocks or changes the call, and prints
// nothing on any failure or after toolBudget.
func preTool(s *store.Store, in hookInput) {
	if !deliveryOn("TOOL_PUSH", true) {
		return
	}
	paths, command := toolTargets(in.ToolName, in.ToolInput)
	if len(paths) == 0 && strings.TrimSpace(command) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolBudget)
	defer cancel()
	repo, ok := sessionRepo(in.Cwd)
	if !ok {
		return
	}
	sit := search.Situation{Repo: repo.Name, Host: store.ShortHost(), RepoAliases: repoAliases(repo)}
	var rel []string
	for _, p := range paths {
		if r, ok := repoRel(repo, in.Cwd, p); ok {
			rel = append(rel, r)
		}
	}
	if len(rel) == 0 && command == "" {
		return
	}
	ix, err := openIndex(ctx, s)
	if err != nil {
		return
	}
	defer ix.Close()
	all, err := ix.MetaFacts(ctx)
	if err != nil || ctx.Err() != nil {
		return
	}
	hits := metaSelect(all, repoNames(sit), rel, command, "", delivered(in.SessionID), deliveryInt("META_CEILING", metaCeiling))
	if len(hits) == 0 || ctx.Err() != nil {
		return
	}
	remember(in.SessionID, hits)
	block := search.Block(hits, "# tacit: before this step")
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "additionalContext": block}})
	fmt.Println(string(out))
}
