package digest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// crossing are pairs of texts where, scanned as one stream, a hit that
// starts in the first runs into the second (#84). Secret-shaped parts are
// built from pieces.
func crossing() [][2]string {
	pw := "pass" + "word"
	next := pw + ": '" + "Zq7Vx9" + "Lm2Kp4" + "' and the rest"
	return [][2]string{
		// The issue's repro: a setting's quoted value left open runs to the
		// next quote; the filter keeps the setting and the scan resumes past
		// the next text's key.
		{"deploy token_url: 'https://auth.example.invalid/oauth/" + strings.Repeat("p", 80), next},
		// A reference to a vault, filtered the same way.
		{"DB_PASS" + "WORD='$(vault read -field=value secret/db/" + strings.Repeat("q", 80), next},
		// A credential's value left open is masked into the next text.
		{"api_key: 'Wq8Ux0" + "Kn3Jp5 cut here", next},
		// A keyword rule's reach across lines takes the next text's token.
		{"notes on the abstract", "a1b2c3d4e5f6a7b8c9d0" + "e1f2a3b4c5d6 is the build id"},
	}
}

// hasRecords reports whether the installed scrump has records mode.
func hasRecords(t *testing.T) bool {
	t.Helper()
	_, err := RedactRecordsContext(context.Background(), []string{"a", "b"})
	if err != nil && !errors.Is(err, ErrNoRecords) {
		t.Fatal(err)
	}
	return err == nil
}

// Every text comes back as it does alone, the issue's repro's password
// masked: in records mode and with the boundary a scrump without it gets.
func TestRedactAllMasksEachTextAsAlone(t *testing.T) {
	requireScrump(t)
	var texts []string
	for _, p := range crossing() {
		texts = append(texts, p[0], p[1])
	}
	alone := make([]string, len(texts))
	for i, s := range texts {
		r, err := RedactErr(s)
		if err != nil {
			t.Fatal(err)
		}
		alone[i] = r
	}
	if strings.Contains(alone[1], "Zq7Vx9") {
		t.Fatalf("scrump keeps the password even alone: %q", alone[1])
	}
	records := hasRecords(t)
	for _, mode := range []struct {
		name string
		set  int32
	}{{"records", 1}, {"boundary", 2}} {
		t.Run(mode.name, func(t *testing.T) {
			if mode.set == 1 && !records {
				t.Skip("needs scrump with records mode (0.2.0 or later)")
			}
			prev := recordsMode.Load()
			recordsMode.Store(mode.set)
			defer recordsMode.Store(prev)
			out, err := RedactAll(texts)
			if err != nil {
				t.Fatal(err)
			}
			for i := range texts {
				if out[i] != alone[i] {
					t.Errorf("text %d:\n got %q\nwant %q", i, out[i], alone[i])
				}
			}
		})
	}
}

// A text that holds a record separator is masked alone, in records mode and
// with the boundary, and the others still come back in their places.
func TestRedactAllMasksATextWithASeparatorAlone(t *testing.T) {
	requireScrump(t)
	secret := "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"
	texts := []string{secret, "binary\x1eoutput " + secret, "plain"}
	records := hasRecords(t)
	for _, set := range []int32{1, 2} {
		if set == 1 && !records {
			continue
		}
		prev := recordsMode.Load()
		recordsMode.Store(set)
		out, err := RedactAll(texts)
		recordsMode.Store(prev)
		if err != nil || len(out) != 3 {
			t.Fatalf("mode %d: %v %q", set, err, out)
		}
		if strings.Contains(out[0], "Zq7Vx9") || strings.Contains(out[1], "Zq7Vx9") || out[2] != "plain" || !strings.HasPrefix(out[1], "binary\x1eoutput ") {
			t.Fatalf("mode %d: %q", set, out)
		}
	}
}

// A digest is masked as one text and line by line: a hit from one line does
// not keep the next line's password, and a keyword at the end of a line
// still masks the token that starts the next one.
func TestFinishMasksEachLineAndAcrossLines(t *testing.T) {
	requireScrump(t)
	var lines []string
	for _, p := range crossing() {
		lines = append(lines, "T: Bash "+p[0], "U: "+p[1])
	}
	out, err := Finish(lines)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range out {
		if strings.Contains(l, "Zq7Vx9") {
			t.Errorf("line %d keeps the password: %q", i, l)
		}
	}
	if last := out[len(out)-1]; strings.Contains(last, "a1b2c3d4") {
		t.Errorf("the token after a keyword on the line before is kept: %q", last)
	}
	for _, i := range []int{0, 2} {
		if !strings.Contains(out[i], "pppp") && !strings.Contains(out[i], "qqqq") {
			t.Errorf("line %d, a setting, was masked: %q", i, out[i])
		}
	}
}

// A scrump whose text profile does not mask the canary is told which scrump
// to install: 0.2.0 or later, not "main". One that masks only a value with
// its closing quote (before 0.2.0) fails OpenValueReady; one whose records
// answer is malformed is an error of RecordsReady, never ErrNoRecords, as
// it fails every batched redaction.
func TestRedactionReadyNamesTheScrumpToInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	stub := func(script string) {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	stub("#!/bin/sh\nexec cat\n")
	err := RedactionReady()
	if err == nil || !strings.Contains(err.Error(), "install scrump 0.2.0 or later") || strings.Contains(err.Error(), "from main") {
		t.Fatalf("RedactionReady = %v", err)
	}
	if err := RecordsReady(); !errors.Is(err, ErrNoRecords) {
		t.Fatalf("a scrump that masks nothing keeps no records apart: %v", err)
	}
	if err := OpenValueReady(); err == nil || !strings.Contains(err.Error(), "0.2.0") {
		t.Fatalf("OpenValueReady = %v", err)
	}
	stub("#!/bin/sh\nsed 's/Zq7Vx9/******/g' | tr '\\036' '*'\n")
	if err := RecordsReady(); err == nil || errors.Is(err, ErrNoRecords) {
		t.Fatalf("a malformed records answer: %v, want an error that is not ErrNoRecords", err)
	}
	if err := OpenValueReady(); err != nil {
		t.Fatalf("a scrump that masks the open value: %v", err)
	}
}
