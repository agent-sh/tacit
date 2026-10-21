package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/store"
)

func refreshModels() {
	curate.DefaultModel = modelrun.Model("curate", "opus")
	curate.JudgeModel = modelrun.Model("judge", "haiku")
	dream.DefaultModel = modelrun.Model("dream", "opus")
	verifyModel = modelrun.Model("verify", "opus")
	verifyFallbackModel = modelrun.Model("verify_fallback", "sonnet")
}

// maintain is the same daily sequence as the curator unit, on every OS:
// curate, the rotation (rotation.go), verify.
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
	rotated := 0
	if s, err := store.Default(); err == nil && s.Exists() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		if maintainRotation(ctx, s) > 0 {
			rotated = 1
		}
		cancel()
	}
	verified := runVerify(nil)
	if curated != 0 {
		return curated
	}
	if rotated != 0 {
		return rotated
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
