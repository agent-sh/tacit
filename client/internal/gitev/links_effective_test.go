package gitev

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/links"
)

// commitFact rewrites the fact file of storeWithFact and commits it on a day
// with a subject.
func commitFact(t *testing.T, dir, day, subject, text string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "projects/x"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "projects/x/engine.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, nil, "add", "-A")
	when := day + "T12:00:00+00:00"
	run(t, dir, []string{"GIT_AUTHOR_NAME=S", "GIT_AUTHOR_EMAIL=s@e.test", "GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_NAME=S", "GIT_COMMITTER_EMAIL=s@e.test", "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", subject)
}

const repairSubject = links.CommitPrefix + " 1 link(s) in 1 fact(s) repaired (0 rewritten, 1 unwrapped)"

func factText(body string) string {
	return "---\nname: engine\ndescription: d\ntype: design_decision\nscope: projects/x\nsource:\n  harness: curator\n  host: host-a\n  kind: observed\ncreated: 2026-09-01\n---\n\n" + body + "\n"
}

// A link repair is not an edit of what the fact says, so the day it was made
// must not become the day the fact counts as current: that would hide every
// deletion between the fact's own date and the repair from the staleness check.
// The commit subject is not proof: the skip needs the change to be a repair.
func TestALinkRepairCommitDoesNotMoveTheEffectiveDate(t *testing.T) {
	ctx := context.Background()
	dir, f := storeWithFact(t) // no commits yet
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	commitFact(t, dir, "2026-09-10", "curator: fact", factText("The engine is fine, see [[gone]] and [[other|the label]]."))

	commitFact(t, dir, "2026-09-25", repairSubject, factText("The engine is fine, see gone and [[other|the label]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-01")) || e.Why != "created" {
		t.Errorf("after a link repair: %+v, want the day it was created", e)
	}

	// The same subject on a change that is more than a repair is an edit.
	commitFact(t, dir, "2026-09-27", repairSubject, factText("The engine is broken, see gone and [[other|the label]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-27")) || e.Why != "store" {
		t.Errorf("a sentence changed under the repair subject: %+v, want that day", e)
	}

	// Control: any other commit of the file moves it too.
	commitFact(t, dir, "2026-09-28", "curator: edit", factText("The engine is fixed, see gone and [[other|the label]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-28")) || e.Why != "store" {
		t.Errorf("after an edit: %+v, want the edit's day", e)
	}
}

// A link replaced by a sentence under the repair subject is an edit.
func TestASentenceWhereALinkWasIsNotARepair(t *testing.T) {
	ctx := context.Background()
	dir, f := storeWithFact(t)
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	commitFact(t, dir, "2026-09-10", "curator: fact", factText("See [[gone]]."))
	commitFact(t, dir, "2026-09-25", repairSubject, factText("See gone. CORRECTION: the opposite now."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-25")) || e.Why != "store" {
		t.Errorf("%+v, want the commit's day", e)
	}
}

// The step writes a new target and the old label. A link that says something
// else under the repair subject is an edit.
func TestARelabelledLinkIsNotARepair(t *testing.T) {
	ctx := context.Background()
	dir, f := storeWithFact(t)
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	commitFact(t, dir, "2026-09-10", "curator: fact", factText("See [[gone|the old note]]."))
	commitFact(t, dir, "2026-09-25", repairSubject, factText("See [[other|the old note]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-01")) || e.Why != "created" {
		t.Errorf("a retargeted link with its label: %+v, want the day it was created", e)
	}
	commitFact(t, dir, "2026-09-27", repairSubject, factText("See [[other|the engine is broken]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-27")) || e.Why != "store" {
		t.Errorf("a relabelled link: %+v, want that day", e)
	}
}

// Putting back a link the step unwrapped (`tacit links --restore`) is the
// repair the other way round: not an edit either.
func TestAPutBackLinkDoesNotMoveTheEffectiveDate(t *testing.T) {
	ctx := context.Background()
	dir, f := storeWithFact(t)
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	commitFact(t, dir, "2026-09-10", "curator: fact", factText("Made from [[old-statement]]."))
	commitFact(t, dir, "2026-09-25", repairSubject, factText("Made from old-statement."))
	commitFact(t, dir, "2026-10-30", links.CommitPrefix+" 1 link(s) to what the fact replaced put back in 1 fact(s), unwrapped by older runs", factText("Made from [[old-statement]]."))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-01")) || e.Why != "created" {
		t.Errorf("after the link was put back: %+v, want the day it was created", e)
	}
}
