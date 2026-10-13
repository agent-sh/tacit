package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/follow"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Budgets. SessionStart may spend a little time syncing; a prompt hook must
// answer fast and never block the user on the network.
const (
	syncBudget  = 8 * time.Second
	queryBudget = 2 * time.Second
	startLimit  = 10
	promptLimit = 3
	// startLocalLimit caps this machine's local notes (read-only clone
	// overlay) in the SessionStart block: the prompt hook's count, so a
	// session starts with no more of them than one prompt can add.
	startLocalLimit = promptLimit
	// bm25 floors: below these a match is one or two common words.
	promptFloor = 15.0 // measured: junk prompts peak near 13.6, real requests 18 to 21
	startFloor  = 1.0
	// derivedPromptFloor is the floor for the one derived fact (hot spots,
	// module map) a prompt may add. Lower than promptFloor because the field
	// is different: derived facts rank alone in their own scope, and a hit is
	// a prompt that names a file or module in them. Measured on the live store
	// plus the derived facts with 31 prompts for two repos
	// (eval/VALUE.md, bm25 as the hook computes it): the 18 prompts
	// that name no file or module scored up to 7.9; a deploy prompt that said
	// "console worker" 9.7; the 6 that name one hot file by a short name 9.0,
	// 9.1, 12.3, 12.6, 15.4 and 16.8; the 5 that name two files, a full path or
	// ask for hot spots or the layout 15.9 to 29.4. There is no clean gap below
	// 12.3, so 12 is a judgment inside that band, not an optimum. A false
	// positive costs one line of about 60 tokens (the hook prints name and
	// description only), so it is set lower than the curated floor.
	//
	// Result facts (type result, from git) share this floor, measured again on
	// the same kind of prompt set (eval/VALUE.md, "Results from git"): 31
	// prompts for two repos, the highest score of a result fact on a
	// prompt that asks for no number was 9.7; 21 prompts that ask for a
	// measurement, the 8 result lines the gated hook printed scored 15.3 to
	// 35.3, and helpful and weak lines overlap in that range, so there is no
	// floor between them. Quality comes from what may be printed at all (only
	// result facts of row logs and of groups a curated fact quotes), not from a
	// higher floor.
	derivedPromptFloor = 12.0
)

// hookInput is the subset of Claude Code's hook JSON the client uses.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	Source    string `json:"source"`
}

func openIndex(ctx context.Context, s *store.Store) (*search.Index, error) {
	head, err := s.Head(ctx)
	if err != nil {
		return nil, err
	}
	return search.Open(ctx, s.Dir, head)
}

// runHook implements `tacit hook session-start|prompt`. It reads the hook
// JSON on stdin, prints the fact block on stdout (Claude Code adds it as
// context), and exits 0 on every failure: a missing store or a slow network
// must never cost the user a turn.
func runHook(args []string) int {
	if os.Getenv("TACIT_MODEL_JOB") == "1" {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "tacit hook:", r)
			os.Exit(0)
		}
	}()
	if len(args) < 1 {
		usage()
	}
	var in hookInput
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	_ = json.Unmarshal(raw, &in)
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryBudget+syncBudget)
	defer cancel()
	sit := search.Situation{Repo: store.RepoName(ctx, in.Cwd), Host: store.ShortHost()}

	switch args[0] {
	case "session-start":
		// Only a real start: resume, clear and compact re-fire this hook and
		// the facts are already in the transcript.
		if in.Source != "" && in.Source != "startup" {
			return 0
		}
		// Best effort sync, bounded. Facts load from whatever HEAD is. A
		// healthy `tacit follow` has just confirmed the clone matches the
		// server (and holds no announced push it has not merged), so the
		// network round trip is skipped; without one, sync as before.
		if !follow.Current(follow.StatePath(s.Dir), s.Dir, time.Now()) {
			sctx, scancel := context.WithTimeout(ctx, syncBudget)
			_ = s.Sync(sctx)
			scancel()
		}
		ix, err := openIndex(ctx, s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tacit hook:", err)
			return 0
		}
		defer ix.Close()
		hits, err := sessionStartHits(ctx, ix, sit)
		if err != nil {
			return 0
		}
		remember(in.SessionID, hits)
		fmt.Print(search.Block(hits, fmt.Sprintf("# tacit: facts for %s on %s (read before acting; `tacit fact <name>` for detail)", sit.Repo, sit.Host)))
	case "prompt":
		p := strings.TrimSpace(in.Prompt)
		if !promptWorthy(p) {
			return 0
		}
		ix, err := openIndex(ctx, s)
		if err != nil {
			return 0
		}
		defer ix.Close()
		strong, err := promptHits(ctx, ix, sit, p, delivered(in.SessionID))
		if err != nil {
			return 0
		}
		remember(in.SessionID, strong)
		fmt.Print(search.Block(strong, "# tacit: facts matching this request"))
	default:
		usage()
	}
	return 0
}

// isDerived reports a generated fact (derived/<repo>): hot spots, module map.
func isDerived(h search.Hit) bool { return strings.HasPrefix(h.Scope, "derived/") }

// sessionStartHits is the SessionStart block: the repo name is the query,
// the host a scope filter (a host name can also be the user name and would
// match every generic fact). Derived facts are not in the query's
// scopes at all, so they cannot take one of the ten lines.
func sessionStartHits(ctx context.Context, ix *search.Index, sit search.Situation) ([]search.Hit, error) {
	sit.Query = sit.Repo
	sit.Derived = false
	hits, err := ix.Query(ctx, sit, startLimit*2, false)
	if err != nil {
		return nil, err
	}
	return startSelection(hits, sit.Repo, startLimit), nil
}

// promptWorthy: harness-injected turns (notifications, command echoes,
// subagent reports) are not requests, and a prompt needs at least two
// searchable terms before a match means anything.
func promptWorthy(p string) bool {
	return p != "" && !strings.HasPrefix(p, "<") && len(search.Terms(p)) >= 2
}

// promptHits is the UserPromptSubmit block for prompt p. The curated facts
// are ranked exactly as before derived facts existed: one query over the
// curated scopes. The repo's derived facts (hot spots, module map) are
// ranked on their own, in a second query over their scope only, so they can
// neither displace a curated fact nor be crowded out of the candidate list
// by a repo with a thousand curated ones. At most one joins the block, after
// the curated facts, and only if the prompt matches it above its floor.
func promptHits(ctx context.Context, ix *search.Index, sit search.Situation, p string, seen map[string]bool) ([]search.Hit, error) {
	sit.Query = p
	sit.Derived, sit.OnlyDerived = false, false
	hits, err := ix.Query(ctx, sit, promptLimit*2, false)
	if err != nil {
		return nil, err
	}
	sit.OnlyDerived, sit.Printable = true, true
	dhits, err := ix.Query(ctx, sit, 1, false)
	if err != nil {
		dhits = nil // the curated block stands without them
	}
	return promptSelection(hits, dhits, seen), nil
}

// promptSelection takes the few best curated matches above the floor, drops
// what this session already saw, and then adds the best derived fact if it
// clears its own floor and was not seen. No backfill: a repeated request
// must not pull weaker facts into the context.
func promptSelection(hits, derived []search.Hit, seen map[string]bool) []search.Hit {
	var strong []search.Hit
	for _, h := range hits {
		if h.Score >= promptFloor && len(strong) < promptLimit && !isDerived(h) {
			strong = append(strong, h)
		}
	}
	var out []search.Hit
	for _, h := range strong {
		if !seen[h.Name] {
			out = append(out, h)
		}
	}
	for _, h := range derived {
		if isDerived(h) && h.Score >= derivedPromptFloor && !seen[h.Name] {
			out = append(out, h)
		}
		break // only the best one is considered
	}
	return out
}

// startSelection orders the SessionStart block: owner rules first, then
// repo and machine facts, then the rest; nothing under the score floor and
// nothing derived (the block is for owner rulings and host facts).
func startSelection(hits []search.Hit, repo string, limit int) []search.Hit {
	rank := func(h search.Hit) int {
		switch {
		case h.Ruling:
			return 0
		case strings.HasPrefix(h.Scope, "projects/"):
			return 1
		case strings.HasPrefix(h.Scope, "machines/"):
			return 2
		}
		return 3
	}
	var keep []search.Hit
	local := 0
	for _, h := range hits {
		if h.Score < startFloor || isDerived(h) {
			continue
		}
		// This machine's own notes (a read-only clone's overlay) are
		// unreviewed: a few, so they never crowd out rulings and curated facts.
		if h.Local {
			if local >= startLocalLimit {
				continue
			}
			local++
		}
		keep = append(keep, h)
	}
	sort.SliceStable(keep, func(i, j int) bool { return rank(keep[i]) < rank(keep[j]) })
	if len(keep) > limit {
		keep = keep[:limit]
	}
	return keep
}

// Per-session memory of delivered facts, so a fact is injected once per
// session. State lives under ~/.local/state/tacit/sessions/<id>.
func sessionFile(id string) string {
	if id == "" || !fact.IsSlug(strings.ToLower(strings.ReplaceAll(id, "-", ""))) {
		return ""
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tacit", "sessions", id)
}

func delivered(id string) map[string]bool {
	out := map[string]bool{}
	p := sessionFile(id)
	if p == "" {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out[l] = true
		}
	}
	return out
}

func remember(id string, hits []search.Hit) {
	p := sessionFile(id)
	if p == "" || len(hits) == 0 {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, h := range hits {
		fmt.Fprintln(f, h.Name)
	}
	// Opportunistic cleanup of old sessions.
	if entries, err := os.ReadDir(filepath.Dir(p)); err == nil && len(entries) > 200 {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
				os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
			}
		}
	}
}

// runSearch implements `tacit search [--repo R] [--host H] [--limit N] [--inbox] [--json] <query...>`.
func runSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	repo := fs.String("repo", "", "repo scope (default: the repo of the current directory)")
	host := fs.String("host", store.ShortHost(), "host scope")
	limit := fs.Int("limit", 10, "max facts")
	inbox := fs.Bool("inbox", false, "include unreviewed inbox facts")
	asJSON := fs.Bool("json", false, "JSON output")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "search: no store; run `tacit clone <url> <harness>`")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if *repo == "" {
		cwd, _ := os.Getwd()
		*repo = store.RepoName(ctx, cwd)
	}
	ix, err := openIndex(ctx, s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "search:", err)
		return 2
	}
	defer ix.Close()
	// A person or script asking for facts gets the derived ones too.
	hits, err := ix.Query(ctx, search.Situation{Repo: *repo, Host: *host, Query: strings.Join(fs.Args(), " "), Derived: true}, *limit, *inbox)
	if err != nil {
		fmt.Fprintln(os.Stderr, "search:", err)
		return 2
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(hits)
		return 0
	}
	fmt.Print(search.Block(hits, fmt.Sprintf("# %d fact(s)", len(hits))))
	return 0
}

// runFact prints one fact file by name: `tacit fact <name>`.
func runFact(args []string) int {
	if len(args) != 1 || !fact.IsSlug(args[0]) {
		usage()
	}
	s, err := store.Default()
	if err != nil {
		return 2
	}
	var found string
	filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == args[0]+".md" && !strings.Contains(p, "/.git/") {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		// This machine's own notes (a read-only clone's local overlay).
		found, _ = overlay.Find(overlay.Dir(), args[0])
	}
	if found == "" {
		fmt.Fprintln(os.Stderr, "fact: not found:", args[0])
		return 1
	}
	b, err := os.ReadFile(found)
	if err != nil {
		return 2
	}
	os.Stdout.Write(b)
	return 0
}

// runSync implements `tacit sync` and `tacit clone <url> <harness>`.
func runSync(args []string) int {
	s, err := store.Default()
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 1
	}
	head, _ := s.Head(ctx)
	ix, err := search.Open(ctx, s.Dir, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync: index:", err)
		return 1
	}
	ix.Close()
	fmt.Println("synced", head[:12])
	return 0
}

// runClone implements `tacit clone <url> <harness>` and
// `tacit clone --read-only [--as <harness>] <url>` (no push ever; every
// write refused or, for an agent's fact, kept in this machine's overlay).
// With --as the fetches sign with that key, which the server's public path
// asks for on every request.
func runClone(args []string) int {
	readOnly, as := false, ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--read-only" || a == "-read-only":
			readOnly = true
		case (a == "--as" || a == "-as") && i+1 < len(args):
			as = args[i+1]
			i++
		default:
			rest = append(rest, a)
		}
	}
	switch {
	case readOnly && len(rest) == 1 && (as == "" || fact.IsSlug(as)):
	case !readOnly && as == "" && len(rest) == 2:
	default:
		usage()
	}
	if as != "" {
		id := as + "@" + shortHost(mustHostname())
		dir, err := auth.Dir()
		if err == nil {
			_, err = auth.Load(dir, id)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "clone: no key %s here; run `tacit keygen %s` and enroll it: %v\n", id, as, err)
			return 2
		}
	}
	s, err := store.Default()
	if err != nil {
		return 2
	}
	if s.Exists() {
		fmt.Fprintln(os.Stderr, "clone: store exists at", s.Dir)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	clone := func() error { return s.Clone(ctx, rest[0], rest[1]) }
	if readOnly {
		clone = func() error { return s.CloneReadOnly(ctx, rest[0], as) }
	}
	if err := clone(); err != nil {
		fmt.Fprintln(os.Stderr, "clone:", err)
		return 1
	}
	if readOnly {
		signed := ""
		if as != "" {
			signed = ", fetches signed as " + as + "@" + shortHost(mustHostname())
		}
		fmt.Println("cloned to", s.Dir, "(read-only: nothing is pushed from this machine"+signed+")")
		return 0
	}
	fmt.Println("cloned to", s.Dir)
	return 0
}
