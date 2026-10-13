package gitev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Window says which part of history to read.
type Window struct {
	// Since is the oldest commit read. Callers set it a little before the
	// older fact: a fact is usually written after the change it records.
	Since time.Time
	// Anchor is when the older fact was written. Commits before it are
	// context; commits from it on are the ones that may have made a fact stale.
	Anchor time.Time
	// AsOf reads each repo as it was at this time: HEAD is the last commit
	// before it. Zero reads HEAD now. A replay of an old decision sets it.
	AsOf time.Time
	// Limit is how many commits one repo returns (default 8): up to two of
	// context, then the first and the last of what came after the anchor.
	Limit int
	// Light reads only what the staleness check needs (collectLight): HEAD, the
	// state of each term, the commits since the anchor that removed a name HEAD
	// no longer holds, and the commits that deleted a path HEAD no longer has.
	// Anchor is then a hard bound; Since and Limit are not used.
	Light bool
}

// Person is the identity on a commit. Agents commit under the owner's name,
// so a caller that cares who acted reads the trailers as well.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Commit is one commit that touched the subject.
type Commit struct {
	SHA       string    `json:"sha"`
	Short     string    `json:"short"`
	At        time.Time `json:"at"` // author date
	Subject   string    `json:"subject"`
	Author    Person    `json:"author"`
	Committer Person    `json:"committer"`
	CoAuthors []string  `json:"co_authors,omitempty"` // Co-Authored-By trailers, "Name <email>"
	Files     []string  `json:"files,omitempty"`      // subject paths it touched, at most 3
	Terms     []string  `json:"terms,omitempty"`      // identifiers its diff adds or removes
	Dropped   []string  `json:"dropped,omitempty"`    // identifiers on lines its diff removed or rewrote (a line that only moved is not one)
	Touched   int       `json:"touched,omitempty"`    // files its diff touched (a diff cut at showMax counts what was read)
	Committed time.Time `json:"committed,omitempty"`  // committer date: what --since filters on, and when it reached the repo
	Message   bool      `json:"message,omitempty"`    // its message names a term
}

// TermState is what the repo's tree says about one subject term.
type TermState struct {
	Term    string   `json:"term"`
	Kind    string   `json:"kind"` // path | ident
	Present bool     `json:"present"`
	Count   int      `json:"count"`           // files holding it, or paths matching
	Where   []string `json:"where,omitempty"` // first two
	Generic bool     `json:"generic,omitempty"`
	// Unknown: the search failed, so the tree says nothing about the term.
	Unknown bool `json:"unknown,omitempty"`
}

// RepoEvidence is everything read from one repo for one subject.
type RepoEvidence struct {
	Name         string    `json:"name"`
	Branch       string    `json:"branch,omitempty"`
	Head         string    `json:"head"` // short sha the evidence was read at
	HeadAt       time.Time `json:"head_at"`
	HeadSubject  string    `json:"head_subject,omitempty"`
	CommitsSince int       `json:"commits_since"` // all commits after the anchor
	Matched      int       `json:"matched"`       // commits touching the subject in the window
	Capped       bool      `json:"capped,omitempty"`
	Commits      []Commit  `json:"commits"` // chronological
	Between      int       `json:"between,omitempty"`
	BetweenFrom  time.Time `json:"between_from,omitempty"`
	BetweenTo    time.Time `json:"between_to,omitempty"`
	// A light read (Window.Light) fills these instead of Commits: the commits
	// since the anchor whose diff touched a line holding a name HEAD no longer
	// holds (ChangedN found, Changed has them all, at most queryMax), and the
	// paths HEAD no longer has with the newest commit that touched each.
	ChangedN int         `json:"changed_n,omitempty"`
	Changed  []Commit    `json:"changed,omitempty"`
	Deleted  []Gone      `json:"deleted,omitempty"`
	State    []TermState `json:"state"`
	Notes    []string    `json:"notes,omitempty"`
	// Incomplete: a read failed or ran out of time. Shown with its notes,
	// never cached.
	Incomplete bool `json:"incomplete,omitempty"`
}

const (
	queryMax     = 60      // matching commits one query returns; a term that matches more is not distinctive
	genericFiles = 26      // a term in this many files at HEAD names no subject
	showMax      = 600_000 // bytes of one commit's diff read to attribute terms
	treeMax      = 16 << 20
	cacheVersion = "gitev4"
)

// Collect reads the evidence for a subject, one repo after another in
// parallel. A repo that cannot be read yields an entry with Notes.
func (g *Git) Collect(ctx context.Context, s Subject, w Window) []RepoEvidence {
	out := make([]RepoEvidence, len(s.Repos))
	var wg sync.WaitGroup
	for i, name := range s.Repos {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, g.repoBudget())
			defer cancel()
			out[i] = g.collectRepo(rctx, name, s, w)
		}(i, name)
	}
	wg.Wait()
	return out
}

func (g *Git) repoBudget() time.Duration {
	if g.RepoBudget > 0 {
		return g.RepoBudget
	}
	return 40 * time.Second
}

func (g *Git) parallel() int {
	if g.Parallel > 0 {
		return g.Parallel
	}
	return 3
}

func gitDate(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 -0700") }

func (g *Git) collectRepo(ctx context.Context, name string, s Subject, w Window) RepoEvidence {
	if w.Light {
		return g.collectLight(ctx, name, s, w)
	}
	ev := RepoEvidence{Name: name}
	dir := filepath.Join(g.Root, name)
	limit := w.Limit
	if limit <= 0 {
		limit = 8
	}
	var mu sync.Mutex
	// fail records a read that did not finish. The evidence is then marked
	// incomplete: it is shown with the note, and never kept in the cache.
	fail := func(msg string) {
		mu.Lock()
		ev.Notes = append(ev.Notes, msg)
		ev.Incomplete = true
		mu.Unlock()
	}
	// An older git would fetch a partial clone's missing files into the repo.
	if !g.noLazyFetch(ctx) && g.isPartial(ctx, dir) {
		fail("partial clone, and this git is older than 2.44 and would fetch the files the queries need, so the repo is not read")
		return ev
	}
	// Which commit is HEAD for this read.
	var sha string
	if w.AsOf.IsZero() {
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
	} else {
		// The last commit on the first-parent line before the time: a side
		// branch merged later is not part of what the repo was then.
		o, _, err := g.run(ctx, dir, 1<<10, "rev-list", "-1", "--first-parent", "--before="+gitDate(w.AsOf), "HEAD")
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
	key := g.cacheKey(name, sha, s, w, limit)
	if cached, ok := g.cacheGet(key); ok {
		return cached
	}
	ev.Head = shortSHA(sha)
	if o, _, err := g.run(ctx, dir, 1<<12, "log", "-1", "--format=%at%x1f%s", sha); err == nil {
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

	// Terms present in many files at HEAD are not distinctive; they stay out of
	// the content search. The state of every term is read from the tree.
	states := make([]TermState, len(s.Paths)+len(s.Idents))
	task(func() { g.pathStates(ctx, dir, sha, s.Paths, states[:len(s.Paths)], fail) })
	for i, id := range s.Idents {
		i, id := i, id
		task(func() { states[len(s.Paths)+i] = g.identState(ctx, dir, sha, id, fail) })
	}
	wg.Wait()
	ev.State = states
	var search, words []string
	for _, st := range states[len(s.Paths):] {
		if !st.Generic {
			search = append(search, st.Term)
			if len(st.Term) >= 6 {
				words = append(words, st.Term)
			}
		}
	}

	since := w.Since
	byID := map[string]*Commit{}
	capped := false
	merge := func(cs []Commit, files bool, msg bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(cs) >= queryMax {
			capped = true
		}
		for _, c := range cs {
			c := c
			cur := byID[c.SHA]
			if cur == nil {
				cur = &c
				byID[c.SHA] = cur
			}
			if files {
				for _, f := range c.Files {
					if len(cur.Files) < 3 && !contains(cur.Files, f) {
						cur.Files = append(cur.Files, f)
					}
				}
			}
			if msg {
				cur.Message = true
			}
		}
	}
	base := []string{"log", sha, "--no-merges", "-n", strconv.Itoa(queryMax), "--format=" + logFormat}
	if !since.IsZero() {
		base = append(base, "--since="+gitDate(since))
	}
	if len(s.Paths) > 0 {
		task(func() {
			pathLog := func(paths []string) (string, bool, error) {
				args := append(append([]string{}, base...), "--name-only", "--")
				for _, p := range paths {
					if isGlob(p) {
						args = append(args, ":(glob)"+p)
					} else {
						args = append(args, ":(literal)"+strings.TrimSuffix(p, "/"))
					}
				}
				return g.run(ctx, dir, 4<<20, args...)
			}
			o, trunc, err := pathLog(s.Paths)
			if err != nil && len(s.Paths) > 1 && !stopped(err) {
				// One pathspec git refuses (outside the repository, bad magic)
				// must not hide what the others show: ask one by one.
				for _, p := range s.Paths {
					o1, trunc1, err1 := pathLog([]string{p})
					switch {
					case err1 != nil && stopped(err1):
						fail("path query timed out")
					case err1 != nil:
						fail("path " + p + " refused by git: " + shortErr(err1))
					case trunc1:
						fail("path query cut at 4 MB")
					}
					if err1 == nil {
						merge(parseLog(o1), true, false)
					}
				}
				return
			}
			switch {
			case err != nil:
				fail("path query failed: " + shortErr(err))
			case trunc:
				fail("path query cut at 4 MB")
			}
			if err == nil {
				merge(parseLog(o), true, false)
			}
		})
	}
	if len(search) > 0 {
		task(func() {
			args := append(append([]string{}, base...), "-G"+orRegex(search), "--")
			args = append(args, ".")
			args = append(args, noise...)
			o, trunc, err := g.run(ctx, dir, 4<<20, args...)
			switch {
			case err != nil:
				fail("content search failed, commits may be missing: " + shortErr(err))
			case trunc:
				fail("content search cut at 4 MB, commits may be missing")
			}
			merge(parseLog(o), false, false)
		})
	}
	if len(words) > 0 {
		task(func() {
			args := append([]string{}, base...)
			args = append(args, "-i", "-F")
			for _, t := range words {
				args = append(args, "--grep="+t)
			}
			o, trunc, err := g.run(ctx, dir, 4<<20, args...)
			switch {
			case err != nil:
				fail("message search failed: " + shortErr(err))
			case trunc:
				fail("message search cut at 4 MB")
			}
			merge(parseLog(o), false, true)
		})
	}
	anchor := w.Anchor
	if anchor.IsZero() {
		anchor = since
	}
	task(func() {
		args := []string{"rev-list", "--count", "--first-parent", sha}
		if !anchor.IsZero() {
			args = append(args, "--since="+gitDate(anchor))
		}
		o, _, err := g.run(ctx, dir, 64, args...)
		if err != nil {
			fail("commit count failed: " + shortErr(err))
			return
		}
		n, _ := strconv.Atoi(strings.TrimSpace(o))
		ev.CommitsSince = n
	})
	wg.Wait()
	ev.Capped = capped

	all := make([]*Commit, 0, len(byID))
	for _, c := range byID {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].At.Equal(all[j].At) {
			return all[i].At.Before(all[j].At)
		}
		return all[i].SHA < all[j].SHA
	})
	ev.Matched = len(all)
	kept, between, from, to := selectCommits(all, w.Anchor, limit)
	ev.Between, ev.BetweenFrom, ev.BetweenTo = between, from, to
	// Which identifiers each kept commit changed.
	if len(search) > 0 {
		for _, c := range kept {
			c := c
			task(func() {
				args := []string{"show", "--format=", "-U0", "--no-color", "--no-ext-diff", c.SHA, "--", "."}
				args = append(args, noise...)
				// A diff cut at showMax only shortens the list of terms named.
				o, _, err := g.run(ctx, dir, showMax, args...)
				if err != nil {
					fail("diff of " + c.Short + " unreadable: " + shortErr(err))
					return
				}
				c.Terms = termsIn(o, search)
			})
		}
		wg.Wait()
	}
	for _, c := range kept {
		ev.Commits = append(ev.Commits, *c)
	}
	if ev.Incomplete {
		if g.isPartial(ctx, dir) {
			ev.Notes = append(ev.Notes, "this repo is a partial clone: history queries need file contents that are not here, and the checks do not fetch them")
		}
	}
	ev.Notes = uniqueSorted(ev.Notes)
	g.cachePut(key, ev)
	return ev
}

// stopped reports whether an error is a timeout or a cancel, not a refusal.
func stopped(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// shortErr is the first line of an error, short enough for a note.
func shortErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	s, _, _ := strings.Cut(err.Error(), "\n")
	return clip(s, 140)
}

func uniqueSorted(xs []string) []string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

const logFormat = "%x1e%H%x1f%at%x1f%ct%x1f%an%x1f%ae%x1f%cn%x1f%ce%x1f%(trailers:key=Co-authored-by,valueonly,unfold,separator=%x1d)%x1f%s"

func unix(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// parseLog reads logFormat records, each optionally followed by file names.
func parseLog(out string) []Commit {
	var cs []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		head, rest, _ := strings.Cut(rec, "\n")
		f := strings.Split(head, "\x1f")
		if len(f) < 9 {
			continue
		}
		c := Commit{SHA: f[0], Short: shortSHA(f[0]), At: unix(f[1]), Committed: unix(f[2]), Author: Person{f[3], f[4]}, Committer: Person{f[5], f[6]}, Subject: f[8]}
		if f[7] != "" {
			for _, a := range strings.Split(f[7], "\x1d") {
				if a = strings.TrimSpace(a); a != "" {
					c.CoAuthors = append(c.CoAuthors, a)
				}
			}
		}
		for _, name := range strings.Split(rest, "\n") {
			if name = strings.TrimSpace(name); name != "" {
				c.Files = append(c.Files, name)
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// orRegex joins literal terms into one POSIX extended expression.
func orRegex(terms []string) string {
	q := make([]string, len(terms))
	for i, t := range terms {
		q[i] = regexp.QuoteMeta(t)
	}
	return strings.Join(q, "|")
}

// termsIn lists which terms appear on added or removed lines of a diff.
func termsIn(diff string, terms []string) []string {
	found := map[string]bool{}
	for _, l := range strings.Split(diff, "\n") {
		if len(l) < 2 || (l[0] != '+' && l[0] != '-') || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") {
			continue
		}
		for _, t := range terms {
			if !found[t] && strings.Contains(l, t) {
				found[t] = true
			}
		}
	}
	var out []string
	for _, t := range terms {
		if found[t] {
			out = append(out, t)
		}
	}
	return out
}

// selectCommits keeps at most limit commits of an ascending list: up to two
// from before the anchor (what was going on when the fact was written), then
// the first and the last of those after it. The middle is counted, not shown.
func selectCommits(all []*Commit, anchor time.Time, limit int) (kept []*Commit, between int, from, to time.Time) {
	var before, after []*Commit
	for _, c := range all {
		if !anchor.IsZero() && c.At.Before(anchor) {
			before = append(before, c)
		} else {
			after = append(after, c)
		}
	}
	nb := 2
	if limit < 6 {
		nb = 1
	}
	if len(before) > nb {
		before = before[len(before)-nb:]
	}
	rem := limit - len(before)
	if rem < 2 {
		rem = 2
	}
	kept = append(kept, before...)
	if len(after) <= rem {
		return append(kept, after...), 0, time.Time{}, time.Time{}
	}
	first, last := (rem+1)/2, rem/2
	kept = append(kept, after[:first]...)
	kept = append(kept, after[len(after)-last:]...)
	return kept, len(after) - rem, after[first-1].At, after[len(after)-last].At
}

// pathStates fills states for path terms from the repo's file list at sha.
func (g *Git) pathStates(ctx context.Context, dir, sha string, paths []string, states []TermState, fail func(string)) {
	for i, p := range paths {
		states[i] = TermState{Term: p, Kind: "path"}
	}
	if len(paths) == 0 {
		return
	}
	o, trunc, err := g.run(ctx, dir, treeMax, "ls-tree", "-r", "--name-only", "-z", sha)
	switch {
	case err != nil:
		fail("file list failed: " + shortErr(err))
	case trunc:
		fail("file list cut at 16 MB")
	}
	// A list that failed or was cut can show that a path is present, never that
	// it is absent: such a path is neither present nor absent.
	incomplete := err != nil || trunc
	files := strings.Split(o, "\x00")
	for i, p := range paths {
		st := &states[i]
		var re *regexp.Regexp
		switch {
		case isGlob(p):
			re = reGlob(p)
		case strings.HasSuffix(p, "/"):
			re = reGlob(p)
		default:
			re = regexp.MustCompile("^" + regexp.QuoteMeta(p) + "(?:/.*)?$")
		}
		for _, f := range files {
			if f != "" && re.MatchString(f) {
				st.Count++
				if len(st.Where) < 2 {
					st.Where = append(st.Where, f)
				}
			}
		}
		st.Present = st.Count > 0
		st.Unknown = incomplete && !st.Present
	}
}

// identState greps the tree at sha for one literal identifier. Only a clean
// "no match" (exit status 1) means absent: any other failure, a timeout with
// output half read included, is a note and the term's state is unknown.
func (g *Git) identState(ctx context.Context, dir, sha, id string, fail func(string)) TermState {
	st := TermState{Term: id, Kind: "ident"}
	args := []string{"grep", "-l", "-I", "-F", "-e", id, sha, "--", "."}
	args = append(args, noise...)
	o, trunc, err := g.run(ctx, dir, 8<<10, args...)
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		fail("search of " + clip(id, 40) + " failed: " + shortErr(err))
		st.Unknown = true
		return st
	}
	for _, l := range strings.Split(strings.TrimSpace(o), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = strings.TrimPrefix(l, sha+":")
		st.Count++
		if len(st.Where) < 2 {
			st.Where = append(st.Where, l)
		}
	}
	if trunc && st.Count < genericFiles {
		st.Count = genericFiles
	}
	st.Present = st.Count > 0
	st.Generic = st.Count >= genericFiles
	return st
}

func (g *Git) cacheKey(name, sha string, s Subject, w Window, limit int) string {
	if g.CacheDir == "" {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%d|%v|%v|%v", cacheVersion, name, sha, w.Since.Unix(), w.Anchor.Unix(), limit, s.Paths, s.Idents, w.Light)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (g *Git) cacheGet(key string) (RepoEvidence, bool) {
	var ev RepoEvidence
	if key == "" {
		return ev, false
	}
	b, err := os.ReadFile(filepath.Join(g.CacheDir, key+".json"))
	if err != nil || json.Unmarshal(b, &ev) != nil || ev.Incomplete {
		return ev, false
	}
	return ev, true
}

// cacheMaxFiles bounds the disk cache: one file per repo, subject and HEAD,
// a few KB each, so 4000 files stay under about 40 MB. The oldest go first.
const cacheMaxFiles = 4000

func (g *Git) cachePut(key string, ev RepoEvidence) {
	if key == "" || ev.Incomplete {
		// An incomplete read is not worth keeping: the next run may finish it.
		return
	}
	if os.MkdirAll(g.CacheDir, 0o700) != nil {
		return
	}
	b, _ := json.Marshal(ev)
	f, err := os.CreateTemp(g.CacheDir, key+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), filepath.Join(g.CacheDir, key+".json")) != nil {
		os.Remove(f.Name())
		return
	}
	g.mu.Lock()
	g.puts++
	prune := g.puts%64 == 1
	g.mu.Unlock()
	if prune {
		g.pruneCache()
	}
}

// pruneCache removes the oldest cache files until a quarter of the allowance
// is free again.
func (g *Git) pruneCache() {
	max := g.CacheMaxFiles
	if max <= 0 {
		max = cacheMaxFiles
	}
	ents, err := os.ReadDir(g.CacheDir)
	if err != nil || len(ents) <= max {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var fs []file
	for _, e := range ents {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			fs = append(fs, file{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) })
	for len(fs) > max*3/4 {
		os.Remove(filepath.Join(g.CacheDir, fs[0].name))
		fs = fs[1:]
	}
}
