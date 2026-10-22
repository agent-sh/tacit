//go:build windows

package main

// execSelf is nil on Windows, which has no exec in place: a server whose
// binary changed says so in its tool results (outdatedNote) instead.
var execSelf func(path string, args, env []string) error
