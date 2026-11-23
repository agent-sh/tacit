package main

// `tacit dedupe --near` (SPEC.md, Near-duplicates, "Near-copies"): one fact
// per rule. Code finds clusters of facts of one folder that may say the same
// thing (curate.NearCandidates); the curator's model says which of them do,
// a cluster at a time, batched; code keeps one fact of each set and
// retires the rest with superseded_by it, as `tacit dedupe` does. The
// model's answers are kept in the state directory by cluster, so a dry run
// pays for them and the real run that follows reads them back: the real run
// asks the model before it takes the store lock, and under the lock uses
// only answers for clusters that are still as they were asked.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// nearRun is the near pass's model call; tests substitute it.
var nearRun = curate.JudgeCLI

// nearBatch is how many clusters one model call reads.
const nearBatch = 10

// nearState is the answers the model gave, by cluster key: the sets of
// 0-based fact indices that say the same thing.
type nearState struct {
	Answers map[string][][]int `json:"answers"`
}

// nearStatePath is the answers' file for the store at dir: one file per
// store, since a key hashes paths within it.
func nearStatePath(dir string) string {
	home, _ := os.UserHomeDir()
	h := sha256.Sum256([]byte(dir))
	return filepath.Join(platform.StateDir(home), "dedupe-near-"+hex.EncodeToString(h[:4])+".json")
}

func readNearState(dir string) nearState {
	st := nearState{Answers: map[string][][]int{}}
	if b, err := os.ReadFile(nearStatePath(dir)); err == nil {
		_ = json.Unmarshal(b, &st)
		if st.Answers == nil {
			st.Answers = map[string][][]int{}
		}
	}
	return st
}

func writeNearState(dir string, st nearState) error {
	p := nearStatePath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// nearOpts are the near pass's settings.
type nearOpts struct {
	model       string
	jobs        int
	maxClusters int // 0: every cluster
	list        bool
}

// nearAsk asks the model about every cluster of dir that has no answer yet,
// in batches, jobs at a time, and keeps each answer as it comes. A batch
// whose answer does not read is asked again cluster by cluster; a cluster
// that fails alone stays unanswered for the next run. It returns how many
// clusters it asked about and the errors.
func nearAsk(ctx context.Context, dir string, cs []curate.NearCluster, o nearOpts, st *nearState) (int, []error) {
	var todo []curate.NearCluster
	for _, c := range cs {
		if _, ok := st.Answers[c.Key]; !ok {
			todo = append(todo, c)
		}
	}
	if o.maxClusters > 0 && len(todo) > o.maxClusters {
		todo = todo[:o.maxClusters]
	}
	var batches [][]curate.NearCluster
	for i := 0; i < len(todo); i += nearBatch {
		batches = append(batches, todo[i:min(i+nearBatch, len(todo))])
	}
	jobs := max(o.jobs, 1)
	var mu sync.Mutex
	var errs []error
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	save := func(ans map[string][][]int) {
		mu.Lock()
		defer mu.Unlock()
		for k, v := range ans {
			st.Answers[k] = v
		}
		if err := writeNearState(dir, *st); err != nil {
			errs = append(errs, err)
		}
	}
	// An answer that does not read is the batch's: it is asked again
	// cluster by cluster. A call that failed (the route, the runner) is not.
	var errUnread = errors.New("unread")
	ask := func(b []curate.NearCluster) (map[string][][]int, error) {
		res, _, err := nearRun(ctx, o.model, curate.NearPrompt(b))
		if err != nil {
			return nil, err
		}
		v, err := curate.ParseNear(res, b)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errUnread, err)
		}
		return v, nil
	}
	for _, b := range batches {
		wg.Add(1)
		sem <- struct{}{}
		go func(b []curate.NearCluster) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			ans, err := ask(b)
			if errors.Is(err, errUnread) && len(b) > 1 {
				fmt.Fprintf(os.Stderr, "dedupe --near: a batch of %d: %v; asking its clusters one at a time\n", len(b), err)
				ans, err = map[string][][]int{}, nil
				for _, c := range b {
					one, e := ask([]curate.NearCluster{c})
					if e != nil {
						mu.Lock()
						errs = append(errs, fmt.Errorf("cluster %s: %w", c.Key, e))
						mu.Unlock()
						continue
					}
					for k, v := range one {
						ans[k] = v
					}
				}
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			save(ans)
		}(b)
	}
	wg.Wait()
	return len(todo), errs
}

// nearPlan is the copies the answers in st make of dir's clusters, under
// dedupe's guards, and how many clusters have no answer (a cluster that
// changed since it was asked). No model.
func nearPlan(dir string, st nearState) ([]curate.Copies, int, error) {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return nil, 0, err
	}
	cs := curate.NearCandidates(facts)
	unanswered := 0
	for _, c := range cs {
		if _, ok := st.Answers[c.Key]; !ok {
			unanswered++
		}
	}
	g := dedupeGuard(dir)
	return curate.NearCopies(cs, st.Answers, g), unanswered, nil
}

// runDedupeNear is `tacit dedupe --near`.
func runDedupeNear(dry bool, o nearOpts, out string) int {
	s, err := storeForDedupe(dry)
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	// A real run asks about the store as the server has it, so the answers
	// still hold under the lock.
	if !dry {
		if err := s.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "dedupe --near: sync:", err)
			return 1
		}
	}
	st := readNearState(s.Dir)
	facts, _, err := fact.Walk(s.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dedupe --near:", err)
		return 1
	}
	cs := curate.NearCandidates(facts)
	if o.list {
		n := 0
		for _, c := range cs {
			n += len(c.Facts)
			fmt.Printf("cluster %s (%s):\n", c.Key, c.Scope)
			for _, f := range c.Facts {
				fmt.Printf("  - %s: %s\n", f.Meta.Name, f.Meta.Description)
			}
		}
		fmt.Printf("dedupe --near: %d candidate cluster(s) of %d fact(s)\n", len(cs), n)
		return 0
	}
	asked, errs := nearAsk(ctx, s.Dir, cs, o, &st)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "dedupe --near:", e)
	}
	answered := 0
	for _, c := range cs {
		if _, ok := st.Answers[c.Key]; ok {
			answered++
		}
	}
	fmt.Printf("dedupe --near: %d candidate cluster(s), %d answered (%d asked now)\n", len(cs), answered, asked)
	var copies []curate.Copies
	unanswered := 0
	if dry {
		copies, unanswered, err = nearPlan(s.Dir, st)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dedupe --near:", err)
			return 1
		}
	} else {
		wctx, wcancel := context.WithTimeout(ctx, 20*time.Minute)
		defer wcancel()
		pushed, err := curatorWrite(wctx, s, func() (map[string][]byte, string, error) {
			var err error
			if copies, unanswered, err = nearPlan(s.Dir, st); err != nil || len(copies) == 0 {
				return nil, "", err
			}
			files, msg, err := dedupeFiles(s.Dir, copies, time.Now())
			if msg != "" {
				msg = "curator: dedupe --near: same rule or fact in other words (the curator's model read each cluster; code kept the fact with the owner's words, then the earliest)" + msg[len("curator: dedupe"):]
			}
			return files, msg, err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "dedupe --near:", err)
			return 1
		}
		if !pushed {
			copies = nil
		}
		// Answers of clusters that are no more are dropped.
		live := map[string]bool{}
		for _, c := range cs {
			live[c.Key] = true
		}
		for k := range st.Answers {
			if !live[k] {
				delete(st.Answers, k)
			}
		}
		_ = writeNearState(s.Dir, st)
	}
	if unanswered > 0 {
		fmt.Printf("dedupe --near: %d cluster(s) have no answer yet (not asked, failed, or changed since they were asked); the next run asks them\n", unanswered)
	}
	sort.SliceStable(copies, func(i, j int) bool { return copies[i].Kept.Path < copies[j].Kept.Path })
	printDedupe(copies, dry)
	if code := writeDedupeJSON(out, copies); code != 0 {
		return code
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}
