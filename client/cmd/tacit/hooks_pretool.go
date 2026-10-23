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
// tools of Claude Code, Codex's apply_patch, the shell tool of both, and
// every MCP tool (mcp__<server>__<tool>), whose calls an area's tool
// patterns match (areaPush). The owner accepted a hook process on every MCP
// call (phase 2, answer 3); its cost is measured (eval/VALUE.md).
const toolPushTools = "Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch|mcp__.*"

// legacyToolPushTools are matchers earlier releases wrote; install replaces
// its entry under one of them in place (Codex keys hook trust by position).
var legacyToolPushTools = []string{"Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch"}

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
// before an edit, a shell command or an MCP tool call,
//
//   - in a repository, at most metaCeiling of this repo's rules, standards,
//     decisions and past mistakes whose applies_to paths cover a file the
//     call writes, or whose command the call runs;
//   - anywhere, in a repository or not, for each area whose tool patterns
//     match the call's tool name or whose commands it runs, the first time
//     in the session: a few of the area's rules, standards, decisions and
//     past mistakes (areaPush) and a line to the area's folder. Dormant,
//     dead and shipped areas never push.
//
// Each fact once per session (the session's delivered set, shared with the
// other hooks); a fact's own path or command match comes before the area's.
// It answers as JSON additionalContext, the only PreToolUse output both
// harnesses give the model, never blocks or changes the call, and prints
// nothing on any failure or after toolBudget. It reads no file beyond the
// index (the records are in it).
func preTool(s *store.Store, in hookInput) {
	if !deliveryOn("TOOL_PUSH", true) {
		return
	}
	paths, command := toolTargets(in.ToolName, in.ToolInput)
	if strings.HasPrefix(in.ToolName, "mcp__") {
		// An MCP tool's input is its own: a "path" or "command" field
		// there is no file the call writes and no shell command it runs.
		paths, command = nil, ""
	}
	if len(paths) == 0 && strings.TrimSpace(command) == "" && in.ToolName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolBudget)
	defer cancel()
	var rel []string
	var sit search.Situation
	repo, inRepo := sessionRepo(in.Cwd)
	if inRepo {
		sit = search.Situation{Repo: repo.Name, Host: store.ShortHost(), RepoAliases: repoAliases(repo)}
		for _, p := range paths {
			if r, ok := repoRel(repo, in.Cwd, p); ok {
				rel = append(rel, r)
			}
		}
	}
	// An area matches an MCP tool's name or a shell command (the matcher
	// sends no other tool here but the edit tools): an edit outside a
	// repository opens nothing, as before areas.
	areas := deliveryOn("AREA_PUSH", true) && (strings.HasPrefix(in.ToolName, "mcp__") || command != "")
	if !areas && (!inRepo || (len(rel) == 0 && command == "")) {
		return
	}
	ix, err := openHookIndex(ctx, s)
	if err != nil {
		return
	}
	defer ix.Close()
	recs := hookRecords(ctx, ix)
	seen := delivered(in.SessionID)
	var hits []search.Hit
	if inRepo && (len(rel) > 0 || command != "") {
		sit = withRecordAliases(sit, recs)
		all, err := ix.MetaFacts(ctx)
		if err != nil || ctx.Err() != nil {
			return
		}
		hits = metaSelect(all, repoNames(sit), rel, command, "", seen, deliveryInt("META_CEILING", metaCeiling))
	}
	var lines, pushed []string
	if areas && len(recs) > 0 {
		var ah []search.Hit
		ah, lines, pushed = areaPush(ctx, ix, recs, in.ToolName, command, withSeen(seen, hits))
		hits = append(hits, ah...)
	}
	hits = withoutClaims(hits)
	if ctx.Err() != nil || (len(hits) == 0 && len(pushed) == 0) {
		return
	}
	names := make([]string, 0, len(hits)+len(pushed))
	for _, h := range hits {
		names = append(names, h.Name)
	}
	for _, f := range pushed {
		names = append(names, areaSeen(f))
	}
	rememberNames(in.SessionID, names)
	if len(hits) == 0 {
		return
	}
	block := search.Block(hits, "# tacit: before this step")
	for _, l := range lines {
		block += l + "\n"
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "additionalContext": block}})
	fmt.Println(string(out))
}
