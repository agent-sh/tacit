package gitev

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// collectLight is the read behind Window.Light, for the staleness check. It
// asks only for what a deletion needs, so nothing it does not read can hide one:
//
//   - HEAD is the commit Git.Resolve names (the repo's default or configured
//     branch), not whatever is checked out;
//   - the tree says which named paths and identifiers are gone;
//   - only identifiers that are gone are searched for in history, so the
//     search is short and the commit that removed one is among its few matches;
//   - for each gone path the newest commit that touched it is read directly, not
//     picked from a sample of commits.
//
// A read that failed, ran out of time or was cut says so (Incomplete) and is
// not cached. Dates are committer dates throughout: that is what git's --since
// filters on and when a commit reached the repo.
func (g *Git) collectLight(ctx context.Context, name string, s Subject, w Window) RepoEvidence {
	ev := RepoEvidence{Name: name}
	dir := filepath.Join(g.Root, name)
	anchor := w.Anchor
	var mu sync.Mutex
	fail := func(msg string) {
		mu.Lock()
		ev.Notes = append(ev.Notes, msg)
		ev.Incomplete = true
		mu.Unlock()
	}
	if !g.noLazyFetch(ctx) && g.isPartial(ctx, dir) {
		fail("partial clone, and this git is older than 2.44 and would fetch the files the queries need, so the repo is not read")
		return ev
	}
	// The commit this read is at.
	sha, start := "", "HEAD"
	if g.Resolve != nil {
		full, ref, err := g.Resolve(ctx, name, dir)
		if err != nil {
			fail("the default branch could not be read: " + shortErr(err))
			return ev
		}
		sha, start, ev.Branch = full, full, ref
	}
	if w.AsOf.IsZero() {
		if sha == "" {
			o, _, err := g.run(ctx, dir, 1<<10, "rev-parse", "HEAD")
			if err != nil {
				fail("no commits: " + shortErr(err))
				return ev
			}
			sha = strings.TrimSpace(o)
			if b, _, err := g.run(ctx, dir, 1<<10, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				ev.Branch = strings.TrimSpace(b)
				if ev.Branch == "HEAD" {
					ev.Branch = "detached"
				}
			}
		}
	} else {
		o, _, err := g.run(ctx, dir, 1<<10, "rev-list", "-1", "--first-parent", "--before="+gitDate(w.AsOf), start)
		sha = strings.TrimSpace(o)
		if err != nil {
			fail("no commit read: " + shortErr(err))
			return ev
		}
		if sha == "" {
			ev.Notes = append(ev.Notes, "no commit before "+w.AsOf.Format("2006-01-02"))
			return ev
		}
	}
	key := g.cacheKey(name, sha, s, w, 0)
	if cached, ok := g.cacheGet(key); ok {
		return cached
	}
	ev.Head = shortSHA(sha)
	if o, _, err := g.run(ctx, dir, 1<<12, "log", "-1", "--format=%ct%x1f%s", sha); err == nil {
		at, sub, _ := strings.Cut(strings.TrimSpace(o), "\x1f")
		ev.HeadAt = unix(at)
		ev.HeadSubject = sub
	} else {
		fail("HEAD commit unreadable: " + shortErr(err))
	}

	sem := make(chan struct{}, g.parallel())
	var wg sync.WaitGroup
	task := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f()
		}()
	}
	states := make([]TermState, len(s.Paths)+len(s.Idents))
	task(func() { g.pathStates(ctx, dir, sha, s.Paths, states[:len(s.Paths)], fail) })
	for i, id := range s.Idents {
		i, id := i, id
		task(func() { states[len(s.Paths)+i] = g.identState(ctx, dir, sha, id, fail) })
	}
	wg.Wait()
	ev.State = states

	// Only a name that is gone can have been removed.
	var goneIdents, gonePaths []string
	for _, st := range states {
		if st.Present || st.Unknown {
			continue
		}
		if st.Kind == "path" {
			gonePaths = append(gonePaths, st.Term)
		} else {
			goneIdents = append(goneIdents, st.Term)
		}
	}

	var changed []*Commit
	if len(goneIdents) > 0 {
		args := []string{"log", sha, "--no-merges", "-n", strconv.Itoa(queryMax), "--format=" + logFormat, "--since=" + gitDate(anchor), "-G" + orRegex(goneIdents), "--", "."}
		args = append(args, noise...)
		o, trunc, err := g.run(ctx, dir, 4<<20, args...)
		switch {
		case err != nil:
			fail("content search failed, commits may be missing: " + shortErr(err))
		case trunc:
			fail("content search cut at 4 MB, commits may be missing")
		}
		cs := parseLog(o)
		for i := range cs {
			if !cs[i].Committed.Before(anchor) {
				changed = append(changed, &cs[i])
			}
		}
		ev.ChangedN = len(changed)
		ev.Capped = len(cs) >= queryMax
		for _, c := range changed {
			c := c
			task(func() {
				// -G keeps only the files whose change holds a gone name, so the other
				// files of a mass refactor do not fill the read.
				a := []string{"show", "--format=", "-U0", "--no-color", "--no-ext-diff", "-G" + orRegex(goneIdents), c.SHA, "--", "."}
				a = append(a, noise...)
				out, trunc, err := g.run(ctx, dir, showMax, a...)
				switch {
				case err != nil:
					fail("diff of " + c.Short + " unreadable: " + shortErr(err))
					return
				case trunc:
					// What was cut may hold the removal: the read is not complete.
					fail("diff of " + c.Short + " cut at " + strconv.Itoa(showMax/1000) + " KB, the removal may be past it")
					return
				}
				c.Terms = termsIn(out, goneIdents)
				c.Dropped = termsRemoved(out, goneIdents)
				c.Touched = strings.Count("\n"+out, "\ndiff --git ")
			})
		}
		wg.Wait()
		sort.SliceStable(changed, func(i, j int) bool { return changed[i].Committed.Before(changed[j].Committed) })
		hit := false
		for _, c := range changed {
			ev.Changed = append(ev.Changed, *c)
			hit = hit || len(c.Dropped) > 0
		}
		// More matches than the search returns: the commit that removed the name
		// may be one it did not return.
		if ev.Capped && !hit {
			fail("more than " + strconv.Itoa(queryMax) + " commits touch a deleted name and none of the newest removed it")
		}
	}

	// The newest commit that touched each gone path is the one that deleted or
	// renamed it. Asked for one path at a time, so no sample can miss it.
	deleted := make([]*Gone, len(gonePaths))
	for i, p := range gonePaths {
		i, p := i, p
		task(func() {
			spec := ":(literal)" + strings.TrimSuffix(p, "/")
			if isGlob(p) {
				spec = ":(glob)" + p
			}
			o, trunc, err := g.run(ctx, dir, 1<<20, "log", sha, "--no-merges", "-n", "1", "--format="+logFormat, "--", spec)
			switch {
			case err != nil:
				fail("path " + clip(p, 60) + " could not be read: " + shortErr(err))
				return
			case trunc:
				fail("path " + clip(p, 60) + " cut")
				return
			}
			if cs := parseLog(o); len(cs) == 1 && !cs[0].Committed.Before(anchor) {
				deleted[i] = &Gone{Term: p, Commit: cs[0]}
			}
		})
	}
	wg.Wait()
	for _, d := range deleted {
		if d != nil {
			ev.Deleted = append(ev.Deleted, *d)
		}
	}
	ev.Notes = uniqueSorted(ev.Notes)
	g.cachePut(key, ev)
	return ev
}

// termsRemoved lists which terms appear on lines a diff removed or rewrote.
// A removed line that the same diff adds back unchanged (indentation aside)
// only moved, and does not count: splitting a document or moving a function
// takes lines away from one place and puts them in another.
func termsRemoved(diff string, terms []string) []string {
	added := map[string]bool{}
	var removed []string
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added[strings.TrimSpace(l[1:])] = true
		case strings.HasPrefix(l, "-"):
			removed = append(removed, l)
		}
	}
	var gone []string
	for _, l := range removed {
		if !added[strings.TrimSpace(l[1:])] {
			gone = append(gone, l)
		}
	}
	return termsIn(strings.Join(gone, "\n"), terms)
}
