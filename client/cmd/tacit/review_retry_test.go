package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// fakeGHRaw installs a gh that answers each `gh <args>` in answers with its
// stdout, `gh auth status` and `gh api user --jq .login` (user), and a 404
// for anything else.
func fakeGHRaw(t *testing.T, user string, answers map[string]string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(bin, 0o755)
	var cases strings.Builder
	for k, v := range answers {
		fmt.Fprintf(&cases, "  '%s') cat <<'EOF'\n%s\nEOF\n  exit 0;;\n", k, v)
	}
	script := "#!/bin/sh\ncase \"$*\" in\n  'auth status') exit 0;;\n  'api user --jq .login') echo " + user + "; exit 0;;\n" + cases.String() +
		"esac\necho '{\"message\":\"Not Found\",\"status\":\"404\"}'\necho 'gh: Not Found (HTTP 404)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := newForge
	newForge = func(ctx context.Context) check.Forge { return check.NewGitHub(ctx) }
	t.Cleanup(func() { newForge = prev })
}

// reviewModel leaves a folder for review unless its block shows an own
// commit the forge lists, which it cites for active. It is told nothing of
// the answers.
type reviewModel struct{ prompts []string }

var ownForgeCommitRx = regexp.MustCompile(`the owner's: an own commit, cite as (commit:[0-9a-f]{12})`)

func (m *reviewModel) run(_ context.Context, _, prompt string) (string, float64, error) {
	m.prompts = append(m.prompts, prompt)
	var out []assess.Decision
	for _, b := range blockRx.FindAllStringSubmatch(prompt, -1) {
		d := assess.Decision{Folder: b[1], Kind: "repo", Status: "needs-review", Description: "what " + b[1] + " is"}
		if c := ownForgeCommitRx.FindStringSubmatch(b[2]); c != nil {
			d.Status, d.Why = "active", "the owner pushed to it lately, "+c[1]
		} else {
			d.Why = "nothing settles it " + citeInRx.FindString(b[2])
		}
		out = append(out, d)
	}
	bs, _ := json.Marshal(out)
	return string(bs), 0.05, nil
}

// A folder left for review is asked again with more evidence, and settles
// on it: the forge's own commits and a fact elsewhere that names it reach
// the model on the second run, which cites the commit for active. A folder
// whose wider search finds nothing waits, and is asked again on the weekly
// retry, by the assessment and by the rotation, which has it in its due set.
func TestNeedsReviewIsWorkedNotParked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell gh fixture")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "overlay"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": "off", "TACIT_FOLLOW_STATE": filepath.Join(home, "no-follower.json"),
		"GIT_CONFIG_GLOBAL": filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	fakeGHRaw(t, "owner", map[string]string{
		"api repos/owner/beacon":                     `{"full_name":"owner/beacon","pushed_at":"2026-10-20T10:00:00Z","archived":false}`,
		"api repos/owner/beacon/commits?per_page=20": `[{"sha":"` + sha + `","commit":{"author":{"date":"2026-10-20T09:00:00Z"}},"author":{"login":"owner"}},{"sha":"fedcba9876543210fedcba9876543210fedcba98","commit":{"author":{"date":"2026-10-01T09:00:00Z"}},"author":{"login":"dependabot[bot]"}}]`,
	})
	os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\temail = owner@example.com\n\tname = Owner\n"), 0o644)
	os.MkdirAll(filepath.Join(home, "projects"), 0o755)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	add := func(scope, name, session, desc string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "history", Scope: scope,
			Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-10"}, Body: desc + ".\n"})
	}
	add("projects/beacon", "beacon-note", "s-b1", "The beacon service batches pings every ten seconds")
	add("projects/lonely", "lonely-note", "s-l1", "The lonely tool writes its log next to the binary")
	add("projects/widget", "widget-uses-beacon", "s-w9", "Widget reports its health to beacon over HTTP")
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "facts")
	m := &reviewModel{}
	prev := assessRun
	assessRun = m.run
	t.Cleanup(func() { assessRun = prev })
	ctx := context.Background()
	only := map[string]bool{"projects/beacon": true, "projects/lonely": true}
	recs := func() map[string]*record.Record { r, _ := record.Walk(s.Dir); return r }

	// Run 1: both are left for review.
	if errs := assessStep(ctx, s, assessOpts{model: "opus", only: only}, &curate.Report{}); errs != 0 {
		t.Fatalf("run 1: %d errors", errs)
	}
	for _, f := range []string{"projects/beacon", "projects/lonely"} {
		if r := recs()[f]; r == nil || r.Status != record.StatusNeedsReview {
			t.Fatalf("run 1 %s: %+v", f, r)
		}
	}
	// Run 2: beacon comes back with the extra evidence and settles; lonely's
	// wider search finds nothing, so it waits.
	n := len(m.prompts)
	rep2 := &curate.Report{}
	if errs := assessStep(ctx, s, assessOpts{model: "opus", only: only}, rep2); errs != 0 {
		t.Fatalf("run 2: %d errors", errs)
	}
	p := strings.Join(m.prompts[n:], "\n")
	for _, want := range []string{"### folder=projects/beacon\n", "why asked: left for review on ", "code's wider search found 5 new item(s)",
		"extra evidence code gathered for re-attempt 1", "[[widget-uses-beacon]] (projects/widget", "session:s-w9 (2026-10-10): 1 fact(s) naming it",
		"forge: github.com/owner/beacon: last push 2026-10-20", "commit:0123456789ab 2026-10-20 by owner (the owner's: an own commit", "by dependabot[bot] [new]\n"} {
		if !strings.Contains(p, want) {
			t.Errorf("run 2's prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "### folder=projects/lonely\n") {
		t.Errorf("lonely was asked though its wider search found nothing:\n%s", p)
	}
	b := recs()["projects/beacon"]
	if b == nil || b.Status != record.StatusActive || !strings.Contains(b.Was[0].Why, "commit:0123456789ab") {
		t.Fatalf("beacon after its re-attempt: %+v", b)
	}
	if rep2.ReviewRetried != 1 || rep2.ReviewSettled != 1 || rep2.AssessCandidates != 1 || rep2.ReviewWaiting != 1 {
		t.Fatalf("run 2 report: %+v", rep2)
	}
	logb, _ := os.ReadFile(filepath.Join(platform.StateDir(home), reviewAttemptsLog))
	if !strings.Contains(string(logb), `"folder":"projects/beacon","attempt":1`) || !strings.Contains(string(logb), `"outcome":"active"`) ||
		!strings.Contains(string(logb), `"forge:commit:`+sha+`"`) || !strings.Contains(string(logb), `"folder":"projects/lonely","attempt":1,"why":"left for review"`) {
		t.Fatalf("attempts log:\n%s", logb)
	}

	// A week later lonely is asked again anyway: by the rotation, whose due
	// set holds it, and then not again the same day by the assessment.
	heldPath := filepath.Join(platform.StateDir(home), assessHeldFile)
	h := loadHeld(heldPath)
	st := h.Review["projects/lonely"]
	st.At = time.Now().AddDate(0, 0, -reviewRetryDays).Format("2006-01-02")
	h.Review["projects/lonely"] = st
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	if due := reviewFoldersDue(recs(), loadHeld(heldPath), time.Now().Format("2006-01-02")); len(due) != 1 || due[0] != "projects/lonely" {
		t.Fatalf("rotation's due folders: %v", due)
	}
	n = len(m.prompts)
	var rst rotationStats
	if errs := rotateReviewFolders(ctx, s, false, rotationOpts{model: "opus"}, &rst, &curate.Report{}); errs != 0 || rst.Folders != 1 {
		t.Fatalf("rotation: %d errors, %+v", errs, rst)
	}
	if p := strings.Join(m.prompts[n:], "\n"); !strings.Contains(p, "### folder=projects/lonely\n") || !strings.Contains(p, "the weekly retry") {
		t.Fatalf("the rotation's prompt:\n%s", p)
	}
	n = len(m.prompts)
	rep3 := &curate.Report{}
	assessStep(ctx, s, assessOpts{model: "opus", only: only}, rep3)
	if len(m.prompts) != n || rep3.AssessCandidates != 0 {
		t.Fatalf("asked again the same day: %+v", rep3)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// The extra evidence's items are the fingerprint's: a search that finds the
// same items leaves it as it was, whatever the attempt or what was seen.
func TestFingerprintReadsTheExtraItems(t *testing.T) {
	c := assess.Candidate{Folder: "projects/x"}
	base := assess.Fingerprint(c)
	c.Extra = []assess.Extra{{Line: "store search: header"}}
	if assess.Fingerprint(c) != base {
		t.Fatal("a header changed the fingerprint")
	}
	c.Extra = append(c.Extra, assess.Extra{Key: "fact:a", Line: "  - [[a]]"})
	with := assess.Fingerprint(c)
	if with == base {
		t.Fatal("a new item left the fingerprint")
	}
	c.Attempt, c.LastAt, c.Seen = 3, "2026-10-01", map[string]bool{"fact:a": true}
	if assess.Fingerprint(c) != with || c.NewExtra() != 0 {
		t.Fatal("the attempt or what was seen moved the fingerprint")
	}
}

func TestRetryDue(t *testing.T) {
	for _, c := range []struct {
		at, today string
		want      bool
	}{{"2026-10-15", "2026-10-22", true}, {"2026-10-16", "2026-10-22", false}, {"", "2026-10-22", true}} {
		if got := retryDue(c.at, c.today); got != c.want {
			t.Errorf("retryDue(%q, %q) = %v", c.at, c.today, got)
		}
	}
}
