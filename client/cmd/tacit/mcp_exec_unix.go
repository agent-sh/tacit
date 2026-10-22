//go:build !windows

package main

import "syscall"

// execSelf replaces this process with the binary at path (same pid, same
// open stdin, stdout and stderr): the MCP connection stays the client's.
var execSelf = func(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
}
