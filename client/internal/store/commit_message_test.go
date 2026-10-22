package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CodeQL flow is review Action -> commit message -> git argv. Shell
// punctuation is already literal with os/exec; keep it that way, and keep the
// entire message out of command arguments by feeding git commit on stdin.
func TestCommitMessageIsDataNotAnArgument(t *testing.T) {
	for _, named := range []bool{false, true} {
		name := "staged"
		if named {
			name = "named"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := seedPair(t)
			run(t, s.Dir, "config", "user.email", "test@example.test")
			trace := filepath.Join(t.TempDir(), "trace.jsonl")
			t.Setenv("GIT_TRACE2_EVENT", trace)
			marker := filepath.Join(s.Dir, "injected")
			message := "--help; touch '" + marker + "'\n$(touch '" + marker + "')\n`touch injected`\n\"quoted\" & literal"
			rel := "note.md"
			if err := os.WriteFile(filepath.Join(s.Dir, rel), []byte("note\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := s.WithLock(context.Background(), func() error {
				if named {
					return s.CommitPushNamedOnceLocked(context.Background(), []string{rel}, message, "curator")
				}
				return s.CommitPushOnceLocked(context.Background(), []string{rel}, message, "curator")
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(run(t, s.Dir, "log", "-1", "--format=%B")); got != message {
				t.Fatalf("message changed: got %q, want %q", got, message)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("shell payload ran or marker check failed: %v", err)
			}
			b, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				var ev struct {
					Event string   `json:"event"`
					Argv  []string `json:"argv"`
				}
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Event != "start" || len(ev.Argv) < 2 || ev.Argv[1] != "commit" {
					continue
				}
				found = true
				hasStdin := false
				for _, arg := range ev.Argv {
					if arg == message || arg == "-m" {
						t.Error("commit message was exposed in git argv")
					}
					hasStdin = hasStdin || arg == "--file=-"
				}
				if !hasStdin {
					t.Error("git commit did not read its message from stdin")
				}
			}
			if !found {
				t.Fatal("trace did not contain git commit")
			}
		})
	}
}
