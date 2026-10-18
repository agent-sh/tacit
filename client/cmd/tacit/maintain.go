package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func refreshModels() {
	curate.DefaultModel = modelrun.Model("curate", "sonnet")
	curate.JudgeModel = modelrun.Model("judge", "haiku")
	dream.DefaultModel = modelrun.Model("dream", "opus")
	verifyModel = modelrun.Model("verify", "opus")
	verifyFallbackModel = modelrun.Model("verify_fallback", "sonnet")
}

// maintain is the same daily sequence as the curator unit, on every OS.
func runMaintain(args []string) int {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	env := fs.String("env-file", "", "optional provider KEY=VALUE file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	if *env != "" {
		if err := loadEnvFile(*env); err != nil {
			fmt.Fprintln(os.Stderr, "maintain:", err)
			return 2
		}
	}
	refreshModels()
	curated := runCurate(nil)
	verified := runVerify(nil)
	if curated != 0 {
		return curated
	}
	return verified
}

// A command-line selection remains authoritative after a job env file loads.
func setExplicitRunner() {
	if explicitRunner == "" {
		return
	}
	os.Setenv("TACIT_RUNNER", explicitRunner)
	for _, role := range []string{"DREAM", "CURATE", "JUDGE", "VERIFY"} {
		os.Setenv("TACIT_"+role+"_RUNNER", explicitRunner)
	}
}
