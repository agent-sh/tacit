package gitev

import (
	"context"
	"errors"
	"time"
)

// One fact against git: its effective date, the evidence read from an anchor
// the caller chooses, and its timeline. The staleness check is the first
// caller; a pair of facts uses Build and Evidence.

// Effective is the day a fact is known to be current as of: when what it says
// was true, or when someone last changed it.
type Effective struct {
	At  time.Time // midnight UTC of that day
	Why string    // text | source | created | updated | store
}

// Effective dates a fact the way the timeline does, then lets a later edit
// move it forward: the newest of the date its text says it is from (else its
// source note's date, else the day it was filed), its `updated` day when that
// differs from `created`, and the day of its last store commit when the file
// was committed more than once. A curator fact filed on 2026-10-08 from a note
// written in May is current as of May, not as of the filing: the filing was a
// rewrite, not a check against git. The zero value means the fact carries no
// usable date.
func (b *Builder) Effective(ctx context.Context, r FactRef) Effective {
	d := b.factDates(ctx, SideA, r)
	e := Effective{At: d.asOf, Why: d.asOfWhy}
	f := r.Fact
	created, okC := parseDay(f.Meta.Created)
	if u, ok := parseDay(f.Meta.Updated); ok && (!okC || !u.Equal(created)) && u.After(e.At) && (b.AsOf.IsZero() || !u.After(b.AsOf)) {
		e = Effective{At: u, Why: "updated"}
	}
	if d.storeN > 1 {
		day := time.Date(d.lastStore.UTC().Year(), d.lastStore.UTC().Month(), d.lastStore.UTC().Day(), 0, 0, 0, 0, time.UTC)
		if day.After(e.At) {
			e = Effective{At: day, Why: "store"}
		}
	}
	return e
}

// ReadSince is the light read for one fact (collectLight): the subject (only
// identifiers specific enough to mean something, see Specific) and, per repo
// read at its default branch when Git.Resolve is set, which named paths and
// identifiers HEAD no longer holds (State), the commits at or after `anchor`
// (by committer date) that touched a line holding a gone identifier with what
// each removed or rewrote (RepoEvidence.Changed), and for each gone path its
// newest commit (RepoEvidence.Deleted). The texts are as git gave them: a
// caller redacts what it keeps or shows, in one call, with RedactAll, before
// clipping. A read that failed, ran out of time or was cut is marked
// Incomplete and must not be taken for "nothing found".
func (b *Builder) ReadSince(ctx context.Context, r FactRef, anchor time.Time) (Subject, []RepoEvidence, error) {
	if r.Fact == nil {
		return Subject{}, nil, errors.New("gitev: needs a fact")
	}
	sub := b.G.SubjectOf(r.Fact)
	var idents []string
	for _, id := range sub.Idents {
		if Specific(id) {
			idents = append(idents, id)
		}
	}
	sub.Idents = idents
	if anchor.IsZero() || sub.Empty() {
		return sub, nil, nil
	}
	return sub, b.G.Collect(ctx, sub, Window{Anchor: anchor, AsOf: b.AsOf, Light: true}), nil
}

// RedactAll masks secrets in texts with the builder's redactor, which fails
// closed: on an error nothing is returned.
func (b *Builder) RedactAll(ctx context.Context, texts []string) ([]string, error) {
	ptrs := make([]*string, len(texts))
	out := append([]string(nil), texts...)
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := b.redactPtrs(ctx, ptrs); err != nil {
		return nil, err
	}
	return out, nil
}

// Specific says whether an identifier names something: a flag, or a name with
// an underscore, a hyphen, a scope or a dot in it, a digit, or capitals inside
// it. A plain lowercase word such as "suspended" or "replay" names nothing, and
// searching a repository's history for it finds every commit that mentions the
// word.
func Specific(id string) bool {
	if len(id) < 4 {
		return false
	}
	for i, r := range id {
		switch {
		case r == '_' || r == '-' || r == ':' || r == '.' || r == '/' || (r >= '0' && r <= '9'):
			return true
		case r >= 'A' && r <= 'Z' && i > 0:
			return true
		}
	}
	return false
}

// BuildOne is Build for one fact: its own dates, the commits on what it names
// and what HEAD shows, with no side on the fact's dates.
func (b *Builder) BuildOne(ctx context.Context, r FactRef) (*Timeline, error) {
	if r.Fact == nil {
		return nil, errors.New("gitev: a timeline needs a fact")
	}
	t, err := b.Build(ctx, Input{A: r, B: r})
	if err != nil {
		return nil, err
	}
	for i := range t.Events {
		if t.Events[i].Kind == KindFact {
			t.Events[i].Side = ""
		}
	}
	return t, nil
}

// Gone is a path term the repo no longer holds, with the newest commit that
// touched it (the one that deleted or renamed it).
type Gone struct {
	Term   string
	Commit Commit
}
