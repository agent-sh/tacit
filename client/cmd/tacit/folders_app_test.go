package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/record"
)

// The Folders page is the owner's channel: each action is one owner commit
// whose entries are the owner's, checked like any record; a refused one
// writes nothing; without the format marker nothing is written.
func TestFoldersPageIsTheOwnersChannel(t *testing.T) {
	bare, clone := ownerFixture(t)
	rs := clone("app")
	ctx := ctxT(t)
	if res, err := rs.folders(ctx); err != nil || res.Format == "" {
		t.Fatalf("no marker: %+v %v", res, err)
	}
	if _, err := rs.folderSave(ctx, folderDraft{Action: "create", Folder: "projects/x", Kind: "repo", Description: "the x service"}); err == nil {
		t.Fatal("a record written without the format marker")
	}
	// The owner sets the marker (on the server, as tacit format --set would).
	seed := filepath.Join(t.TempDir(), "marker")
	git(t, filepath.Dir(seed), "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	os.WriteFile(filepath.Join(seed, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "format")
	git(t, seed, "push", "-q", "origin", "HEAD:main")

	save := func(d folderDraft) *saveResult {
		t.Helper()
		res, err := rs.folderSave(ctx, d)
		if err != nil || !res.Saved {
			t.Fatalf("%s %s: %+v %v", d.Action, d.Folder, res, err)
		}
		return res
	}
	refuse := func(d folderDraft, want string) {
		t.Helper()
		res, err := rs.folderSave(ctx, d)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s %s: want refusal %q, got %+v %v", d.Action, d.Folder, want, res, err)
		}
	}
	if err := rs.s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := rs.folders(ctx)
	if err != nil || res.Format != "" || len(res.Rows) != 1 || res.Rows[0].Folder != "projects/x" || res.Rows[0].Record || res.Rows[0].Facts != 2 {
		t.Fatalf("folders %+v %v", res, err)
	}
	save(folderDraft{Action: "create", Folder: "projects/x", Kind: "repo", Description: "the x service"})
	if subj := strings.TrimSpace(git(t, rs.s.Dir, "log", "-1", "--format=%s")); subj != "owner: folder create projects/x" {
		t.Fatalf("subject %q", subj)
	}
	save(folderDraft{Action: "status", Folder: "projects/x", Status: "shipped", Why: "released for good"})
	save(folderDraft{Action: "alias", Folder: "projects/x", Alias: "x-svc"})
	save(folderDraft{Action: "create", Folder: "projects/x-old", Kind: "repo", Description: "the x service before the rename"})
	save(folderDraft{Action: "suppress", Folder: "projects/x-old", Into: "projects/x"})
	save(folderDraft{Action: "create", Folder: "areas/ops", Kind: "area", Description: "on-call and tickets"})
	save(folderDraft{Action: "tools", Folder: "areas/ops", Tools: []string{"mcp__*__Ticketing*"}, Commands: []string{"tickets list"}})
	save(folderDraft{Action: "create", Folder: "areas/ops/topics/q4", Kind: "topic", Description: "the Q4 move"})
	refuse(folderDraft{Action: "status", Folder: "projects/x", Status: "active"}, "say why")
	refuse(folderDraft{Action: "suppress", Folder: "areas/ops", Into: "projects/x"}, "another kind")
	refuse(folderDraft{Action: "alias", Folder: "areas/ops", Alias: "x-svc"}, "another folder's name or alias")
	refuse(folderDraft{Action: "create", Folder: "topics/q4", Kind: "topic", Description: "again"}, "unique")
	refuse(folderDraft{Action: "create", Folder: "system", Kind: "repo"}, "no folder that holds a record")
	recs, errs := record.Walk(rs.s.Dir)
	if len(errs) != 0 || len(recs) != 4 {
		t.Fatalf("records %v %v", recs, errs)
	}
	x := recs["projects/x"]
	if x.CreatedBy != "owner" || x.Status != "shipped" || len(x.Was) != 2 || x.Was[0].By != "owner" || x.Was[0].Why != "released for good" || x.Aliases[0] != "x-svc" {
		t.Fatalf("x %+v", x)
	}
	if recs["projects/x-old"].SuppressedBy != "projects/x" || recs["areas/ops"].Tools[0] != "mcp__*__Ticketing*" {
		t.Fatalf("x-old %+v ops %+v", recs["projects/x-old"], recs["areas/ops"])
	}
	if fs, err := lint.Run(rs.s.Dir); err != nil {
		t.Fatal(err)
	} else {
		for _, f := range fs {
			if f.Rule == "L17" || f.Rule == "L16" {
				t.Errorf("lint: %v", f)
			}
		}
	}
	if n := strings.Count(git(t, rs.s.Dir, "log", "--format=%s"), "owner: folder "); n != 8 {
		t.Errorf("%d owner folder commits, want 8", n)
	}
	// On the server.
	if out := git(t, rs.s.Dir, "ls-remote", bare, "main"); !strings.HasPrefix(out, strings.TrimSpace(git(t, rs.s.Dir, "rev-parse", "HEAD"))) {
		t.Errorf("not pushed: %s", out)
	}
	// A machine that cannot write sees the page and writes nothing.
	ro := clone("viewer")
	ro.viewOnly = viewOnlyLine
	if _, err := ro.folderSave(ctx, folderDraft{Action: "alias", Folder: "projects/x", Alias: "x-two"}); err == nil {
		t.Fatal("a view-only machine wrote")
	}
	ro.s.Sync(ctx)
	if res, _ := ro.folders(ctx); res.ViewOnly == "" || len(res.Rows) != 4 {
		t.Fatalf("view only %+v", res)
	}
}

// An agent's tacit folder writes by opus and must cite a session.
func TestFolderCommandWritesByOpus(t *testing.T) {
	_, s := assessHome(t)
	if code := runFolder([]string{"create", "--folder", "projects/widget", "--kind", "repo", "--desc", "the widget service", "--status", "active", "--why", "no session"}); code != 1 {
		t.Fatalf("an uncited why: exit %d", code)
	}
	if code := runFolder([]string{"create", "--folder", "projects/widget", "--kind", "repo", "--desc", "the widget service", "--status", "active", "--why", "worked in session:s-one"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	recs, _ := record.Walk(s.Dir)
	if r := recs["projects/widget"]; r == nil || r.CreatedBy != "opus" || r.Was[0].By != "opus" {
		t.Fatalf("record %+v", r)
	}
	if subj := strings.TrimSpace(gitIn(t, s.Dir, "log", "-1", "--format=%s")); subj != "curator: folder create projects/widget" {
		t.Fatalf("subject %q", subj)
	}
}
