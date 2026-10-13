package follow

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The public path ends each stream after a bounded life with `event:
// reconnect`. The follower reconnects at once (no backoff), resumes from the
// last seq, never logs the stream as down, and keeps its heartbeat.
func TestPlannedReconnectIsNotAFailure(t *testing.T) {
	var mu sync.Mutex
	var starts, ends []time.Time
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		fl := stream(w)
		emit(w, fl, ": connected head=%d\n\n", n)
		if n < 4 {
			emit(w, fl, "%s", event(n))
			time.Sleep(250 * time.Millisecond) // past one reconnect delay
			emit(w, fl, "event: reconnect\ndata: {}\n\n")
			mu.Lock()
			ends = append(ends, time.Now())
			mu.Unlock()
			return
		}
		hold(r)
	})
	var down atomic.Int32
	rg := start(t, srv.URL, func(o *Options, _ *rig) {
		o.Reconnect = Backoff{Base: 200 * time.Millisecond, Max: 5 * time.Second}
		o.Logf = func(f string, a ...any) {
			if strings.HasPrefix(f, "stream down") {
				down.Add(1)
			}
			t.Logf(f, a...)
		}
	})
	eventually(t, "four connections", func() bool { return srv.conns() >= 4 })
	mu.Lock()
	for k := 0; k < 3; k++ {
		// A failure would wait at least half the 200 ms base.
		if gap := starts[k+1].Sub(ends[k]); gap > 90*time.Millisecond {
			t.Errorf("reconnect %d waited %v: the planned close took the backoff", k+1, gap)
		}
	}
	mu.Unlock()
	for k := 2; k <= 4; k++ {
		if got, want := srv.lastID(k), fmt.Sprint(k-1); got != want {
			t.Errorf("connection %d sent Last-Event-ID %q, want %q", k, got, want)
		}
	}
	if n := down.Load(); n != 0 {
		t.Errorf("the stream was logged down %d time(s)", n)
	}
	eventually(t, "connected heartbeat", func() bool { return rg.state().Connected })
}

// A server that asks for a reconnect at once is backed off like a failure,
// so it cannot spin the follower.
func TestImmediateReconnectIsBackedOff(t *testing.T) {
	srv := newFeed(t, func(n int, w http.ResponseWriter, r *http.Request) {
		fl := stream(w)
		emit(w, fl, ": connected head=0\n\nevent: reconnect\ndata: {}\n\n")
	})
	start(t, srv.URL, func(o *Options, _ *rig) {
		o.Reconnect = Backoff{Base: 200 * time.Millisecond, Max: time.Second}
	})
	time.Sleep(500 * time.Millisecond)
	if n := srv.conns(); n > 4 {
		t.Fatalf("%d connections in 500 ms: an immediate reconnect was not backed off", n)
	}
}
