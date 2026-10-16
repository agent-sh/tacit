//go:build unix

package platform

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
)

// OpenPrivateDirectory pins and validates a directory descriptor. Later
// pathname replacements cannot redirect append or retention operations.
func OpenPrivateDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, os.ErrPermission
	}
	return f, nil
}

func privateEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

// OpenPrivateAppend refuses links and special files, relative to a validated
// directory. O_NONBLOCK keeps a raced FIFO from blocking before fstat.
func OpenPrivateAppend(dir *os.File, name string) (*os.File, error) {
	if !privateEntryName(name) {
		return nil, os.ErrPermission
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, os.ErrPermission
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func RemovePrivateEntry(dir *os.File, name string) error {
	if !privateEntryName(name) {
		return os.ErrPermission
	}
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}
