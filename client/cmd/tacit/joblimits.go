package main

import (
	"strconv"
	"time"
)

// Each scheduled job's unit stops a run that outlives the run's own limits:
// systemd's TimeoutStartSec= and the Windows task's ExecutionTimeLimit
// (launchd has no such key). Every git call of a run has a deadline of its
// own, so a dead or stalled server fails the run in minutes; this bound is
// for a run that hangs outside all of them, which would otherwise hold its
// job for good (a plain git fetch with no limit once ran 13 hours). Each
// value is above the sum of the run's own limits at their defaults, so the
// unit never stops a run the binary allows.
const (
	// tacit dream: up to 45 minutes waiting for an idle dream (dreamLockWait),
	// 20 to push notes an earlier run left (TACIT_DREAM_TIMEOUT), 45 of
	// batches (--max-runtime): 110 minutes, plus the transcript scan.
	dreamJobLimit = 150 * time.Minute
	// The idle job: the idle dream (20 and 45 minutes; it never waits for the
	// lock) and the quick curate (its 20-minute budget and 10-minute tail):
	// 95 minutes, plus the transcript scan.
	dreamIdleJobLimit = 150 * time.Minute
	// tacit curate: up to an hour waiting for a quick curate
	// (curateLockWait), the 150-minute --budget and the 75-minute tail of the
	// steps after the batches: 4 hours 45 minutes.
	curateJobLimit = 5 * time.Hour
)

// tacit maintain has no bound: verify has no overall budget. A batch also
// splits at verifyBatchBytes, so 60 findings can take 60 calls of up to 20
// minutes and a fallback call 20 more, and no bound under the daily timer
// is above that.

// timeoutLine is a unit's TimeoutStartSec= line for limit.
func timeoutLine(limit time.Duration) string {
	return "TimeoutStartSec=" + span(limit, "h", "min") + "\n"
}

// windowsLimit is limit as a task's ExecutionTimeLimit (an ISO 8601
// duration); 0 is no limit, the follower's.
func windowsLimit(limit time.Duration) string {
	if limit <= 0 {
		return "PT0S"
	}
	return "PT" + span(limit, "H", "M")
}

// span writes limit as hours and minutes with the given unit names.
func span(limit time.Duration, hours, minutes string) string {
	h, m := int(limit/time.Hour), int(limit%time.Hour/time.Minute)
	s := ""
	if h > 0 {
		s += strconv.Itoa(h) + hours
	}
	if m > 0 || h == 0 {
		s += strconv.Itoa(m) + minutes
	}
	return s
}
