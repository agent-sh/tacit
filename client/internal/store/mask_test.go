package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// scrumpErr is why scrump with its text profile cannot run here, or nil;
// read before TestMain puts a stand-in on PATH.
var scrumpErr error

// TestMain: every write masks what it adds with scrump (Redact), so a test
// that writes needs it. Where scrump with its text profile is absent, a
// stand-in that masks only the canary digest.RedactionReady checks goes
// first on PATH; the tests of the mask itself use their own redactor, or the
// real scrump and skip without it.
func TestMain(m *testing.M) {
	scrumpErr = digest.RedactionReady()
	var stub string
	if scrumpErr != nil && runtime.GOOS != "windows" {
		if dir, err := os.MkdirTemp("", "tacit-store-scrump-"); err == nil {
			stub = dir
			_ = os.WriteFile(filepath.Join(dir, "scrump"), []byte("#!/bin/sh\nexec sed 's/Zq7Vx9/******/g'\n"), 0o755)
			os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	code := m.Run()
	if stub != "" {
		os.RemoveAll(stub)
	}
	os.Exit(code)
}

// fakeKey is the shape the test redactor masks; no real key looks like it.
// The lower-case form is a fact name's shape, which a link holds.
var fakeKey = regexp.MustCompile(`(?i)fakekey-[0-9]+`)

// stars is s masked.
func stars(s string) string { return strings.Repeat("*", len(s)) }

// withRedact runs the test with red as the store's redactor.
func withRedact(t *testing.T, red func(context.Context, []string) ([]string, error)) {
	t.Helper()
	prev := Redact
	Redact = red
	t.Cleanup(func() { Redact = prev })
}

// maskFake masks fakeKey, each text on its own, and records what it was
// asked.
func maskFake(asked *[]string) func(context.Context, []string) ([]string, error) {
	return func(_ context.Context, texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, s := range texts {
			*asked = append(*asked, s)
			out[i] = fakeKey.ReplaceAllStringFunc(s, func(k string) string { return strings.Repeat("*", len(k)) })
		}
		return out, nil
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// committed is a file as HEAD and the server hold it.
func committed(t *testing.T, s *Store, rel string) (head, server string) {
	t.Helper()
	head = run(t, s.Dir, "show", "HEAD:"+rel)
	server = run(t, s.Dir, "show", "origin/main:"+rel)
	return head, server
}

// A write masks what it adds, whoever wrote it: a new note's text and the
// lines a write adds to a fact. What HEAD already holds is not rewritten,
// and only the added lines go to the redactor.
func TestAWriteMasksWhatItAddsAndNothingElse(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	// A fact committed before the mask, holding a key: the store's own past.
	writeFile(t, s.Dir, "machines/h/serve.md", "---\nname: serve\n---\n\nold flags FAKEKEY-111 here\n")
	run(t, s.Dir, "add", "machines/h/serve.md")
	run(t, s.Dir, "commit", "-q", "-m", "old")
	run(t, s.Dir, "push", "-q", "origin", "HEAD:main")

	var asked []string
	withRedact(t, maskFake(&asked))
	writeFile(t, s.Dir, "machines/h/serve.md", "---\nname: serve\n---\n\nold flags FAKEKEY-111 here\nnew flags FAKEKEY-222 there\n")
	writeFile(t, s.Dir, "inbox/x@h/note.md", "---\nname: note\n---\n\nthe wrapper passes FAKEKEY-333\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPush(ctx, []string{"machines/h/serve.md", "inbox/x@h/note.md"}, "curator: test", "curator"); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"machines/h/serve.md": "---\nname: serve\n---\n\nold flags FAKEKEY-111 here\nnew flags " + stars("FAKEKEY-222") + " there\n",
		"inbox/x@h/note.md":   "---\nname: note\n---\n\nthe wrapper passes " + stars("FAKEKEY-333") + "\n",
	} {
		head, server := committed(t, s, rel)
		if head != want || server != want {
			t.Errorf("%s committed as\n%q\non the server\n%q\nwant\n%q", rel, head, server, want)
		}
		if b, _ := os.ReadFile(filepath.Join(s.Dir, rel)); string(b) != want {
			t.Errorf("%s on disk %q, want the masked text", rel, b)
		}
	}
	for _, a := range asked {
		if strings.Contains(a, "FAKEKEY-111") {
			t.Errorf("the redactor was asked about a line HEAD already held: %q", a)
		}
	}
}

// The mask takes prose only: a description it masks is written back as
// YAML that reads, while the frontmatter's other fields (a session, an
// origin, a name), a [[link]] and the owner's recorded words stay as they
// are, whatever the redactor would make of them.
func TestTheMaskLeavesNamesSessionsLinksAndTheOwnersWords(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	var asked []string
	withRedact(t, maskFake(&asked))
	note := "---\nname: serve-flags\ndescription: FAKEKEY-121 opens the description\ntype: machine_fact\norigin: fakekey-131\nsource:\n  harness: claude\n  session: claude:FAKEKEY-141\n  owner_quote: keep FAKEKEY-151 exactly\n---\n\nSee [[fakekey-161]] for the wrapper.\nThe owner said: keep FAKEKEY-151 exactly.\nThe agent pasted FAKEKEY-171 here.\n"
	writeFile(t, s.Dir, "machines/h/serve-flags.md", note)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPush(ctx, []string{"machines/h/serve-flags.md"}, "curator: test", "curator"); err != nil {
		t.Fatal(err)
	}
	head, _ := committed(t, s, "machines/h/serve-flags.md")
	for _, kept := range []string{"origin: fakekey-131", "session: claude:FAKEKEY-141", "owner_quote: keep FAKEKEY-151 exactly", "[[fakekey-161]]", "The owner said: keep FAKEKEY-151 exactly."} {
		if !strings.Contains(head, kept) {
			t.Errorf("the mask changed %q:\n%s", kept, head)
		}
	}
	if strings.Contains(head, "FAKEKEY-171") || strings.Contains(head, "FAKEKEY-121") {
		t.Errorf("the body's or the description's key reached the commit:\n%s", head)
	}
	fm, _, ok := fact.Split([]byte(head))
	var meta struct{ Description, Name, Origin string }
	if !ok || yaml.Unmarshal(fm, &meta) != nil || meta.Description != stars("FAKEKEY-121")+" opens the description" || meta.Name != "serve-flags" || meta.Origin != "fakekey-131" {
		t.Fatalf("the frontmatter does not read back as masked: %+v\n%s", meta, head)
	}
	for _, a := range asked {
		if strings.Contains(a, "session:") || strings.Contains(a, "origin:") {
			t.Errorf("a frontmatter field went to the redactor: %q", a)
		}
	}
}

// A file with no frontmatter (a review file) is prose throughout, its
// [[links]] aside.
func TestAReviewFileIsMaskedAroundItsLinks(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	var asked []string
	withRedact(t, maskFake(&asked))
	writeFile(t, s.Dir, "review/2026-10-30.md", "- stale? [[fakekey-181]] (machines/h) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FAKEKEY-191 - FOO (gone at HEAD)\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPush(ctx, []string{"review/2026-10-30.md"}, "curator: test", "curator"); err != nil {
		t.Fatal(err)
	}
	head, _ := committed(t, s, "review/2026-10-30.md")
	if want := "- stale? [[fakekey-181]] (machines/h) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop " + stars("FAKEKEY-191") + " - FOO (gone at HEAD)\n"; head != want {
		t.Fatalf("committed\n%q\nwant\n%q", head, want)
	}
}

// A write whose mask cannot run is refused before anything is staged.
func TestAWriteIsRefusedWhenItsMaskCannotRun(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	withRedact(t, func(context.Context, []string) ([]string, error) { return nil, errors.New("scrump: not found") })
	before := strings.TrimSpace(run(t, s.Dir, "rev-parse", "HEAD"))
	writeFile(t, s.Dir, "inbox/x@h/note.md", "---\nname: note\n---\n\nthe wrapper passes FAKEKEY-333\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.CommitPush(ctx, []string{"inbox/x@h/note.md"}, "claude: add note", "claude")
	if !errors.Is(err, ErrRedaction) {
		t.Fatalf("CommitPush = %v, want ErrRedaction", err)
	}
	if now := strings.TrimSpace(run(t, s.Dir, "rev-parse", "HEAD")); now != before {
		t.Fatalf("a commit was made: %s -> %s", before, now)
	}
	if staged := strings.TrimSpace(run(t, s.Dir, "diff", "--cached", "--name-only")); staged != "" {
		t.Fatalf("staged: %q", staged)
	}
	if server := strings.TrimSpace(run(t, s.Dir, "ls-remote", "origin", "main")); !strings.HasPrefix(server, before) {
		t.Fatalf("the server moved: %s", server)
	}
}

// A redactor that answers fewer texts than it was asked is a failure too.
func TestAShortRedactorAnswerRefusesTheWrite(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	withRedact(t, func(context.Context, []string) ([]string, error) { return nil, nil })
	writeFile(t, s.Dir, "inbox/x@h/note.md", "---\nname: note\n---\n\nbody\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPush(ctx, []string{"inbox/x@h/note.md"}, "claude: add note", "claude"); !errors.Is(err, ErrRedaction) {
		t.Fatalf("CommitPush = %v, want ErrRedaction", err)
	}
}

// The owner's own text from tacit app is theirs: their write is not masked.
func TestTheOwnersOwnTextIsNotMasked(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	var asked []string
	withRedact(t, maskFake(&asked))
	body := "---\nname: rule\n---\n\nmy dummy FAKEKEY-444 stays as I wrote it\n"
	writeFile(t, s.Dir, "user/rule.md", body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPushNamedOnceLocked(WithOwnerText(ctx), []string{"user/rule.md"}, "owner: new ruling rule (user)", "curator"); err != nil {
		t.Fatal(err)
	}
	if head, _ := committed(t, s, "user/rule.md"); head != body {
		t.Fatalf("committed %q, want the owner's text %q", head, body)
	}
	if len(asked) != 0 {
		t.Fatalf("the redactor was asked about the owner's text: %q", asked)
	}
}

// The indexes are generated from the facts, never masked themselves: a write
// that masked a fact writes them again from the masked text (Reindex), and
// what Reindex wrote joins the commit.
func TestAMaskedFactWritesTheIndexesAgain(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	var asked []string
	withRedact(t, maskFake(&asked))
	calls := 0
	prev := Reindex
	Reindex = func(_ context.Context, dir string) ([]string, error) {
		calls++
		b, err := os.ReadFile(filepath.Join(dir, "machines", "h", "serve.md"))
		if err != nil {
			return nil, err
		}
		line := strings.TrimSpace(strings.Split(string(b), "\n---\n")[1])
		return []string{"MEMORY.md", "machines/h/_index.md"}, errors.Join(
			os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- serve: "+line+"\n"), 0o644),
			os.WriteFile(filepath.Join(dir, "machines", "h", "_index.md"), []byte("- serve: "+line+"\n"), 0o644))
	}
	t.Cleanup(func() { Reindex = prev })
	writeFile(t, s.Dir, "machines/h/serve.md", "---\nname: serve\n---\n\nflags FAKEKEY-555\n")
	// The caller generated MEMORY.md before the commit, from the text it wrote.
	writeFile(t, s.Dir, "MEMORY.md", "- serve: flags FAKEKEY-555\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPushNamedOnceLocked(ctx, []string{"machines/h/serve.md", "MEMORY.md"}, "curator: test", "curator"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Reindex ran %d times, want 1", calls)
	}
	for _, rel := range []string{"MEMORY.md", "machines/h/_index.md"} {
		head, server := committed(t, s, rel)
		if want := "- serve: flags " + stars("FAKEKEY-555") + "\n"; head != want || server != want {
			t.Errorf("%s committed as %q (server %q), want %q", rel, head, server, want)
		}
	}
	for _, a := range asked {
		if strings.HasPrefix(a, "- serve") {
			t.Errorf("a generated index went to the redactor: %q", a)
		}
	}
	// A write that masks nothing does not write the indexes again.
	writeFile(t, s.Dir, "machines/h/other.md", "---\nname: other\n---\n\nplain\n")
	if err := s.CommitPushNamedOnceLocked(ctx, []string{"machines/h/other.md"}, "curator: test 2", "curator"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Reindex ran on a write that masked nothing (%d calls)", calls)
	}
}

// The real mask (scrump's text profile) takes the shape the 2026-10-26
// rebuild wrote into a curated fact: a loopback llama-server key on a
// command line. The key here is synthetic.
func TestTheRealMaskTakesALoopbackServerKey(t *testing.T) {
	if scrumpErr != nil {
		t.Skip("needs scrump with the text profile: " + scrumpErr.Error())
	}
	s, _ := seedPair(t)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	key := "qwen" + "local" + "7Hc3Pz9Rk2Lm5"
	writeFile(t, s.Dir, "machines/h/serve-wrapper.md", fmt.Sprintf("---\nname: serve-wrapper\ndescription: the serve wrapper is the source of truth for argv\n---\n\nProduction flags include -fa on, --jinja, `--api-key %s`, `--metrics`.\n", key))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.CommitPush(ctx, []string{"machines/h/serve-wrapper.md"}, "curator: test", "curator"); err != nil {
		t.Fatal(err)
	}
	head, server := committed(t, s, "machines/h/serve-wrapper.md")
	if strings.Contains(head, key) || strings.Contains(server, key) {
		t.Fatalf("the key reached the commit:\n%s", head)
	}
	if !strings.Contains(head, "--api-key ****") || !strings.Contains(head, "the serve wrapper is the source of truth") {
		t.Fatalf("the mask took more or less than the key:\n%s", head)
	}
}

func TestAddedRuns(t *testing.T) {
	for _, c := range []struct {
		old, now string
		want     [][2]int
	}{
		{"", "a\nb\n", [][2]int{{0, 2}}},
		{"a\nb\n", "a\nb\n", nil},
		{"a\nb\n", "a\nx\nb\ny\n", [][2]int{{1, 2}, {3, 4}}},
		{"a\nb", "a\nb\n", nil},             // a newline added at the end
		{"a\n", "a\na\n", [][2]int{{1, 2}}}, // a copy of an old line is new
		{"a\nb\n", "b\na\n", nil},           // moved lines are old
	} {
		if got := addedRuns(splitLines(c.old), splitLines(c.now)); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("addedRuns(%q, %q) = %v, want %v", c.old, c.now, got, c.want)
		}
	}
}

// The mask reads and writes no file outside the clone or through a link in
// it: git stages none of those, and a write back would land outside.
func TestTheMaskStaysInsideTheClone(t *testing.T) {
	s, _ := seedPair(t)
	var asked []string
	withRedact(t, maskFake(&asked))
	outside := t.TempDir()
	writeFile(t, outside, "f.md", "---\nname: f\n---\n\nFAKEKEY-911\n")
	if err := os.MkdirAll(filepath.Join(s.Dir, "inbox", "x@h"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Dir, "inbox", "x@h", "link")); err != nil {
		t.Skip("no symlinks here")
	}
	masked, err := s.maskAdded(context.Background(), []string{"inbox/x@h/link/f.md", "../f.md"})
	if err != nil || len(masked) != 0 || len(asked) != 0 {
		t.Fatalf("masked %v (%v), asked %q", masked, err, asked)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "f.md")); !strings.Contains(string(b), "FAKEKEY-911") {
		t.Fatal("the mask wrote a file outside the clone")
	}
}
