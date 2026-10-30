package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/links"
	"github.com/agent-sh/tacit/client/internal/lint"
)

var linksNow = func() time.Time { return time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC) }

// linkFixture is a store with a real remote whose curated facts carry every
// kind of dead link: a superseded target, a name that only a different
// spelling matches, an ambiguous one, one that matches nothing, a repo name,
// and (withRulings) owner rulings, plus an inbox note and a generated fact.
func linkFixture(t *testing.T, withRulings bool) *rig {
	t.Helper()
	r := newRig(t, rigOpts{})
	w := func(scope, name string, m fact.Meta, body string) {
		m.Name, m.Scope = name, scope
		if m.Description == "" {
			m.Description = "fact " + name
		}
		if m.Type == "" {
			m.Type = "design_decision"
		}
		writeFact(t, r.clone, m, body)
	}
	w("projects/x", "new-engine", fact.Meta{}, "The engine replaced [[old-engine]]. See [[old-engine|the old note]].")
	w("projects/x", "old-engine", fact.Meta{SupersededBy: "new-engine"}, "Old engine notes. Related: [[missing-thing]].")
	w("projects/x", "uses-old", fact.Meta{}, "Read [[old-engine]] first.\nAlso [[old-engine|the engine note]] and [[old-engine#section]].")
	w("projects/x", "gpu-notes", fact.Meta{}, "About the GPU.")
	w("projects/x", "uses-gpu", fact.Meta{}, "Check [[Gpu_Notes]] and [[gpu-notes.md]] and [[GPU-NOTES]], and the live [[gpu-notes]].")
	w("projects/x", "dup-name", fact.Meta{}, "One of two.")
	w("projects/y", "dup-name", fact.Meta{}, "Two of two.")
	w("projects/x", "uses-dup", fact.Meta{}, "See [[Dup_Name]] here.")
	w("projects/x", "no-match", fact.Meta{}, "Copy the TOML [[keys]] blocks; see [[nothing-here|the label]] and [[nothing-here]].\n\n**Why** it matters.")
	w("projects/x", "toml-note", fact.Meta{}, "Copy the `[[keys]]` blocks:\n\n```toml\n[[keys]]\nprefix = \"x\"\n```\n\nThen see [[toml-gone]].")
	w("projects/some-repo", "some-repo-note", fact.Meta{}, "A note about the repo.")
	w("projects/x", "mentions-repo", fact.Meta{}, "Built in [[some-repo]].")
	if withRulings {
		w("projects/x", "ruling-a", fact.Meta{Source: ownerSource("keep the gpu notes short")}, "Owner said \"keep the gpu notes short\"; see [[gone-a]].")
		w("projects/x", "ruling-b", fact.Meta{Type: "user_standard", Source: endorsedSource("ruling-b")}, "A standard: see [[gone-b]] and [[gone-b]] again.")
		// An agent's reading of their preference is not their ruling: its link is repaired like any fact's.
		w("projects/x", "agents-standard", fact.Meta{Type: "user_standard"}, "Agents think the owner wants this; see [[gone-agents]].")
		w("derived/demo", "demo-note", fact.Meta{Type: "structure", DerivedFrom: "abc1234", Source: fact.Source{Kind: "derived"}}, "Generated; see [[gone-c]].")
	}
	inbox := &fact.Fact{Path: "inbox/claude@host-a/an-inbox-note.md", Meta: fact.Meta{Name: "an-inbox-note", Description: "d", Type: "unclassified", Scope: "projects/x",
		Source: fact.Source{Harness: "claude", Host: "host-a", Kind: "observed"}, Created: "2026-10-08"}, Body: "A raw note pointing at [[nowhere]].\n"}
	b, err := inbox.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(r.clone, "inbox", "claude@host-a"), 0o755)
	os.WriteFile(filepath.Join(r.clone, inbox.Path), b, 0o644)
	r.commitIndex(r.clone, "links fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	return r
}

// onServer is a file as the server holds it, byte for byte.
func (r *rig) onServer(rel string) string {
	r.t.Helper()
	out, err := exec.Command("git", "-C", r.bare, "show", "main:"+rel).Output()
	if err != nil {
		r.t.Fatalf("server has no %s: %v", rel, err)
	}
	return string(out)
}

func (r *rig) serverLint() []lint.Finding {
	r.t.Helper()
	fresh := filepath.Join(r.t.TempDir(), "fresh")
	gitIn(r.t, r.root, "clone", "-q", r.bare, fresh)
	fs, err := lint.Run(fresh)
	if err != nil {
		r.t.Fatal(err)
	}
	return fs
}

// reviewMDs reports a review file (review/*.md) in dir; the audit's records
// (the quote ledger, decisions, the marker) are not review files.
func reviewMDs(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, "review", "*.md"))
	return len(m) > 0
}

func (r *rig) links(dry bool) (*linksReport, error) {
	return linksStep(ctxMinute(r.t), r.s, dry, linksNow)
}

func lintRules(fs []lint.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule+" "+f.Path)
	}
	return strings.Join(out, "\n")
}

// What each kind of dead link becomes, what is left alone, the body
// otherwise byte for byte, and one commit that lints clean.
func TestLinksRewriteUnwrapAndLeaveRulingsInboxAndGenerated(t *testing.T) {
	r := linkFixture(t, true)
	before := map[string]string{}
	for _, rel := range []string{"projects/x/new-engine.md", "projects/x/old-engine.md", "projects/x/uses-old.md", "projects/x/uses-gpu.md", "projects/x/uses-dup.md", "projects/x/no-match.md", "projects/x/mentions-repo.md", "projects/x/toml-note.md",
		"projects/x/ruling-a.md", "projects/x/ruling-b.md", "derived/demo/demo-note.md", "inbox/claude@host-a/an-inbox-note.md", "projects/x/gpu-notes.md", "projects/x/agents-standard.md"} {
		before[rel] = r.onServer(rel)
	}
	// Before: lint finds the dead links (and only those) of curated facts.
	if got := lintRules(r.serverLint()); strings.Count(got, "L6 ") != 13 {
		t.Fatalf("lint before:\n%s", got)
	}
	commits := r.commitCount()

	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Pushed || r.commitCount() != commits+1 {
		t.Fatalf("pushed=%v, %d new commits, want one", rep.Pushed, r.commitCount()-commits)
	}
	if subj := gitIn(t, r.bare, "log", "-1", "--format=%s", "main"); !strings.HasPrefix(subj, links.CommitPrefix) {
		t.Fatalf("commit subject %q", subj)
	}
	if who := gitIn(t, r.bare, "log", "-1", "--format=%an", "main"); who != "curator" {
		t.Fatalf("committed as %q", who)
	}

	body := func(rel string) string {
		_, b, _ := strings.Cut(strings.TrimPrefix(r.onServer(rel), "---\n"), "\n---\n\n")
		return b
	}
	for rel, want := range map[string]string{
		// the live successor; a label stays, a section does not (the old fact's section)
		"projects/x/uses-old.md": "Read [[new-engine]] first.\nAlso [[new-engine|the engine note]] and [[new-engine]].\n",
		// a superseded fact is repaired too
		"projects/x/old-engine.md": "Old engine notes. Related: missing-thing.\n",
		// one live fact answers to the spelling
		"projects/x/uses-gpu.md": "Check [[gpu-notes]] and [[gpu-notes]] and [[gpu-notes]], and the live [[gpu-notes]].\n",
		// two live facts answer: no rewrite, plain text
		"projects/x/uses-dup.md": "See Dup_Name here.\n",
		// no match: plain text, the label wins
		"projects/x/no-match.md": "Copy the TOML keys blocks; see the label and nothing-here.\n\n**Why** it matters.\n",
		// TOML table headers in a fence and in an inline span are code: untouched; the real link beside them is not
		"projects/x/toml-note.md": "Copy the `[[keys]]` blocks:\n\n```toml\n[[keys]]\nprefix = \"x\"\n```\n\nThen see toml-gone.\n",
		// a repo name is not a fact
		"projects/x/mentions-repo.md": "Built in some-repo.\n",
		// an agent's reading of their preference is not their ruling: repaired like any fact
		"projects/x/agents-standard.md": "Agents think the owner wants this; see gone-agents.\n",
	} {
		if got := body(rel); got != want {
			t.Errorf("%s:\n got %q\nwant %q", rel, got, want)
		}
		if !links.IsRepair([]byte(before[rel]), []byte(r.onServer(rel))) {
			t.Errorf("%s: more than its links changed", rel)
		}
	}
	// Not edited: rulings, the inbox note, the generated fact, a live target,
	// and a link to the fact this one replaced (history, not dead).
	for _, rel := range []string{"projects/x/ruling-a.md", "projects/x/ruling-b.md", "inbox/claude@host-a/an-inbox-note.md", "derived/demo/demo-note.md", "projects/x/gpu-notes.md", "projects/x/new-engine.md"} {
		if r.onServer(rel) != before[rel] {
			t.Errorf("%s was edited", rel)
		}
	}
	// One line per ruling and dead link, in the day's review file.
	review := r.onServer("review/2026-10-10.md")
	if n := strings.Count(review, "- LINK? "); n != 2 || !strings.Contains(review, "- LINK? [[ruling-a]] (projects/x) links to [[gone-a]]") || !strings.Contains(review, "- LINK? [[ruling-b]] (projects/x) links to [[gone-b]]") {
		t.Fatalf("review file:\n%s", review)
	}
	if strings.Contains(review, "\u2014") {
		t.Fatal("em dash in the review file")
	}
	// What lint still finds is what the step may not touch: the two rulings and
	// the generated fact. Everything else, the index included, is clean.
	left := lintRules(r.serverLint())
	if left != "L6 derived/demo/demo-note.md\nL6 projects/x/ruling-a.md\nL6 projects/x/ruling-b.md" {
		t.Fatalf("lint after:\n%s", left)
	}
	r.assertClean("after the repair")
}

// The same fixture without the facts the step may not edit: lint L6 is clean.
func TestLinksLeaveTheStoreLintClean(t *testing.T) {
	r := linkFixture(t, false)
	if got := lintRules(r.serverLint()); !strings.Contains(got, "L6 ") {
		t.Fatalf("the fixture has no dead links:\n%s", got)
	}
	if _, err := r.links(false); err != nil {
		t.Fatal(err)
	}
	if got := lintRules(r.serverLint()); got != "" {
		t.Fatalf("lint after:\n%s", got)
	}
}

// A second run, and a run on a store that holds nothing to repair, change
// nothing: no commit, no file, no new review line.
func TestLinksSecondRunChangesNothing(t *testing.T) {
	r := linkFixture(t, true)
	if _, err := r.links(false); err != nil {
		t.Fatal(err)
	}
	head, review := r.serverHead(), r.onServer("review/2026-10-10.md")
	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pushed || r.serverHead() != head || r.onServer("review/2026-10-10.md") != review {
		t.Fatalf("the second run changed the server (pushed=%v)", rep.Pushed)
	}
	if raise, held := rep.ruled(); raise != 0 || held != 2 || len(rep.Plan.Fixes) != 0 {
		t.Fatalf("second run: %d to raise, %d held, %d fixes", raise, held, len(rep.Plan.Fixes))
	}
	r.assertClean("after the second run")
	// The next day, still no repeat.
	next := func() time.Time { return linksNow().AddDate(0, 0, 1) }
	if rep, err := linksStep(ctxMinute(t), r.s, false, next); err != nil || rep.Pushed || r.serverHead() != head {
		t.Fatalf("the next day: pushed=%v err=%v", rep.Pushed, err)
	}
}

// A ruling gets one line per fact and link; a new dead link in the same
// ruling gets its own line and the first is not repeated.
func TestLinksRulingLineOncePerFactAndLink(t *testing.T) {
	r := linkFixture(t, true)
	if _, err := r.links(false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.clone, "projects/x/ruling-a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, []byte("\nAnd [[gone-a2]].\n")...), 0o644)
	gitIn(t, r.clone, "add", "-A")
	gitIn(t, r.clone, "commit", "-q", "-m", "the owner adds a link")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	if _, err := r.links(false); err != nil {
		t.Fatal(err)
	}
	review := r.onServer("review/2026-10-10.md")
	if strings.Count(review, "links to [[gone-a]]") != 1 || strings.Count(review, "links to [[gone-a2]]") != 1 || strings.Count(review, "- LINK? ") != 3 {
		t.Fatalf("review file:\n%s", review)
	}
}

// A ruling whose dead link has a clear match is still not edited; the line
// says what the curator would write.
func TestLinksRulingLineSaysWhatItWouldWrite(t *testing.T) {
	r := newRig(t, rigOpts{})
	writeFact(t, r.clone, fact.Meta{Name: "new-one", Description: "d", Type: "design_decision", Scope: "projects/x"}, "live")
	writeFact(t, r.clone, fact.Meta{Name: "old-one", Description: "d", Type: "design_decision", Scope: "projects/x", SupersededBy: "new-one"}, "old")
	writeFact(t, r.clone, fact.Meta{Name: "the-rule", Description: "d", Type: "design_decision", Scope: "projects/x", Source: endorsedSource("the-rule")}, "see [[old-one]]")
	r.commitIndex(r.clone, "fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	before := r.onServer("projects/x/the-rule.md")
	if _, err := r.links(false); err != nil {
		t.Fatal(err)
	}
	if r.onServer("projects/x/the-rule.md") != before {
		t.Fatal("the ruling was edited")
	}
	if review := r.onServer("review/2026-10-10.md"); !strings.Contains(review, "links to [[old-one]], and [[old-one]] is superseded by [[new-one]]") || !strings.Contains(review, "it would write [[new-one]]") {
		t.Fatalf("review file:\n%s", review)
	}
}

// A dry run reads the clone as it is: nothing is written, nothing synced.
func TestLinksDryRunTouchesNothing(t *testing.T) {
	r := linkFixture(t, true)
	// the server moves on; a dry run must not fetch it
	other := filepath.Join(r.root, "other")
	gitIn(t, r.root, "clone", "-q", r.bare, other)
	gitIn(t, other, "config", "user.email", "o@o")
	gitIn(t, other, "config", "user.name", "o")
	writeFact(t, other, fact.Meta{Name: "later", Description: "d", Type: "machine_fact", Scope: "machines/host-a"}, "x")
	gitIn(t, other, "add", "-A")
	gitIn(t, other, "commit", "-q", "-m", "later")
	gitIn(t, other, "push", "-q", "origin", "HEAD:main")
	head, server := gitIn(t, r.clone, "rev-parse", "HEAD"), r.serverHead()

	rep, err := r.links(true)
	if err != nil {
		t.Fatal(err)
	}
	re, un := rep.Plan.Links()
	if re != 6 || un != 8 || len(rep.Plan.Fixes) != 8 || len(rep.Rulings) != 2 || len(rep.Plan.Generated) != 1 {
		t.Fatalf("plan: %d rewritten, %d unwrapped, %d fixes, %d rulings, %d generated", re, un, len(rep.Plan.Fixes), len(rep.Rulings), len(rep.Plan.Generated))
	}
	if gitIn(t, r.clone, "rev-parse", "HEAD") != head || r.serverHead() != server {
		t.Fatal("a dry run moved a ref")
	}
	r.assertClean("after the dry run")
	if reviewMDs(r.clone) {
		t.Fatal("a dry run wrote a review file")
	}
	if code := curateLinks(context.Background(), r.s, true); code != 0 {
		t.Fatalf("curateLinks in a dry run: %d", code)
	}
	r.assertClean("after curate's dry run")
}

// A killed run that wrote its repair and its review lines but never committed
// them: the next run commits them (the files hold no dead link any more, so
// no plan names them), and no line is written twice.
func TestLinksCommitsWhatAKilledRunLeft(t *testing.T) {
	r := linkFixture(t, true)
	tree, err := readLinksTree(ctxMinute(t), r.s)
	if err != nil {
		t.Fatal(err)
	}
	plan, rulings := tree.plan, tree.rulings
	for _, fx := range plan.Fixes {
		if fx.Name == "uses-old" {
			os.WriteFile(filepath.Join(r.clone, fx.Path), fx.Data, 0o644)
		}
	}
	var lines []string
	for _, x := range rulings {
		lines = append(lines, links.Line(x.Fix, x.Edit))
	}
	if err := appendReview(filepath.Join(r.clone, "review", "2026-10-10.md"), linksNow(), lines); err != nil {
		t.Fatal(err)
	}
	if gitIn(t, r.clone, "status", "--porcelain") == "" {
		t.Fatal("the fixture left nothing behind")
	}

	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Pushed {
		t.Fatal("nothing was pushed")
	}
	if !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
		t.Fatal("the repair a killed run left was not committed")
	}
	review := r.onServer("review/2026-10-10.md")
	if strings.Count(review, "- LINK? ") != 2 {
		t.Fatalf("review file:\n%s", review)
	}
	r.assertClean("after adopting the leftovers")
}

// Uncommitted changes that are not link repairs are someone's work: the file
// is left alone and reported, the rest is repaired.
func TestLinksLeaveAFileWithOtherChangesAlone(t *testing.T) {
	r := linkFixture(t, false)
	p := filepath.Join(r.clone, "projects/x/uses-dup.md")
	b, _ := os.ReadFile(p)
	edited := append(b, []byte("A line someone is writing.\n")...)
	os.WriteFile(p, edited, 0o644)

	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.NotDone) != 1 || rep.NotDone[0].Path != "projects/x/uses-dup.md" {
		t.Fatalf("not done %v", rep.NotDone)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, edited) {
		t.Fatal("the file was touched")
	}
	if strings.Contains(r.onServer("projects/x/uses-dup.md"), "A line someone") || !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
		t.Fatal("the server holds the wrong files")
	}
}

// On any failure the clone is put back and nothing reaches the server.
func TestLinksFailuresPutTheCloneBack(t *testing.T) {
	fresh := func(t *testing.T) (*rig, string, string) {
		r := linkFixture(t, true)
		return r, r.serverHead(), gitIn(t, r.clone, "rev-parse", "HEAD")
	}
	check := func(t *testing.T, r *rig, server, head string, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("the failure was not reported")
		}
		if r.serverHead() != server || gitIn(t, r.clone, "rev-parse", "HEAD") != head {
			t.Fatal("a commit survived")
		}
		r.assertClean("after the failure")
		if reviewMDs(r.clone) {
			t.Fatal("a review file survived")
		}
	}
	t.Run("lint finds something new the repair did not account for", func(t *testing.T) {
		r, server, head := fresh(t)
		real := lintRun
		defer func() { lintRun = real }()
		calls := 0
		lintRun = func(dir string) ([]lint.Finding, error) {
			fs, err := real(dir)
			if calls++; calls > 1 { // after the write
				fs = append(fs, lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})
			}
			return fs, err
		}
		_, err := r.links(false)
		check(t, r, server, head, err)
	})
	t.Run("lint cannot run", func(t *testing.T) {
		r, server, head := fresh(t)
		real := lintRun
		defer func() { lintRun = real }()
		lintRun = func(dir string) ([]lint.Finding, error) { return nil, os.ErrPermission }
		_, err := r.links(false)
		check(t, r, server, head, err)
	})
	t.Run("the server refuses", func(t *testing.T) {
		r, server, head := fresh(t)
		os.WriteFile(filepath.Join(r.bare, "hooks", "pre-receive"), []byte("#!/bin/sh\necho 'tacit: refused' >&2\nexit 1\n"), 0o755)
		_, err := r.links(false)
		check(t, r, server, head, err)
		if !strings.Contains(err.Error(), "refused") {
			t.Fatalf("the refusal is not in the error: %v", err)
		}
	})
	t.Run("the run is cancelled before the push", func(t *testing.T) {
		r, server, head := fresh(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		real := lintRun
		defer func() { lintRun = real }()
		calls := 0
		lintRun = func(dir string) ([]lint.Finding, error) {
			fs, err := real(dir)
			if calls++; calls > 1 { // after the write
				cancel()
			}
			return fs, err
		}
		_, err := linksStep(ctx, r.s, false, linksNow)
		check(t, r, server, head, err)
	})
	// and the next run, with the failure gone, repairs
	t.Run("the next run repairs", func(t *testing.T) {
		r, _, _ := fresh(t)
		if rep, err := r.links(false); err != nil || !rep.Pushed {
			t.Fatalf("pushed=%v err=%v", rep != nil && rep.Pushed, err)
		}
	})
}

// Another writer lands a commit between our commit and our push: the locked
// part starts over on the fresh clone and the repair goes up once.
func TestLinksAnotherWriterPushingMeanwhile(t *testing.T) {
	r := linkFixture(t, true)
	other := filepath.Join(r.root, "other")
	gitIn(t, r.root, "clone", "-q", r.bare, other)
	gitIn(t, other, "config", "user.email", "o@o")
	gitIn(t, other, "config", "user.name", "other")
	writeFact(t, other, fact.Meta{Name: "other-note", Description: "d", Type: "machine_fact", Scope: "machines/host-a"}, "x")
	marker := filepath.Join(r.root, "pushed-once")
	script := "#!/bin/sh\n[ -e " + marker + " ] && exit 0\ntouch " + marker + "\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\ngit -C " + other + " add -A && git -C " + other + " commit -q -m other && git -C " + other + " push -q origin HEAD:main\nexit 0\n"
	os.WriteFile(filepath.Join(r.clone, ".git", "hooks", "pre-push"), []byte(script), 0o755)

	rep, err := r.links(false)
	if err != nil || !rep.Pushed {
		t.Fatalf("pushed=%v err=%v", rep != nil && rep.Pushed, err)
	}
	files := strings.Join(r.serverFiles(), ",")
	if !strings.Contains(files, "machines/host-a/other-note.md") || !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
		t.Fatalf("the server lacks one of the two writes: %s", files)
	}
	if n := strings.Count(gitIn(t, r.bare, "log", "--format=%s", "main"), links.CommitPrefix); n != 1 {
		t.Fatalf("%d repair commits", n)
	}
	r.assertClean("after the race")
}

// `tacit curate` runs the step once, also when no inbox note is pending, and
// the repair commit is the curator's.
func TestCurateRunsTheLinkStep(t *testing.T) {
	r := linkFixture(t, true)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "no-projects"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_DERIVE_CONFIG", filepath.Join(home, "derive.yaml"))
	runCurate([]string{"--max-batches", "0"}) // the exit code also carries the stale step, which needs scrump
	if subj := gitIn(t, r.bare, "log", "--format=%s", "main"); !strings.Contains(subj, links.CommitPrefix) {
		t.Fatalf("no link repair commit:\n%s", subj)
	}
	if !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
		t.Fatal("the repair is not on the server")
	}
	// curate --dry-run reads and prints
	head := r.serverHead()
	if code := runCurate([]string{"--dry-run", "--max-batches", "0"}); code != 0 || r.serverHead() != head {
		t.Fatalf("curate --dry-run exited %d or moved the server", code)
	}
	// and `tacit links` alone
	r2 := linkFixture(t, false)
	t.Setenv("TACIT_STORE", r2.clone)
	if code := runLinks([]string{"--dry-run"}); code != 0 {
		t.Fatalf("links --dry-run exited %d", code)
	}
	r2.assertClean("after links --dry-run")
	if code := runLinks(nil); code != 0 {
		t.Fatalf("links exited %d", code)
	}
	if got := lintRules(r2.serverLint()); got != "" {
		t.Fatalf("lint after `tacit links`:\n%s", got)
	}
}

// A ruling made of the owner's statements links to them, and the statements
// point at the ruling (superseded_by): history, not a dead link. It is not
// put to the owner, and when the ruling is later replaced in turn its links
// are not pointed at the new rule, which was not made of them.
func TestLinksLeaveALinkToWhatTheFactReplaced(t *testing.T) {
	r := newRig(t, rigOpts{})
	writeFact(t, r.clone, fact.Meta{Name: "said-once", Description: "d", Type: "design_decision", Scope: "projects/x", SupersededBy: "made-rule"}, "you said once")
	writeFact(t, r.clone, fact.Meta{Name: "made-rule", Description: "d", Type: "design_decision", Scope: "projects/x", Source: ownerSource("keep the gpu notes short")},
		"You decided (2026-10-20): \"keep the gpu notes short\"\nMade from your statement [[said-once]].")
	writeFact(t, r.clone, fact.Meta{Name: "said-twice", Description: "d", Type: "design_decision", Scope: "projects/x", SupersededBy: "older-rule"}, "you said it again")
	writeFact(t, r.clone, fact.Meta{Name: "older-rule", Description: "d", Type: "design_decision", Scope: "projects/x", SupersededBy: "newer-rule", Source: ownerSource("keep the cpu notes short")},
		"You decided (2026-10-19): \"keep the cpu notes short\"\nMade from your statement [[said-twice]].")
	writeFact(t, r.clone, fact.Meta{Name: "newer-rule", Description: "d", Type: "design_decision", Scope: "projects/x", Source: ownerSource("keep all notes short")},
		"You decided (2026-10-21): \"keep all notes short\"\nIt replaced [[older-rule]] and, through it, [[said-twice]].")
	r.commitIndex(r.clone, "fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	commits := r.commitCount()
	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Plan.Fixes) != 0 || len(rep.Rulings) != 0 || rep.Pushed || r.commitCount() != commits {
		t.Fatalf("%d fixes, %d ruling lines, pushed=%v; want nothing", len(rep.Plan.Fixes), len(rep.Rulings), rep.Pushed)
	}
}
