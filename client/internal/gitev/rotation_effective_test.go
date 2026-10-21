package gitev

import (
	"context"
	"strings"
	"testing"
)

// A rotation stamp says the fact was looked at, not that it is new: the day
// of the stamp must not become the day the fact counts as current, or the
// staleness check would skip every deletion before it. The subject is no
// proof: a rotation commit that changed more than last_checked is an edit.
func TestARotationStampDoesNotMoveTheEffectiveDate(t *testing.T) {
	ctx := context.Background()
	dir, f := storeWithFact(t)
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	text := factText("The engine retries three times.")
	commitFact(t, dir, "2026-09-10", "curator: fact", text)
	stamped := strings.Replace(text, "created: 2026-09-01\n", "created: 2026-09-01\nlast_checked: 2026-09-25\n", 1)
	commitFact(t, dir, "2026-09-25", RotationPrefix+" 1 checked (1 still)", stamped)
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-01")) || e.Why != "created" {
		t.Errorf("after a stamp: %+v, want the day it was created", e)
	}
	// A second stamp changes only the date: still not an edit.
	commitFact(t, dir, "2026-10-05", RotationPrefix+" 1 checked (1 still)", strings.Replace(stamped, "last_checked: 2026-09-25", "last_checked: 2026-10-05", 1))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-09-01")) {
		t.Errorf("after a second stamp: %+v", e)
	}
	// The same subject on a change of the words is an edit.
	commitFact(t, dir, "2026-10-07", RotationPrefix+" 1 checked (1 still)", strings.Replace(stamped, "three times", "five times", 1))
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day(t, "2026-10-07")) || e.Why != "store" {
		t.Errorf("words changed under the rotation subject: %+v, want that day", e)
	}
}
