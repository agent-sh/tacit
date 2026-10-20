//go:build unix

// Package platform provides process and file-lock operations used by the client.
package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func TryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	return err
}

func Unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// StartDetached starts cmd so it outlives the process that starts it: its
// own session, so the terminal or harness that ends the parent's process
// group does not end it too.
func StartDetached(cmd *exec.Cmd) (*os.Process, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
