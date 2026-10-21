package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runFormat implements `tacit format [--set] [--records-complete]`: the
// store's format marker (ledger.FormatPath, SPEC.md "Store format"). With
// no flag it prints the marker and this binary's format. --set is the
// owner's one-time command once every machine runs a binary that reads the
// folder records format: it writes the marker in one commit, and from then
// on the writers write records, the areas/ and topics/ folders and
// last_checked. --records-complete records that every project, area and
// topic folder with facts has a record (the migration's end): lint then
// requires one for every such folder, and the metadata checks drop the
// interim rule that a project folder holding facts is a known repository.
func runFormat(args []string) int {
	fs := flag.NewFlagSet("format", flag.ExitOnError)
	set := fs.Bool("set", false, "write the format marker of this binary's format (once every machine runs a binary that reads it)")
	complete := fs.Bool("records-complete", false, "with the marker set: every project, area and topic folder with facts has a record")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "format: no store")
		return 2
	}
	cur, ok, rerr := ledger.ReadFormat(s.Dir)
	if !*set && !*complete {
		fmt.Printf("tacit %s writes store format %d\n", version, ledger.FormatVersion)
		switch {
		case rerr != nil:
			fmt.Printf("store: the marker %s cannot be read: %v\n", ledger.FormatPath, rerr)
			return 1
		case !ok:
			fmt.Printf("store: no format marker; folder records are not written (tacit format --set, once every machine runs this binary)\n")
		default:
			fmt.Printf("store: format %d set %s, records complete: %v\n", cur.Version, cur.At, cur.RecordsComplete)
		}
		return 0
	}
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "format: nothing done: the marker cannot be read:", rerr)
		return 2
	}
	if *complete && !*set && !ok {
		fmt.Fprintln(os.Stderr, "format: nothing done: --records-complete needs the format marker (--set first)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if refuseWriter(ctx, s, "curator", "format") {
		return 2
	}
	pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		now, have, err := ledger.ReadFormat(s.Dir)
		if err != nil {
			return nil, "", err
		}
		if have && now.Version > ledger.FormatVersion {
			return nil, "", fmt.Errorf("the store's format %d is newer than this binary's %d", now.Version, ledger.FormatVersion)
		}
		want := now
		if !have || *set {
			want.Version = ledger.FormatVersion
		}
		if *complete {
			want.RecordsComplete = true
		}
		if have && want == now {
			return nil, "", nil
		}
		want.At = time.Now().UTC().Format(time.RFC3339)
		msg := fmt.Sprintf("curator: format: store format %d", want.Version)
		if want.RecordsComplete {
			msg += ", records complete"
		}
		return map[string][]byte{ledger.FormatPath: want.Encode()}, msg, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "format:", err)
		return 1
	}
	if !pushed {
		fmt.Println("format: the marker already says so; nothing written")
		return 0
	}
	fmt.Println("format: marker written")
	return 0
}
