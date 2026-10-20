//go:build windows

package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

func TryLock(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{Offset: 64})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return err
}

func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{Offset: 64})
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true
	}
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

// taskkill /T terminates git's helpers as well as git. The cancellation
// command has its own deadline and falls back to killing the root process.
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	cmd.Cancel = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		kill := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		kill.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
		kill.WaitDelay = time.Second
		if err := kill.Run(); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

// StartDetached starts cmd so it outlives the process that starts it: no
// console, its own process group, and out of the starter's job object
// (CREATE_BREAKAWAY_FROM_JOB), so neither closing the harness's window nor
// the harness ending its job ends it. A job that forbids breakaway makes
// CreateProcess fail with access denied: then it starts again without the
// flag, as a job member.
func StartDetached(cmd *exec.Cmd) (*os.Process, error) {
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB}
	err := cmd.Start()
	if err == nil {
		return cmd.Process, nil
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, err
	}
	again := &exec.Cmd{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, Stdin: cmd.Stdin, Stdout: cmd.Stdout, Stderr: cmd.Stderr}
	again.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := again.Start(); err != nil {
		return nil, err
	}
	return again.Process, nil
}
