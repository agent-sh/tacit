package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// hookExe is the binary the PreCompact hook starts; tests substitute it.
var hookExe = os.Executable

// dreamSessionLog caps the log of the dreams the PreCompact hook starts: past
// it the file moves to .1 (replacing the older one) before the next run.
const dreamSessionLog = 1 << 20

// preCompact is `tacit hook pre-compact` (Claude Code's and Codex's
// PreCompact, the Hermes provider's on_pre_compress): it starts `tacit dream
// --session <transcript>` for the session about to compact and returns. The
// harness waits for the hook before it compacts, and a dream takes minutes,
// so the dream runs detached (platform.Detach), with no stdin and its output
// in dream-session.log in the state dir: nothing holds a pipe the harness
// reads. The provider variables come from the env file the dream job uses,
// when it exists. Every failure is silent (fail open); the idle dream finds
// the compaction marker later anyway (dream.Select).
func preCompact(in hookInput) {
	target := in.TranscriptPath
	if target == "" {
		target = in.HermesSession
	}
	if target == "" || strings.ContainsAny(target, "\n\r") {
		return
	}
	exe, err := hookExe()
	if err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	args := []string{"dream", "--session", target}
	if env := defaultDreamEnv(runtime.GOOS, home); fileExists(env) {
		args = append(args, "--env-file", env)
	}
	dir := platform.StateDir(home)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "dream-session.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > dreamSessionLog {
		os.Rename(path, path+".1")
	}
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer log.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	platform.Detach(cmd)
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// sessionIDs are the names the dreamer may have filed this session's notes
// under (source.session): the harness's session id, and its transcript's
// name, which is what the dreamer records for Codex (rollout-<time>-<id>)
// and pi (the part after <time>_).
func sessionIDs(in hookInput) []string {
	var ids []string
	add := func(id string) {
		if id != "" && len(id) < 200 && !strings.ContainsAny(id, " \t\r\n\"'") && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(in.SessionID)
	if in.TranscriptPath != "" {
		name := strings.TrimSuffix(filepath.Base(in.TranscriptPath), ".jsonl")
		add(name)
		if i := strings.LastIndexByte(name, '_'); i >= 0 {
			add(name[i+1:])
		}
	}
	return ids
}

// sessionNotes are this session's own dreamer notes that no curator has split
// yet: notes in inbox/dreamer@<this host>/ whose source.session is the
// session (sessionIDs), with no curated fact made from them (origin), newest
// first. They are what the dreamer took from the turns a compaction just
// dropped from the agent's context. Marked Session for the block ("this
// session, not yet reviewed") and never a ruling: no curator has checked
// their quotes against the owner's turns.
func sessionNotes(ctx context.Context, s *store.Store, in hookInput) []search.Hit {
	ids := sessionIDs(in)
	if len(ids) == 0 {
		return nil
	}
	dir := "inbox/dreamer@" + store.ShortHost()
	args := []string{"grep", "-l", "-F", "-I", "--untracked"}
	for _, id := range ids {
		args = append(args, "-e", "session: "+id)
	}
	out, err := store.Git(ctx, s.Dir, append(args, "--", dir)...)
	if err != nil {
		return nil // also no match: git grep exits 1
	}
	type note struct {
		f   *fact.Fact
		mod int64
	}
	var notes []note
	for _, rel := range strings.Split(strings.TrimSpace(out), "\n") {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if !strings.HasSuffix(rel, ".md") {
			continue
		}
		full := filepath.Join(s.Dir, filepath.FromSlash(rel))
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		f, err := fact.Parse(rel, b)
		if err != nil || !f.InInbox() || f.Meta.Source.Harness != "dreamer" || !slices.Contains(ids, f.Meta.Source.Session) || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		var mod int64
		if fi, err := os.Stat(full); err == nil {
			mod = fi.ModTime().UnixNano()
		}
		notes = append(notes, note{f, mod})
	}
	if len(notes) == 0 {
		return nil
	}
	// A note the curator split is delivered as its curated facts, reviewed.
	args = []string{"grep", "-h", "-F", "-I"}
	for _, n := range notes {
		args = append(args, "-e", "origin: "+n.f.Meta.Name)
	}
	curated := map[string]bool{}
	if out, err := store.Git(ctx, s.Dir, append(args, "--", ".", ":(exclude)inbox")...); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if name, ok := strings.CutPrefix(strings.TrimSpace(l), "origin: "); ok {
				curated[strings.Trim(name, `"'`)] = true
			}
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].mod > notes[j].mod })
	var hits []search.Hit
	for _, n := range notes {
		if curated[n.f.Meta.Name] {
			continue
		}
		hits = append(hits, search.Hit{Path: n.f.Path, Name: n.f.Meta.Name, Scope: n.f.Meta.Scope, Type: n.f.Meta.Type, Description: n.f.Meta.Description, Inbox: true, Session: true, Kind: n.f.Meta.Source.Kind, Score: startFloor})
	}
	return hits
}

// compactStartHits is the SessionStart block after a compaction (source
// compact, `hook session-start --compact`): the start block's facts and this
// session's own fresh notes (sessionNotes), less every fact the session was
// already given. Each fact still reaches a session once; the start block's
// limits and order apply (startSelection).
func compactStartHits(ctx context.Context, ix *search.Index, sit search.Situation, own []search.Hit, seen map[string]bool) ([]search.Hit, error) {
	sit.Query = sit.Repo
	sit.RestrictRepos = true
	sit.PromptRelevance = false
	sit.Derived = false
	hits, err := ix.Query(ctx, sit, startLimit*2, false)
	if err != nil {
		return nil, err
	}
	var fresh []search.Hit
	for _, h := range append(hits, own...) {
		if !seen[h.Name] {
			fresh = append(fresh, h)
		}
	}
	return startSelection(fresh, sit.Repo, startLimit), nil
}
