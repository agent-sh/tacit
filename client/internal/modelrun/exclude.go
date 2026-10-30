package modelrun

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/platform"
)

// A model call tacit makes is a session of its runner's harness, and the
// dreamer reads that harness's sessions: a prompt that holds the owner's
// evidence (a dream window, a batch of facts, a review) would come back as
// the owner's work. So a caller names its session before the call
// (NewSessionID) and lists it, and the session id the runner reports, in the
// dreamer's exclude list (Exclude), which the dreamer reads (Excluded) and
// never dreams. Without it the dreamer once mined an evaluation battery's
// synthetic prompts as the owner's work.

// ExcludeFile is the dreamer's exclude list under home: one session id per
// line, in the state directory (platform.StateDir).
func ExcludeFile(home string) string {
	return filepath.Join(platform.StateDir(home), "dream-exclude")
}

// NewSessionID returns a fresh random UUID (version 4) for a model call's
// session (`claude -p --session-id`).
func NewSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Exclude appends session ids to the dreamer's exclude list (ExcludeFile).
// An error means an id may not be on the list: a write or the close failed.
func Exclude(ids ...string) error {
	home, _ := os.UserHomeDir()
	p := ExcludeFile(home)
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := fmt.Fprintln(f, id); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// Excluded is the dreamer's exclude list as a set; empty when there is none.
func Excluded() map[string]bool {
	out := map[string]bool{}
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(ExcludeFile(home))
	if err != nil {
		return out
	}
	for _, l := range strings.Fields(string(b)) {
		out[l] = true
	}
	return out
}
