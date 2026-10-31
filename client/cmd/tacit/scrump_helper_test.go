package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// scrumpErr is nil when scrump with the text profile works on this machine,
// checked once at start, before any test changes PATH.
var scrumpErr = digest.RedactionReady()

func realScrump() error { return scrumpErr }

// requireScrump skips a test that checks what the real redactor masks.
func requireScrump(t *testing.T) {
	t.Helper()
	if err := realScrump(); err != nil {
		t.Skip("needs scrump with the text profile (https://github.com/avifenesh/scrump): " + err.Error())
	}
}

// stubScrump is for tests that need redaction to run, not to mask: it puts a
// stand-in first on PATH that masks only the canary digest.RedactionReady
// checks and passes everything else through, whether or not scrump is
// installed. A test that waits on the real one pays its start, about half a
// second a call, so a machine with scrump ran the suite three times slower
// than one without (the store's redactor is the suite's canaryRedact for the
// same reason). The tests of what the real one masks call requireScrump. On
// Windows the stand-in, a shell script, cannot run, so a scrump there stays.
func stubScrump(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && realScrump() == nil {
		return
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec sed 's/Zq7Vx9/******/g'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
