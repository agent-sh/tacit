package follow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FreshFor is how old the follower's heartbeat may be before a hook stops
// trusting it. The server sends a keepalive every 20 s and the follower
// stamps the heartbeat on each one, so a healthy follower is never more than
// about 20 s old. 45 s lets one keepalive slip (20 + 20 s plus 5 s for a
// loaded machine to schedule the write) and stays under the follower's own
// 60 s silence timeout, so a half-open connection the follower has not yet
// noticed can never look fresh for longer than it takes to notice.
//
// The worst case is therefore a push from another machine that this clone
// cannot see, and a hook that does not sync, for up to FreshFor after the
// last keepalive. A live stream shortens that to the push's own notification.
const FreshFor = 45 * time.Second

// VerifyEvery is how often a connected follower checks the clone against the
// server's main directly (one ls-remote), because the stream only says what
// the journal says: a ref can move without a journal row (the server dies
// between receive-pack and the journal append) or the journal can be
// restored. 5 minutes costs 288 small requests a day and bounds how long such
// a miss can last once the follower notices it.
const VerifyEvery = 5 * time.Minute

// VerifiedFor is how old the last verification may be before a hook stops
// trusting the follower: two check intervals, as FreshFor is two keepalives.
// If the follower cannot verify (the check keeps failing) the hooks sync for
// themselves again after this long.
const VerifiedFor = 2 * VerifyEvery

// State is the follower's file under ~/.local/state/tacit. It is both the
// resume point (Seq) and the heartbeat hooks read.
type State struct {
	PID   int    `json:"pid"`   // the follower process; 0 after a clean stop
	Store string `json:"store"` // the clone this follower keeps current
	// Seq is the newest journal seq whose push is fetched, merged and
	// indexed. A restart resumes the feed from here.
	Seq      int64     `json:"seq"`
	Head     string    `json:"head"` // clone HEAD after the last sync
	SyncedAt time.Time `json:"synced_at"`
	// VerifiedAt is when the clone was last proven to hold the server's main:
	// the start of the last successful sync or ls-remote check.
	VerifiedAt time.Time `json:"verified_at"`
	// AliveAt is the last moment the stream showed the clone current: stamped
	// on connect, on every keepalive and after every sync, and only while
	// Connected and not Behind.
	AliveAt   time.Time `json:"alive_at"`
	Connected bool      `json:"connected"`
	// Behind is set when the feed announced a push the clone does not have
	// yet (or a check found the server ahead), and from start until the first
	// sync.
	Behind bool `json:"behind"`
}

// StatePath is where the follower for the clone at store keeps its state:
// $TACIT_FOLLOW_STATE when set; else ~/.local/state/tacit/follow.json for the
// default clone (~/.local/share/tacit/store); else follow-<hash of the path>.json,
// so two stores never share a file.
func StatePath(store string) string {
	if p := os.Getenv("TACIT_FOLLOW_STATE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	dir := platform.StateDir(home)
	store = filepath.Clean(store)
	if store == filepath.Join(home, ".local", "share", "tacit", "store") {
		return filepath.Join(dir, "follow.json")
	}
	sum := sha256.Sum256([]byte(store))
	return filepath.Join(dir, "follow-"+hex.EncodeToString(sum[:6])+".json")
}

// Load reads the state file; a missing file is the zero State.
func Load(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// Save writes the state atomically, so a hook never reads half a file.
func Save(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Current reports whether a healthy follower has confirmed, within FreshFor,
// that the clone at store matches the server, and verified that against the
// server itself within VerifiedFor. Any doubt answers false: no file, a
// different store, a dead process, a dropped stream, a pending event, a
// stale or future stamp. The caller then syncs itself, as it did before the
// follower existed.
func Current(path, store string, now time.Time) bool {
	st, err := Load(path)
	if err != nil || st.PID <= 0 || st.Store != store || !st.Connected || st.Behind {
		return false
	}
	if age := now.Sub(st.AliveAt); age < 0 || age > FreshFor {
		return false
	}
	if age := now.Sub(st.VerifiedAt); age < 0 || age > VerifiedFor {
		return false
	}
	// kill(pid, 0) sends nothing; EPERM still means the process exists.
	return platform.Alive(st.PID)
}

// ErrRunning means another follower holds the lock for this state file.
type ErrRunning struct{ PID int }

func (e *ErrRunning) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another tacit follow (pid %d) is already running for this store", e.PID)
	}
	return "another tacit follow is already running for this store"
}

// Lock takes the single-instance lock for a state file: an flock on
// <state>.lock, held until the returned release is called or the process
// exits. Two followers would overwrite each other's heartbeat, and a manual
// one stopping would clear the service's.
func Lock(statePath string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.TryLock(f); err != nil {
		b := make([]byte, 32)
		n, _ := f.Read(b)
		f.Close()
		if errors.Is(err, platform.ErrBusy) {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
			return nil, &ErrRunning{PID: pid}
		}
		return nil, err
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { f.Close() }, nil // closing drops the flock
}
