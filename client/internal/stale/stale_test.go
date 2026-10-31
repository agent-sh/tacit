package stale

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
)

func git(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit writes files, removes rm, and commits on the given day as who.
func commit(t *testing.T, dir, day, msg string, files map[string]string, rm ...string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range rm {
		os.Remove(filepath.Join(dir, name))
	}
	git(t, dir, nil, "add", "-A")
	when := day + "T12:00:00+00:00"
	git(t, dir, []string{"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.test", "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.test", "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", msg)
}

const engine = "// engine\nconst MAX_RETRIES: u32 = 3;\nconst BACKOFF_MS: u32 = 200;\nfn run() {\n    retry(MAX_RETRIES);\n    sleep(BACKOFF_MS);\n}\nfn other() {}\n"

// rename is the engine file with a name changed everywhere it appears.
func rename(body, from, to string) string { return strings.ReplaceAll(body, from, to) }

// repoX is a repo "x" under a projects directory, with src/engine.rs written
// on 2026-09-20, before the facts below.
func repoX(t *testing.T) (projects, repo string) {
	projects = t.TempDir()
	repo = filepath.Join(projects, "x")
	os.MkdirAll(repo, 0o755)
	git(t, repo, nil, "init", "-q", "-b", "main")
	commit(t, repo, "2026-09-20", "engine: first version", map[string]string{"src/engine.rs": engine, "src/old.rs": "pub fn old() {}\n", "README.md": "x\n"})
	return projects, repo
}

func builder(projects string) *gitev.Builder {
	return &gitev.Builder{G: gitev.New(projects), Redact: func(in []string) ([]string, error) { return in, nil }}
}

func mkfact(name, created, body string) *fact.Fact {
	return &fact.Fact{
		Path: "projects/x/" + name + ".md",
		Meta: fact.Meta{Name: name, Description: "about " + name, Type: "design_decision", Scope: "projects/x", Created: created,
			Source: fact.Source{Harness: "curator", Host: "h", Kind: "observed"}},
		Body: body,
	}
}

func check(t *testing.T, b *gitev.Builder, in Input) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return Check(ctx, b, in)
}

// A later commit that deletes a name the fact states (a rename leaves the old
// name nowhere) flags the fact; the finding names the commit and the name.
func TestFlagsAFactWhenALaterCommitRemovesANameItStates(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-05", "engine: name the retry limit", map[string]string{"src/engine.rs": rename(engine, "MAX_RETRIES", "RETRY_LIMIT")})
	f := mkfact("engine-retries", "2026-10-01", "The engine retries `MAX_RETRIES` times, three, in src/engine.rs.\n")
	r := check(t, builder(projects), Input{Fact: f})
	if r.Finding == nil {
		t.Fatalf("not flagged: skip %q err %v", r.Skip, r.Err)
	}
	fd := r.Finding
	if fd.N != 1 || len(fd.Triggers) != 1 || fd.Triggers[0].Subject != "engine: name the retry limit" || fd.Triggers[0].Repo != "x" || fd.Sha == "" {
		t.Errorf("finding %+v", fd)
	}
	if len(fd.Terms) != 1 || fd.Terms[0] != "MAX_RETRIES" || fd.Triggers[0].Terms[0] != "MAX_RETRIES (gone at HEAD)" {
		t.Errorf("terms %v %v", fd.Terms, fd.Triggers[0].Terms)
	}
	if fd.SinceWhy != "created" || fd.Since.Format("2006-01-02") != "2026-10-02" {
		t.Errorf("since %v why %q", fd.Since, fd.SinceWhy)
	}
	if !strings.HasPrefix(fd.Block(), "- stale? [[engine-retries]] (projects/x) @"+fd.Sha+" n=1 since=2026-10-02") {
		t.Errorf("block:\n%s", fd.Block())
	}
}

// A fact nothing touched after its date is not flagged.
func TestAnUntouchedFactIsNotFlagged(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-09-25", "engine: name the retry limit (before the fact)", map[string]string{"src/engine.rs": rename(engine, "MAX_RETRIES", "RETRY_LIMIT")})
	f := mkfact("engine-retries", "2026-10-01", "The engine retries `RETRY_LIMIT` times, in src/engine.rs.\n")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding != nil || r.Skip != SkipQuiet {
		t.Fatalf("flagged or wrong skip: %+v %q", r.Finding, r.Skip)
	}
	// Nor is a fact about a name that was already gone when it was written.
	old := mkfact("engine-old-name", "2026-10-01", "The engine retries `MAX_RETRIES` times, in src/engine.rs.\n")
	if r := check(t, builder(projects), Input{Fact: old}); r.Finding != nil || r.Skip != SkipQuiet {
		t.Fatalf("a name removed before the fact's date flagged it: %+v %q", r.Finding, r.Skip)
	}
}

// A changed value with its name standing is not a deletion: the commit rewrites
// the line, HEAD still holds the name, and the fact may well still be true. In
// a hand-checked sample from a real store no such flag was stale.
func TestAChangedValueWithItsNameStandingIsNotFlagged(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-05", "engine: retry five times", map[string]string{"src/engine.rs": strings.Replace(engine, "= 3;", "= 5;", 1)})
	f := mkfact("engine-retries", "2026-10-01", "The engine retries `MAX_RETRIES` times, three, in src/engine.rs.\n")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding != nil || r.Skip != SkipQuiet {
		t.Fatalf("flagged on a changed value: %+v %q", r.Finding, r.Skip)
	}
}

// A hot file that every commit touches does not flag a fact about it: only a
// deletion of what the fact names does. Five later commits edit other lines of
// src/engine.rs, a sixth adds a caller and a seventh rewrites the value.
func TestHotFileTouchedByUnrelatedCommitsIsNotFlagged(t *testing.T) {
	projects, repo := repoX(t)
	body := engine
	for i, day := range []string{"2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07"} {
		body += "fn extra_" + string(rune('a'+i)) + "() {}\n"
		commit(t, repo, day, "engine: touch "+day, map[string]string{"src/engine.rs": body})
	}
	body += "fn another() { retry(MAX_RETRIES); }\n" // adds a caller, removes nothing
	commit(t, repo, "2026-10-08", "engine: one more caller", map[string]string{"src/engine.rs": body})
	body = strings.Replace(body, "= 3;", "= 4;", 1)
	commit(t, repo, "2026-10-09", "engine: retry four times", map[string]string{"src/engine.rs": body})
	f := mkfact("engine-retries", "2026-10-01", "The engine in src/engine.rs retries `MAX_RETRIES` times.\n")
	r := check(t, builder(projects), Input{Fact: f})
	if r.Finding != nil || r.Skip != SkipQuiet {
		t.Fatalf("flagged on path touches, an addition or a rewritten value: %+v skip %q", r.Finding, r.Skip)
	}
}

// A file the fact names that was deleted or renamed after its date flags it.
func TestAFileDeletedAfterTheFactFlagsIt(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-06", "drop the old module", nil, "src/old.rs")
	f := mkfact("old-module", "2026-10-01", "The helper lives in `src/old.rs`.\n")
	r := check(t, builder(projects), Input{Fact: f})
	if r.Finding == nil || r.Finding.N != 1 || !strings.Contains(r.Finding.Triggers[0].Terms[0], "src/old.rs (gone at HEAD)") {
		t.Fatalf("finding %+v skip %q", r.Finding, r.Skip)
	}
}

// What the store already says: the same newest commit is not flagged twice, a
// finding settled "still true" moves the fact's date to that instant, and a
// deletion after the mark is a new event, a deleted file included.
func TestSettledFindingsDoNotMuteALaterDeletion(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-05", "engine: name the retry limit", map[string]string{"src/engine.rs": rename(engine, "MAX_RETRIES", "RETRY_LIMIT")})
	f := mkfact("engine-retries", "2026-10-01", "The engine retries `MAX_RETRIES` times with a `BACKOFF_MS` pause, in src/engine.rs, and the helper is `src/old.rs`.\n")
	b := builder(projects)
	first := check(t, b, Input{Fact: f})
	if first.Finding == nil {
		t.Fatal("first flag missing")
	}
	// The same newest commit never makes a second finding.
	if r := check(t, b, Input{Fact: f, LastSha: first.Finding.Sha}); r.Finding != nil || r.Skip != SkipSame {
		t.Fatalf("flagged twice for %s: %+v skip %q", first.Finding.Sha, r.Finding, r.Skip)
	}
	// Settled still true on 2026-10-09: the 10-05 commit is behind the mark.
	verified := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if r := check(t, b, Input{Fact: f, Verified: verified, LastSha: first.Finding.Sha}); r.Finding != nil || r.Skip != SkipQuiet {
		t.Fatalf("a settled finding flagged again: %+v skip %q", r.Finding, r.Skip)
	}
	// One name deleted after the mark flags again, however many the settled
	// finding had.
	commit(t, repo, "2026-10-12", "engine: name the pause", map[string]string{"src/engine.rs": rename(rename(engine, "MAX_RETRIES", "RETRY_LIMIT"), "BACKOFF_MS", "PAUSE_MS")})
	r := check(t, b, Input{Fact: f, Verified: verified, LastSha: first.Finding.Sha})
	if r.Finding == nil || r.Finding.SinceWhy != "verified" || r.Finding.N != 1 {
		t.Fatalf("a deleted name after a settled finding: %+v skip %q", r.Finding, r.Skip)
	}
	// And so does a deleted file.
	commit(t, repo, "2026-10-13", "drop the old module", nil, "src/old.rs")
	r = check(t, b, Input{Fact: f, Verified: verified.AddDate(0, 0, 4), LastSha: first.Finding.Sha})
	if r.Finding == nil || r.Finding.N != 1 || !strings.Contains(r.Finding.Terms[0], "src/old.rs") {
		t.Fatalf("a deleted file after a settled finding: %+v skip %q", r.Finding, r.Skip)
	}
}

// failingGit puts a git on PATH that fails every command naming one of fail.
func failingGit(t *testing.T, fail ...string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	var cases []string
	for _, w := range fail {
		cases = append(cases, `"`+w+`"`)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in " + strings.Join(cases, "|") + ") echo \"fatal: injected failure\" >&2; exit 128;; esac; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A read that did not finish flags nothing, whatever else it found, and is not
// "quiet": a failed file list must not make a file that was only edited look
// deleted. With the real git it is flagged by a deleted file; with ls-tree
// failing the same fact is incomplete, a ruling included.
func TestAFailedReadFlagsNothingAndIsNotQuiet(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-05", "engine: edit the file", map[string]string{"src/engine.rs": engine + "fn extra() {}\n"})
	commit(t, repo, "2026-10-06", "drop the old module", nil, "src/old.rs")
	f := mkfact("engine-files", "2026-10-01", "The engine is `src/engine.rs` and the helper `src/old.rs`.\n")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding == nil || r.Finding.N != 1 {
		t.Fatalf("control: %+v skip %q", r.Finding, r.Skip)
	}
	failingGit(t, "ls-tree")
	for _, ruling := range []bool{false, true} {
		r := check(t, builder(projects), Input{Fact: f, Ruling: ruling})
		if r.Finding != nil || r.Skip != SkipIncomplete {
			t.Fatalf("ruling %v: finding %+v skip %q, want incomplete and nothing flagged", ruling, r.Finding, r.Skip)
		}
	}
}

// A search of the tree that failed leaves an identifier neither present nor
// absent: nothing is flagged on it.
func TestAFailedIdentifierSearchIsNotAnAbsence(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-10-05", "engine: name the retry limit", map[string]string{"src/engine.rs": rename(engine, "MAX_RETRIES", "RETRY_LIMIT")})
	f := mkfact("engine-retries", "2026-10-01", "The engine retries `MAX_RETRIES` times.\n")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding == nil {
		t.Fatalf("control: skip %q", r.Skip)
	}
	failingGit(t, "grep")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding != nil || r.Skip != SkipIncomplete {
		t.Fatalf("finding %+v skip %q", r.Finding, r.Skip)
	}
}

// A name is gone only when no repo the fact is about holds it. The fact is
// about repo x and mentions sidecar; sidecar deleted its own config.toml, x
// still has its own.
func TestANameAnotherRepoDeletedIsNotGoneWhenTheFactsRepoHoldsIt(t *testing.T) {
	projects, repo := repoX(t)
	commit(t, repo, "2026-09-21", "x: config", map[string]string{"config.toml": "a = 1\n", "src/names.rs": "const SIDE_NAME: u32 = 1;\n"})
	side := filepath.Join(projects, "sidecar")
	os.MkdirAll(side, 0o755)
	git(t, side, nil, "init", "-q", "-b", "main")
	commit(t, side, "2026-09-20", "sidecar: first", map[string]string{"config.toml": "b = 2\n", "src/names.rs": "const SIDE_NAME: u32 = 2;\n"})
	commit(t, side, "2026-10-06", "sidecar: drop its config", nil, "config.toml", "src/names.rs")
	f := mkfact("engine-config", "2026-10-01", "The engine reads `config.toml` and `SIDE_NAME`, and talks to the sidecar.\n")
	f.Meta.AppliesTo.Repos = []string{"sidecar"}
	r := check(t, builder(projects), Input{Fact: f})
	if r.Finding != nil {
		t.Fatalf("flagged on another repo's deletion: %+v", r.Finding)
	}
	if r.Skip != SkipQuiet {
		t.Fatalf("skip %q", r.Skip)
	}
	// Nothing in repo x holds SIDE_NAME and config.toml is gone from sidecar only
	// if x does not have it either: remove it from x too and it is gone everywhere.
	commit(t, repo, "2026-10-07", "x: drop config and names", nil, "config.toml", "src/names.rs")
	if r := check(t, builder(projects), Input{Fact: f}); r.Finding == nil || r.Finding.N < 2 {
		t.Fatalf("gone from every repo: %+v skip %q", r.Finding, r.Skip)
	}
}

// What is written to the review file is redacted before it is clipped: a token
// that straddles the 90-rune clip would otherwise lose the tail scrump needs to
// recognise it. Needs the real scrump text profile.
func TestASecretInACommitSubjectIsRedactedBeforeTheClip(t *testing.T) {
	if err := digest.RedactionReady(); err != nil {
		t.Skip("no scrump with the text profile: " + err.Error())
	}
	projects, repo := repoX(t)
	// A GitHub-shaped token built from pieces: 4 + 36 characters starting at
	// rune 76 of the subject, so the 90-rune clip cuts it.
	token := "ghp" + "_" + strings.Repeat("aB3dE5fG7h", 3) + "iJ4kL6"
	subject := strings.Repeat("x", 71) + " rotate " + token + " now"
	commit(t, repo, "2026-10-06", subject, nil, "src/old.rs")
	f := mkfact("old-module", "2026-10-01", "The helper lives in `src/old.rs`.\n")
	b := &gitev.Builder{G: gitev.New(projects)} // the real redactor
	r := check(t, b, Input{Fact: f})
	if r.Finding == nil {
		t.Fatalf("not flagged: skip %q err %v", r.Skip, r.Err)
	}
	got := r.Finding.Triggers[0].Subject
	if strings.Contains(got, "aB3dE5") || strings.Contains(r.Finding.Block(), "aB3dE5") {
		t.Fatalf("a piece of the token survived the clip:\n%s", r.Finding.Block())
	}
	if len([]rune(got)) > 90 || !strings.Contains(got, "****") {
		t.Fatalf("subject %q: want it masked and at most 90 runes", got)
	}
}

// A fact that names no repo on this machine, or nothing git can be asked
// about, is skipped with the reason.
func TestFactsWithoutAGitTraceAreSkipped(t *testing.T) {
	projects, _ := repoX(t)
	b := builder(projects)
	user := mkfact("prefers-short-answers", "2026-10-01", "Keep answers short.\n")
	user.Meta.Scope, user.Path = "user", "user/prefers-short-answers.md"
	if r := check(t, b, Input{Fact: user}); r.Skip != SkipNoRepo {
		t.Errorf("no repo: %q", r.Skip)
	}
	bare := mkfact("x-general", "2026-10-01", "Be careful with this one.\n")
	if r := check(t, b, Input{Fact: bare}); r.Skip != SkipNoSubject {
		t.Errorf("no subject: %q", r.Skip)
	}
	undated := mkfact("undated", "", "The engine retries `MAX_RETRIES` times.\n")
	if r := check(t, b, Input{Fact: undated}); r.Skip != SkipNoDate {
		t.Errorf("no date: %q", r.Skip)
	}
}

// Order puts the finding with the most changes first, then the newest.
func TestOrderPutsTheStrongestFirst(t *testing.T) {
	at := func(d int) []Trigger { return []Trigger{{At: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC)}} }
	fs := []*Finding{{Name: "b", N: 1, Triggers: at(9)}, {Name: "a", N: 1, Triggers: at(9)}, {Name: "c", N: 3, Triggers: at(2)}, {Name: "d", N: 1, Triggers: at(10)}}
	Order(fs)
	var got []string
	for _, f := range fs {
		got = append(got, f.Name)
	}
	if strings.Join(got, "") != "cdab" {
		t.Fatalf("order %v", got)
	}
}
