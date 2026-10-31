package results

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/gitev"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fixtureRepo(t *testing.T) (root, ref string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "r")
	os.MkdirAll(dir, 0o755)
	gitT(t, dir, "init", "-q", "-b", "main")
	for i, v := range []string{"10.5", "11.5"} {
		line := `{"ts":"2026-09-0` + string(rune('1'+i)) + `T10:00:00Z","cell":"a-1","toks":` + v + "}\n"
		prev, _ := os.ReadFile(filepath.Join(dir, "res.jsonl"))
		os.WriteFile(filepath.Join(dir, "res.jsonl"), append(prev, line...), 0o644)
		gitT(t, dir, "add", "-A")
		gitT(t, dir, "commit", "-q", "-m", "c")
	}
	return root, gitT(t, dir, "rev-parse", "HEAD")
}

// A read bound the step sets itself is a partial read: what was read stands
// and the caller retires nothing. A read that git failed is a failed read.
func TestReaderTellsAPartialReadFromAFailedOne(t *testing.T) {
	root, ref := fixtureRepo(t)
	g := gitev.New(root)
	ctx := context.Background()

	r := &Reader{G: g, Dir: filepath.Join(root, "r"), Ref: ref}
	st := &Stats{}
	if series := r.Series(ctx, "r", st); len(series) != 1 || len(r.Failed)+len(r.Partial) != 0 {
		t.Fatalf("a whole read: %d series, failed %v partial %v", len(series), r.Failed, r.Partial)
	}

	r = &Reader{G: g, Dir: filepath.Join(root, "r"), Ref: ref}
	got := r.Blobs(ctx, []blobReq{{ref, "res.jsonl"}}, blobLimit, 1)
	if len(got) != 0 || len(r.Partial) != 1 || len(r.Failed) != 0 {
		t.Fatalf("a byte budget that ran out is partial: got %d, partial %v, failed %v", len(got), r.Partial, r.Failed)
	}

	r = &Reader{G: g, Dir: filepath.Join(root, "missing"), Ref: ref}
	r.Series(ctx, "r", &Stats{})
	if len(r.Failed) == 0 {
		t.Fatal("a repo git cannot read is a failed read")
	}
}

// The series of a finished read are kept per commit and read back whole.
func TestSeriesCacheRoundTrips(t *testing.T) {
	root, ref := fixtureRepo(t)
	r := &Reader{G: gitev.New(root), Dir: filepath.Join(root, "r"), Ref: ref}
	st := &Stats{}
	series := r.Series(context.Background(), "r", st)
	dir := t.TempDir()
	cacheSave(dir, "r", ref, series, *st)
	c, ok := cacheLoad(dir, "r", ref)
	if !ok || len(c.Series) != 1 || c.Series[0].Cur().Raw != "11.5" || c.Stats.Series != st.Series {
		t.Fatalf("cache: %v %+v", ok, c)
	}
	if _, ok := cacheLoad(dir, "r", strings.Repeat("0", 40)); ok {
		t.Fatal("another commit must miss")
	}
	cacheSave(dir, "r", strings.Repeat("1", 40), series, *st)
	if _, ok := cacheLoad(dir, "r", ref); ok {
		t.Fatal("an older commit's entry is removed when a newer one is saved")
	}
}

// A header that says one unit and a cell that says another: the source
// disagrees with itself, so no number is taken from that cell.
func TestTableCellWithAUnitThatContradictsItsHeaderIsSkipped(t *testing.T) {
	data := "## T\n\n| Shape | ms | vs single |\n|---|---:|---:|\n| split-K at 64 | 5.31 us | 1.212x |\n| split-K at 512 | 2.84 ms | 1.075x |\n"
	got := map[string]Obs{}
	for _, o := range extractMarkdown([]byte(data)) {
		got[o.Key] = o
	}
	if _, ok := got["split-K at 64|ms"]; ok {
		t.Fatal("5.31 us under a header in ms was taken")
	}
	if o, ok := got["split-K at 512|ms"]; !ok || o.Unit != "ms" {
		t.Fatalf("a cell that agrees with its header: %+v", o)
	}
	if _, ok := got["split-K at 64|vs single"]; !ok {
		t.Fatal("the other column of the row is fine")
	}
}
