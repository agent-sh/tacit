package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
)

func refreshModels() {
	curate.DefaultModel = envOr("TACIT_CURATE_MODEL", "opus")
	curate.JudgeModel = envOr("TACIT_JUDGE_MODEL", "haiku")
	dream.DefaultModel = envOr("TACIT_DREAM_MODEL", "haiku")
	verifyModel = envOr("TACIT_VERIFY_MODEL", "opus")
	verifyFallbackModel = envOr("TACIT_VERIFY_FALLBACK_MODEL", "sonnet")
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
