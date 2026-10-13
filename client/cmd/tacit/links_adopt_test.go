package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
)

// newFindingAfterTheWrite makes the linter report one extra finding from its
// second call on (the call after the write), and returns the restore.
func newFindingAfterTheWrite(f lint.Finding) func() {
	real := lintRun
	calls := 0
	lintRun = func(dir string) ([]lint.Finding, error) {
		fs, err := real(dir)
		if calls++; calls > 1 {
			fs = append(fs, f)
		}
		return fs, err
	}
	return func() { lintRun = real }
}

func (r *rig) read(rel string) []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.clone, rel))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *rig) write(rel string, b []byte) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.clone, rel), b, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// addFact puts a fact on the server (through the clone).
func (r *rig) addFact(scope, name string, m fact.Meta, body string) {
	r.t.Helper()
	m.Name, m.Scope, m.Description = name, scope, "fact "+name
	if m.Type == "" {
		m.Type = "design_decision"
	}
	writeFact(r.t, r.clone, m, body)
	r.commitIndex(r.clone, "add "+name)
	gitIn(r.t, r.clone, "push", "-q", "origin", "HEAD:main")
}

// Owner rulings and derived facts are never adopted, whatever was done to them
// in the clone: a hand edit that only touches the link, and says anything else
// where the link was, survives a run that fails and a run that succeeds, and is
// never committed under the curator's subject.
func TestLinksNeverCommitOrLoseAHandEditedRuling(t *testing.T) {
	for _, failing := range []bool{true, false} {
		name := "a clean run"
		if failing {
			name = "a failed run"
		}
		t.Run(name, func(t *testing.T) {
			r := linkFixture(t, true)
			edited := bytes.Replace(r.read("projects/x/ruling-a.md"), []byte("[[gone-a]]"), []byte("gone-a. CORRECTION: the owner now says the opposite"), 1)
			r.write("projects/x/ruling-a.md", edited)
			// An exact unwrap of a ruling, byte for byte what the step writes for any
			// other fact, is still not the step's: only the guard keeps it out.
			exact := bytes.ReplaceAll(r.read("projects/x/ruling-b.md"), []byte("[[gone-b]]"), []byte("gone-b"))
			r.write("projects/x/ruling-b.md", exact)
			demo := bytes.Replace(r.read("derived/demo/demo-note.md"), []byte("[[gone-c]]"), []byte("gone-c"), 1)
			r.write("derived/demo/demo-note.md", demo)
			server := r.serverHead()
			if failing {
				defer newFindingAfterTheWrite(lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})()
			}
			rep, err := r.links(false)
			if failing != (err != nil) {
				t.Fatalf("failing=%v err=%v", failing, err)
			}
			if !bytes.Equal(r.read("projects/x/ruling-a.md"), edited) || !bytes.Equal(r.read("projects/x/ruling-b.md"), exact) || !bytes.Equal(r.read("derived/demo/demo-note.md"), demo) {
				t.Fatal("a hand edit was lost")
			}
			if failing {
				if r.serverHead() != server {
					t.Fatal("a failed run pushed")
				}
				return
			}
			if !rep.Pushed {
				t.Fatal("the clean run pushed nothing")
			}
			if strings.Contains(r.onServer("projects/x/ruling-a.md"), "CORRECTION") || !strings.Contains(r.onServer("projects/x/ruling-b.md"), "[[gone-b]]") || !strings.Contains(r.onServer("derived/demo/demo-note.md"), "[[gone-c]]") {
				t.Fatal("a hand edit reached the server")
			}
			files := gitIn(t, r.bare, "show", "--name-only", "--format=", "main")
			if strings.Contains(files, "ruling-a") || strings.Contains(files, "ruling-b") || strings.Contains(files, "demo-note") {
				t.Fatalf("the repair commit carries the edits:\n%s", files)
			}
			status := gitIn(t, r.clone, "status", "--porcelain")
			if !strings.Contains(status, "projects/x/ruling-a.md") || !strings.Contains(status, "projects/x/ruling-b.md") {
				t.Fatal("the edits are not still in the clone")
			}
		})
	}
}

// A hand edit of a repairable fact that is not exactly what the step would
// write (a sentence put where the link was) is someone's work: not adopted,
// not committed, not lost.
func TestLinksNeverAdoptASentenceWhereALinkWas(t *testing.T) {
	for _, failing := range []bool{true, false} {
		r := linkFixture(t, false)
		edited := bytes.Replace(r.read("projects/x/uses-dup.md"), []byte("[[Dup_Name]]"), []byte("Dup_Name is wrong, the opposite holds"), 1)
		r.write("projects/x/uses-dup.md", edited)
		if failing {
			func() {
				defer newFindingAfterTheWrite(lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})()
				if _, err := r.links(false); err == nil {
					t.Fatal("the failure was not reported")
				}
			}()
		} else if rep, err := r.links(false); err != nil || len(rep.Adopted) != 0 {
			t.Fatalf("err=%v adopted=%v", err, rep.Adopted)
		}
		if !bytes.Equal(r.read("projects/x/uses-dup.md"), edited) {
			t.Fatalf("failing=%v: the edit was lost", failing)
		}
		if strings.Contains(r.onServer("projects/x/uses-dup.md"), "the opposite") {
			t.Fatal("the edit reached the server")
		}
	}
}

// A file a killed run wrote (exactly what the step writes from its committed
// version) goes back to those bytes when the next run fails, not to HEAD.
func TestLinksFailureKeepsWhatAKilledRunWrote(t *testing.T) {
	r := linkFixture(t, false)
	tree, err := readLinksTree(ctxMinute(t), r.s)
	if err != nil {
		t.Fatal(err)
	}
	var written []byte
	for _, fx := range tree.plan.Fixes {
		if fx.Name == "uses-old" {
			written = fx.Data
			r.write(fx.Path, fx.Data)
		}
	}
	defer newFindingAfterTheWrite(lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})()
	if _, err := r.links(false); err == nil {
		t.Fatal("the failure was not reported")
	}
	if !bytes.Equal(r.read("projects/x/uses-old.md"), written) {
		t.Fatal("the leftover was put back to HEAD, not to the bytes the killed run wrote")
	}
}

// A fact the clone does not track is not the step's: never planned, written,
// committed or removed, a failed run does not delete it, and a link to it is
// left alone rather than unwrapped.
func TestLinksLeaveAnUntrackedFactAlone(t *testing.T) {
	for _, failing := range []bool{true, false} {
		r := linkFixture(t, false)
		r.addFact("projects/x", "links-to-new", fact.Meta{}, "See [[brand-new]].")
		writeFact(t, r.clone, fact.Meta{Name: "brand-new", Description: "d", Type: "design_decision", Scope: "projects/x"}, "Not on the server; see [[gone-z]].")
		mine := r.read("projects/x/brand-new.md")
		if failing {
			func() {
				defer newFindingAfterTheWrite(lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})()
				if _, err := r.links(false); err == nil {
					t.Fatal("the failure was not reported")
				}
			}()
		} else {
			rep, err := r.links(false)
			if err != nil || !rep.Pushed {
				t.Fatalf("err=%v", err)
			}
			var found bool
			for _, n := range rep.NotDone {
				found = found || n.Path == "projects/x/brand-new.md"
			}
			if !found {
				t.Fatalf("the untracked fact is not reported: %v", rep.NotDone)
			}
			if !strings.Contains(r.onServer("projects/x/links-to-new.md"), "[[brand-new]]") {
				t.Fatal("a link to a fact the server lacks was rewritten")
			}
			if strings.Contains(r.onServer("MEMORY.md"), "brand-new") {
				t.Fatal("the index on the server names a fact the server does not have")
			}
			if !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
				t.Fatal("the rest was not repaired")
			}
			// the one finding left is the link to the fact the server lacks, kept on purpose
			if got := lintRules(r.serverLint()); got != "L6 projects/x/links-to-new.md" {
				t.Fatalf("lint of the server:\n%s", got)
			}
		}
		if !bytes.Equal(r.read("projects/x/brand-new.md"), mine) {
			t.Fatalf("failing=%v: the untracked fact was changed or removed", failing)
		}
		if !strings.Contains(gitIn(t, r.clone, "status", "--porcelain"), "?? projects/x/brand-new.md") {
			t.Fatalf("failing=%v: the fact is no longer untracked", failing)
		}
	}
}

// What someone already staged in the clone does not ride along in the commit,
// and the index on the server does not name it.
func TestLinksCommitOnlyTheFilesItNames(t *testing.T) {
	r := linkFixture(t, false)
	writeFact(t, r.clone, fact.Meta{Name: "staged-note", Description: "d", Type: "machine_fact", Scope: "machines/host-a"}, "staged by hand")
	p := filepath.Join(r.clone, "projects/x/gpu-notes.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, []byte("A staged edit.\n")...), 0o644)
	gitIn(t, r.clone, "add", "machines/host-a/staged-note.md", "projects/x/gpu-notes.md")

	rep, err := r.links(false)
	if err != nil || !rep.Pushed {
		t.Fatalf("err=%v", err)
	}
	files := gitIn(t, r.bare, "show", "--name-only", "--format=", "main")
	if strings.Contains(files, "staged-note") || strings.Contains(files, "gpu-notes") {
		t.Fatalf("staged files rode along:\n%s", files)
	}
	if strings.Contains(r.onServer("MEMORY.md"), "staged-note") {
		t.Fatal("the index names a fact the server does not have")
	}
	if strings.Contains(r.onServer("projects/x/gpu-notes.md"), "A staged edit") {
		t.Fatal("a staged edit reached the server")
	}
	if staged := gitIn(t, r.clone, "diff", "--cached", "--name-only"); !strings.Contains(staged, "staged-note") || !strings.Contains(staged, "gpu-notes") {
		t.Fatalf("the staged changes were disturbed:\n%s", staged)
	}
	if fs := r.serverLint(); len(fs) != 0 {
		t.Fatalf("lint of the server: %v", fs)
	}
}

// One finding the repair did not cause, or one fact that cannot be repaired
// cleanly, does not block every other repair.
func TestLinksOneFileDoesNotBlockTheRest(t *testing.T) {
	t.Run("an unrelated finding on a repaired file, and a nested link", func(t *testing.T) {
		r := linkFixture(t, false)
		r.addFact("projects/x", "l7-fact", fact.Meta{SupersededBy: "ghost-successor"}, "see [[gone-l7]]")
		r.addFact("projects/x", "nested", fact.Meta{}, "twice over [[[[gone-y]]]]")
		if got := lintRules(r.serverLint()); !strings.Contains(got, "L7 projects/x/l7-fact.md") {
			t.Fatalf("the fixture has no L7:\n%s", got)
		}
		rep, err := r.links(false)
		if err != nil || !rep.Pushed {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(r.onServer("projects/x/l7-fact.md"), "see gone-l7\n") || !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
			t.Fatal("the other repairs did not go through")
		}
		// the inner link is unwrapped and what that leaves is a link again; the next run settles it
		if !strings.Contains(r.onServer("projects/x/nested.md"), "twice over [[gone-y]]\n") {
			t.Fatalf("nested: %q", r.onServer("projects/x/nested.md"))
		}
		if _, err := r.links(false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.onServer("projects/x/nested.md"), "twice over gone-y\n") {
			t.Fatalf("nested after the second run: %q", r.onServer("projects/x/nested.md"))
		}
	})
	t.Run("a new finding on one written file drops that file", func(t *testing.T) {
		r := linkFixture(t, false)
		before := r.read("projects/x/uses-old.md")
		real := lintRun
		defer func() { lintRun = real }()
		lintRun = func(dir string) ([]lint.Finding, error) {
			fs, err := real(dir)
			if b, rerr := os.ReadFile(filepath.Join(dir, "projects/x/uses-old.md")); rerr == nil && !bytes.Equal(b, before) {
				fs = append(fs, lint.Finding{Rule: "L6", Path: "projects/x/uses-old.md", Msg: "a finding the repair caused"})
			}
			return fs, err
		}
		rep, err := r.links(false)
		if err != nil || !rep.Pushed {
			t.Fatalf("err=%v", err)
		}
		if !bytes.Equal(r.read("projects/x/uses-old.md"), before) || strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
			t.Fatal("the dropped file was written")
		}
		if !strings.Contains(r.onServer("projects/x/uses-gpu.md"), "[[gpu-notes]]") {
			t.Fatal("the others were not repaired")
		}
		var named bool
		for _, n := range rep.NotDone {
			named = named || n.Path == "projects/x/uses-old.md" && strings.Contains(n.Why, "lint rejects")
		}
		if !named {
			t.Fatalf("the drop is not reported: %v", rep.NotDone)
		}
		for _, fx := range rep.Repaired {
			if fx.Path == "projects/x/uses-old.md" {
				t.Fatal("the dropped file is counted as repaired")
			}
		}
	})
}

// The report says "not done" for what was skipped, dropped or lost to a failure,
// and counts only what was committed.
func TestLinksReportCountsOnlyWhatWasCommitted(t *testing.T) {
	r := linkFixture(t, false)
	p := filepath.Join(r.clone, "projects/x/uses-dup.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, []byte("A line someone is writing.\n")...), 0o644)

	var out, errw bytes.Buffer
	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	writeLinks(&out, &errw, rep, false)
	if strings.Contains(out.String(), "Dup_Name") {
		t.Fatalf("a skipped repair is reported as made:\n%s", out.String())
	}
	if !strings.Contains(errw.String(), "not done: projects/x/uses-dup.md") || !strings.Contains(out.String(), "1 not done") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), errw.String())
	}
	if !strings.Contains(out.String(), "14 link(s) in 7 fact(s) repaired") {
		t.Fatalf("the total is not what was committed:\n%s", out.String())
	}

	// A failed run reports nothing as made.
	r2 := linkFixture(t, false)
	defer newFindingAfterTheWrite(lint.Finding{Rule: "L10", Path: "MEMORY.md", Msg: "differs"})()
	rep2, err := r2.links(false)
	if err == nil {
		t.Fatal("the failure was not reported")
	}
	out.Reset()
	errw.Reset()
	writeLinks(&out, &errw, rep2, false)
	if strings.Contains(out.String(), "rewrote") || strings.Contains(out.String(), "unwrapped [[") || !strings.Contains(out.String(), "0 link(s) in 0 fact(s) repaired") || !strings.Contains(out.String(), "15 not done") {
		t.Fatalf("after a failed run:\n%s", out.String())
	}
}
