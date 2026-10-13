//go:build !unix

package search

import "github.com/agent-sh/tacit/client/internal/platform"

func processAlive(pid int) bool { return platform.Alive(pid) }
