package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// The writer keys this machine needs (SPEC.md, "Install", doctor). A key is
// named <writer>@<short host name>, so a host name change leaves every key
// of the machine under the old name, and each job that writes refuses: the
// timers' dream and curate end there with no line. `tacit doctor` fails on
// it instead:
//
//   - a role whose job this machine runs (dreamer: the dream timers;
//     curator: the curate timer) and that cannot write here;
//   - a harness with a key under another host name only (a key for
//     claude@old and none for claude@<this host>).
//
// "Cannot write" is writerCheck's answer, the one the jobs act on: a local
// store, a path remote and a read-only clone need no key.

// roleJobs maps a writer to the units that run its job: systemd timers
// enabled under timers.target.wants, launchd agents installed.
var roleJobs = []struct {
	writer  string
	timers  []string
	plists  []string
	install string
}{
	{"dreamer", []string{"tacit-dream.timer", "tacit-dream-idle.timer"}, []string{launchdDreamLabel, launchdIdleLabel}, "tacit install dream"},
	{"curator", []string{"tacit-curate.timer"}, []string{"ai.tacit.curate"}, "tacit install curate"},
}

// runsJob reports the unit of this machine that runs one of the job's
// units, or "".
func runsJob(home, goos string, timers, plists []string) string {
	if goos == "darwin" {
		for _, l := range plists {
			if _, err := os.Stat(filepath.Join(launchAgentsDir(home), l+".plist")); err == nil {
				return l
			}
		}
		return ""
	}
	if goos == "windows" {
		return ""
	}
	wants := filepath.Join(systemdUserDir(home), "timers.target.wants")
	for _, t := range timers {
		if _, err := os.Lstat(filepath.Join(wants, t)); err == nil {
			return t
		}
	}
	return ""
}

// keyHosts are the host names of keyDir's <writer>@<host>.key files.
func keyHosts(keyDir, writer string) []string {
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if h, ok := strings.CutPrefix(n, writer+"@"); ok && strings.HasSuffix(h, ".key") {
			out = append(out, strings.TrimSuffix(h, ".key"))
		}
	}
	sort.Strings(out)
	return out
}

// keyChecks are the doctor lines on writer keys. canWrite is writerCheck on
// the store for one writer.
func keyChecks(home, goos, keyDir, host string, canWrite func(writer string) error) []doctorCheck {
	var out []doctorCheck
	elsewhere := func(writer string) string {
		var others []string
		for _, h := range keyHosts(keyDir, writer) {
			if h != host {
				others = append(others, writer+"@"+h)
			}
		}
		if len(others) == 0 {
			return ""
		}
		return "; this machine holds " + strings.Join(others, ", ") + ": did its host name change? Generate and enroll the new key, or set the host name back"
	}
	for _, r := range roleJobs {
		unit := runsJob(home, goos, r.timers, r.plists)
		if unit == "" {
			continue
		}
		id := r.writer + "@" + host
		if err := canWrite(r.writer); err != nil {
			out = append(out, doctorCheck{r.writer + " key", "fail", unit + " runs here, but " + id + " cannot write: " + err.Error() + elsewhere(r.writer)})
			continue
		}
		out = append(out, doctorCheck{r.writer + " key", "ok", unit + " runs here and " + id + " can write"})
	}
	var harnesses []string
	for h := range fact.Harnesses {
		if agentHarness(h) && h != "dreamer" && h != "import" {
			harnesses = append(harnesses, h)
		}
	}
	sort.Strings(harnesses)
	for _, h := range harnesses {
		hosts := keyHosts(keyDir, h)
		if len(hosts) == 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(keyDir, h+"@"+host+".key")); err == nil {
			continue
		}
		if canWrite(h) == nil {
			continue // a store that needs no key
		}
		out = append(out, doctorCheck{h + " key", "fail", "no " + h + "@" + host + " key for this host name" + elsewhere(h)})
	}
	return out
}

// doctorKeyChecks is keyChecks for this machine and its store.
func doctorKeyChecks(canWrite func(writer string) error) []doctorCheck {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir, err := auth.Dir()
	if err != nil {
		return nil
	}
	return keyChecks(home, runtime.GOOS, dir, shortHost(mustHostname()), canWrite)
}
