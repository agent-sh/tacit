package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/agent-sh/tacit/client/internal/store"
)

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := storeFlag(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Println("init:", err)
		return 2
	}
	s := &store.Store{Dir: abs}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Init(ctx); err != nil {
		fmt.Println("init:", err)
		return 1
	}
	fmt.Println("Local store ready:", abs)
	fmt.Println("No server, tailnet or enrollment keys are needed. Install your harness hooks, then add notes or open tacit app --browser.")
	return 0
}
