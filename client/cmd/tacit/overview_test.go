package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overview"
	"github.com/agent-sh/tacit/client/internal/record"
)

var promptFactRx = regexp.MustCompile(`(?m)^- \[\[([a-z0-9-]+)\]\]`)

// The overview step writes a folder's overview from its live facts, sends
// a refused one back once, writes nothing more while the folder holds still,
// and writes it again on a material change.
func TestOverviewStepWritesCheckedOverviews(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"widget"}})
	for _, n := range []string{"widget-port", "widget-build", "widget-deploy"} {
		os.WriteFile(filepath.Join(r.clone, "projects", "widget", n+".md"), []byte(rotFile(n, "projects/widget", "design_decision", "the "+n+" fact", "the "+n+" fact", "2026-09-01", "")), 0o644)
	}
	writeRecordFile(t, r.clone, &record.Record{Folder: "projects/widget", Kind: record.KindRepo, Name: "widget", Description: "the widget service", Status: record.StatusActive})
	os.MkdirAll(filepath.Join(r.clone, "review"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	r.commitIndex(r.clone, "facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	var prompts []string
	prev := overviewRun
	t.Cleanup(func() { overviewRun = prev })
	overviewRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		prompts = append(prompts, prompt)
		var cites []string
		for _, m := range promptFactRx.FindAllStringSubmatch(prompt, -1) {
			cites = append(cites, "[["+m[1]+"]]")
		}
		if strings.Contains(prompt, "CODE REFUSED IT") {
			return "The widget service and how it ships " + strings.Join(cites, " ") + ".\n\n- It builds before it deploys " + cites[0] + ".", 0.5, nil
		}
		return "The widget service, with no citation.\n\n- It builds " + cites[0] + ".", 0.5, nil
	}
	run := func() int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		n, errs := overviewStep(ctx, r.s, "m", 10, false, &curate.Report{ModelCalls: &modelrun.Trace{}})
		if errs > 0 {
			t.Fatalf("errors %d", errs)
		}
		return n
	}
	if n := run(); n != 1 || len(prompts) != 2 {
		t.Fatalf("written %d, prompts %d", n, len(prompts))
	}
	b, err := os.ReadFile(filepath.Join(r.clone, "projects", "widget", overview.File))
	if err != nil {
		t.Fatal(err)
	}
	o, err := overview.Parse(b)
	if err != nil || o.Meta.Folder != "projects/widget" || o.Meta.Facts != 4 || o.Meta.Status != "active" || !strings.Contains(o.Body, "[[widget-port]]") {
		t.Fatalf("overview %+v %v\n%s", o, err, b)
	}
	if subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%s")); subj != "curator: overview: projects/widget (4 facts, new)" {
		t.Fatalf("subject %q", subj)
	}
	if fs, _ := lint.Run(r.clone); len(fs) > 0 {
		t.Fatalf("lint: %v", fs)
	}
	// Nothing changed: nothing written, no model call.
	prompts = nil
	if n := run(); n != 0 || len(prompts) != 0 {
		t.Fatalf("a second run wrote %d", n)
	}
	// A cited fact retired: written again.
	p := filepath.Join(r.clone, "projects", "widget", "widget-port.md")
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), "created:", "retired: 2026-10-21\nupdated: 2026-10-21\ncreated:", 1)), 0o644)
	r.commitAs("retire")
	if n := run(); n != 1 {
		t.Fatalf("after a retirement: %d", n)
	}
	b, _ = os.ReadFile(filepath.Join(r.clone, "projects", "widget", overview.File))
	if strings.Contains(string(b), "[[widget-port]]") {
		t.Fatalf("the retired fact is still cited:\n%s", b)
	}
	if subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%s")); !strings.Contains(subj, "is no longer live") {
		t.Fatalf("subject %q", subj)
	}
}
