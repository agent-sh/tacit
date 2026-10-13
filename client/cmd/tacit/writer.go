package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Every command that writes the store checks, before it does any work, that
// this machine can write as the writer it pushes as: the clone is not
// read-only (`tacit clone --read-only`) and, for a clone of the server, the
// writer's key <harness>@<host> is here. A machine without the curator's key
// (a Mac) never curates, verifies or marks; its app and review page are view
// only. Nothing is computed, called, written or moved before the refusal.

// viewOnlyLine is what the app and the review page say on such a machine.
const viewOnlyLine = "View only on this machine. Edit from the curator's machine."

// errViewOnly is the refusal of a page write on a machine that cannot write.
var errViewOnly = errors.New(viewOnlyLine)

// hasKey reports whether this machine holds the writer key <harness>@<host>.
// A variable so tests can say yes or no.
var hasKey = func(harness string) bool {
	dir, err := auth.Dir()
	if err != nil {
		return false
	}
	_, err = auth.Load(dir, harness+"@"+shortHost(mustHostname()))
	return err == nil
}

// writerCheck says why this machine cannot write the store as harness, or
// nil when it can. A clone of a path or file:// remote (a test, a mirror)
// pushes without a key; one of the server (http, https) needs it.
func writerCheck(ctx context.Context, s *store.Store, harness string) error {
	if s.ReadOnly(ctx) {
		return store.ErrReadOnly
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	origin, _ := s.RemoteURL(rctx)
	if (strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://")) && !hasKey(harness) {
		id := harness + "@" + shortHost(mustHostname())
		if harness == "curator" {
			return fmt.Errorf("no %s key on this machine: the curator's steps run on the machine that holds the curator key", id)
		}
		return fmt.Errorf("no %s key on this machine: run `tacit keygen %s` and enroll it", id, harness)
	}
	return nil
}

// refuseWriter prints the refusal for cmd and reports whether there was one.
func refuseWriter(ctx context.Context, s *store.Store, harness, cmd string) bool {
	if err := writerCheck(ctx, s, harness); err != nil {
		fmt.Fprintf(os.Stderr, "%s: nothing done: %v\n", cmd, err)
		return true
	}
	return false
}

// viewOnlyReason is the page's line when this machine cannot write as the
// curator, else "".
func viewOnlyReason(ctx context.Context, s *store.Store) string {
	if writerCheck(ctx, s, "curator") != nil {
		return viewOnlyLine
	}
	return ""
}

// writeRefusal is checked first by every page write (marks and the owner's
// edits): set at start for a machine without the curator key, and read again
// for a clone made read-only since.
func (rs *reviewServer) writeRefusal(ctx context.Context) error {
	if rs.viewOnly != "" || rs.s.ReadOnly(ctx) {
		return errViewOnly
	}
	return nil
}
