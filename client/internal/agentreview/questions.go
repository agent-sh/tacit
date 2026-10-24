package agentreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The queue's bounds (SPEC.md, "Questions"): questions never pile up.
const (
	// MaxOpen is how many questions wait at once; past it the oldest expire
	// (their notes stay low-weight agent notes).
	MaxOpen = 12
	// PerSession is how many questions one session is asked.
	PerSession = 2
	// MaxAsks is how many sessions are asked one question before it
	// expires unanswered.
	MaxAsks = 3
	// Expiry is how long a question waits for an answer.
	Expiry = 30 * 24 * time.Hour
)

// Question states.
const (
	StateOpen     = "open"     // waiting for the owner
	StateAnswered = "answered" // their answer read from their turn, not applied yet
	StateApplied  = "applied"  // their answer is in the ledger
	StateSettled  = "settled"  // later evidence decided the note: not asked again
	StateExpired  = "expired"  // unanswered past its time, its asks or the cap
)

// Ask is one session the question was put in: the session, its transcript
// (or Hermes store and session), and where its content stood then (Offset:
// the transcript's size; for Hermes the last message id), so its answer is
// read from what came after.
type Ask struct {
	Harness    string `json:"harness"`
	Session    string `json:"session"`
	Transcript string `json:"transcript,omitempty"`
	Offset     int64  `json:"offset,omitempty"`
	At         string `json:"at"`
	// Passed: the owner's turn after the question was no answer to it.
	Passed bool `json:"passed,omitempty"`
}

// Item is one question for the owner about one agent's note the reviewer
// was unsure of.
type Item struct {
	ID      string  `json:"id"`
	Note    string  `json:"note"`
	Name    string  `json:"name"`
	Fact    string  `json:"fact,omitempty"` // a curated fact made from it, when there is one
	Desc    string  `json:"desc"`
	Harness string  `json:"harness,omitempty"` // where it was saved
	Session string  `json:"session,omitempty"`
	Day     string  `json:"day"`
	Repo    string  `json:"repo,omitempty"`
	Cwd     string  `json:"cwd,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Queued  string  `json:"queued"`
	State   string  `json:"state"`
	Closed  string  `json:"closed,omitempty"`
	Why     string  `json:"why,omitempty"` // why it was settled or expired
	Asks    []Ask   `json:"asks,omitempty"`
	Answer  *Answer `json:"answer,omitempty"`
}

// Tag is how the question is named in the text the agent asks: the code
// finds the agent's message that asked it by the tag.
func (it Item) Tag() string { return "tacit " + it.ID }

// Text is the question as the agent asks it, verbatim: what was saved, from
// which session and day, and the three answers.
func (it Item) Text() string {
	where := it.Harness + " session"
	if it.Session != "" {
		where += " " + shortID(it.Session)
	}
	if it.Repo != "" {
		where += " in " + it.Repo
	}
	return fmt.Sprintf("[%s] On %s an agent saved this in a %s: \"%s\". Right? yes / no / correct it: <your words>",
		it.Tag(), it.Day, where, strings.Join(strings.Fields(it.Desc), " "))
}

func shortID(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ItemID is a note's question id: q- and six hex of its path.
func ItemID(note string) string {
	h := sha256.Sum256([]byte(note))
	return "q-" + hex.EncodeToString(h[:3])
}

// Queue is the questions file of this machine's state directory
// (questions.json). The reviewer adds to it and applies answers; the prompt
// hook asks and reads answers; the rulebook mod shows the open ones.
type Queue struct {
	Items []Item `json:"items"`
}

// Path is the queue's file in the state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, "questions.json") }

// Load reads the queue; a missing file is an empty queue.
func Load(path string) (*Queue, error) {
	q := &Queue{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return q, err
	}
	if err := json.Unmarshal(b, q); err != nil {
		return &Queue{}, err
	}
	return q, nil
}

// Save writes the queue atomically (mode 0600).
func (q *Queue) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(q, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".questions-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Locked runs fn on the queue under the queue's lock (questions.lock in the
// same directory) and saves it when fn reports a change. wait bounds how
// long it waits for another holder: a hook passes a few hundred
// milliseconds and gives up (fail open), the reviewer step longer.
func Locked(path string, wait time.Duration, fn func(q *Queue) bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(strings.TrimSuffix(path, ".json")+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(wait)
	for {
		err = platform.TryLock(lf)
		if err == nil {
			break
		}
		if !errors.Is(err, platform.ErrBusy) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer platform.Unlock(lf)
	q, err := Load(path)
	if err != nil {
		return err
	}
	if fn(q) {
		return q.Save(path)
	}
	return nil
}

// Find is the item for a note.
func (q *Queue) Find(note string) *Item {
	for i := range q.Items {
		if q.Items[i].Note == note {
			return &q.Items[i]
		}
	}
	return nil
}

// ByID is the item with the id.
func (q *Queue) ByID(id string) *Item {
	for i := range q.Items {
		if q.Items[i].ID == id {
			return &q.Items[i]
		}
	}
	return nil
}

// Add queues a question for it unless the note has one already (whatever
// its state: a note is asked about once). It reports whether it added.
func (q *Queue) Add(it Item, now time.Time) bool {
	if q.Find(it.Note) != nil {
		return false
	}
	it.ID = ItemID(it.Note)
	it.State = StateOpen
	it.Queued = now.UTC().Format(time.RFC3339)
	q.Items = append(q.Items, it)
	return true
}

// Close ends an open item with a state and why.
func (it *Item) Close(state, why string, now time.Time) {
	it.State, it.Why, it.Closed = state, why, now.UTC().Format(time.RFC3339)
}

// Tidy expires what waited too long or was asked enough, and keeps at most
// MaxOpen open (the newest). It reports a change.
func (q *Queue) Tidy(now time.Time) bool {
	changed := false
	var open []*Item
	for i := range q.Items {
		it := &q.Items[i]
		if it.State != StateOpen {
			continue
		}
		at, err := time.Parse(time.RFC3339, it.Queued)
		switch {
		case err == nil && now.Sub(at) > Expiry:
			it.Close(StateExpired, "unanswered for 30 days", now)
			changed = true
		case len(it.Asks) >= MaxAsks && allPassed(it.Asks):
			it.Close(StateExpired, fmt.Sprintf("asked in %d sessions, no answer", len(it.Asks)), now)
			changed = true
		default:
			open = append(open, it)
		}
	}
	if len(open) > MaxOpen {
		sort.SliceStable(open, func(i, j int) bool { return open[i].Queued > open[j].Queued })
		for _, it := range open[MaxOpen:] {
			it.Close(StateExpired, fmt.Sprintf("more than %d questions waiting: the oldest go", MaxOpen), now)
			changed = true
		}
	}
	return changed
}

// allPassed: every ask has been read through (the owner's turn after it was
// no answer). An ask whose session has not reached the owner's next turn
// yet still counts as waiting.
func allPassed(asks []Ask) bool {
	for _, a := range asks {
		if !a.Passed {
			return false
		}
	}
	return true
}

// Situation is the session a question may be put in: its harness and id,
// its transcript and where it stands, the names its repository goes by,
// its working directory and the prompt.
type Situation struct {
	Harness, Session, Transcript string
	Offset                       int64
	Repos                        []string
	Cwd, Prompt                  string
}

// Relevant reports a question that belongs in the session, by code: the
// note's repository is the session's (any of its names), or, for a note of
// no repository, the session runs in the folder the note's session ran in,
// or the prompt shares the note's words (three content words, or two with
// one a specific identifier).
func (it Item) Relevant(s Situation) bool {
	if it.Repo != "" {
		for _, r := range s.Repos {
			if r != "" && strings.EqualFold(r, it.Repo) {
				return true
			}
		}
		return false
	}
	if it.Cwd != "" && s.Cwd != "" && filepath.Clean(it.Cwd) == filepath.Clean(s.Cwd) {
		return true
	}
	return sharesWords(it.Desc, s.Prompt)
}

func sharesWords(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	have := map[string]bool{}
	for _, t := range search.Terms(a) {
		if len(t) >= 4 {
			have[search.Stem(t)] = true
		}
	}
	n, specific := 0, false
	seen := map[string]bool{}
	for _, t := range search.Terms(b) {
		st := search.Stem(t)
		if len(t) < 4 || seen[st] || !have[st] {
			continue
		}
		seen[st] = true
		n++
		specific = specific || search.SpecificIdentifier(t)
	}
	return n >= 3 || (n >= 2 && specific)
}

// asksIn is how many questions this session has been asked.
func (q *Queue) asksIn(harness, session string) int {
	n := 0
	for _, it := range q.Items {
		for _, a := range it.Asks {
			if a.Harness == harness && a.Session == session {
				n++
			}
		}
	}
	return n
}

// asked reports an item asked in the session already.
func (it Item) asked(harness, session string) bool {
	for _, a := range it.Asks {
		if a.Harness == harness && a.Session == session {
			return true
		}
	}
	return false
}

// Pick chooses the question to put in the session, records the ask and
// returns it: an open one not asked here yet, relevant to the session, the
// newest first; none when the session was asked PerSession already, or a
// question it was asked still waits for the owner's turn (one at a time).
func (q *Queue) Pick(s Situation, now time.Time) *Item {
	if s.Session == "" || q.asksIn(s.Harness, s.Session) >= PerSession {
		return nil
	}
	for _, it := range q.Items {
		if it.State != StateOpen {
			continue
		}
		for _, a := range it.Asks {
			if a.Harness == s.Harness && a.Session == s.Session && !a.Passed {
				return nil // its answer is still to come
			}
		}
	}
	var best *Item
	for i := range q.Items {
		it := &q.Items[i]
		if it.State != StateOpen || it.asked(s.Harness, s.Session) || len(it.Asks) >= MaxAsks || !it.Relevant(s) {
			continue
		}
		if best == nil || it.Queued > best.Queued {
			best = it
		}
	}
	if best != nil {
		best.Asks = append(best.Asks, Ask{Harness: s.Harness, Session: s.Session, Transcript: s.Transcript, Offset: s.Offset, At: now.UTC().Format(time.RFC3339)})
	}
	return best
}

// Waiting is the asks of open questions whose answer has not been read:
// (item index, ask index) pairs, for the reader of answers.
func (q *Queue) Waiting() [][2]int {
	var out [][2]int
	for i, it := range q.Items {
		if it.State != StateOpen {
			continue
		}
		for j, a := range it.Asks {
			if !a.Passed {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

// Open is the questions waiting for the owner.
func (q *Queue) Open() []Item {
	var out []Item
	for _, it := range q.Items {
		if it.State == StateOpen {
			out = append(out, it)
		}
	}
	return out
}
