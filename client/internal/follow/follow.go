package follow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Backoff is exponential with equal jitter: attempt n waits between half and
// all of min(Max, Base*2^n). The floor keeps a flapping server from being
// hit in a tight loop; the jitter spreads the machines that all lose the
// feed together when the server restarts.
type Backoff struct {
	Base, Max time.Duration
}

// Delay is the wait before retry number attempt (0 is the first retry).
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.Max
	if attempt < 62 && b.Base <= b.Max>>attempt { // Base<<attempt <= Max, no overflow
		d = b.Base << attempt
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
}

// Options configures Run. Zero durations take the defaults below.
type Options struct {
	URL       string // the /events endpoint; or set Resolve
	StatePath string
	Store     string // recorded in the state so hooks know whose heartbeat it is

	// Resolve, when set instead of URL, supplies the endpoint. It is retried
	// (Retry backoff) until it works: a clone that is not there yet, or a
	// remote lookup that times out on a loaded machine, is something to wait
	// out, not a reason to exit and be restarted.
	Resolve func(ctx context.Context) (url string, err error)

	// Sync fast-forwards the clone and refreshes the index; it returns the
	// new HEAD. It must not hold the store lock across the network.
	Sync func(ctx context.Context) (head string, err error)

	// Auth, when set, supplies HTTP basic credentials for each connection.
	// The server needs none to read the feed; this is for a server that
	// later asks.
	Auth func() (user, pass string, err error)

	// Verify asks whether the server's main holds commits the clone lacks (one
	// ls-remote). The stream only reports journal rows, and a ref can move
	// without one, so a connected follower checks directly every VerifyEvery.
	// Nil turns the check off.
	Verify func(ctx context.Context) (ahead bool, err error)
	// VerifyEvery is the interval between checks (see the package constant).
	VerifyEvery time.Duration

	Client *http.Client
	Logf   func(format string, args ...any)

	// Debounce is the quiet time after an event before syncing; MaxWait caps
	// how long an unbroken run of events can postpone the sync. 1 s covers
	// the back-to-back pushes of one curator or dreamer run, so a burst costs
	// one fetch and one index build; 5 s keeps a busy store from starving
	// the sync.
	Debounce, MaxWait time.Duration
	// Idle is how long the stream may be silent before it counts as dead:
	// three server keepalives (20 s each).
	Idle time.Duration
	// Poll is the fallback: while the stream is down, sync this often. It
	// equals the backoff cap, so a server that is up but whose feed is
	// broken is never more than a minute behind.
	Poll time.Duration
	// Reconnect paces stream retries: 1 s so a server restart (about 5 s) is
	// followed quickly, 60 s at most so an outage costs one request a minute.
	Reconnect Backoff
	// Retry paces a failing sync (fetch error, busy lock): 2 s to 60 s.
	Retry Backoff
	// Healthy is how long a stream must stay up before the reconnect backoff
	// resets: longer than one keepalive, so a connection that was accepted and
	// then dropped at once does not reset it.
	Healthy time.Duration
	// SyncTimeout bounds one sync, as `tacit sync` does.
	SyncTimeout time.Duration
}

func (o *Options) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&o.Debounce, time.Second)
	set(&o.MaxWait, 5*time.Second)
	set(&o.Idle, 60*time.Second)
	set(&o.Poll, 60*time.Second)
	set(&o.Healthy, 30*time.Second)
	set(&o.SyncTimeout, 2*time.Minute)
	set(&o.VerifyEvery, VerifyEvery)
	if o.Reconnect == (Backoff{}) {
		o.Reconnect = Backoff{Base: time.Second, Max: 60 * time.Second}
	}
	if o.Retry == (Backoff{}) {
		o.Retry = Backoff{Base: 2 * time.Second, Max: 60 * time.Second}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
		}}
	}
}

type follower struct {
	o    Options
	wake chan struct{} // a push was announced or a sync is owed

	mu        sync.Mutex
	st        State
	seen      int64 // highest seq the stream has delivered this run
	connected bool
}

// Run follows the feed until ctx ends. It returns an error only when it
// cannot start (another follower holds the state lock, bad options); stream,
// resolve and sync failures are logged and retried.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	if (o.URL == "" && o.Resolve == nil) || o.StatePath == "" || o.Sync == nil {
		return fmt.Errorf("follow: URL (or Resolve), StatePath and Sync are required")
	}
	release, err := lockWithRetry(ctx, o)
	if err != nil {
		return err
	}
	if release == nil {
		return nil // stopped while waiting for the state directory
	}
	defer release()
	prev, err := Load(o.StatePath)
	if err != nil {
		o.Logf("state file unreadable, starting from seq 0: %v", err)
		prev = State{}
	}
	if prev.Store != "" && prev.Store != o.Store {
		prev = State{} // another clone's seq says nothing about this one
	}
	f := &follower{o: o, wake: make(chan struct{}, 1)}
	f.st = State{PID: os.Getpid(), Store: o.Store, Seq: prev.Seq, Head: prev.Head, SyncedAt: prev.SyncedAt, Behind: true}
	f.seen = prev.Seq
	f.saveLocked()
	defer func() {
		f.mu.Lock()
		f.st.PID, f.st.Connected = 0, false
		f.saveLocked()
		f.mu.Unlock()
	}()

	if f.o.URL == "" {
		url, ok := f.resolve(ctx)
		if !ok {
			return nil
		}
		f.o.URL = url
	}
	f.o.Logf("following %s from seq %d", f.o.URL, prev.Seq)
	f.kick() // the clone may have drifted while no one was watching
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.syncLoop(ctx) }()
	go func() { defer wg.Done(); f.verifyLoop(ctx) }()
	f.streamLoop(ctx)
	wg.Wait()
	return nil
}

// lockWithRetry takes the single-instance lock. Another follower holding it
// is final (ErrRunning). Anything else is an I/O problem that may pass (the
// home directory not mounted yet at login, a full disk): wait it out with
// the Retry backoff. A nil release with a nil error means ctx ended first.
func lockWithRetry(ctx context.Context, o Options) (func(), error) {
	for attempt := 0; ; attempt++ {
		release, err := Lock(o.StatePath)
		var running *ErrRunning
		if err == nil || errors.As(err, &running) {
			return release, err
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		d := o.Retry.Delay(attempt)
		o.Logf("state directory not usable: %v; retrying in %v", err, d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(d):
		}
	}
}

// resolve waits for Options.Resolve to succeed. False means ctx ended first.
func (f *follower) resolve(ctx context.Context) (string, bool) {
	for attempt := 0; ; attempt++ {
		url, err := f.o.Resolve(ctx)
		if err == nil && url != "" {
			return url, true
		}
		if ctx.Err() != nil {
			return "", false
		}
		d := f.o.Retry.Delay(attempt)
		f.o.Logf("waiting for the store: %v; retrying in %v", err, d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(d):
		}
	}
}

func (f *follower) kick() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// saveLocked persists f.st; the caller holds f.mu.
func (f *follower) saveLocked() {
	if err := Save(f.o.StatePath, f.st); err != nil {
		f.o.Logf("state: %v", err)
	}
}

// beat stamps the heartbeat when the stream is up and nothing is pending.
func (f *follower) beatLocked() {
	if f.connected && !f.st.Behind {
		f.st.AliveAt = time.Now()
	}
}

// markDown records that the stream is gone, so hooks stop trusting the
// heartbeat at once instead of waiting for it to age out.
func (f *follower) markDown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
	f.st.Connected = false
	f.saveLocked()
}

// streamLoop holds the connection, reconnecting with backoff.
func (f *follower) streamLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		start := time.Now()
		err := f.connect(ctx)
		// A stream asked to end sooner than one reconnect delay is treated as
		// a failure, so a server that sends it at once cannot spin the loop.
		if errors.Is(err, errReconnect) && ctx.Err() == nil && time.Since(start) >= f.o.Reconnect.Base {
			// A planned end: not down, no backoff, nothing missed (Last-Event-ID).
			attempt = 0
			continue
		}
		f.markDown()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= f.o.Healthy {
			attempt = 0
		}
		d := f.o.Reconnect.Delay(attempt)
		attempt++
		f.o.Logf("stream down: %v; reconnecting in %v", err, d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
}

// connect opens one stream and handles frames until it ends.
func (f *follower) connect(ctx context.Context) error {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idled atomic.Bool
	watchdog := time.AfterFunc(f.o.Idle, func() { idled.Store(true); cancel() })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, f.o.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	f.mu.Lock()
	last := f.seen
	f.mu.Unlock()
	if last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(last, 10))
	}
	if f.o.Auth != nil {
		u, p, err := f.o.Auth()
		if err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		if u != "" { // "" means no key for this connection: anonymous
			req.SetBasicAuth(u, p)
		}
	}
	resp, err := f.o.Client.Do(req)
	if err != nil {
		return silence(err, &idled, f.o.Idle)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("server answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return fmt.Errorf("not an event stream: Content-Type %q", resp.Header.Get("Content-Type"))
	}

	rd := NewReader(activity{resp.Body, watchdog, f.o.Idle})
	for {
		fr, err := rd.Next()
		if err != nil {
			return silence(err, &idled, f.o.Idle)
		}
		if fr.IsEvent && fr.Type == "reconnect" {
			return errReconnect
		}
		f.handle(fr)
	}
}

// errReconnect is the server ending a stream on purpose (`event: reconnect`:
// the public path bounds a stream's life so each connection authenticates
// with a fresh token). It is not a failure: the follower reconnects at once,
// resuming from the last seq, and the heartbeat stays as it was.
var errReconnect = errors.New("the server asked for a reconnect")

// silence names the idle watchdog's cancellation for what it is.
func silence(err error, idled *atomic.Bool, d time.Duration) error {
	if idled.Load() {
		return fmt.Errorf("no data for %v", d)
	}
	return err
}

// activity restarts the idle watchdog whenever bytes arrive.
type activity struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (a activity) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.t.Reset(a.d)
	}
	return n, err
}

// handle applies one frame.
func (f *follower) handle(fr Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !fr.IsEvent {
		if rest, ok := strings.CutPrefix(fr.Comment, "connected head="); ok {
			f.connectedLocked(rest)
		} else {
			f.beatLocked() // keepalive
			f.saveLocked()
		}
		return
	}
	if fr.Type != "head_moved" {
		return // unknown event types are for a newer server
	}
	seq, ok := eventSeq(fr)
	if !ok {
		f.o.Logf("head_moved without a seq (id %q): syncing anyway", fr.ID)
		seq = f.seen
	}
	if seq > f.seen {
		f.seen = seq
	}
	if !f.st.Behind {
		f.st.Behind = true
		f.saveLocked() // hooks must stop trusting the heartbeat now
	}
	f.kick()
}

// connectedLocked handles ": connected head=N", the first line of every
// stream: where the server's journal ends. Past the follower's seq, a sync
// is owed to N whether or not the replay that follows delivers every row.
// Behind it, the journal was rebuilt and numbering restarted: take N as the
// seq and resync. Equal means nothing is owed and the heartbeat starts.
func (f *follower) connectedLocked(headText string) {
	f.connected = true
	f.st.Connected = true
	head, err := strconv.ParseInt(strings.TrimSpace(headText), 10, 64)
	f.o.Logf("connected: server head %s, local seq %d", strings.TrimSpace(headText), f.seen)
	if err == nil && head != f.seen {
		if head < f.seen {
			f.o.Logf("server head %d is behind seq %d: the journal was rebuilt, resyncing", head, f.seen)
			f.st.Seq = head
		}
		f.seen = head
		f.st.Behind = true
		f.kick()
	}
	f.beatLocked()
	f.saveLocked()
}

func eventSeq(fr Frame) (int64, bool) {
	if n, err := strconv.ParseInt(fr.ID, 10, 64); err == nil && n >= 0 {
		return n, true
	}
	var row struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal([]byte(fr.Data), &row) == nil && row.Seq > 0 {
		return row.Seq, true
	}
	return 0, false
}

// syncLoop turns wake-ups into syncs: wait for a quiet moment, sync once
// for everything announced so far, record it.
func (f *follower) syncLoop(ctx context.Context) {
	poll := time.NewTicker(f.o.Poll)
	defer poll.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-poll.C:
			f.mu.Lock()
			idle := f.connected && !f.st.Behind
			f.mu.Unlock()
			if idle {
				continue // the stream is up and says nothing is owed
			}
		}
		if !f.settle(ctx) {
			return
		}
		f.mu.Lock()
		target := f.seen
		f.mu.Unlock()

		started := time.Now()
		sctx, cancel := context.WithTimeout(ctx, f.o.SyncTimeout)
		head, err := f.o.Sync(sctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d := f.o.Retry.Delay(failures)
			failures++
			f.o.Logf("sync failed: %v; retrying in %v", err, d.Round(time.Millisecond))
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			f.kick()
			continue
		}
		failures = 0
		f.mu.Lock()
		f.st.Seq, f.st.Head, f.st.SyncedAt = target, head, time.Now()
		f.st.VerifiedAt = started // the fetch began then; the clone held the server's main as of that moment
		if f.seen == target {
			f.st.Behind = false
		}
		f.beatLocked()
		f.saveLocked()
		f.mu.Unlock()
		f.o.Logf("synced to %s (seq %d)", short(head), target)
	}
}

// verifyLoop checks, while the stream is up and nothing is pending, that the
// clone really holds the server's main. A mismatch the journal did not
// announce marks the follower behind (hooks stop skipping at once) and
// syncs. A failed check stamps nothing: after VerifiedFor of that, hooks
// sync for themselves again.
func (f *follower) verifyLoop(ctx context.Context) {
	if f.o.Verify == nil {
		return
	}
	t := time.NewTicker(f.o.VerifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.mu.Lock()
		skip := !f.connected || f.st.Behind
		f.mu.Unlock()
		if skip {
			continue // a sync is owed or the poll covers it; a sync verifies as it goes
		}
		started := time.Now()
		vctx, cancel := context.WithTimeout(ctx, f.o.SyncTimeout)
		ahead, err := f.o.Verify(vctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				f.o.Logf("verify: %v", err)
			}
			continue
		}
		f.mu.Lock()
		switch {
		case ahead:
			f.o.Logf("the server's main is ahead of the clone and no event said so; syncing")
			f.st.Behind = true
			f.saveLocked()
			f.kick()
		case !f.st.Behind: // an event that arrived mid-check has the last word
			f.st.VerifiedAt = started
			f.saveLocked()
		}
		f.mu.Unlock()
	}
}

// settle waits for Debounce of quiet, but never longer than MaxWait from the
// first wake-up. False means ctx ended.
func (f *follower) settle(ctx context.Context) bool {
	deadline := time.Now().Add(f.o.MaxWait)
	t := time.NewTimer(f.o.Debounce)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-f.wake:
			if !time.Now().Before(deadline) {
				return true
			}
			t.Reset(min(f.o.Debounce, time.Until(deadline)))
		}
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
