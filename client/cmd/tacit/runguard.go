package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

// The schedule a model job (curate, dream) backs off on after consecutive
// failed runs: the first wait is the quick curate's --min-interval, so a run
// that fails once is tried when it would have been anyway, and each further
// failure doubles it, up to backoffCap. The cap is the full dream's cadence
// (and dream.FailureBackoff): a route down for hours is tried about as often
// as the jobs batch their work, so once it is back the notes wait at most one
// period more. Without it, a judge route down for a day had the quick curate
// pay for the split again every 30 minutes and write nothing. On this
// schedule the first failure is the only paid run; the rest are probes, a
// few a day.
const (
	backoffBase = 30 * time.Minute
	backoffCap  = 4 * time.Hour
)

// maxRouteFailures is how many batches (or windows) of one run may fail on a
// route before the run stops paying for more, even when each probe passed.
const maxRouteFailures = 3

// backoffDelay is the wait after n consecutive failed runs.
func backoffDelay(n int) time.Duration {
	d := backoffBase
	for i := 1; i < n && d < backoffCap; i++ {
		d *= 2
	}
	return min(d, backoffCap)
}

// jobBackoff is a model job's state between runs (curate-backoff.json,
// curate-local-backoff.json, dream-backoff.json in the state directory):
// how many runs in a row failed, why, and when the next one may pay for
// model calls again.
type jobBackoff struct {
	Failures int       `json:"failures"`
	First    time.Time `json:"first_failure,omitzero"`
	Last     time.Time `json:"last_failure,omitzero"`
	Next     time.Time `json:"next_try,omitzero"`
	// Reason is the last failure: the pass and what failed ("judge:
	// provider", "curate: invalid_result", "conflict index").
	Reason string `json:"reason,omitempty"`
	// Role and Model are the route the probe calls before the next run pays
	// for its work; "" when the failure was not a model call's.
	Role  string `json:"role,omitempty"`
	Model string `json:"model,omitempty"`
	// Skipped counts the runs that came before Next and paid for nothing.
	Skipped int `json:"skipped_runs,omitempty"`
	// Probes and ProbeCostUSD count the probes since the first failure.
	Probes       int       `json:"probes,omitempty"`
	ProbeCostUSD float64   `json:"probe_cost_usd,omitempty"`
	LastProbe    time.Time `json:"last_probe,omitzero"`
	LastProbeOK  bool      `json:"last_probe_ok,omitempty"`
}

// backoffNow is the guard's clock; tests set it.
var backoffNow = time.Now

// probePrompt is the probe's whole prompt: a route that answers it can take
// a batch. The dreamer's route answers in its JSON schema, an empty facts
// list.
const probePrompt = "This is a connectivity check of a scheduled job, not a task. Reply with the single word OK, or with an empty list if a JSON answer is required."

// probeRoute sends probePrompt on role's route through the same call the
// job makes (the dreamer's dreamRun, the judge's judgeRun, the split's
// curate.RunCLI: the route and its fallback, each call's session excluded
// from dreaming), and returns what it cost. Tests substitute it, or the
// job's call it goes through.
var probeRoute = func(ctx context.Context, role, model string) (float64, error) {
	timeout, err := modelrun.Timeout(role)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, min(timeout, 5*time.Minute))
	defer cancel()
	var answer string
	var cost float64
	switch role {
	case "dream":
		answer, cost, _, err = dreamRun(ctx, model, probePrompt)
	case "judge":
		answer, cost, err = judgeRun(ctx, model, probePrompt)
	default:
		answer, cost, err = curate.RunCLI(ctx, model, probePrompt)
	}
	if err == nil && strings.TrimSpace(answer) == "" {
		err = fmt.Errorf("%w: empty answer to the probe", modelrun.ErrInvalidResult)
	}
	return cost, err
}

// runGuard is one run's view of its job's backoff: whether the run may pay
// for model calls at all (admit), whether it may go on after a route failed
// (next), and what the run's outcome does to the state (finish).
type runGuard struct {
	job  string // "curate" or "dream", for the messages
	path string
	st   jobBackoff
	// The route failure of this run not yet probed, and the run's count.
	failRole, failModel, failKind string
	routeFails                    int
	tripped                       bool    // a probe failed, or too many route failures: the run stopped paying
	refused                       bool    // admit refused the run (its state is saved already)
	cost                          float64 // the probes' cost this run
	probes                        int
}

func loadGuard(job, path string) *runGuard {
	g := &runGuard{job: job, path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &g.st)
	}
	return g
}

func (g *runGuard) save() {
	if g.st.Failures == 0 {
		os.Remove(g.path)
		return
	}
	os.MkdirAll(filepath.Dir(g.path), 0o700)
	b, _ := json.MarshalIndent(g.st, "", " ")
	os.WriteFile(g.path, append(b, '\n'), 0o600)
}

// probe calls the route and records the probe in the state.
func (g *runGuard) probe(ctx context.Context, role, model string) error {
	cost, err := probeRoute(ctx, role, model)
	g.cost += cost
	g.probes++
	g.st.Probes++
	g.st.ProbeCostUSD += cost
	g.st.LastProbe = backoffNow()
	g.st.LastProbeOK = err == nil
	return err
}

// admit reports whether the run may pay for its model calls. With no failed
// run before it, it may. Otherwise a run before the backoff's next try pays
// for nothing (a scheduled run, the daily curate or the full dream, does not
// wait for it), and a run at or after it first probes the failing route: a
// failed probe is a failed run, and the wait doubles.
func (g *runGuard) admit(ctx context.Context, scheduled bool) bool {
	if g.st.Failures == 0 {
		return true
	}
	now := backoffNow()
	if !scheduled && now.Before(g.st.Next) {
		g.st.Skipped++
		g.save()
		g.refused = true
		fmt.Printf("%s: backing off after %d failed run(s) (last: %s); next try after %s\n", g.job, g.st.Failures, g.st.Reason, g.st.Next.Local().Format(time.RFC3339))
		return false
	}
	if g.st.Role == "" {
		return true
	}
	if err := g.probe(ctx, g.st.Role, g.st.Model); err != nil {
		g.record(g.st.Role+": probe: "+modelrun.FailureKind(err), g.st.Role, g.st.Model)
		fmt.Fprintf(os.Stderr, "%s: the %s route still fails (%s); nothing paid for, next try after %s\n", g.job, g.st.Role, modelrun.FailureKind(err), g.st.Next.Local().Format(time.RFC3339))
		g.refused = true
		return false
	}
	fmt.Printf("%s: the %s route answered its probe after %d failed run(s); running\n", g.job, g.st.Role, g.st.Failures)
	return true
}

// routeFailure reports whether err says the route failed, not the answer:
// an invalid answer is the batch's (bisection, the one retry), and a run
// that ran out of its own time stopped, it did not fail.
func routeFailure(ctx context.Context, err error) bool {
	return err != nil && !splittable(err) && ctx.Err() == nil
}

// splittable is a failure that belongs to the notes sent, not the route: a
// malformed or refused answer, or the model's safety filter refusing the
// prompt (the same notes are refused every time, so the run splits the batch
// down to the note it refuses, which is then set aside like a note that fails
// alone, while the route keeps working for the other notes).
func splittable(err error) bool {
	return errors.Is(err, modelrun.ErrInvalidResult) || modelrun.FailureKind(err) == "refusal"
}

// failed records that a model call of this run failed on its route.
func (g *runGuard) failed(role, model string, err error) {
	g.failRole, g.failModel, g.failKind = role, model, modelrun.FailureKind(err)
	g.routeFails++
}

// next reports whether the run may pay for its next batch or window. After
// a route failure it may only once a probe of that route passes; after
// maxRouteFailures, not at all.
func (g *runGuard) next(ctx context.Context) bool {
	if g.tripped {
		return false
	}
	if g.failRole == "" {
		return true
	}
	if g.routeFails >= maxRouteFailures {
		g.tripped = true
		fmt.Fprintf(os.Stderr, "%s: %d model calls failed on their route this run; the rest wait for the next run\n", g.job, g.routeFails)
		return false
	}
	if err := g.probe(ctx, g.failRole, g.failModel); err != nil {
		g.tripped = true
		fmt.Fprintf(os.Stderr, "%s: the %s route failed (%s) and its probe failed (%s); the rest wait for the next run\n", g.job, g.failRole, g.failKind, modelrun.FailureKind(err))
		return false
	}
	g.failRole = ""
	return true
}

// record counts a failed run and schedules the next try.
func (g *runGuard) record(reason, role, model string) {
	now := backoffNow()
	if g.st.Failures == 0 {
		g.st.First = now
	}
	g.st.Failures++
	g.st.Last = now
	g.st.Next = now.Add(backoffDelay(g.st.Failures))
	g.st.Reason, g.st.Role, g.st.Model = reason, role, model
	g.save()
}

// runOutcome is what a run did with the work it paid for.
type runOutcome struct {
	Attempted int // batches or windows that reached a model call
	Done      int // of those, the ones that completed (written, or nothing to write)
	// Reason, Role and Model are the run's last failure that was not one
	// note's own (a lone note's invalid answer is the note's, its strikes
	// count it): "" when there was none.
	Reason, Role, Model string
}

// finish records the run's outcome: a run that paid and completed nothing
// for a failure that is not one note's, or one that stopped paying because
// its route failed, is a failed run; a run that completed a batch ends any
// backoff. A run with nothing to do changes nothing: nothing pending is not
// a success of the route.
func (g *runGuard) finish(o runOutcome) {
	if g.refused {
		return
	}
	switch {
	case g.tripped || o.Attempted > 0 && o.Done == 0 && (o.Reason != "" || g.routeFails > 0):
		reason, role, model := o.Reason, o.Role, o.Model
		if g.failRole != "" {
			reason, role, model = g.failRole+": "+g.failKind, g.failRole, g.failModel
		}
		if reason == "" {
			reason = g.st.Reason
			role, model = g.st.Role, g.st.Model
		}
		g.record(reason, role, model)
		fmt.Fprintf(os.Stderr, "%s: %d failed run(s) in a row (%s); the next run that pays waits until %s and probes the route first\n", g.job, g.st.Failures, g.st.Reason, g.st.Next.Local().Format(time.RFC3339))
	case o.Done > 0 && g.st.Failures > 0:
		fmt.Printf("%s: back to normal after %d failed run(s) (%d skipped, %d probe(s), $%.3f)\n", g.job, g.st.Failures, g.st.Skipped, g.st.Probes, g.st.ProbeCostUSD)
		g.st = jobBackoff{}
		g.save()
	}
}

// backoffLine is tacit doctor's line for a job's backoff state at path.
func backoffLine(path string) (status, detail string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "ok", "no failed runs"
	}
	var st jobBackoff
	if err != nil || json.Unmarshal(b, &st) != nil {
		return "warn", "the state file cannot be read"
	}
	if st.Failures == 0 {
		return "ok", "no failed runs"
	}
	d := fmt.Sprintf("%d failed run(s) in a row since %s, last %s (%s); next try after %s", st.Failures, st.First.Local().Format(time.RFC3339), st.Last.Local().Format(time.RFC3339), st.Reason, st.Next.Local().Format(time.RFC3339))
	if st.Skipped > 0 || st.Probes > 0 {
		d += fmt.Sprintf("; %d run(s) skipped, %d probe(s) ($%.3f)", st.Skipped, st.Probes, st.ProbeCostUSD)
	}
	return "warn", d
}
