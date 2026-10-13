package follow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// feedServer is a scripted /events endpoint. script gets the 1-based
// connection number and writes SSE to w until it returns (which closes the
// connection) or the client goes away.
type feedServer struct {
	*httptest.Server
	mu      sync.Mutex
	lastIDs []string // Last-Event-ID header of each connection
	auths   []string
}

func newFeed(t *testing.T, script func(n int, w http.ResponseWriter, r *http.Request)) *feedServer {
	t.Helper()
	fs := &feedServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.lastIDs = append(fs.lastIDs, r.Header.Get("Last-Event-ID"))
		fs.auths = append(fs.auths, r.Header.Get("Authorization"))
		n := len(fs.lastIDs)
		fs.mu.Unlock()
		script(n, w, r)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (f *feedServer) conns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lastIDs)
}

func (f *feedServer) lastID(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIDs[n-1]
}

// stream starts an event stream response.
func stream(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	fl.Flush()
	return fl
}

func emit(w http.ResponseWriter, fl http.Flusher, format string, args ...any) {
	fmt.Fprintf(w, format, args...)
	fl.Flush()
}

func event(seq int) string {
	return fmt.Sprintf("id: %d\nevent: head_moved\ndata: {\"seq\":%d}\n\n", seq, seq)
}

// hold keeps the connection open until the client leaves.
func hold(r *http.Request) { <-r.Context().Done() }

type rig struct {
	t         *testing.T
	statePath string
	store     string
	syncs     atomic.Int32
	done      chan struct{}
	cancel    context.CancelFunc
}

// start runs a follower against url with test-sized timings.
func start(t *testing.T, url string, mod func(*Options, *rig)) *rig {
	t.Helper()
	r := &rig{t: t, statePath: filepath.Join(t.TempDir(), "follow.json"), store: "/store/a", done: make(chan struct{})}
	o := Options{
		URL:         url,
		StatePath:   r.statePath,
		Store:       r.store,
		Debounce:    60 * time.Millisecond,
		MaxWait:     400 * time.Millisecond,
		Idle:        5 * time.Second,
		Poll:        time.Hour,
		Reconnect:   Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond},
		Retry:       Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond},
		Healthy:     time.Hour,
		SyncTimeout: 5 * time.Second,
		Logf:        func(f string, a ...any) { t.Logf(f, a...) },
		Sync: func(ctx context.Context) (string, error) {
			n := r.syncs.Add(1)
			return fmt.Sprintf("head%d", n), nil
		},
	}
	if mod != nil {
		mod(&o, r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer close(r.done)
		if err := Run(ctx, o); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(r.stop)
	return r
}

func (r *rig) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.t.Error("follower did not stop")
	}
}

func (r *rig) state() State {
	st, err := Load(r.statePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A burst of twenty pushes, plus the startup sync, is one sync, and the
// state records the newest seq.
func TestBurstOfEventsIsOneSync(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=20\n\n")
		for i := 1; i <= 20; i++ {
			emit(w, fl, "%s", event(i))
		}
		hold(r)
	})
	r := start(t, srv.URL, func(o *Options, _ *rig) { o.Debounce = 500 * time.Millisecond; o.MaxWait = 3 * time.Second })
	eventually(t, "seq 20 synced", func() bool { st := r.state(); return st.Seq == 20 && !st.Behind })
	time.Sleep(time.Second) // a second sync would show up here
	if n := r.syncs.Load(); n != 1 {
		t.Fatalf("%d syncs for one burst, want 1", n)
	}
	if st := r.state(); st.Head != "head1" || st.Store != r.store || st.PID == 0 {
		t.Fatalf("state: %+v", st)
	}
}

// Events that keep arriving cannot postpone the sync past MaxWait.
func TestUnbrokenStreamStillSyncsAtMaxWait(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		for i := 1; i <= 40; i++ {
			emit(w, fl, "%s", event(i))
			time.Sleep(25 * time.Millisecond)
		}
		hold(r)
	})
	r := start(t, srv.URL, func(o *Options, _ *rig) { o.Debounce = 200 * time.Millisecond; o.MaxWait = 350 * time.Millisecond })
	eventually(t, "seq 40 synced", func() bool { return r.state().Seq == 40 })
	n := r.syncs.Load()
	if n < 2 || n > 8 {
		t.Fatalf("%d syncs over a 1 s stream with MaxWait 350 ms, want 2..8", n)
	}
}

// A restart resumes the feed from the persisted seq, and what it syncs is
// written back for the next restart.
func TestResumeFromPersistedSeq(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=43\n\n")
		if n == 1 {
			emit(w, fl, "%s%s", event(42), event(43))
		}
		hold(r)
	})
	path := filepath.Join(t.TempDir(), "follow.json")
	if err := Save(path, State{PID: 1, Store: "/store/a", Seq: 41, Head: "old"}); err != nil {
		t.Fatal(err)
	}
	r := start(t, srv.URL, func(o *Options, r *rig) { o.StatePath = path; r.statePath = path })
	eventually(t, "seq 43", func() bool { return r.state().Seq == 43 })
	if got := srv.lastID(1); got != "41" {
		t.Fatalf("first connection sent Last-Event-ID %q, want 41", got)
	}
	r.stop()
	if st := r.state(); st.PID != 0 || st.Connected {
		t.Fatalf("a stopped follower left a live heartbeat: %+v", st)
	}

	r2 := start(t, srv.URL, func(o *Options, r *rig) { o.StatePath = path; r.statePath = path })
	eventually(t, "second run connects", func() bool { return srv.conns() == 2 })
	if got := srv.lastID(2); got != "43" {
		t.Fatalf("restart sent Last-Event-ID %q, want 43", got)
	}
	r2.stop()
}

// Another clone's seq means nothing here.
func TestStateOfAnotherStoreIsIgnored(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		hold(r)
	})
	path := filepath.Join(t.TempDir(), "follow.json")
	Save(path, State{Store: "/store/other", Seq: 99})
	start(t, srv.URL, func(o *Options, r *rig) { o.StatePath = path; r.statePath = path })
	eventually(t, "connect", func() bool { return srv.conns() == 1 })
	if got := srv.lastID(1); got != "" {
		t.Fatalf("sent Last-Event-ID %q from another store's state", got)
	}
}

// A dropped connection is reopened with the newest seq seen, and events on
// the new connection are handled.
func TestReconnectResumesFromSeen(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		switch n {
		case 1:
			emit(w, fl, ": connected head=1\n\n%s", event(1))
			time.Sleep(150 * time.Millisecond) // let it sync, then drop
		case 2:
			emit(w, fl, ": connected head=2\n\n%s", event(2))
			hold(r)
		}
	})
	r := start(t, srv.URL, nil)
	eventually(t, "seq 2 synced", func() bool { return r.state().Seq == 2 })
	if got := srv.lastID(2); got != "1" {
		t.Fatalf("reconnect sent Last-Event-ID %q, want 1", got)
	}
	if srv.conns() != 2 {
		t.Fatalf("%d connections", srv.conns())
	}
}

// A stream that goes silent is dropped after Idle and reopened.
func TestSilentStreamIsReopened(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		if n == 1 {
			hold(r) // never a keepalive
			return
		}
		for r.Context().Err() == nil {
			emit(w, fl, ": keepalive\n\n")
			time.Sleep(20 * time.Millisecond)
		}
	})
	start(t, srv.URL, func(o *Options, _ *rig) { o.Idle = 150 * time.Millisecond })
	eventually(t, "second connection", func() bool { return srv.conns() >= 2 })
	time.Sleep(400 * time.Millisecond) // keepalives hold the second one open
	if n := srv.conns(); n != 2 {
		t.Fatalf("%d connections; keepalives should have kept the second open", n)
	}
}

// Errors from the server (wrong status, wrong type) are retried with
// backoff, and while the stream is down the fallback poll still syncs.
func TestBadStreamRetriesAndPolls(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch {
		case n%2 == 1:
			http.Error(w, "boom", 500)
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>captive portal</html>"))
		}
	})
	r := start(t, srv.URL, func(o *Options, _ *rig) { o.Poll = 80 * time.Millisecond })
	eventually(t, "three syncs from the poll", func() bool { return r.syncs.Load() >= 3 })
	if srv.conns() < 3 {
		t.Fatalf("only %d connection attempts", srv.conns())
	}
	if st := r.state(); st.Connected {
		t.Fatalf("claims connected: %+v", st)
	}
	if Current(r.statePath, r.store, time.Now()) {
		t.Fatal("a follower that cannot reach the feed must not look fresh")
	}
}

// A failing sync is retried; the seq is persisted only once it works.
func TestFailedSyncKeepsSeqUntilItSucceeds(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=7\n\n%s", event(7))
		hold(r)
	})
	var calls atomic.Int32
	r := start(t, srv.URL, func(o *Options, r *rig) {
		o.Sync = func(ctx context.Context) (string, error) {
			if calls.Add(1) <= 2 {
				return "", errors.New("network down")
			}
			return "good", nil
		}
	})
	eventually(t, "third sync", func() bool { return calls.Load() >= 3 })
	eventually(t, "seq saved", func() bool { return r.state().Seq == 7 })
	if st := r.state(); st.Behind || st.Head != "good" {
		t.Fatalf("state: %+v", st)
	}
}

// The heartbeat means "connected and nothing owed": an announced push that is
// not merged yet takes it away at once, and the merge gives it back.
func TestHeartbeatFollowsCaughtUp(t *testing.T) {
	release := make(chan struct{})
	push := make(chan struct{})
	entered := make(chan struct{}, 1)
	var gate atomic.Bool
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=1\n\n%s", event(1))
		select {
		case <-push:
			emit(w, fl, "%s", event(2))
		case <-r.Context().Done():
			return
		}
		hold(r)
	})
	r := start(t, srv.URL, func(o *Options, r *rig) {
		o.Sync = func(ctx context.Context) (string, error) {
			if gate.Load() {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return "h", nil
		}
	})
	eventually(t, "first sync", func() bool { return r.state().Seq == 1 && !r.state().Behind })
	if !Current(r.statePath, r.store, time.Now()) {
		t.Fatalf("caught-up follower not current: %+v", r.state())
	}
	if Current(r.statePath, "/store/other", time.Now()) {
		t.Fatal("current for another store")
	}
	gate.Store(true)
	close(push)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the push never reached the sync")
	}
	if Current(r.statePath, r.store, time.Now()) {
		t.Fatalf("current while a push is unmerged: %+v", r.state())
	}
	close(release)
	eventually(t, "caught up again", func() bool { return Current(r.statePath, r.store, time.Now()) })
	if st := r.state(); st.Seq != 2 {
		t.Fatalf("state: %+v", st)
	}
}

// Keepalives refresh the heartbeat while the store is current.
func TestKeepaliveStampsTheHeartbeat(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		time.Sleep(300 * time.Millisecond)
		for r.Context().Err() == nil {
			emit(w, fl, ": keepalive\n\n")
			time.Sleep(30 * time.Millisecond)
		}
	})
	r := start(t, srv.URL, nil)
	eventually(t, "synced", func() bool { st := r.state(); return st.Connected && !st.Behind && !st.AliveAt.IsZero() })
	first := r.state().AliveAt
	eventually(t, "heartbeat moves", func() bool { return r.state().AliveAt.After(first.Add(100 * time.Millisecond)) })
}

// If the server's journal restarted its numbering, the follower takes the
// new head as its seq instead of waiting for events that will never come.
func TestJournalRebuiltResetsSeq(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=3\n\n")
		if n == 1 {
			return // drop, to see what the reconnect asks for
		}
		hold(r)
	})
	path := filepath.Join(t.TempDir(), "follow.json")
	Save(path, State{Store: "/store/a", Seq: 50})
	r := start(t, srv.URL, func(o *Options, r *rig) { o.StatePath = path; r.statePath = path })
	eventually(t, "reconnect", func() bool { return srv.conns() >= 2 })
	if got := srv.lastID(2); got != "3" {
		t.Fatalf("reconnect sent Last-Event-ID %q, want 3", got)
	}
	eventually(t, "seq reset", func() bool { return r.state().Seq == 3 })
}

// A server ahead of the follower with no events to replay still owes a sync.
func TestHeadAheadOfSeqSyncsWithoutEvents(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=9\n\n")
		hold(r)
	})
	r := start(t, srv.URL, nil)
	eventually(t, "seq 9", func() bool { return r.state().Seq == 9 })
}

func TestAuthIsSentOnlyWhenConfigured(t *testing.T) {
	handler := func(n int, w http.ResponseWriter, r *http.Request) {
		stream(w)
		hold(r)
	}
	anon, signed := newFeed(t, handler), newFeed(t, handler)
	start(t, anon.URL, nil)
	start(t, signed.URL, func(o *Options, _ *rig) {
		o.Auth = func() (string, string, error) { return "claude@host-a", "tok", nil }
	})
	eventually(t, "both connect", func() bool { return anon.conns() >= 1 && signed.conns() >= 1 })
	anon.mu.Lock()
	signed.mu.Lock()
	defer anon.mu.Unlock()
	defer signed.mu.Unlock()
	if anon.auths[0] != "" {
		t.Errorf("anonymous follower sent %q", anon.auths[0])
	}
	req := &http.Request{Header: http.Header{"Authorization": {signed.auths[0]}}}
	user, token, ok := req.BasicAuth()
	if !ok || user != "claude@host-a" || token != "tok" {
		t.Errorf("auth header %q", signed.auths[0])
	}
}

func TestBackoffStaysInItsBand(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 60 * time.Second}
	for attempt := 0; attempt < 200; attempt++ {
		ceil := 60 * time.Second
		if attempt < 6 {
			ceil = time.Second << attempt
		}
		for i := 0; i < 50; i++ {
			d := b.Delay(attempt)
			if d < ceil/2 || d > ceil {
				t.Fatalf("attempt %d: %v outside [%v, %v]", attempt, d, ceil/2, ceil)
			}
		}
	}
}

// Connections that are accepted and dropped at once never count as healthy,
// so the delay keeps doubling instead of resetting to the base.
func TestBackoffGrowsWhenTheStreamDropsAtOnce(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
	})
	base := 20 * time.Millisecond
	start(t, srv.URL, func(o *Options, _ *rig) {
		o.Reconnect = Backoff{Base: base, Max: time.Second}
		o.Healthy = 10 * time.Second
	})
	eventually(t, "six connections", func() bool { mu.Lock(); defer mu.Unlock(); return len(starts) >= 6 })
	mu.Lock()
	defer mu.Unlock()
	for k := 1; k <= 5; k++ {
		gap := starts[k].Sub(starts[k-1])
		if min := (base << (k - 1)) / 2; gap < min {
			t.Errorf("gap %d = %v, want at least %v: the backoff reset", k, gap, min)
		}
	}
}

// A stream that stays up past Healthy resets the backoff: the next retry is
// the first one again, not the fifth.
func TestBackoffResetsAfterAHealthyStream(t *testing.T) {
	var mu sync.Mutex
	var starts, ends []time.Time
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		switch n {
		case 5:
			time.Sleep(400 * time.Millisecond) // longer than Healthy
		case 6:
			hold(r)
			return
		default:
			return
		}
		mu.Lock()
		ends = append(ends, time.Now())
		mu.Unlock()
	})
	start(t, srv.URL, func(o *Options, _ *rig) {
		o.Reconnect = Backoff{Base: 100 * time.Millisecond, Max: 1600 * time.Millisecond}
		o.Healthy = 250 * time.Millisecond
	})
	eventually(t, "six connections", func() bool { mu.Lock(); defer mu.Unlock(); return len(starts) >= 6 && len(ends) >= 1 })
	mu.Lock()
	defer mu.Unlock()
	// Without the reset the sixth delay is attempt 4: 800 ms to 1.6 s.
	if gap := starts[5].Sub(ends[0]); gap > 600*time.Millisecond {
		t.Fatalf("retry after a healthy stream waited %v; the backoff did not reset", gap)
	}
}

// A missing clone or an unreadable origin is waited out, not fatal.
func TestResolveIsRetriedUntilItWorks(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		hold(r)
	})
	var tries atomic.Int32
	r := start(t, "", func(o *Options, _ *rig) {
		o.Resolve = func(ctx context.Context) (string, error) {
			if tries.Add(1) < 3 {
				return "", errors.New("no store yet")
			}
			return srv.URL, nil
		}
	})
	eventually(t, "connects after the third try", func() bool { return srv.conns() == 1 })
	if tries.Load() != 3 {
		t.Fatalf("%d resolve tries", tries.Load())
	}
	eventually(t, "synced", func() bool { return !r.state().Behind })
}

// A follower waiting for its store stops on a signal without an error and
// leaves no live heartbeat.
func TestResolveWaitEndsWithTheContext(t *testing.T) {
	r := start(t, "", func(o *Options, _ *rig) {
		o.Resolve = func(ctx context.Context) (string, error) { return "", errors.New("still no store") }
	})
	eventually(t, "state written while waiting", func() bool { return r.state().PID != 0 })
	if Current(r.statePath, r.store, time.Now()) {
		t.Fatal("a follower with no stream looks fresh")
	}
	r.stop() // fails the test if Run does not return or returns an error
	if st := r.state(); st.PID != 0 {
		t.Fatalf("state after stop: %+v", st)
	}
}

// A second follower for the same state file refuses to start and leaves the
// running one's state alone.
func TestSecondFollowerIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.json")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	Save(path, State{PID: 4242, Store: "/store/a", Seq: 9})
	// A deadline, so a regression that waits instead of refusing fails here
	// rather than hanging the run.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = Run(ctx, Options{URL: "http://127.0.0.1:1/events", StatePath: path, Store: "/store/a",
		Sync: func(context.Context) (string, error) { return "", nil }})
	var running *ErrRunning
	if !errors.As(err, &running) {
		t.Fatalf("Run: %v, want ErrRunning", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run waited out the deadline instead of refusing at once")
	}
	if st, _ := Load(path); st.PID != 4242 || st.Seq != 9 {
		t.Fatalf("the refused follower touched the state: %+v", st)
	}
}

// A state directory that cannot be created yet (a home not mounted at login)
// is waited out, not fatal: Run neither returns an error nor exits, and
// starts following once the directory can be made.
func TestUnusableStateDirectoryIsRetried(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		hold(r)
	})
	blocker := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocker, []byte("a file where the directory should go"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := start(t, srv.URL, func(o *Options, r *rig) {
		r.statePath = filepath.Join(blocker, "follow.json")
		o.StatePath = r.statePath
	})
	time.Sleep(150 * time.Millisecond) // several failed attempts at a 5 to 20 ms backoff
	select {
	case <-r.done:
		t.Fatal("Run returned while the state directory was unusable")
	default:
	}
	if srv.conns() != 0 {
		t.Fatal("followed without a state file")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	eventually(t, "following once the directory exists", func() bool { return srv.conns() == 1 })
	eventually(t, "synced", func() bool { return !r.state().Behind })
}

// The stream only reports journal rows. A ref that moved with no row is
// caught by the periodic check: hooks stop skipping at once, the follower
// syncs, and the stamp comes back after the sync.
func TestVerifyCatchesAMoveTheJournalMissed(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		hold(r)
	})
	var ahead, gate atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	r := start(t, srv.URL, func(o *Options, r *rig) {
		o.VerifyEvery = 40 * time.Millisecond
		o.Verify = func(ctx context.Context) (bool, error) { return ahead.Load(), nil }
		o.Sync = func(ctx context.Context) (string, error) {
			if gate.Load() {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			ahead.Store(false)
			return "h", nil
		}
	})
	eventually(t, "first sync", func() bool { return !r.state().Behind && !r.state().VerifiedAt.IsZero() })
	first := r.state().VerifiedAt
	eventually(t, "a check stamps the verification", func() bool { return r.state().VerifiedAt.After(first.Add(80 * time.Millisecond)) })

	gate.Store(true)
	ahead.Store(true) // the server moved; no event comes
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the check never triggered a sync")
	}
	if st := r.state(); !st.Behind || Current(r.statePath, r.store, time.Now()) {
		t.Fatalf("hooks may still skip while the server is ahead: %+v", st)
	}
	close(release)
	eventually(t, "current again", func() bool { return Current(r.statePath, r.store, time.Now()) })
}

// A check that cannot reach the server proves nothing and stamps nothing.
func TestFailedVerifyDoesNotStamp(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\n")
		hold(r)
	})
	var checks atomic.Int32
	r := start(t, srv.URL, func(o *Options, _ *rig) {
		o.VerifyEvery = 30 * time.Millisecond
		o.Verify = func(ctx context.Context) (bool, error) { checks.Add(1); return false, errors.New("ls-remote: timeout") }
	})
	eventually(t, "synced", func() bool { return !r.state().Behind && !r.state().VerifiedAt.IsZero() })
	stamp := r.state().VerifiedAt
	eventually(t, "five failed checks", func() bool { return checks.Load() >= 5 })
	if got := r.state().VerifiedAt; !got.Equal(stamp) {
		t.Fatalf("failed checks moved the stamp: %v -> %v", stamp, got)
	}
}
