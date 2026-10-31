package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/store"
)

// newTimelineBuilder reads checkouts under $TACIT_PROJECTS (else ~/projects)
// and keeps its cache in the user cache directory. asOf, when set, reads
// every repo as it was then (a replay of an old decision). derive.yaml's
// ordinary_names apply; a config that does not load is reported by the steps
// that need its branches, so here it only leaves the names out.
func newTimelineBuilder(s *store.Store, asOf time.Time) *gitev.Builder {
	g := gitev.New(defaultProjects())
	if cfg, err := derive.LoadConfig(derive.ConfigPath()); err == nil {
		g.Ordinary = cfg.Ordinary()
	}
	if cd, err := os.UserCacheDir(); err == nil {
		g.CacheDir = filepath.Join(cd, "tacit", "timeline")
	}
	return &gitev.Builder{G: g, StoreDir: s.Dir, AsOf: asOf}
}

// timelineInput pairs a finding's two facts with the inbox notes they were
// split from.
func (rs *reviewServer) timelineInput(it reviewItem) (gitev.Input, bool) {
	a, b := rs.facts[it.A.Name], rs.facts[it.B.Name]
	if it.Kind == kindName {
		// Two files of one name: each side is its own file.
		a, b = rs.byPath[it.A.Path], rs.byPath[it.B.Path]
	}
	if a == nil || b == nil {
		return gitev.Input{}, false
	}
	// Copies: a mark made while the timeline is read changes the stored facts.
	ref := func(f *fact.Fact) gitev.FactRef {
		c := *f
		r := gitev.FactRef{Fact: &c}
		if o := rs.inbox[f.Meta.Origin]; o != nil {
			oc := *o
			r.Origin = &oc
		}
		return r
	}
	return gitev.Input{A: ref(a), B: ref(b)}, true
}

// timelineFor builds the timeline of one finding. A finished, complete
// timeline is remembered for ten minutes: the page asks again each time the
// owner comes back to a proof. Requests for the same proof share one build,
// two builds run at once at most (each runs up to nine git processes), and a
// build nobody is waiting for any more is cancelled. A timeline with a read
// that failed or ran out of time is returned but never remembered.
func (rs *reviewServer) timelineFor(ctx context.Context, id string) (*gitev.Timeline, error) {
	rs.mu.Lock()
	var it *reviewItem
	for _, x := range rs.items() {
		if x.ID == id {
			xx := x
			it = &xx
		}
	}
	if it == nil {
		it = rs.nameItemByID(id)
	}
	var in gitev.Input
	var key string
	if it == nil {
		// A stale finding is about one fact: its timeline has no second side.
		st := rs.staleByID(id)
		if st == nil {
			rs.mu.Unlock()
			return nil, errors.New("no such finding")
		}
		ref, ok := rs.staleFactRef(st.A.Name)
		if !ok {
			rs.mu.Unlock()
			return nil, errors.New("the fact is gone from the store")
		}
		in, key = gitev.Input{A: ref}, id+"|"+st.A.Path+st.A.Updated
	} else {
		var ok bool
		if in, ok = rs.timelineInput(*it); !ok {
			rs.mu.Unlock()
			return nil, errors.New("one of the facts is gone from the store")
		}
		key = id + "|" + it.A.Path + it.A.Updated + "|" + it.B.Path + it.B.Updated
	}
	return rs.flightLocked(ctx, key, in)
}

// flightLocked returns the timeline for key, from the memo, from a build
// already running, or from a new build. It is called with rs.mu held and
// releases it.
func (rs *reviewServer) flightLocked(ctx context.Context, key string, in gitev.Input) (*gitev.Timeline, error) {
	if m, hit := rs.tlMemo[key]; hit && time.Since(m.at) < 10*time.Minute {
		rs.mu.Unlock()
		return m.t, nil
	}
	f := rs.tlFlights[key]
	if f == nil {
		bctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		f = &tlFlight{done: make(chan struct{}), cancel: cancel}
		if rs.tlFlights == nil {
			rs.tlFlights = map[string]*tlFlight{}
		}
		if rs.tlSem == nil {
			rs.tlSem = make(chan struct{}, 2)
		}
		rs.tlFlights[key] = f
		go rs.runFlight(bctx, key, in, f)
	}
	f.waiters++
	rs.mu.Unlock()
	select {
	case <-f.done:
		return f.t, f.err
	case <-ctx.Done():
		rs.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			// Nobody wants it: stop the build, and let the next request start its own.
			f.cancel()
			if rs.tlFlights[key] == f {
				delete(rs.tlFlights, key)
			}
		}
		rs.mu.Unlock()
		return nil, ctx.Err()
	}
}

// tlFlight is one running build that every request for the same proof waits on.
type tlFlight struct {
	done    chan struct{}
	t       *gitev.Timeline
	err     error
	waiters int
	cancel  context.CancelFunc
}

func (rs *reviewServer) runFlight(ctx context.Context, key string, in gitev.Input, f *tlFlight) {
	defer f.cancel()
	var t *gitev.Timeline
	var err error
	select {
	case rs.tlSem <- struct{}{}:
		if in.B.Fact == nil {
			t, err = rs.tl.BuildOne(ctx, in.A)
		} else {
			t, err = rs.tl.Build(ctx, in)
		}
		<-rs.tlSem
	case <-ctx.Done():
		err = ctx.Err()
	}
	rs.mu.Lock()
	if rs.tlFlights[key] == f {
		delete(rs.tlFlights, key)
	}
	if err == nil && ctx.Err() == nil && !t.Incomplete {
		if rs.tlMemo == nil {
			rs.tlMemo = map[string]tlMemo{}
		}
		rs.tlMemo[key] = tlMemo{t: t, at: time.Now()}
	}
	rs.mu.Unlock()
	f.t, f.err = t, err
	close(f.done)
}

type tlMemo struct {
	t  *gitev.Timeline
	at time.Time
}

// handleTimeline serves GET /api/timeline?id=: the git timeline of one proof.
// It carries commit subjects and file names from every repo under ~/projects,
// so like a mark it needs the page's token. A request that goes away stops
// its build.
func (rs *reviewServer) handleTimeline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || !tokenOK(r.Header.Get("X-Tacit-Token"), rs.token) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
		return
	}
	if rs.tl == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no timeline on this server"})
		return
	}
	to := rs.tlTimeout
	if to <= 0 {
		to = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), to)
	defer cancel()
	t, err := rs.timelineFor(ctx, r.URL.Query().Get("id"))
	if err != nil {
		if r.Context().Err() != nil {
			return // the page left; nobody reads an answer
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Our own limit: say so, so the page does not read silence as "nothing".
			w.WriteHeader(http.StatusGatewayTimeout)
			json.NewEncoder(w).Encode(map[string]string{"error": "reading git took too long on this machine; try again in a moment"})
			return
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(t)
}

// parseAsOf reads a replay time: RFC 3339, "2006-01-02 15:04" or a date, the
// last two in local time.
func parseAsOf(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		var t time.Time
		var err error
		if layout == time.RFC3339 {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, time.Local)
		}
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time (RFC 3339, YYYY-MM-DD HH:MM or YYYY-MM-DD)", s)
}

// runTimeline implements `tacit timeline [--as-of T] [--json] [--ids FILE] [ID...]`:
// print the git timeline of review findings, built without a model.
func runTimeline(args []string) int {
	fsf := flag.NewFlagSet("timeline", flag.ExitOnError)
	asOf := fsf.String("as-of", "", "read every repo as it was at this time (RFC 3339, YYYY-MM-DD HH:MM or YYYY-MM-DD)")
	asJSON := fsf.Bool("json", false, "print the timelines as JSON")
	ids := fsf.String("ids", "", "JSON array of {id} findings, as for tacit verify --ids")
	fsf.Parse(args)
	want := fsf.Args()
	if *ids != "" {
		b, err := os.ReadFile(*ids)
		if err != nil {
			fmt.Fprintln(os.Stderr, "timeline:", err)
			return 2
		}
		var items []struct{ ID string }
		if err := json.Unmarshal(b, &items); err != nil {
			fmt.Fprintln(os.Stderr, "timeline: --ids needs a JSON array of {id}")
			return 2
		}
		for _, it := range items {
			want = append(want, it.ID)
		}
	}
	if len(want) == 0 {
		fmt.Fprintln(os.Stderr, "timeline: name finding ids, or pass --ids")
		return 2
	}
	var at time.Time
	if *asOf != "" {
		var err error
		if at, err = parseAsOf(*asOf); err != nil {
			fmt.Fprintln(os.Stderr, "timeline:", err)
			return 2
		}
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "timeline: redaction needs scrump with the text profile:", err)
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "timeline: no store")
		return 2
	}
	rs := &reviewServer{s: s, tl: newTimelineBuilder(s, at)}
	rs.loadFacts()
	byID := map[string]reviewItem{}
	for _, it := range append(rs.items(), rs.nameItems()...) {
		byID[it.ID] = it
	}
	code := 0
	for _, id := range want {
		it, ok := byID[id]
		if !ok {
			fmt.Fprintf(os.Stderr, "timeline: %s is not a finding in this store\n", id)
			code = 1
			continue
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		t, err := rs.timelineFor(ctx, id)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "timeline: %s: %v\n", id, err)
			code = 1
			continue
		}
		if *asJSON {
			b, _ := json.Marshal(map[string]any{"id": id, "a": it.A.Name, "b": it.B.Name, "timeline": t})
			fmt.Println(string(b))
		} else {
			fmt.Printf("%s  A %s  B %s\n  repos %s; terms %s\n", id, it.A.Name, it.B.Name, strings.Join(t.Repos, ", "), strings.Join(t.Terms, ", "))
			for _, l := range t.Compact().Lines {
				fmt.Println("  " + l)
			}
		}
		fmt.Fprintf(os.Stderr, "timeline: %s took %.1fs\n", id, time.Since(start).Seconds())
	}
	return code
}
