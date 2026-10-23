package assess

// A folder's status between active and dormant is a question of dates, and
// dates are checkable: code gathers every date it can verify before the first
// model call (the owner's commits in a checkout here, other machines'
// catalogs, the forge's newest commits by the owner's account, the sessions
// that worked in the folder or filed its notes) and sets the status from how
// recent the last one is, measured against the repository's own rhythm. Every
// run reads the same dates, so every run gets the same status (the owner,
// 2026-10-23: "why would different runs see different dates? last commit is
// last commit"). The model keeps what dates cannot say: the kind, the
// ancestor, and dead or shipped when explicit evidence says so, which code
// checks.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/record"
)

// The rhythm's bounds, from the Mac shadow copies of 2026-10-23 (108 project
// folders with owner activity: forge commits by the owner's account, the
// rig's catalog first and last own commit, the days sessions filed notes):
//
//   - minRhythmGaps: a folder's own typical gap is the median of its gaps
//     from 3 on, a middle value and not one extreme; 63 of the 108 have 3 or
//     more.
//   - defaultGapDays: below that, the typical gap is 8 days, the median of
//     those 63 folders' own medians (quartiles 5.5, 8, 10.5).
//   - minWindowDays: a folder is never dormant after 7 idle days or fewer:
//     half of all the owner's 622 gaps between stretches of work are 7 days
//     or less, so a pause that short is the owner's ordinary break, not a
//     sign nobody is on it (a folder worked every other day has a window of
//     4 days without the floor).
//   - maxWindowDays: a folder idle more than 32 days is dormant whatever its
//     rhythm: 80% of the owner's gaps end within 32 days (deciles 2, 3, 4,
//     5, 7, 9, 16, 32, 58), and a folder touched every two months would
//     otherwise be active four months after its last commit.
const (
	minRhythmGaps  = 3
	defaultGapDays = 8
	minWindowDays  = 7
	maxWindowDays  = 32
	// rhythmFactor: active while idle at most twice the typical gap.
	rhythmFactor = 2
)

// ActivityDay is one day the owner worked on a folder, with what says so:
// Source is "commit" (a commit under the owner's git identity in a checkout
// here), "catalog" (another machine's catalog's first or last own commit
// or work day), "forge" (a commit by the owner's forge account) or "session"
// (a session worked in it, or filed a note there); Cite is what a why may
// cite for it (commit:<sha>, session:<id>), "" when there is none.
type ActivityDay struct {
	Day, Source, Cite string
}

// Activity is the checkable evidence of a folder's life, gathered by code
// before its first model call (the same for every run).
type Activity struct {
	// Days are the owner's activity days, newest first, one per day (the
	// best citable source of the day kept).
	Days []ActivityDay
	// Forge is the forge's record of its repository (nil: none asked or no
	// answer), User the owner's forge account.
	Forge *check.Activity
	User  string
	// Deleted: the forge says no repository has its name now while a remote
	// names it, and none renamed it.
	Deleted bool
	// What code measured: the stretches of work (consecutive days are one),
	// the gaps between them, the typical gap, the window and the idle days;
	// Status is code's, "" when the folder has no activity day.
	Gaps, TypicalGap, Window, Idle int
	Measured                       bool // the typical gap is the folder's own (minRhythmGaps)
	Status                         string
	Today                          string
}

// AddDay adds one activity day (YYYY-MM-DD); a day already there keeps a
// citable source over one with no citation.
func (a *Activity) AddDay(day, source, cite string) {
	if len(day) != 10 {
		return
	}
	for i := range a.Days {
		if a.Days[i].Day == day {
			if a.Days[i].Cite == "" && cite != "" || a.Days[i].Cite != "" && cite != "" && rank(source) < rank(a.Days[i].Source) {
				a.Days[i] = ActivityDay{Day: day, Source: source, Cite: cite}
			}
			return
		}
	}
	a.Days = append(a.Days, ActivityDay{Day: day, Source: source, Cite: cite})
}

// rank orders the sources of one day's citation: a commit here, the forge's,
// a session.
func rank(s string) int {
	switch s {
	case "commit":
		return 0
	case "forge":
		return 1
	case "session":
		return 2
	}
	return 3
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// Measure sorts the days and sets the rhythm and code's status as of today.
// An archived repository is never active (its owner archived it): dormant,
// or dead or shipped on the explicit evidence the model cites.
func (a *Activity) Measure(today string) {
	a.Today = today
	sort.SliceStable(a.Days, func(i, j int) bool { return a.Days[i].Day > a.Days[j].Day })
	now, ok := parseDay(today)
	a.Status, a.Gaps, a.TypicalGap, a.Window, a.Idle, a.Measured = "", 0, 0, 0, 0, false
	var days []time.Time
	for _, d := range a.Days {
		if t, ok := parseDay(d.Day); ok && !t.After(now) {
			days = append(days, t)
		}
	}
	if !ok || len(days) == 0 {
		return
	}
	var gaps []int
	for i := 0; i+1 < len(days); i++ {
		if g := int(days[i].Sub(days[i+1]).Hours() / 24); g > 1 {
			gaps = append(gaps, g)
		}
	}
	a.Gaps = len(gaps)
	a.TypicalGap = defaultGapDays
	if len(gaps) >= minRhythmGaps {
		a.TypicalGap, a.Measured = median(gaps), true
	}
	a.Window = min(max(rhythmFactor*a.TypicalGap, minWindowDays), maxWindowDays)
	a.Idle = int(now.Sub(days[0]).Hours() / 24)
	a.Status = record.StatusDormant
	if a.Idle <= a.Window && (a.Forge == nil || !a.Forge.Archived) {
		a.Status = record.StatusActive
	}
}

// median of gaps, the lower middle of an even count (whole days).
func median(xs []int) int {
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Last is the newest activity day ("" for none).
func (a *Activity) Last() ActivityDay {
	if a == nil || len(a.Days) == 0 {
		return ActivityDay{}
	}
	return a.Days[0]
}

// OwnCommits reports an activity day of the owner's commits: in a checkout
// here, another machine's catalog or on the forge by their account.
func (a *Activity) OwnCommits() bool {
	if a == nil {
		return false
	}
	for _, d := range a.Days {
		if d.Source == "commit" || d.Source == "forge" || d.Source == "catalog" {
			return true
		}
	}
	return false
}

// Cite is the newest activity's citation, else the newest one any day has.
func (a *Activity) Cite() string {
	if a == nil {
		return ""
	}
	for _, d := range a.Days {
		if d.Cite != "" {
			return d.Cite
		}
	}
	return ""
}

// StatusWhy is the sentence code's status rests on, its why's start.
func (a *Activity) StatusWhy() string {
	last := a.Last()
	src := last.Source
	if last.Cite != "" {
		src += " " + last.Cite
	}
	rhythm := fmt.Sprintf("its typical gap between stretches of work is %d days over %d gaps", a.TypicalGap, a.Gaps)
	if !a.Measured {
		rhythm = fmt.Sprintf("it has %d gaps between stretches of work, too few to measure (%d needed), so the typical gap is the owner's usual %d days", a.Gaps, minRhythmGaps, defaultGapDays)
	}
	s := fmt.Sprintf("code: %s from its dates: the owner's last activity is %s (%s), %d days before %s; %s, so it is active while idle at most %d days", a.Status, last.Day, src, a.Idle, a.Today, rhythm, a.Window)
	if a.Forge != nil && a.Forge.Archived {
		s += "; the forge has it archived, so it is not active"
	}
	if c := a.Cite(); c != "" && c != last.Cite {
		s += " (" + c + ")"
	}
	return s + "."
}

// Lines are what the model reads of the activity: the forge's record and
// code's status with its measure.
func (a *Activity) Lines() []string {
	var out []string
	if f := a.Forge; f != nil {
		head := fmt.Sprintf("forge (checked): github.com/%s: last push %s", f.FullName, orUnknown(f.PushedAt))
		if f.Archived {
			head += ", archived"
		}
		if f.Release != "" {
			head += fmt.Sprintf(", latest release %s (%s)", f.Release, f.ReleasedAt)
		} else {
			head += ", no release"
		}
		if a.User != "" {
			head += "; the owner's forge account is " + a.User
		}
		out = append(out, head)
		var cs []string
		for i, cm := range f.Commits {
			if i == forgeCommitsShown {
				break
			}
			who := orUnknown(cm.Login)
			if a.User != "" && strings.EqualFold(cm.Login, a.User) {
				who += " (the owner's: commit:" + short(cm.SHA) + ")"
			}
			cs = append(cs, cm.Day+" by "+who)
		}
		if len(cs) > 0 {
			out = append(out, "forge newest commits: "+strings.Join(cs, ", "))
		}
	}
	if a.Deleted {
		out = append(out, "forge (checked): no repository has this name now and none renamed it: deleted from the forge")
	}
	if a.Status != "" {
		var ds []string
		for i, d := range a.Days {
			if i == 8 {
				ds = append(ds, fmt.Sprintf("... %d more", len(a.Days)-8))
				break
			}
			s := d.Day + " " + d.Source
			if d.Cite != "" {
				s += " " + d.Cite
			}
			ds = append(ds, s)
		}
		out = append(out, fmt.Sprintf("activity (code's, from the dates above; newest first): %s", strings.Join(ds, ", ")))
		out = append(out, "status (code's): "+strings.TrimPrefix(a.StatusWhy(), "code: "))
	}
	return out
}

// forgeCommitsShown bounds the forge's commits a prompt lists.
const forgeCommitsShown = 8

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Key is what a fingerprint reads of the activity: the days, the forge's
// archived, deleted and release, and the status; not the push day (a bot's
// branch moves it) or the idle count (it grows every day the status holds).
func (a *Activity) Key() string {
	if a == nil {
		return ""
	}
	var b strings.Builder
	for _, d := range a.Days {
		b.WriteString(d.Day + " " + d.Source + " " + d.Cite + ";")
	}
	if a.Forge != nil {
		fmt.Fprintf(&b, " archived=%v release=%s", a.Forge.Archived, a.Forge.Release)
	}
	fmt.Fprintf(&b, " deleted=%v status=%s", a.Deleted, a.Status)
	return b.String()
}

// CodeStatus is code's status for c from its dates, "" when code has none
// (no activity day) or the folder is no project folder code dates (a
// proposed area or topic).
func (c Candidate) CodeStatus() string {
	if c.Kind != "" || c.Activity == nil {
		return ""
	}
	return c.Activity.Status
}

// ApplyStatus is code's status on a decision for a folder whose dates it
// has (the owner's rule: active vs dormant is set by code from dates; dead
// and shipped are never set from dates): an answer of active or dormant takes
// code's; dead stands only on explicit evidence (the forge has it archived or
// deleted with no live successor, or the why cites a note shown for this
// folder), shipped only on a
// release the forge lists or a note shown, together with the owner's words
// shown for it and cited; anything else takes code's status, saying why.
// needs-review stands only for a folder dated by sessions alone (what it is
// may be unsettled); a fold, kind none and the owner's quote (verified by the
// caller) stand. shown is the folder's rendered evidence. changed reports a status
// code set.
func ApplyStatus(c Candidate, d Decision, shown string) (Decision, bool) {
	cs := c.CodeStatus()
	if cs == "" || d.Kind == KindNone || strings.TrimSpace(d.SuppressedBy) != "" || strings.TrimSpace(d.OwnerQuote) != "" {
		return d, false
	}
	if c.Old != nil && len(c.Old.Was) > 0 && c.Old.Was[0].By == record.ByOwner {
		return d, false // the owner's status: never an agent's to replace
	}
	why := strings.TrimSpace(d.Why)
	var refused string
	switch d.Status {
	case cs:
		if strings.HasPrefix(why, "code: ") {
			return d, false
		}
	case record.StatusNeedsReview:
		// What a folder of the owner's own commits is, git says (real work,
		// theirs), and its dates say whether it is worked: nothing is left for
		// review but its ancestor, which a fold or the independence it cites
		// settles (Enforce). A folder dated by sessions alone may still be
		// something else (an upstream clone a session took notes on).
		if !c.Activity.OwnCommits() {
			return d, false
		}
		refused = "needs-review, but its own commits say what it is and its dates whether it is worked"
	case record.StatusActive, record.StatusDormant:
	case record.StatusDead:
		if c.forgeDead() {
			return d, false
		}
		if citesShownNote(why, shown) {
			if ch, day, ok := c.changedAfterNote(why); ok {
				refused = fmt.Sprintf("dead on a note of %s, but the owner worked on it after that day (%s)", day, ch.Summary(3))
				break
			}
			return d, false
		}
		refused = "dead, which code does not take without explicit evidence (the forge has it archived or deleted and no successor is in sight, a note shown for it cited, the owner's words)"
	case record.StatusShipped:
		if c.shippedEvidence(why, shown) {
			if ch, day, ok := c.changedAfterNote(why); ok {
				refused = fmt.Sprintf("shipped on a note of %s, but the owner worked on it after that day (%s)", day, ch.Summary(3))
				break
			}
			return d, false
		}
		refused = "shipped, which code takes only on a release, package or deploy shown for it together with the owner's statement or decision, cited"
	default:
		return d, false // not a status: Validate refuses it
	}
	d.Status = cs
	d.Why = c.Activity.StatusWhy()
	if refused != "" {
		d.Why += " The answer was " + refused + "."
	}
	if why != "" {
		d.Why += " What it is: " + why
	}
	return d, true
}

// forgeDead reports the forge's explicit evidence of a dead repository: it
// has it archived or deleted and code found no live successor for it (no
// ancestor candidate, no candidate real repository: one with a successor
// folds into it).
func (c Candidate) forgeDead() bool {
	a := c.Activity
	return a != nil && (a.Deleted || a.Forge != nil && a.Forge.Archived) && len(c.Ancestors) == 0 && len(c.Proposed()) == 0
}

// changedAfterNote reports the owner's real work on the folder after the day
// of the first of its notes why cites (words.go): a dead or shipped answer
// resting on a note is the note's word against the dates, which stands only
// while the owner has not worked on it since.
func (c Candidate) changedAfterNote(why string) (Changes, string, bool) {
	_, day, ok := NoteDay(c, why)
	if !ok || day == "" {
		return Changes{}, "", false
	}
	ch := c.Changes(day)
	return ch, day, ch.Changed()
}

// shippedEvidence reports a release (the forge's) or a cited note shown,
// together with the owner's words shown for the folder and cited by ref.
func (c Candidate) shippedEvidence(why, shown string) bool {
	release := citesShownNote(why, shown) || c.Activity != nil && c.Activity.Forge != nil && c.Activity.Forge.Release != ""
	if !release {
		return false
	}
	for _, cite := range record.Cites(why) {
		if strings.HasPrefix(cite, "note:") || strings.HasPrefix(cite, "commit:") {
			continue
		}
		for _, l := range strings.Split(shown, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "owner's ") && strings.Contains(l, cite) {
				return true
			}
		}
	}
	return false
}

// citesShownNote reports a note:<name> in why that the folder's rendered
// evidence lists (as [[name]] or note:name).
func citesShownNote(why, shown string) bool {
	for _, cite := range record.Cites(why) {
		if n, ok := strings.CutPrefix(cite, "note:"); ok && n != "" && (strings.Contains(shown, "[["+n+"]]") || record.WholeWord(shown, "note:"+n)) {
			return true
		}
	}
	return false
}
