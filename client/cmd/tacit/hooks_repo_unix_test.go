//go:build unix

package main

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadTailDoesNotBlockOnAFIFO(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	fifo := filepath.Join(home, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no mkfifo here")
	}
	if got := readTail(fifo, home); got != "" { // must return, not block
		t.Errorf("a FIFO was read")
	}
}
