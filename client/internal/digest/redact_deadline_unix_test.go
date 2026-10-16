//go:build unix

package digest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRedactionCancellationClosesDescendantPipes(t *testing.T) {
	dir := t.TempDir()
	// The descendant retains stdout after its parent exits. Waiting for EOF
	// without WaitDelay would hold the request for five seconds.
	wrapper := "#!/bin/sh\n/bin/sleep 5 &\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "scrump"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := RedactAllContext(ctx, []string{"synthetic"}); err == nil {
		t.Fatal("incomplete redactor accepted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("descendant held redaction open")
	}
}
