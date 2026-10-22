package main

// The Folders page of tacit app: the owner's channel for folder records
// (SPEC.md, "Folder records"). The owner creates a folder, sets its status
// with a reason, adds an alias, folds it into another folder, and sets an
// area's tools; each is one owner commit through the app's write path (lock,
// sync, record.Validate as the owner, lint gate), and every entry it writes
// is by owner. An agent's `tacit folder` command writes by opus.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// folderRow is one folder on the page.
type folderRow struct {
	Folder       string   `json:"folder"`
	Kind         string   `json:"kind,omitempty"`
	Status       string   `json:"status,omitempty"`
	Description  string   `json:"description,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	SuppressedBy string   `json:"suppressed_by,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	Commands     []string `json:"commands,omitempty"`
	Repos        []string `json:"repos,omitempty"`
	Why          string   `json:"why,omitempty"`  // the newest history entry's
	By           string   `json:"by,omitempty"`   // its writer: you or the curator
	Date         string   `json:"date,omitempty"` // its day
	Facts        int      `json:"facts"`          // live facts in it
	Record       bool     `json:"record"`         // it has a record
}

// foldersResult is the page: every folder with a record, every project
// folder with facts and none, and the questions the folder steps left for
// the owner (review/folders.jsonl).
type foldersResult struct {
	Rows      []folderRow `json:"rows"`
	Questions []folderAsk `json:"questions,omitempty"`
	Statuses  []string    `json:"statuses"`
	Format    string      `json:"format,omitempty"` // why records cannot be written yet, "" when they can
	ViewOnly  string      `json:"view_only,omitempty"`
}

// folderDraft is one of the owner's folder actions.
type folderDraft struct {
	Action      string   `json:"action"` // create | status | alias | suppress | tools
	Folder      string   `json:"folder"`
	Kind        string   `json:"kind,omitempty"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status,omitempty"`
	Why         string   `json:"why,omitempty"`
	Alias       string   `json:"alias,omitempty"`
	Into        string   `json:"into,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	Repos       []string `json:"repos,omitempty"`
}

func (rs *reviewServer) folders(ctx context.Context) (*foldersResult, error) {
	recs, _ := record.Walk(rs.s.Dir)
	facts, _, err := fact.Walk(rs.s.Dir)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, f := range facts {
		if !f.InInbox() && f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			count[f.Meta.Scope]++
		}
	}
	res := &foldersResult{Statuses: record.Statuses(), ViewOnly: rs.viewOnly}
	if ok, why := fact.FormatWrites(rs.s.Dir); !ok {
		res.Format = why
	}
	seen := map[string]bool{}
	for f, r := range recs {
		seen[f] = true
		row := folderRow{Folder: f, Kind: r.Kind, Status: r.Status, Description: r.Description, Aliases: r.Aliases, SuppressedBy: r.SuppressedBy,
			Tools: r.Tools, Commands: r.Commands, Repos: r.Repos, Facts: count[f], Record: true}
		if len(r.Was) > 0 {
			row.Why, row.By, row.Date = r.Was[0].Why, r.Was[0].By, r.Was[0].Date
		}
		res.Rows = append(res.Rows, row)
	}
	for scope, n := range count {
		if !seen[scope] && record.IsFolder(scope) {
			res.Rows = append(res.Rows, folderRow{Folder: scope, Facts: n})
		}
	}
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Folder < res.Rows[j].Folder })
	res.Questions = openFolderAsks(rs.s.Dir)
	return res, nil
}

// applyFolder is the record an action of by's (the owner in the app, an
// agent's `tacit folder`) makes of old (nil: none).
func applyFolder(old *record.Record, d folderDraft, today, by string) (*record.Record, string, error) {
	folder := strings.TrimSpace(d.Folder)
	why := cleanLine(d.Why)
	entry := func(status string) record.Entry {
		return record.Entry{Status: status, Date: today, By: by, Why: why}
	}
	switch d.Action {
	case "create":
		if old != nil {
			return nil, "", fmt.Errorf("%s has a record already", folder)
		}
		status := d.Status
		if status == "" {
			status = record.StatusActive
		}
		if why == "" && by == record.ByOwner {
			why = "made by the owner"
		}
		r := &record.Record{Kind: d.Kind, Name: folder[strings.LastIndex(folder, "/")+1:], Description: cleanLine(d.Description), Status: status,
			Created: today, CreatedBy: by, Folder: folder, Tools: cleanList(d.Tools), Commands: cleanList(d.Commands), Repos: cleanList(d.Repos)}
		r.Was = []record.Entry{entry(status)}
		if d.Alias != "" {
			r.Aliases = []string{strings.TrimSpace(d.Alias)}
		}
		return r, "create " + folder, nil
	}
	if old == nil {
		return nil, "", fmt.Errorf("%s has no record: create it first", folder)
	}
	r := old.Clone()
	switch d.Action {
	case "status":
		if why == "" {
			return nil, "", errors.New("say why the status changes; it stays in the folder's history")
		}
		r.Status = d.Status
		r.Was = append([]record.Entry{entry(d.Status)}, r.Was...)
		if desc := cleanLine(d.Description); desc != "" {
			r.Description = desc
		}
		return r, fmt.Sprintf("status %s %s", folder, d.Status), nil
	case "alias":
		a := strings.TrimSpace(d.Alias)
		if a == "" {
			return nil, "", errors.New("give the other name")
		}
		r.Aliases = append(r.Aliases, a)
		return r, fmt.Sprintf("alias %s %s", folder, a), nil
	case "suppress":
		into := strings.TrimSpace(d.Into)
		if into == "" {
			return nil, "", errors.New("name the folder that replaces this one")
		}
		if why == "" && by == record.ByOwner {
			why = "folded into " + into + " by the owner"
		}
		r.SuppressedBy = into
		r.Was = append([]record.Entry{entry(r.Status)}, r.Was...)
		return r, fmt.Sprintf("suppress %s into %s", folder, into), nil
	case "tools":
		r.Tools, r.Commands = cleanList(d.Tools), cleanList(d.Commands)
		return r, "tools " + folder, nil
	}
	return nil, "", fmt.Errorf("no folder action %q", d.Action)
}

func cleanList(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// folderSave is one owner action, one owner commit.
func (rs *reviewServer) folderSave(ctx context.Context, d folderDraft) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		if ok, why := fact.FormatWrites(rs.s.Dir); !ok {
			return nil, errors.New(why)
		}
		folder := strings.TrimSpace(d.Folder)
		if !record.IsFolder(folder) {
			return nil, fmt.Errorf("%q is no folder that holds a record: projects/<name>, areas/<name>, areas/<area>/topics/<name>, topics/<name> or orgs/<name>", folder)
		}
		recs, _ := record.Walk(rs.s.Dir)
		old := recs[folder]
		rec, what, err := applyFolder(old, d, today(), record.ByOwner)
		if err != nil {
			return nil, err
		}
		ql, _ := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(ledger.QuotesPath)))
		env := record.Env{Today: today(), Records: recs, QuoteBacked: ledger.Backing(ql), KnownRepo: curateEnv(rs.s.Dir, false).KnownRepo, HoldsFacts: factFolderSet(rs.s.Dir)}
		if ps := record.Validate(old, rec, record.ByOwner, env); len(ps) > 0 {
			var msgs []string
			for _, p := range ps {
				msgs = append(msgs, p.Message())
			}
			return nil, fmt.Errorf("not saved: %s", strings.Join(msgs, "; "))
		}
		b, err := rec.Encode()
		if err != nil {
			return nil, err
		}
		return &ownerChange{write: map[string][]byte{record.Rel(folder): b}, name: folder,
			msg: fmt.Sprintf("%sfolder %s\n\nBy the owner in tacit app.", ownerCommit, what)}, nil
	})
}

// Folders is the Folders page.
func (a *App) Folders() (*foldersResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.rs.folders(ctx)
}

// FolderSave makes one of the owner's folder actions.
func (a *App) FolderSave(d folderDraft) (*saveResult, error) {
	ctx, cancel := writeCtx()
	defer cancel()
	return a.rs.folderSave(ctx, d)
}

// runFolder implements `tacit folder create|status|alias|suppress|tools
// --folder F [--kind K] [--desc D] [--status S] [--why W] [--alias A]
// [--into F2] [--tools a,b] [--commands c,d]`: an agent's or a test's
// change to a folder record, written as the curator (by opus: an agent
// typing a command is not the owner), checked like the model's. A why must
// cite a session or note code can find.
func runFolder(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tacit folder create|status|alias|suppress|tools --folder F [--kind K] [--desc D] [--status S] [--why W] [--alias A] [--into F] [--tools a,b] [--commands c,d]")
		return 2
	}
	fs := flag.NewFlagSet("folder", flag.ExitOnError)
	d := folderDraft{Action: args[0]}
	fs.StringVar(&d.Folder, "folder", "", "the folder")
	fs.StringVar(&d.Kind, "kind", "", "create: repo, folder, area or topic")
	fs.StringVar(&d.Description, "desc", "", "one line: what it is")
	fs.StringVar(&d.Status, "status", "", "the status")
	fs.StringVar(&d.Why, "why", "", "why, citing the sessions or notes it rests on")
	fs.StringVar(&d.Alias, "alias", "", "alias: another name")
	fs.StringVar(&d.Into, "into", "", "suppress: the folder that replaces it")
	tools := fs.String("tools", "", "tools: tool-name patterns, comma separated")
	commands := fs.String("commands", "", "tools: command prefixes, comma separated")
	fs.Parse(args[1:])
	d.Tools, d.Commands = cleanList(strings.Split(*tools, ",")), cleanList(strings.Split(*commands, ","))
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "folder: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if refuseWriter(ctx, s, "curator", "folder") {
		return 2
	}
	if ok, why := fact.FormatWrites(s.Dir); !ok {
		fmt.Fprintln(os.Stderr, "folder: nothing done:", why)
		return 2
	}
	a, err := newAssessor(ctx, s, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "folder:", err)
		return 1
	}
	_, err = curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		recs, _ := record.Walk(s.Dir)
		a.recs = recs
		old := recs[strings.TrimSpace(d.Folder)]
		rec, what, err := applyFolder(old, d, today(), record.ByOpus)
		if err != nil {
			return nil, "", err
		}
		if ps := record.Validate(old, rec, record.ByOpus, a.recordEnv(nil, nil)); len(ps) > 0 {
			var msgs []string
			for _, p := range ps {
				msgs = append(msgs, p.Message())
			}
			return nil, "", fmt.Errorf("refused: %s", strings.Join(msgs, "; "))
		}
		b, err := rec.Encode()
		if err != nil {
			return nil, "", err
		}
		return map[string][]byte{record.Rel(rec.Folder): b}, "curator: folder " + what, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "folder:", err)
		return 1
	}
	fmt.Println("folder: written")
	return 0
}

// factFolderSet reports the folders of the store at dir that hold curated
// facts.
func factFolderSet(dir string) func(string) bool {
	facts, _, _ := fact.Walk(dir)
	set := map[string]bool{}
	for _, f := range facts {
		if !f.InInbox() {
			set[f.Meta.Scope] = true
		}
	}
	return func(folder string) bool { return set[folder] }
}
