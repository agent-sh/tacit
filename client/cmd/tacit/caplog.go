package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// followLogCap bounds `tacit follow --log FILE`. launchd, unlike journald,
// rotates nothing, and a laptop off the network logs a reconnect at every
// backoff (up to one a minute, about 1.4k lines or 100 KB a day): 1 MiB holds
// about ten days, and one older file (FILE.1) the ten before.
const followLogCap = 1 << 20

// capLog is an append-only log file of at most max bytes plus one rotated
// copy: a line that would pass max first moves the file to <path>.1
// (replacing the old one) and starts a new file. Each line is stamped with
// the local time, since launchd adds none.
type capLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openCapLog(path string, max int64) (*capLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &capLog{path: path, max: max}
	return l, l.open()
}

func (l *capLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Printf writes one line. A write that fails is dropped: the log never stops
// the follower.
func (l *capLog) Printf(format string, a ...any) {
	line := time.Now().Format("2006-01-02T15:04:05-07:00") + " " + fmt.Sprintf(format, a...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(line)) > l.max {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			l.f = nil
		}
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}
