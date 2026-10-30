//go:build unix

package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeScrump puts a scrump first on PATH that logs its arguments to the
// returned file and runs body.
func fakeScrump(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "scrump"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := recordsMode.Load()
	recordsMode.Store(0)
	t.Cleanup(func() { recordsMode.Store(prev) })
	return calls
}

// refuseRecords is a scrump before records mode: clap refuses the flag.
const refuseRecords = `for a in "$@"; do
  if [ "$a" = --records ]; then
    echo "error: unexpected argument '--records' found" >&2
    exit 2
  fi
done
exec cat
`

// A scrump without records mode is detected by its answer, not assumed: it
// refuses --records, the texts go joined by the boundary, and it is not
// asked for records again in this process.
func TestRedactAllFallsBackToTheBoundaryWithoutRecordsMode(t *testing.T) {
	calls := fakeScrump(t, refuseRecords)
	for range 2 {
		out, err := RedactAll([]string{"one", "two"})
		if err != nil || len(out) != 2 || out[0] != "one" || out[1] != "two" {
			t.Fatalf("RedactAll: %v %q", err, out)
		}
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{
		"--profile text --records scrub - -o -",
		"--profile text scrub - -o -",
		"--profile text scrub - -o -",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if _, err := RedactRecordsContext(t.Context(), []string{"x"}); err != ErrNoRecords {
		t.Fatalf("RedactRecordsContext: %v, want ErrNoRecords", err)
	}
}

// Any other failure in records mode fails closed: no text comes back, and
// the boundary is not tried with a scrump that broke. The cases that keep
// the output's length reach the record checks themselves.
func TestRedactAllFailsClosedInRecordsMode(t *testing.T) {
	for name, body := range map[string]string{
		"another usage error":           "echo 'error: invalid value for --profile' >&2\nexit 2\n",
		"a runtime error":               "echo 'error: reading stdin' >&2\nexit 1\n",
		"records merged":                "exec tr -d '\\036'\n",
		"a record cut short":            "exec sed 's/two/tw/'\n",
		"a separator masked, same size": "exec tr '\\036' '*'\n",
		"a separator moved, same size":  "exec perl -0777 -pe 's/one\\x1etwo/on\\x1eetwo/'\n",
	} {
		t.Run(name, func(t *testing.T) {
			calls := fakeScrump(t, body)
			if out, err := RedactAll([]string{"one", "two"}); err == nil {
				t.Fatalf("RedactAll returned %q and no error", out)
			}
			b, _ := os.ReadFile(calls)
			if got := strings.TrimSpace(string(b)); got != "--profile text --records scrub - -o -" {
				t.Fatalf("calls %q, want the one records call", got)
			}
		})
	}
}

// A scrump that takes --records but scans one stream would bring #84 back:
// the first records call carries the issue's repro ahead of the texts, the
// stream keeps its password, and the texts go joined by the boundary.
func TestRedactAllCatchesAScrumpThatTakesRecordsButScansOneStream(t *testing.T) {
	calls := fakeScrump(t, "exec cat\n")
	out, err := RedactAll([]string{"one", "two"})
	if err != nil || len(out) != 2 || out[0] != "one" || out[1] != "two" {
		t.Fatalf("RedactAll: %v %q", err, out)
	}
	b, _ := os.ReadFile(calls)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"--profile text --records scrub - -o -", "--profile text scrub - -o -"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if _, err := RedactRecordsContext(t.Context(), []string{"x"}); err != ErrNoRecords {
		t.Fatalf("RedactRecordsContext: %v, want ErrNoRecords", err)
	}
}
