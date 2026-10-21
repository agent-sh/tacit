package main

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/record"
)

func testRec(kind, folder string) *record.Record {
	name := folder[strings.LastIndex(folder, "/")+1:]
	return &record.Record{Kind: kind, Name: name, Description: "d", Status: record.StatusActive,
		Was:     []record.Entry{{Status: record.StatusActive, Date: today(), By: record.ByOwner, Why: "x"}},
		Created: today(), CreatedBy: record.ByOwner, Folder: folder}
}

// A session in either of two folders suppression joins reads both.
func TestSuppressionNamesJoinWinnerAndLosers(t *testing.T) {
	w, l := testRec(record.KindRepo, "projects/widget"), testRec(record.KindRepo, "projects/old-widget")
	w.Aliases = []string{"widget-svc"}
	l.SuppressedBy = "projects/widget"
	recs := map[string]*record.Record{w.Folder: w, l.Folder: l}
	if got := strings.Join(suppressionNames(recs, []string{"old-widget"}), ","); got != "old-widget,widget,widget-svc" {
		t.Errorf("from the loser: %s", got)
	}
	if got := strings.Join(suppressionNames(recs, []string{"Widget"}), ","); got != "Widget,widget-svc,old-widget" {
		t.Errorf("from the winner: %s", got)
	}
	if got := strings.Join(suppressionNames(recs, []string{"gadget"}), ","); got != "gadget" {
		t.Errorf("unrelated: %s", got)
	}
}
