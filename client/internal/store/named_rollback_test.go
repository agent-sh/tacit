package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNamedPushRollbackPreservesUnrelatedStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell git hooks fixture")
	}
	for _, rejection := range []bool{false, true} {
		name := "remote-race"
		if rejection {
			name = "server-refusal"
		}
		t.Run(name, func(t *testing.T) {
			s, server := seedPair(t)
			run(t, s.Dir, "config", "user.email", "writer@example.test")
			run(t, s.Dir, "config", "user.name", "writer")
			// Keep a staged selection distinct from its working copy.
			os.WriteFile(filepath.Join(s.Dir, "README.md"), []byte("staged edit\n"), 0644)
			run(t, s.Dir, "add", "README.md")
			os.WriteFile(filepath.Join(s.Dir, "README.md"), []byte("unstaged edit\n"), 0644)
			before := run(t, s.Dir, "diff", "--cached", "--binary")
			head := run(t, s.Dir, "rev-parse", "HEAD")
			rel := "inbox/dreamer@fixture/note.md"
			os.MkdirAll(filepath.Join(s.Dir, filepath.Dir(rel)), 0755)
			os.WriteFile(filepath.Join(s.Dir, rel), []byte("fixture note\n"), 0644)
			if rejection {
				bare := strings.TrimSpace(run(t, server, "remote", "get-url", "origin"))
				os.WriteFile(filepath.Join(bare, "hooks/pre-receive"), []byte("#!/bin/sh\necho 'tacit: refused fixture' >&2\nexit 1\n"), 0755)
			} else {
				// Advance the bare remote after this writer commits, during its push.
				os.WriteFile(filepath.Join(server, "remote-race.txt"), []byte("remote update\n"), 0644)
				run(t, server, "add", "remote-race.txt")
				run(t, server, "commit", "-q", "-m", "remote race")
				quoted := "'" + strings.ReplaceAll(server, "'", "'\"'\"'") + "'"
				hook := "#!/bin/sh\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\ngit -C " + quoted + " push -q origin HEAD:main\n"
				os.WriteFile(filepath.Join(s.Dir, ".git/hooks/pre-push"), []byte(hook), 0755)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := s.WithLock(ctx, func() error { return s.CommitPushNamedLocked(ctx, []string{rel}, "dreamer note", "dreamer") })
			if rejection {
				if _, ok := err.(*Rejected); !ok {
					t.Fatalf("expected refusal, got %v", err)
				}
			} else if _, ok := err.(*Conflict); !ok {
				t.Fatalf("expected rebase conflict, got %v", err)
			}
			if after := run(t, s.Dir, "diff", "--cached", "--binary"); after != before {
				t.Fatalf("unrelated staging changed: before %q after %q", before, after)
			}
			if run(t, s.Dir, "rev-parse", "HEAD") != head {
				t.Fatal("failed named commit retained")
			}
			body, err := os.ReadFile(filepath.Join(s.Dir, "README.md"))
			if err != nil || string(body) != "unstaged edit\n" {
				t.Fatalf("working copy changed: %q %v", body, err)
			}
			if rejection {
				rel += ".rejected"
			}
			if _, err := os.Stat(filepath.Join(s.Dir, rel)); err != nil {
				t.Fatal("own note lost", err)
			}
			if !rejection {
				if run(t, server, "rev-parse", "HEAD") == head {
					t.Fatal("fixture never raced")
				}
				if out := run(t, s.Dir, "ls-files", "--", rel); out != "" {
					t.Fatal("failed note still staged", out)
				}
			}
		})
	}
}
