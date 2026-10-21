package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/follow"
	"github.com/agent-sh/tacit/client/internal/store"
)

func TestEventsURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://100.64.0.10:7447/git":  "http://100.64.0.10:7447/events",
		"http://100.64.0.10:7447/git/": "http://100.64.0.10:7447/events",
		"http://100.64.0.10:7447":      "http://100.64.0.10:7447/events",
		"http://host:1/":               "http://host:1/events",
	} {
		if got := eventsURL(in); got != want {
			t.Errorf("eventsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The follower end to end against a real clone and git remote: an announced
// push is merged and indexed; a push the journal never announced is caught by
// the periodic check; and the hook skips its own sync only in between.
func TestFollowerKeepsARealCloneCurrent(t *testing.T) {
	root := t.TempDir()
	clone, push := newPair(t, root)
	t.Setenv("TACIT_STORE", clone)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	statePath := filepath.Join(root, "follow.json")
	t.Setenv("TACIT_FOLLOW_STATE", statePath)
	head := func() string { return gitIn(t, clone, "rev-parse", "HEAD") }

	announce := make(chan struct{})
	var verifyOn atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": connected head=1\n\n")
		fl.Flush()
		select {
		case <-announce:
			fmt.Fprint(w, "id: 2\nevent: head_moved\ndata: {\"seq\":2}\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := &store.Store{Dir: clone}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- follow.Run(ctx, follow.Options{
			URL: srv.URL, StatePath: statePath, Store: clone,
			Debounce: 50 * time.Millisecond, VerifyEvery: 300 * time.Millisecond,
			Sync: func(ctx context.Context) (string, error) { return followSync(ctx, s) },
			Verify: func(ctx context.Context) (bool, error) { // off until the second half
				if !verifyOn.Load() {
					return false, nil
				}
				return s.RemoteAhead(ctx)
			},
			Logf: func(f string, a ...any) { t.Logf(f, a...) },
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	current := func() bool { return follow.Current(statePath, clone, time.Now()) }
	waitFor("the start-up sync", current)

	// An announced push is merged and indexed without any hook running.
	pushed := push("a.txt")
	close(announce)
	waitFor("the announced push", func() bool { return head() == pushed })
	waitFor("current again", current)
	if matches, _ := filepath.Glob(filepath.Join(root, "index", "index-*-"+pushed+".db")); len(matches) != 1 {
		t.Fatalf("no index for the announced push: %v", matches)
	}

	// A push the journal never mentioned: the next check finds it and syncs.
	verifyOn.Store(true)
	silent := push("b.txt")
	waitFor("the unannounced push", func() bool { return head() == silent })
	waitFor("current after the check", current)
}

// followSync is `tacit sync` for a long-lived process: it merges what the
// server has and leaves the index for the new HEAD ready.
func TestFollowSyncMergesAndIndexes(t *testing.T) {
	root := t.TempDir()
	clone, push := newPair(t, root)
	idx := filepath.Join(root, "index")
	t.Setenv("TACIT_INDEX", idx)
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "local")) // not this machine's local layer

	moved := push("new.txt")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	head, err := followSync(ctx, &store.Store{Dir: clone})
	if err != nil {
		t.Fatal(err)
	}
	if head != moved {
		t.Fatalf("head %s, want %s", head, moved)
	}
	if _, err := os.Stat(filepath.Join(clone, "new.txt")); err != nil {
		t.Fatal("the clone was not fast-forwarded")
	}
	if matches, _ := filepath.Glob(filepath.Join(idx, "index-*-"+moved+".db")); len(matches) != 1 {
		t.Fatalf("no index for the new HEAD in %s", idx)
	}
}
