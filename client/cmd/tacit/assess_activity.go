package main

// Checkable evidence up front (SPEC.md, "Folder records", Activity): before
// a project folder's first model call code gathers every date it can check
// of the owner's work on it and the forge's record of its repository, from
// caches (the commit evidence by the checkout's HEAD, the forge's answers a
// day), so every run of a day reads the same and decides alike. v0.9.2 had
// the forge's commits only on a re-attempt, so whether a run saw a folder's
// newest commit depended on how its first ask went.

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// forgeActivityCommits is how many of the forge's newest commits a folder's
// rhythm reads: one call of the forge's API either way (it pages at 100),
// and 20 (v0.9.2's) is one busy day of a busy repository.
const forgeActivityCommits = 100

// activity gathers c's Activity: the owner's activity days (own commits in
// a checkout here, other machines' catalogs' first and last own commit and
// work, the sessions that worked in it here or filed its notes, the forge's
// commits by the owner's account), the forge's record of its repository,
// and code's status from them. A project folder only; nil when there is
// nothing.
func (a *assessor) activity(c *assess.Candidate) {
	c.Activity = nil
	name, ok := strings.CutPrefix(c.Folder, "projects/")
	if !ok || c.Kind != "" {
		return
	}
	act := &assess.Activity{}
	shown := map[string]string{} // day -> a shown own commit of that day
	if r, ok := a.catalog.Repo(name); ok && r.Root != "" {
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		if cm, ok := a.commits.Get(ctx, r.Root); ok {
			for _, oc := range repoid.RecentOwnCommits(ctx, r.Root, ownCommitsShown) {
				if _, ok := shown[oc.Day]; !ok {
					shown[oc.Day] = oc.Hash
				}
			}
			for _, d := range cm.Days {
				cite := ""
				if h := shown[d]; h != "" {
					cite = "commit:" + h
				}
				act.AddDay(d, "commit", cite)
			}
		}
		cancel()
		for _, w := range a.catalog.Work(r.Root) {
			act.AddDay(w.Seen, "session", "session:"+w.Session)
		}
	} else if a.host != "" {
		for p, ws := range repoid.FolderWork(a.reg) {
			if filepath.Base(p) == name {
				for _, w := range ws {
					act.AddDay(w.Seen, "session", "session:"+w.Session)
				}
			}
		}
	}
	for _, h := range a.shared[name] {
		if h.Host == a.host {
			continue
		}
		if h.OwnCommits != nil && *h.OwnCommits > 0 {
			act.AddDay(h.FirstCommit, "catalog", "")
			act.AddDay(h.LastCommit, "catalog", "")
		}
		act.AddDay(h.FirstWork, "catalog", "")
		act.AddDay(h.LastWork, "catalog", "")
	}
	// The sessions that filed its notes, each on its newest day there.
	last := map[string]string{}
	for _, f := range a.facts {
		if f.Meta.Scope == c.Folder && f.Meta.Source.Session != "" && f.Meta.Created > last[f.Meta.Source.Session] {
			last[f.Meta.Source.Session] = f.Meta.Created
		}
	}
	for _, id := range sortedKeysStr(last) {
		act.AddDay(last[id], "session", "session:"+id)
	}
	a.forgeActivity(c, act)
	if len(act.Days) == 0 && act.Forge == nil && !act.Deleted {
		return
	}
	act.Measure(a.today)
	c.Activity = act
	if a.codeStatus == nil {
		a.codeStatus = map[string]string{}
	}
	a.codeStatus[c.Folder] = act.Status
}

// forgeActivity fills in the forge's record of c's repository: its forge
// name (the check's), else the first forge URL of its remotes. A remote's
// repository the forge has under no name, and renamed under none, is
// deleted.
func (a *assessor) forgeActivity(c *assess.Candidate, act *assess.Activity) {
	if a.checker == nil || a.checker.Forge == nil {
		return
	}
	full := forgeFull(c)
	if full == "" {
		full = a.remoteFull(c)
	}
	owner, name, ok := strings.Cut(full, "/")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
	defer cancel()
	fa, err := a.checker.Forge.Activity(ctx, owner, name, forgeActivityCommits)
	if errors.Is(err, check.ErrNotFound) {
		act.Deleted = c.Name != nil && c.Name.Renamed == "" && !c.Absent() && forgeFails(c.Name)
		return
	}
	if err != nil {
		return
	}
	act.Forge = &fa
	act.User = strings.ToLower(a.checker.Forge.Forge.User(ctx))
	if act.User == "" {
		return
	}
	for _, cm := range fa.Commits {
		if strings.EqualFold(cm.Login, act.User) {
			act.AddDay(cm.Day, "forge", "commit:"+shortSHA(cm.SHA))
			a.commitsShown[c.Folder] = append(a.commitsShown[c.Folder], strings.ToLower(cm.SHA))
		}
	}
}

// remoteFull is owner/name of the first github.com remote the folder's
// evidence names.
func (a *assessor) remoteFull(c *assess.Candidate) string {
	for _, e := range c.Evidence {
		for _, u := range e.Remotes {
			if rest, ok := strings.CutPrefix(u, "github.com/"); ok && strings.Count(rest, "/") == 1 {
				return strings.TrimSuffix(rest, ".git")
			}
		}
	}
	return ""
}

// forgeFails reports the name check's forge result failing: the forge
// answered that no owner it asked has the name.
func forgeFails(rc *check.RepoCheck) bool {
	for _, r := range rc.Results {
		if strings.HasPrefix(r.Source, "forge:") && r.Verdict == check.Fails {
			return true
		}
	}
	return false
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func sortedKeysStr(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// prefetchWorkers is how many repositories the prefetch asks the forge
// about at once: three calls each, about a second a call through gh, and
// 111 repositories on the Mac's shadow store took 20 minutes one at a time.
const prefetchWorkers = 8

// prefetchActivity asks the forge, a few at a time, for the activity of the
// repositories of every project folder this run may gather (the catalogs'
// and the store's, and the records'), so gathering a folder reads the
// cache. Each folder's name check comes first, one at a time (the checker
// keeps them), as gathering reads it.
func (a *assessor) prefetchActivity() {
	if a.prefetched || a.checker == nil || a.checker.Forge == nil {
		return
	}
	a.prefetched = true
	set := map[string]bool{}
	for _, f := range a.ancestorUniverse() {
		set[f] = true
	}
	for f := range a.recs {
		if strings.HasPrefix(f, "projects/") {
			set[f] = true
		}
	}
	want := map[string]bool{}
	for _, f := range sortedKeysBool(set) {
		name := strings.TrimPrefix(f, "projects/")
		full := ""
		if rc := a.checker.Repo(a.ctx, name); rc.Forge != "" {
			full = rc.Forge
		} else if r := a.recs[f]; r != nil && r.Forge != "" {
			full = r.Forge[strings.Index(r.Forge, "/")+1:]
		} else {
			var urls []string
			if r, ok := a.catalog.Repo(name); ok {
				urls = append(urls, r.URLs...)
			}
			for _, h := range a.shared[name] {
				urls = append(urls, h.URLs...)
			}
			for _, u := range urls {
				if rest, ok := strings.CutPrefix(u, "github.com/"); ok && strings.Count(rest, "/") == 1 {
					full = strings.TrimSuffix(rest, ".git")
					break
				}
			}
		}
		if strings.Contains(full, "/") {
			want[strings.ToLower(full)] = true
		}
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < prefetchWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for full := range jobs {
				owner, name, _ := strings.Cut(full, "/")
				ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
				a.checker.Forge.Activity(ctx, owner, name, forgeActivityCommits)
				cancel()
			}
		}()
	}
	for _, full := range sortedKeysBool(want) {
		jobs <- full
	}
	close(jobs)
	wg.Wait()
	a.checker.Forge.Forge.User(a.ctx)
}
