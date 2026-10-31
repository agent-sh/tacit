// Package stale decides which curated facts git has overtaken. A fact is
// flagged when, after the day it was last current, something it names was
// deleted: a file it names is gone from HEAD, or a commit removed the lines
// holding an identifier (a name, a flag, an environment variable) it names and
// no file of the repo holds that identifier any more. That is all it does: it
// flags, and the verifier or the owner decides. It reads git only through
// gitev and calls no model.
//
// Why deletions and not changes. A fact about a hot file is touched by every
// commit that edits the file, and a name that survives is rewritten all the
// time: the same word reworded in a doc, a refactor of the line around it, a
// value on the same line. Hand-checked on a sample from a real store
// (eval/VALUE.md), no flag raised by a changed line holding a name that HEAD
// still holds was a stale fact, and every flag raised by a deleted file was.
// So a changed line does not flag; a deleted name or file does.
// A term found in 26 or more files at HEAD is never a term (gitev drops it),
// and only specific identifiers are searched (gitev.Specific).
//
// Why a finding is not repeated. The caller passes what the store already
// says about the fact: an unanswered finding means nothing new is flagged, a
// finding settled "still true" at an instant moves the fact's date to that
// instant, and the same newest commit never makes a second finding. A deletion
// after the mark is a new event and flags again.
//
// Why a read that did not finish flags nothing and is not recorded as checked:
// a failed file list makes every named file look deleted, and a cut search can
// miss the commit that did it. Such a fact is skipped as incomplete and tried
// again later.
package stale

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
)

// Skip says why a fact was not flagged.
type Skip string

const (
	Flagged        Skip = ""
	SkipNoRepo     Skip = "no-repo"    // the fact names no checkout on this machine
	SkipNoSubject  Skip = "no-subject" // it names a repo but nothing git can be asked about
	SkipNoDate     Skip = "no-date"    // it carries no usable date
	SkipQuiet      Skip = "quiet"      // nothing it names changed after its date
	SkipSame       Skip = "same"       // already flagged for this commit
	SkipIncomplete Skip = "incomplete" // a read failed, ran out of time or was cut; nothing is flagged and the fact is tried again later
	SkipError      Skip = "error"      // the read failed outright
	SkipUnreached  Skip = "unreached"  // the run's time ran out before this fact
)

// Input is one fact and what the store already says about it.
type Input struct {
	Fact   *fact.Fact
	Origin *fact.Fact // the inbox note it was split from, if any
	Ruling bool

	// Verified is the instant an earlier finding on this fact was settled
	// "still true", or its fix applied; zero if never.
	Verified time.Time
	// LastSha is the newest commit of the last finding on this fact ("" if none).
	LastSha string
}

// Trigger is a commit that made a fact suspect.
type Trigger struct {
	Repo    string    `json:"repo"`
	Short   string    `json:"sha"`
	At      time.Time `json:"at"` // committer date: when it reached the repo
	Subject string    `json:"subject"`
	// Terms are the names the fact has that the commit deleted, each marked
	// "(gone at HEAD)": identifiers it removed, or a file it deleted or renamed.
	Terms []string `json:"terms"`
	By    string   `json:"by,omitempty"`
	// Files is how many files the commit touched.
	Files int `json:"files,omitempty"`
}

// Finding is a fact git has overtaken, with what overtook it.
type Finding struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Ruling bool   `json:"ruling,omitempty"`

	// Since is the instant commits started to count; SinceWhy where it came
	// from (text, source, created, updated, store, verified).
	Since    time.Time `json:"since"`
	SinceWhy string    `json:"since_why"`
	Repos    []string  `json:"repos"`
	// N counts the deleting events: commits after Since that removed a name HEAD
	// no longer holds, plus the files the fact names that are gone.
	N        int       `json:"n"`
	Triggers []Trigger `json:"triggers"` // newest first, at most 3
	// Sha is the newest trigger's short sha: the finding's key.
	Sha string `json:"sha"`
	// Terms is every distinct name the triggers deleted.
	Terms []string `json:"terms"`
}

// Result is the outcome for one input.
type Result struct {
	Name    string
	Finding *Finding
	Skip    Skip
	Err     error
}

// Check decides one fact.
func Check(ctx context.Context, b *gitev.Builder, in Input) Result {
	f := in.Fact
	res := Result{Name: f.Meta.Name}
	ref := gitev.FactRef{Fact: f, Origin: in.Origin}
	eff := b.Effective(ctx, ref)
	if eff.At.IsZero() {
		res.Skip = SkipNoDate
		return res
	}
	// A date is a day; the day itself is already known, so commits count from
	// its end. A settled "still true" is an instant.
	since, why := eff.At.Add(24*time.Hour), eff.Why
	if in.Verified.After(since) {
		since, why = in.Verified, "verified"
	}
	sub, evs, err := b.ReadSince(ctx, ref, since)
	if err != nil {
		res.Skip, res.Err = SkipError, err
		return res
	}
	if len(sub.Repos) == 0 {
		res.Skip = SkipNoRepo
		return res
	}
	if sub.Empty() {
		res.Skip = SkipNoSubject
		return res
	}
	// A repo whose read did not finish says nothing, and nothing found in the
	// others is "quiet" only if every read finished.
	for _, ev := range evs {
		if ev.Incomplete {
			res.Skip = SkipIncomplete
			return res
		}
	}
	// A name is gone only when no repo read holds it at HEAD, identifier or path.
	present := map[string]bool{}
	for _, ev := range evs {
		for _, st := range ev.State {
			if st.Present || st.Unknown {
				present[st.Kind+":"+st.Term] = true
			}
		}
	}
	fd := &Finding{Name: f.Meta.Name, Scope: f.Meta.Scope, Path: f.Path, Ruling: in.Ruling, Since: since, SinceWhy: why}
	terms := map[string]bool{}
	for _, ev := range evs {
		hit := false
		for _, c := range ev.Changed {
			var gone []string
			for _, t := range c.Dropped {
				if !present["ident:"+t] {
					gone = append(gone, t+" (gone at HEAD)")
					terms[t] = true
				}
			}
			if len(gone) == 0 {
				continue
			}
			hit = true
			fd.N++
			fd.Triggers = append(fd.Triggers, Trigger{Repo: ev.Name, Short: c.Short, At: c.Committed, Subject: c.Subject, Terms: gone, By: c.Author.Name, Files: c.Touched})
		}
		for _, g := range ev.Deleted {
			if present["path:"+g.Term] {
				continue
			}
			hit = true
			fd.N++
			fd.Triggers = append(fd.Triggers, Trigger{Repo: ev.Name, Short: g.Commit.Short, At: g.Commit.Committed, Subject: g.Commit.Subject, Terms: []string{g.Term + " (gone at HEAD)"}, By: g.Commit.Author.Name})
			terms[g.Term] = true
		}
		if hit {
			fd.Repos = append(fd.Repos, ev.Name)
		}
	}
	if fd.N == 0 {
		res.Skip = SkipQuiet
		return res
	}
	sort.SliceStable(fd.Triggers, func(i, j int) bool { return fd.Triggers[i].At.After(fd.Triggers[j].At) })
	fd.Sha = fd.Triggers[0].Short
	if len(fd.Triggers) > 3 {
		fd.Triggers = fd.Triggers[:3]
	}
	for t := range terms {
		fd.Terms = append(fd.Terms, t)
	}
	sort.Strings(fd.Terms)
	if in.LastSha != "" && in.LastSha == fd.Sha {
		res.Skip = SkipSame
		return res
	}
	// What leaves this package came from git: masked before it is clipped or
	// laid out (a secret cut in half is no longer recognised), and no finding
	// if the redactor fails.
	if err := redactFinding(ctx, b, fd); err != nil {
		res.Skip, res.Err = SkipError, err
		return res
	}
	for i := range fd.Triggers {
		fd.Triggers[i].Subject = clip(fd.Triggers[i].Subject, 90)
	}
	res.Finding = fd
	return res
}

// redactFinding masks every text of a finding that came from git, in one
// call; if the redactor fails there is no finding.
func redactFinding(ctx context.Context, b *gitev.Builder, fd *Finding) error {
	var ptrs []*string
	for i := range fd.Repos {
		ptrs = append(ptrs, &fd.Repos[i])
	}
	for i := range fd.Terms {
		ptrs = append(ptrs, &fd.Terms[i])
	}
	for i := range fd.Triggers {
		t := &fd.Triggers[i]
		ptrs = append(ptrs, &t.Repo, &t.Subject, &t.By)
		t.Terms = append([]string(nil), t.Terms...)
		for j := range t.Terms {
			ptrs = append(ptrs, &t.Terms[j])
		}
	}
	texts := make([]string, len(ptrs))
	for i, p := range ptrs {
		texts[i] = *p
	}
	out, err := b.RedactAll(ctx, texts)
	if err != nil {
		return err
	}
	for i, p := range ptrs {
		*p = out[i]
	}
	return nil
}

// Run checks the inputs with a few at a time and returns the results in the
// order given. A cancelled context stops starting new ones; the rest come
// back as SkipUnreached.
func Run(ctx context.Context, b *gitev.Builder, ins []Input, workers int) []Result {
	if workers < 1 {
		workers = 1
	}
	out := make([]Result, len(ins))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := range ins {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				out[i] = Result{Name: ins[i].Fact.Meta.Name, Skip: SkipUnreached}
				return
			}
			out[i] = Check(ctx, b, ins[i])
		}()
	}
	wg.Wait()
	return out
}

// Order puts the strongest findings first: the most changed lines, then the
// newest change, then the name, so a run that holds some back is repeatable.
func Order(fs []*Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if !a.Triggers[0].At.Equal(b.Triggers[0].At) {
			return a.Triggers[0].At.After(b.Triggers[0].At)
		}
		return a.Name < b.Name
	})
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Block is the finding as the lines the review files carry: the first line is
// what the page reads back (the fact, the newest commit, how many, since
// when), the indented lines say why for a person reading the file. The page
// and the verifier read git themselves for the rest.
func (f *Finding) Block() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- stale? [[%s]] (%s) @%s n=%d since=%s\n", f.Name, f.Scope, f.Sha, f.N, f.Since.Format("2006-01-02"))
	fmt.Fprintf(&b, "  commits count from %s (%s); %d deleted something the fact names\n", f.Since.Format("2006-01-02"), f.SinceWhy, f.N)
	for _, t := range f.Triggers {
		fmt.Fprintf(&b, "  %s %s %s: %s - %s\n", t.At.Format("2006-01-02"), t.Short, t.Repo, oneLine(t.Subject), oneLine(strings.Join(t.Terms, ", ")))
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneLine collapses whitespace and control characters: a commit subject is
// text anyone can write, and it must not start a new line of the file.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}
