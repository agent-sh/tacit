package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The reviewer's questions in the owner's session (SPEC.md, "Questions"): at
// the prompt, in a session a person types into, the hook first reads the
// owner's answers to what this session was asked (from the transcript, their
// own turns only), then puts at most one question of the queue that belongs
// here (same repository, folder or topic, by code) for the agent to ask
// verbatim. A session is asked at most agentreview.PerSession questions.
// With no question waiting nothing is read beyond the queue's file and
// nothing is printed.

// questionWait is how long the hook waits for the queue's lock.
const questionWait = 150 * time.Millisecond

// questionTitle opens the block: what the agent does with the question.
const questionTitle = "# tacit: a question for the owner. End your reply with it, word for word, as its own last line. Do not answer it yourself: only the owner's own reply counts."

// hookSession is the harness, session and transcript of the hook's session,
// and where its transcript stands; ok is false for a session no person
// types into (an agent's headless run, a subagent) or one tacit cannot read.
func hookSession(ctx context.Context, in hookInput, home string) (agentreview.Situation, bool) {
	var s agentreview.Situation
	switch {
	case in.HermesSession != "":
		db := hermes.DefaultDB(home)
		if db == "" || !agentreview.ValidSession(in.HermesSession) {
			return s, false
		}
		d, err := hermes.Open(db)
		if err != nil {
			return s, false
		}
		defer d.Close()
		sessions, err := d.Sessions(ctx)
		if err != nil {
			return s, false
		}
		owner := false
		for _, x := range sessions {
			if x.ID == in.HermesSession {
				owner = x.Owner
			}
		}
		if !owner {
			return s, false
		}
		ids, err := d.LastIDs(ctx)
		if err != nil {
			return s, false
		}
		s.Harness, s.Session, s.Transcript, s.Offset = "hermes", in.HermesSession, db, ids[in.HermesSession]
		return s, true
	case in.TranscriptPath != "":
		path := filepath.Clean(in.TranscriptPath)
		f, ok := transcriptFormat(home, path)
		if !ok {
			return s, false
		}
		fh, err := os.Open(path)
		if err != nil {
			return s, false
		}
		defer fh.Close()
		fi, err := fh.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return s, false
		}
		head := make([]byte, 256<<10)
		n, _ := io.ReadFull(fh, head)
		if !digest.SessionWho(path, head[:n], f).Owner() {
			return s, false
		}
		s.Harness = map[digest.Format]string{digest.Claude: "claude", digest.Codex: "codex", digest.Pi: "pi"}[f]
		s.Session = in.SessionID
		if s.Session == "" {
			s.Session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
		s.Transcript, s.Offset = path, fi.Size()
		return s, s.Harness != ""
	}
	return s, false
}

// transcriptFormat is the dialect of a transcript, by the dreamer's source
// globs.
func transcriptFormat(home, path string) (digest.Format, bool) {
	for _, src := range dream.DefaultSources(home) {
		if src.Glob == "" {
			continue
		}
		if ok, _ := filepath.Match(src.Glob, path); ok {
			return src.Format, true
		}
	}
	return 0, false
}

// questionBlock reads this session's answers and returns the question to
// ask in it, as a block, or "".
func questionBlock(ctx context.Context, in hookInput, sit search.Situation, prompt string) string {
	if !reviewerOn() {
		return ""
	}
	home, _ := os.UserHomeDir()
	path := agentreview.Path(platform.StateDir(home))
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		return "" // no queue: nothing to read
	}
	q, err := agentreview.Load(path)
	if err != nil || len(q.Open()) == 0 {
		return ""
	}
	sess, ok := hookSession(ctx, in, home)
	if !ok {
		return ""
	}
	sess.Repos = append([]string{sit.Repo}, sit.RepoAliases...)
	sess.Cwd, sess.Prompt = in.Cwd, prompt
	src := agentreview.DefaultSources(home)
	var picked *agentreview.Item
	_ = agentreview.Locked(path, questionWait, func(q *agentreview.Queue) bool {
		now := time.Now()
		changed := agentreview.ReadAnswers(ctx, q, src, func(a agentreview.Ask) bool {
			return a.Harness == sess.Harness && a.Session == sess.Session
		}, now)
		if it := q.Pick(sess, now); it != nil {
			cp := *it
			picked = &cp
			changed = true
		}
		return changed
	})
	if picked == nil {
		return ""
	}
	return questionTitle + "\n" + picked.Text() + "\n"
}
