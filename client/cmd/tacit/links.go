package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/links"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runLinks implements `tacit links`: the curator's link-repair step on its own.
// `tacit curate` runs the same step after the derived and results steps.
//
//	tacit links             sync, repair the dead [[links]] of curated facts, commit and push as curator
//	tacit links --dry-run   read the clone as it is (no sync), print what it would do, change nothing
//
// No model is called. The contract is SPEC.md "Link repair".
func runLinks(args []string) int {
	fs := flag.NewFlagSet("links", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print what would be repaired, write nothing (no sync)")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "links: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && refuseWriter(context.Background(), s, "curator", "links") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep, err := linksStep(ctx, s, *dry, time.Now)
	printLinks(rep, *dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "links:", err)
		return 1
	}
	return 0
}

// curateLinks is the one call `tacit curate` makes. It returns the number of
// errors to add to the run's. A dry run of curate runs the dry run of the step:
// it reads the clone and prints.
func curateLinks(ctx context.Context, s *store.Store, dry bool) int {
	lctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	rep, err := linksStep(lctx, s, dry, time.Now)
	printLinks(rep, dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate: links:", err)
		return 1
	}
	return 0
}

// rulingLink is a dead link in an owner ruling, to put to the owner.
type rulingLink struct {
	Fix  *links.Fix
	Edit links.Edit
	Held bool // a LINK? line for this fact and link is already in a review file
}

// notDone is a repair that was planned and not made, and why.
type notDone struct{ Path, Why string }

type linksReport struct {
	Plan     *links.Plan
	Rulings  []rulingLink
	ToFix    []*links.Fix // the facts to repair this run: the plan minus what is skipped
	Adopted  []string     // fact files a killed run wrote exactly as this step would, committed now
	NotDone  []notDone    // skipped or dropped, with the reason
	Deferred bool         // the owner lines were not written: today's review file holds other uncommitted changes
	Unparsed int          // fact files that did not parse (lint reports them)

	// What reached the server (set after the push only).
	Repaired []*links.Fix // facts committed
	Raised   []rulingLink // owner lines committed
	Pushed   bool
}

// ruled counts the ruling links to raise and the ones already raised.
func (r *linksReport) ruled() (raise, held int) {
	for _, x := range r.Rulings {
		if x.Held {
			held++
		} else {
			raise++
		}
	}
	return
}

// linksTree is the clone as the step reads it.
type linksTree struct {
	planner  *links.Planner
	plan     *links.Plan
	rulings  []rulingLink
	unparsed int
	dirty    map[string]string // path -> why it is not touched: changes the step did not make, or not tracked
	adopt    []string          // dirty fact files whose bytes are exactly what the step would write
	ownDirt  map[string]bool   // review files holding only this step's lines, uncommitted
}

// readLinksTree reads the tree at s.Dir: the plan, the ruling links (marked
// held when a review file already has the line), and what in the clone is not
// the step's to touch.
func readLinksTree(ctx context.Context, s *store.Store) (*linksTree, error) {
	facts, perrs, err := fact.Walk(s.Dir)
	if err != nil {
		return nil, err
	}
	untracked := untrackedFacts(ctx, s)
	planner := links.NewPlanner(s.Dir, facts, isRuling, untracked)
	plan, err := planner.Plan()
	if err != nil {
		return nil, err
	}
	raised := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(s.Dir, "review", "*.md"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			for _, k := range links.Raised(string(b)) {
				raised[k] = true
			}
		}
	}
	t := &linksTree{planner: planner, plan: plan, unparsed: len(perrs), dirty: map[string]string{}}
	for _, fx := range plan.Rulings {
		for _, e := range fx.Edits {
			t.rulings = append(t.rulings, rulingLink{Fix: fx, Edit: e, Held: raised[links.Key(fx.Name, e.Target)]})
		}
	}
	t.readDirt(ctx, s, planner, untracked)
	return t, nil
}

// readDirt sorts what differs from HEAD, and what is not tracked, into the
// step's own (a fact file whose bytes are exactly what the step would write
// from its committed version: what a killed run leaves) and everyone else's.
// Rulings and derived/ facts are never the step's own, and a file the step did
// not write itself is not adopted because it differs only in its links.
func (t *linksTree) readDirt(ctx context.Context, s *store.Store, planner *links.Planner, untracked map[string]bool) {
	for p := range untracked {
		t.dirty[p] = "the clone does not track it (a fact the server does not have)"
	}
	if out, err := store.Git(ctx, s.Dir, "diff", "--name-only", "-z", "HEAD"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if rel == "" {
				continue
			}
			if isFactPath(rel) {
				head, herr := store.Git(ctx, s.Dir, "cat-file", "blob", "HEAD:"+rel)
				cur, cerr := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
				if herr == nil && cerr == nil {
					if want, ok := planner.Expected(rel, []byte(head)); ok && !bytes.Equal(want, []byte(head)) && bytes.Equal(want, cur) {
						t.adopt = append(t.adopt, rel)
						continue
					}
				}
			}
			t.dirty[rel] = "the clone holds changes in it that this step did not make"
		}
	}
	sort.Strings(t.adopt)
	t.ownDirt = ownReviewDirt(ctx, s)
}

// linksStep repairs the dead links of curated facts. A dry run only plans, on
// the clone as it is. A real run syncs, plans and writes under the store lock
// and pushes once as curator; a push that lost a race starts over on the
// fresh clone (the repair is a function of the files, so it is recomputed, not
// replayed), and on any failure the clone is put back as it was.
func linksStep(ctx context.Context, s *store.Store, dry bool, now func() time.Time) (*linksReport, error) {
	if dry {
		t, err := readLinksTree(ctx, s)
		if err != nil {
			return nil, err
		}
		rep := &linksReport{}
		rep.fill(t)
		return rep, nil
	}
	var rep *linksReport
	var err error
	for attempt := 0; ; attempt++ {
		rep, err = linksLocked(ctx, s, now)
		if errors.Is(err, store.ErrBehind) && attempt < 3 {
			continue
		}
		return rep, err
	}
}

// fill sets what the step would do with the tree t: the facts to repair, the
// ones it leaves alone, and the owner lines.
func (r *linksReport) fill(t *linksTree) {
	r.Plan, r.Rulings, r.Unparsed = t.plan, t.rulings, t.unparsed
	r.ToFix, r.Adopted, r.NotDone = nil, t.adopt, nil
	for _, fx := range t.plan.Fixes {
		if why, skip := t.dirty[fx.Path]; skip {
			r.NotDone = append(r.NotDone, notDone{fx.Path, why})
			continue
		}
		r.ToFix = append(r.ToFix, fx)
	}
	for _, fx := range t.plan.Uncommitted {
		r.NotDone = append(r.NotDone, notDone{fx.Path, t.dirty[fx.Path]})
	}
	sort.Slice(r.NotDone, func(i, j int) bool { return r.NotDone[i].Path < r.NotDone[j].Path })
}

func linksLocked(ctx context.Context, s *store.Store, now func() time.Time) (*linksReport, error) {
	rep := &linksReport{}
	err := s.WithLock(ctx, func() error {
		if err := s.SyncLocked(ctx); err != nil {
			var stranded *store.Stranded
			if !errors.As(err, &stranded) { // the clone follows the server now; go on
				return fmt.Errorf("sync: %w", err)
			}
		}
		t, err := readLinksTree(ctx, s)
		if err != nil {
			return err
		}
		rep.fill(t)

		// What is written, in this order: the repaired facts, what a killed run
		// left, the owner lines, MEMORY.md.
		fixes := append([]*links.Fix(nil), rep.ToFix...)
		inFixes := map[string]bool{}
		for _, fx := range fixes {
			inFixes[fx.Path] = true
		}
		var adopted []string
		for _, p := range rep.Adopted {
			if !inFixes[p] {
				adopted = append(adopted, p)
			}
		}
		var lines []string
		var raising []rulingLink
		for _, r := range rep.Rulings {
			if !r.Held {
				lines = append(lines, links.Line(r.Fix, r.Edit))
				raising = append(raising, r)
			}
		}
		reviewRel := "review/" + now().Format("2006-01-02") + ".md"
		if len(lines) > 0 {
			if (t.dirty[reviewRel] != "" || untrackedPath(ctx, s, reviewRel)) && !t.ownDirt[reviewRel] {
				lines, raising, rep.Deferred = nil, nil, true // someone else's unwritten changes are in today's file; the lines are raised on a later run
			}
		}
		// final is what goes in the commit: it shrinks when the gate drops a fact.
		var indexRels []string // the folder indexes regenIndexes wrote
		final := func() []string {
			var ps []string
			for _, fx := range fixes {
				ps = append(ps, fx.Path)
			}
			ps = append(ps, adopted...)
			if len(lines) > 0 {
				ps = append(ps, reviewRel)
			}
			for rel := range t.ownDirt {
				ps = append(ps, rel)
			}
			ps = dedupe(append(append(ps, memoryPaths(ctx, s.Dir)...), indexRels...))
			sort.Strings(ps)
			return ps
		}
		written := final()

		// Whatever happens next, a file goes back to the bytes it had when this
		// run began: for an adopted file or a hand-edited one that is not HEAD.
		before := takeSnapshot(s.Dir, written)
		fail := func(err error) error {
			before.restore(s)
			return err
		}
		base, err := lintRun(s.Dir)
		if err != nil {
			return fmt.Errorf("lint could not run, nothing pushed: %w", err)
		}
		known := map[string]bool{}
		for _, f := range base {
			known[f.String()] = true
		}
		for _, fx := range fixes {
			if err := writeFileAtomic(s.Dir, fx.Path, fx.Data); err != nil {
				return fail(err)
			}
		}
		if len(lines) > 0 {
			if err := appendReview(filepath.Join(s.Dir, filepath.FromSlash(reviewRel)), now(), lines); err != nil {
				return fail(err)
			}
		}
		// The gate: a finding the write did not cause is not its business (a fact
		// with its own L7 must still get its link repaired), a new one on a file it
		// wrote drops that file, and a new one anywhere else stops the run.
		for round := 0; ; round++ {
			all, err := indexFacts(ctx, s, t)
			if err != nil {
				return fail(err)
			}
			if indexRels, err = regenIndexes(ctx, s.Dir, all, before); err != nil {
				return fail(err)
			}
			findings, err := lintRun(s.Dir) // a linter that cannot run is a failed gate
			if err != nil {
				return fail(fmt.Errorf("lint could not run, nothing pushed: %w", err))
			}
			var bad []lint.Finding
			for _, f := range findings {
				if !known[f.String()] && !indexFinding(f) {
					bad = append(bad, f)
				}
			}
			if len(bad) == 0 {
				break
			}
			drop := map[string]lint.Finding{}
			for _, f := range bad {
				if !inFixes[f.Path] || round > 0 {
					return fail(fmt.Errorf("lint rejects the repair, nothing pushed: %s", f))
				}
				drop[f.Path] = f
			}
			var keep []*links.Fix
			for _, fx := range fixes {
				if f, d := drop[fx.Path]; d {
					before.restorePath(s, fx.Path)
					delete(inFixes, fx.Path)
					rep.NotDone = append(rep.NotDone, notDone{fx.Path, "lint rejects the repair: " + f.String()})
					continue
				}
				keep = append(keep, fx)
			}
			fixes = keep
		}
		if len(fixes) == 0 && len(adopted) == 0 && len(lines) == 0 && len(t.ownDirt) == 0 {
			before.restore(s) // everything was dropped: MEMORY.md back as it was
			return nil
		}
		written = final()

		head, _ := s.Head(ctx)
		perr := s.CommitPushNamedOnceLocked(ctx, written, linksMessage(fixes, len(lines)), "curator")
		var rej *store.Rejected
		switch {
		case perr == nil:
			rep.Repaired, rep.Raised, rep.Pushed = fixes, raising, true
			rep.Adopted = adopted
			return nil
		case errors.As(perr, &rej):
			search.Invalidate(head) // the store put the files back to HEAD
			return fail(fmt.Errorf("%s", refusal(rej)))
		case errors.Is(perr, store.ErrBehind):
			before.restore(s) // the store undid the commit; the caller syncs and starts over
			return perr
		default:
			undoKept(s, head, written)
			return fail(perr)
		}
	})
	if err != nil {
		os.Remove(filepath.Join(s.Dir, "review")) // only when empty: a failed run does not leave the directory it made
	}
	return rep, err
}

func linksMessage(fixes []*links.Fix, raised int) string {
	var re, un int
	for _, fx := range fixes {
		for _, e := range fx.Edits {
			if e.To != "" {
				re += e.Count
			} else {
				un += e.Count
			}
		}
	}
	var parts []string
	if re+un > 0 {
		parts = append(parts, fmt.Sprintf("%d link(s) in %d fact(s) repaired (%d rewritten, %d unwrapped)", re+un, len(fixes), re, un))
	}
	if raised > 0 {
		parts = append(parts, fmt.Sprintf("%d ruling link(s) put to the owner", raised))
	}
	if len(parts) == 0 {
		parts = append(parts, "committing what an earlier run left")
	}
	return links.CommitPrefix + " " + strings.Join(parts, ", ")
}

// indexFacts are the facts MEMORY.md is generated from: the tree as the commit
// will hold it. A fact the clone changed without this step, or has staged, is
// read as the committed tree has it, and one the committed tree lacks (not
// tracked, or only staged) is left out: the index on the server must not name a
// fact the server does not have.
func indexFacts(ctx context.Context, s *store.Store, t *linksTree) ([]*fact.Fact, error) {
	all, _, err := fact.Walk(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []*fact.Fact
	for _, f := range all {
		if _, foreign := t.dirty[f.Path]; !foreign {
			out = append(out, f)
			continue
		}
		blob, err := store.Git(ctx, s.Dir, "cat-file", "blob", "HEAD:"+f.Path)
		if err != nil {
			continue
		}
		if h, err := fact.Parse(f.Path, []byte(blob)); err == nil {
			out = append(out, h)
		}
	}
	return out, nil
}

// snapshot is the bytes of some files when the run began; a nil entry is a file
// that did not exist.
type snapshot map[string][]byte

func takeSnapshot(dir string, rels []string) snapshot {
	sn := snapshot{}
	for _, rel := range rels {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			sn[rel] = b
		} else {
			sn[rel] = nil
		}
	}
	return sn
}

// restorePath puts one file back to the bytes it had at the start of the run.
func (sn snapshot) restorePath(s *store.Store, rel string) {
	full := filepath.Join(s.Dir, filepath.FromSlash(rel))
	if b := sn[rel]; b != nil {
		os.WriteFile(full, b, 0o644)
	} else {
		os.Remove(full)
	}
}

// restore puts every file back and forgets the search index built from the
// dirty tree.
func (sn snapshot) restore(s *store.Store) {
	for rel := range sn {
		sn.restorePath(s, rel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if head, err := s.Head(ctx); err == nil {
		search.Invalidate(head)
	}
}

// writeFileAtomic replaces a file whole or not at all (a rename from a temp
// file inside .git), so a killed run leaves a file that is either as it was or
// as the step meant it.
func writeFileAtomic(dir, rel string, data []byte) error {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	mode := os.FileMode(0o644)
	if st, err := os.Stat(full); err == nil {
		mode = st.Mode().Perm()
	}
	tmpDir := filepath.Join(dir, ".git", "tacit-links-tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err // a new folder's first file (a folder record)
	}
	f, err := os.CreateTemp(tmpDir, "w-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, full)
	}
	if werr != nil {
		os.Remove(name)
	}
	return werr
}

func appendReview(full string, at time.Time, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	prefix := ""
	if b, err := os.ReadFile(full); err == nil && len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		prefix = "\n"
	}
	fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(fh, "%s## link check %s, %d dead link(s) in owner rulings\n\n%s\n\n", prefix, at.Format(time.RFC3339), len(lines), strings.Join(lines, "\n"))
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// isFactPath is a curated fact file: a Markdown file outside inbox/, review/,
// keys/, the two root files and the store files (folder indexes, records,
// overviews).
func isFactPath(rel string) bool {
	if !strings.HasSuffix(rel, ".md") || rel == "MEMORY.md" || rel == "README.md" || fact.IsStoreFile(path.Base(rel)) {
		return false
	}
	for _, d := range []string{"inbox/", "review/", "keys/"} {
		if strings.HasPrefix(rel, d) {
			return false
		}
	}
	return !strings.Contains(rel, "/.")
}

// untrackedFacts lists the curated fact files in the clone that the clone does
// not track: a curate batch killed before its push, or a fact written by hand.
// The step does not plan them, write them, commit them or remove them.
func untrackedFacts(ctx context.Context, s *store.Store) map[string]bool {
	out := map[string]bool{}
	if list, err := store.Git(ctx, s.Dir, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
		for _, rel := range strings.Split(list, "\x00") {
			if rel != "" && isFactPath(rel) {
				out[rel] = true
			}
		}
	}
	return out
}

// untrackedPath reports whether rel exists in the clone and is not tracked.
func untrackedPath(ctx context.Context, s *store.Store, rel string) bool {
	out, err := store.Git(ctx, s.Dir, "ls-files", "--others", "--exclude-standard", "--", rel)
	return err == nil && strings.TrimSpace(out) != ""
}

var reviewDirtLine = regexp.MustCompile(`^\+(- LINK\? |## link check |$)`)

// ownReviewDirt returns the review files whose uncommitted change is only
// LINK? blocks appended by this step (what a killed run leaves), so they are
// committed with the next run. Any other change is not ours and stays where it
// is.
func ownReviewDirt(ctx context.Context, s *store.Store) map[string]bool {
	files := map[string]bool{}
	// A review file the killed run created: untracked, all of it ours or none.
	if out, err := store.Git(ctx, s.Dir, "ls-files", "--others", "--exclude-standard", "-z", "--", "review"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if !strings.HasSuffix(rel, ".md") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			ours := len(bytes.TrimSpace(b)) > 0
			for _, l := range strings.Split(string(b), "\n") {
				if !reviewDirtLine.MatchString("+" + l) {
					ours = false
				}
			}
			if ours {
				files[rel] = true
			}
		}
	}
	out, err := store.Git(ctx, s.Dir, "diff", "-U0", "--no-color", "HEAD", "--", "review")
	if err != nil || strings.TrimSpace(out) == "" {
		return files
	}
	cur, ours := "", false
	flush := func() {
		if cur != "" && ours {
			files[cur] = true
		}
	}
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			cur, ours = "", true
		case strings.HasPrefix(l, "+++ b/"):
			cur = strings.TrimPrefix(l, "+++ b/")
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "@@"), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"), l == "":
		case strings.HasPrefix(l, "+"):
			if !reviewDirtLine.MatchString(l) {
				ours = false
			}
		default:
			ours = false // a removed line or any other change
		}
	}
	flush()
	return files
}

func printLinks(rep *linksReport, dry bool) { writeLinks(os.Stdout, os.Stderr, rep, dry) }

// writeLinks reports what the step did, or in a dry run what it would do. A
// real run reports in the past tense only what reached the server: what was
// skipped, dropped or lost to a failure is "not done".
func writeLinks(out, errw io.Writer, rep *linksReport, dry bool) {
	if rep == nil || rep.Plan == nil {
		return
	}
	fixes, raised := rep.Repaired, rep.Raised
	if dry {
		fixes, raised = rep.ToFix, nil
		for _, r := range rep.Rulings {
			if !r.Held {
				raised = append(raised, r)
			}
		}
	}
	say := func(past, future string) string {
		if dry {
			return future
		}
		return past
	}
	for _, fx := range fixes {
		for _, e := range fx.Edits {
			if e.To != "" {
				fmt.Fprintf(out, "links: %s [[%s]] to [[%s]] in %s (%s)\n", say("rewrote", "would rewrite"), e.Target, e.To, fx.Path, e.Detail)
			} else {
				fmt.Fprintf(out, "links: %s [[%s]] to plain text in %s (%s)\n", say("unwrapped", "would unwrap"), e.Target, fx.Path, e.Detail)
			}
		}
	}
	isRaised := map[string]bool{}
	for _, r := range raised {
		isRaised[links.Key(r.Fix.Name, r.Edit.Target)] = true
	}
	for _, r := range rep.Rulings {
		write := "plain text"
		if r.Edit.To != "" {
			write = "[[" + r.Edit.To + "]]"
		}
		switch {
		case r.Held:
			fmt.Fprintf(out, "links: [[%s]] in %s: an owner ruling, not edited; already put to the owner\n", r.Edit.Target, r.Fix.Path)
		case isRaised[links.Key(r.Fix.Name, r.Edit.Target)]:
			fmt.Fprintf(out, "links: %s [[%s]] in %s: an owner ruling, not edited; the repair would be %s (%s)\n", say("put to the owner", "would put to the owner"), r.Edit.Target, r.Fix.Path, write, r.Edit.Detail)
		default:
			fmt.Fprintf(errw, "links: not done: [[%s]] in %s is an owner ruling and was not put to the owner this run\n", r.Edit.Target, r.Fix.Path)
		}
	}
	for _, fx := range rep.Plan.Generated {
		for _, e := range fx.Edits {
			fmt.Fprintf(errw, "links: warning: generated fact %s has a dead link [[%s]]; it is not edited here (%s)\n", fx.Path, e.Target, e.Detail)
		}
	}
	for _, n := range rep.NotDone {
		verb := "not done"
		if dry {
			verb = "would not do"
		}
		fmt.Fprintf(errw, "links: %s: %s: %s\n", verb, n.Path, n.Why)
	}
	if rep.Pushed || dry {
		for _, p := range rep.Adopted {
			fmt.Fprintf(out, "links: %s %s, written by an earlier run that did not commit it\n", say("committed", "would commit"), p)
		}
	}
	// The totals count what was committed (what would be, in a dry run).
	var re, un int
	for _, fx := range fixes {
		for _, e := range fx.Edits {
			if e.To != "" {
				re += e.Count
			} else {
				un += e.Count
			}
		}
	}
	held := 0
	for _, r := range rep.Rulings {
		if r.Held {
			held++
		}
	}
	planned := 0
	for _, fx := range rep.Plan.Fixes {
		for _, e := range fx.Edits {
			planned += e.Count
		}
	}
	notDone := ""
	if n := planned - re - un; n > 0 {
		notDone = fmt.Sprintf("; %d %s", n, say("not done", "would not be done"))
	}
	fmt.Fprintf(out, "links: %d link(s) in %d fact(s) %s (%d rewritten, %d unwrapped); %d ruling link(s) %s, %d already raised%s; %d generated fact(s) left to their generator\n",
		re+un, len(fixes), say("repaired", "would repair"), re, un, len(raised), say("put to the owner", "to put to the owner"), held, notDone, len(rep.Plan.Generated))
}
