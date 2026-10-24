package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
)

// The local layer (SPEC.md, "Local layer"): on a read-only clone with
// local_layer on, the dreamer and the curator work on this machine's overlay
// instead of the store. Their files sit next to the agents' notes, under two
// names no harness directory can have (those hold an @):
//
//	inbox/dreamer@<host>/<name>.md   the dreamer's notes: unreviewed, not delivered
//	curated/<scope>/<name>.md        the curator's facts: delivered here, "(this machine)"
//	curated/review/                  the curator's quote ledger and review lines
//
// Nothing here is a git repository and nothing is ever pushed.
const (
	InboxDir   = "inbox"
	CuratedDir = "curated"
	// ReviewDir is the curated layer's review directory, as review/ is the
	// store's: the quote ledger (owner-quotes.jsonl) and <date>.md lines.
	ReviewDir = CuratedDir + "/review"
)

// LayerOn reports whether the local layer is enabled on this machine:
// local_layer in the settings file, or TACIT_LOCAL_LAYER (on, true, yes or
// 1). It is off by default; off, a read-only clone dreams and curates nothing.
func LayerOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TACIT_LOCAL_LAYER"))) {
	case "on", "true", "yes", "1":
		return true
	}
	return false
}

// IsInbox reports a dreamer note of the local layer (a Prefix path): like a
// store inbox note it is unreviewed and no hook delivers it.
func IsInbox(path string) bool { return strings.HasPrefix(path, Prefix+InboxDir+"/") }

// IsCurated reports a fact the local curator wrote (a Prefix path).
func IsCurated(path string) bool { return strings.HasPrefix(path, Prefix+CuratedDir+"/") }

// IsRuling reports a local fact delivered as the owner's ruling on this
// machine: one the local curator wrote (harness curator under curated/),
// kind user, with an owner_quote and where it is from that the local quote
// ledger records for it. The curator records a quote only after the rulings
// evidence check found it in the owner's turns on this machine (SPEC.md,
// "Local layer"). An agent's note, a dreamer note, and a fact that only
// claims the fields are never rulings, nor are they their words.
func IsRuling(f *fact.Fact) bool {
	s := f.Meta.Source
	return IsCurated(f.Path) && s.Harness == "curator" && s.Kind == "user" && s.OwnerQuote != "" && s.OwnerQuoteFrom != "" && f.QuoteBacked
}

// Rel turns a Prefix path into the file's path in the overlay.
func Rel(path string) (string, bool) {
	rel, ok := strings.CutPrefix(path, Prefix)
	return rel, ok
}

// errEscape is a relative path that leaves the overlay or names no
// directory of the local layer.
var errEscape = errors.New("not a path of the local layer")

// layerPath checks rel (a slash path inside the overlay, under inbox/ or
// curated/) and returns its file path.
func layerPath(dir, storeDir, rel string) (string, error) {
	if dir == "" {
		return "", errors.New("no home directory for the local overlay")
	}
	if !Outside(dir, storeDir) {
		return "", ErrInside
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean != rel || filepath.IsAbs(rel) || strings.HasPrefix(clean, "../") || clean == ".." ||
		!(strings.HasPrefix(clean, InboxDir+"/") || strings.HasPrefix(clean, CuratedDir+"/")) {
		return "", fmt.Errorf("%w: %s", errEscape, rel)
	}
	full := filepath.Join(dir, filepath.FromSlash(clean))
	if err := landsInside(dir, storeDir, full); err != nil {
		return "", err
	}
	return full, nil
}

// landsInside checks where full really lands: every symlink on the way
// resolved (a linked curated/ or inbox/, a dangling link, a linked file),
// the result must be inside the overlay and outside the clone.
func landsInside(dir, storeDir, full string) error {
	final, err := resolve(full)
	if err != nil {
		return err
	}
	root, err := resolve(dir)
	if err != nil {
		return err
	}
	if !Outside(final, storeDir) {
		return ErrInside
	}
	if rel, err := filepath.Rel(root, final); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s resolves outside the overlay", errEscape, full)
	}
	return nil
}

// Write puts data at rel in the overlay (inbox/... or curated/...),
// atomically: a temporary file in the same directory, synced, renamed into
// place, so a killed run leaves the old file or the new one. It refuses an
// overlay inside the clone (ErrInside), like Add.
func Write(dir, storeDir, rel string, data []byte) error {
	full, err := layerPath(dir, storeDir, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	if err := landsInside(dir, storeDir, full); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".tacit-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), full)
}

// Append adds data to the file at rel (the ledger, a review file), through
// Write: the whole file is rewritten and renamed, never left half-appended.
func Append(dir, storeDir, rel string, data []byte) error {
	full, err := layerPath(dir, storeDir, rel)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(full)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return Write(dir, storeDir, rel, append(old, data...))
}

// curatedState is the local curator's quote ledger, read like a store's
// (fact.LoadState on curated/, whose review/ holds owner-quotes.jsonl).
func curatedState(dir string) fact.StoreState {
	st, err := fact.LoadState(filepath.Join(dir, CuratedDir))
	if err != nil {
		// A marker in the local layer is not ours to read; without the ledger
		// no local fact is a ruling (fail closed).
		return fact.StoreState{}
	}
	return st
}

// QuotesRel is the local quote ledger's path in the overlay.
var QuotesRel = CuratedDir + "/" + ledger.QuotesPath

// ReviewsRel is the local reviewer's ledger in the overlay (SPEC.md, "The
// reviewer"): its verdicts on this machine's agents' notes.
var ReviewsRel = CuratedDir + "/" + ledger.ReviewsPath

// Remove deletes the file at rel in the overlay (a stale generated folder
// index), with the same path checks as Write. A file already gone is no
// error.
func Remove(dir, storeDir, rel string) error {
	full, err := layerPath(dir, storeDir, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
