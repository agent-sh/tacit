package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// agentHarness reports a harness an agent may write as. The owner's notes are
// written by tacit app (an owner commit) and curated facts by the curator; no
// agent writes as either, so a note in inbox/human@ is theirs by construction.
// A reader key only reads, so nothing is written as reader either.
func agentHarness(h string) bool {
	return fact.Harnesses[h] && h != "human" && h != "curator" && h != "reader"
}

// addFact writes one fact into this writer's inbox and pushes it. Used by
// `tacit add` and by the MCP tool. The file name is the slug of the name;
// an existing file with the same slug gets a short suffix so nothing is
// overwritten (supersede, never delete).
//
// On a read-only clone the fact goes to this machine's local overlay
// instead (internal/overlay, outside the clone, never pushed), redacted the
// same way, so the harnesses here still share what they learn. The returned
// path then starts with "local/".
func addFact(ctx context.Context, s *store.Store, harness string, m fact.Meta, body string) (string, error) {
	if !fact.Harnesses[harness] {
		return "", fmt.Errorf("unknown harness %q", harness)
	}
	if !agentHarness(harness) {
		return "", fmt.Errorf("harness %q is not one an agent may write as; the owner writes in tacit app", harness)
	}
	readOnly := s.ReadOnly(ctx)
	if readOnly && !overlay.Outside(overlay.Dir(), s.Dir) {
		return "", overlay.ErrInside // before redaction: nothing is written
	}
	host := store.ShortHost()
	writer := harness + "@" + host
	// Agents write here directly (tacit add, the MCP tool, the Hermes
	// provider), so redact before anything touches disk. Fail closed: no
	// scrump, no write.
	red, err := digest.RedactAll([]string{m.Description, body, m.Name, m.Scope})
	if err != nil {
		return "", fmt.Errorf("redaction unavailable, fact not written: %w", err)
	}
	m.Description, body = red[0], red[1]
	// Name and scope reach the file name, the frontmatter and the commit
	// message; a masked name is rebuilt rather than kept, a masked scope is
	// refused (no scope of code's choosing replaces the writer's).
	if red[2] != m.Name {
		m.Name = importer.Slug(red[2], 80)
	}
	if red[3] != m.Scope {
		return "", errors.New("refused: scope: the redaction masked part of it; give a scope without secrets (user, projects/<repo>, machines/<host>)")
	}
	if !fact.IsSlug(m.Name) {
		m.Name = importer.Slug(m.Name, 80)
	}
	if m.Name == "" {
		m.Name = "fact-" + shortHash(m.Description+body)
	}
	// Defaults for what the writer left out; anything it gave is checked
	// (internal/validate) and refused with the reason, never replaced.
	if m.Type == "" {
		m.Type = "unclassified"
	}
	if m.Scope == "" {
		m.Scope = "user"
	}
	// A repo or host in capitals is the same scope in lower case (the store's
	// scopes are lower case, as the curator's split normalizes them).
	if l := strings.ToLower(m.Scope); l != m.Scope && fact.ValidScope(l) {
		m.Scope = l
	}
	m.Source = fact.Source{Harness: harness, Host: host, Kind: firstNonEmpty(m.Source.Kind, "observed"), Session: firstNonEmpty(m.Source.Session, harnessSession(harness))}
	env := addEnv(s)
	if w, ok := env.Redirect(m.Scope); ok {
		m.Scope = w // a suppressed folder's facts go to the folder that replaces it
	}
	env.RepoPaths(&m) // a path inside this machine's checkout of the repo, made relative
	if err := validate.Error(env.Meta(m, body, true, false)); err != nil {
		return "", err
	}
	m.Created = time.Now().Format("2006-01-02")
	m.Description = fact.ClipDescription(m.Description, 200)
	if readOnly {
		f := &fact.Fact{Meta: m, Body: strings.TrimSpace(body) + "\n"}
		return overlay.Add(overlay.Dir(), s.Dir, writer, f, func() string { return shortHash(m.Description + body) })
	}
	var rel string
	err = s.WithLock(ctx, func() error {
		if s.ReadOnly(ctx) {
			return store.ErrReadOnly
		}
		rel = filepath.ToSlash(filepath.Join("inbox", writer, m.Name+".md"))
		if same(filepath.Join(s.Dir, rel), m.Description, body) {
			return nil
		}
		if _, err := os.Stat(filepath.Join(s.Dir, rel)); err == nil {
			m.Name = importer.Slug(m.Name, 70) + "-" + shortHash(m.Description+body)
			rel = filepath.ToSlash(filepath.Join("inbox", writer, m.Name+".md"))
			if same(filepath.Join(s.Dir, rel), m.Description, body) {
				return nil
			}
		}
		f := &fact.Fact{Path: rel, Meta: m, Body: strings.TrimSpace(body) + "\n"}
		b, err := f.Encode()
		if err != nil {
			return err
		}
		full := filepath.Join(s.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return err
		}
		if err := s.CommitPushLocked(ctx, []string{rel}, "fact: "+m.Name, harness); err != nil {
			return fmt.Errorf("written locally, push failed: %w", err)
		}
		return nil
	})
	return rel, err
}

// sessionEnv is where each harness tells a tool it runs which session it
// runs in: Claude Code's Bash tool and MCP servers (CLAUDE_CODE_SESSION_ID),
// Codex's (CODEX_THREAD_ID), pi's bash tool (PI_SESSION_ID). TACIT_SESSION,
// set by an integration that knows its session (the Hermes provider), comes
// first. The reviewer reads the conversation a note came from by it
// (SPEC.md, "The reviewer").
var sessionEnv = map[string][]string{
	"claude": {"CLAUDE_CODE_SESSION_ID"},
	"codex":  {"CODEX_THREAD_ID", "CODEX_SESSION_ID"},
	"pi":     {"PI_SESSION_ID"},
}

// harnessSession is the session a note of harness is written in, from the
// environment its harness gives it, or "" when it gives none (code supplies
// it only where it reads it reliably). An id that could name a path is not
// one.
func harnessSession(harness string) string {
	for _, k := range append([]string{"TACIT_SESSION"}, sessionEnv[harness]...) {
		v := strings.TrimSpace(os.Getenv(k))
		if p, id, ok := strings.Cut(v, ":"); ok && k == "TACIT_SESSION" {
			if p != harness {
				continue // another harness's session
			}
			v = id
		}
		if v != "" && agentreview.ValidSession(v) {
			return v
		}
	}
	return ""
}

// addEnv is what an agent's note is checked against: this machine's
// checkouts and the store's folders (and the local layer's, when it is on).
var addEnv = func(s *store.Store) validate.Env {
	return curateEnv(s.Dir, overlay.LayerOn())
}

// runAdd implements `tacit add --harness H --type T --scope S --kind K --desc D [--body B] <name>`.
func runAdd(args []string) int {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	harness := fs.String("harness", "", "writer harness, required: claude, codex, pi, hermes, grok (the owner writes in tacit app)")
	typ := fs.String("type", "unclassified", "fact type")
	scope := fs.String("scope", "", "scope (projects/<repo>, machines/<host>, user, system); default projects/<current repo>")
	kind := fs.String("kind", "observed", "source kind: user, observed, web")
	desc := fs.String("desc", "", "one-line description that states the fact (required)")
	body := fs.String("body", "", "body; default is the description")
	fs.Parse(args)
	if fs.NArg() != 1 || *desc == "" {
		usage()
	}
	if *harness == "" {
		// No default: a note's harness says who wrote it, and "human" is the
		// owner's claim to their own words (SPEC.md, Rulings).
		fmt.Fprintln(os.Stderr, "add: --harness is required")
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "add: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if *scope == "" {
		cwd, _ := os.Getwd()
		if r := repoid.Slug(store.RepoName(ctx, cwd)); r != "" {
			*scope = "projects/" + r
		} else {
			*scope = "user"
		}
	}
	if *body == "" {
		*body = *desc
	}
	rel, err := addFact(ctx, s, *harness, fact.Meta{Name: fs.Arg(0), Description: *desc, Type: *typ, Scope: *scope, Source: fact.Source{Kind: *kind}}, *body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "add:", err)
		return 1
	}
	fmt.Println(addedLine(rel))
	return 0
}

// addedLine says where an added fact went: the inbox (pushed for the
// curator) or this machine's overlay (a read-only clone; never pushed).
func addedLine(rel string) string {
	if strings.HasPrefix(rel, overlay.Prefix) {
		return "added " + rel + " on this machine only (read-only clone: shared with the other harnesses here, never pushed)"
	}
	return "added " + rel
}

func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%06x", h&0xffffff)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// same reports whether the inbox file at path already holds this fact, so a
// repeated write (a mirrored memory, a retried tool call) adds nothing.
func same(path, desc, body string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	f, err := fact.Parse(filepath.Base(path), data)
	if err != nil {
		return false
	}
	return f.Meta.Description == fact.ClipDescription(desc, 200) && strings.TrimSpace(f.Body) == strings.TrimSpace(body)
}
