package main

import (
	"os"
	"path/filepath"
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

// stubScrump is for tests that need redaction to run, not to mask: when the
// real scrump is absent it puts a stand-in first on PATH that masks only the
// canary digest.RedactionReady checks and passes everything else through.
func stubScrump(t *testing.T) {
	t.Helper()
	if realScrump() == nil {
		return
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec sed 's/Zq7Vx9/******/g'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
