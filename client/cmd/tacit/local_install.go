package main

import (
	"os"
	"path/filepath"
)

// The local layer's daily curate (SPEC.md, "Local layer"): `tacit install
// dream` on a read-only clone with the layer on adds it to the dream and idle
// jobs. It runs `tacit curate`, not `tacit maintain`: the verifier is the
// store's and stays on the curator's machine.
const (
	launchdLocalCurateLabel = "ai.tacit.curate-local"
	localCurateJob          = "curate-local"
	localCurateInterval     = 24 * 60 * 60 // seconds
)

// localCurateArgs is the job's command after the binary.
func localCurateArgs(envFile string) []string { return []string{"curate", "--env-file", envFile} }

// installLocalCurate writes the local layer's daily curate job: a launchd
// agent on macOS, a user timer on Linux, a Task Scheduler definition on
// Windows. It loads nothing.
func installLocalCurate(goos, home, exe, envFile string) ([]string, error) {
	switch goos {
	case "windows":
		p, err := installWindowsJob(home, exe, localCurateJob, envFile)
		return []string{p}, err
	case "darwin":
		p, err := installLaunchAgent(home, launchdLocalCurateLabel, localCuratePlist(exe, envFile, os.Getenv("TACIT_STORE"), home, servicePATH(home, exe, lookPath)))
		return []string{p}, err
	}
	dir := systemdUserDir(home)
	var out []string
	for _, f := range []struct{ name, data string }{
		{"tacit-curate-local.service", localCurateUnit(unitQuote(exe, false), unitQuote(envFile, false))},
		{"tacit-curate-local.timer", localCurateTimer},
	} {
		p := filepath.Join(dir, f.name)
		if err := writeFileMode(p, []byte(f.data), 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}

// localCuratePlist is ~/Library/LaunchAgents/ai.tacit.curate-local.plist.
func localCuratePlist(exe, envFile, storeEnv, home, path string) string {
	p := newPlist()
	p.str("Label", launchdLocalCurateLabel)
	p.array("ProgramArguments", append([]string{exe}, localCurateArgs(envFile)...)...)
	p.dict("EnvironmentVariables", jobEnv(path, storeEnv)...)
	p.integer("StartInterval", localCurateInterval)
	p.boolean("RunAtLoad", false)
	p.str("ProcessType", "Background")
	p.integer("Nice", 19)
	p.boolean("LowPriorityIO", true)
	log := filepath.Join(launchdLogDir(home), "curate-local.log")
	p.str("StandardOutPath", log)
	p.str("StandardErrorPath", log)
	return p.String()
}

// localCurateUnit is the Linux service of the local layer's daily curate.
func localCurateUnit(exec, envFile string) string {
	extra := ""
	for _, key := range jobEnvKeys() {
		if value := os.Getenv(key); value != "" {
			extra += "Environment=" + unitQuote(key+"="+value, true) + "\n"
		}
	}
	return `[Unit]
Description=tacit local curator: this machine's dreamer notes to its local facts

[Service]
EnvironmentFile=` + envFile + `
Type=oneshot
` + timeoutLine(curateJobLimit) + `Nice=19
IOSchedulingClass=idle
` + extra + `ExecStart=` + exec + ` curate
`
}

// localCurateTimer runs it daily, like the store curator's timer.
const localCurateTimer = `[Unit]
Description=tacit local curator daily

[Timer]
OnCalendar=*-*-* 05:30:00
Persistent=true

[Install]
WantedBy=timers.target
`
