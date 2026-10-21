package main

// review/folders.jsonl, the curator's folder log (SPEC.md, "Folder
// records"): one JSON object per line, appended, never rewritten.
//
//	propose  a split entry's proposal of an area or topic no folder fits:
//	         the name, its kind (and area, for a topic), the fact written
//	         and the session its note came from
//	ask      a question for the owner a folder step left open, with a
//	         reason code can state (a ruling's move conflicts with a fact
//	         of the folder it would move to, ...)
//	close    the end of a proposal (made, folded into a folder, rejected)
//	         or of a question, by id
//
// Nothing here is sent anywhere: the app shows the open questions, and the
// owner's other channels are a later step.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// folderEntry is one line of the folder log.
type folderEntry struct {
	Kind string `json:"kind"` // propose | ask | close
	ID   string `json:"id"`
	At   string `json:"at"`
	// propose
	Name        string `json:"name,omitempty"`
	FolderKind  string `json:"folder_kind,omitempty"` // area | topic
	Area        string `json:"area,omitempty"`        // a topic's area, if any
	Description string `json:"description,omitempty"`
	Session     string `json:"session,omitempty"`
	// propose and ask
	Fact string `json:"fact,omitempty"`
	// ask
	Folder string `json:"folder,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
	// close
	Closes  string `json:"closes,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// line encodes the entry with its newline.
func (e folderEntry) line() []byte {
	b, _ := json.Marshal(e)
	return append(b, '\n')
}

// folderID is a stable id for an entry's content.
func folderID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:6])
}

// readFolderLog reads the log at the store root dir (or an overlay's
// curated directory).
func readFolderLog(dir string) []folderEntry {
	b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.FoldersPath)))
	var out []folderEntry
	for _, l := range bytes.Split(b, []byte("\n")) {
		var e folderEntry
		if json.Unmarshal(l, &e) == nil && e.Kind != "" && e.ID != "" {
			out = append(out, e)
		}
	}
	return out
}

// closedFolderIDs are the ids a close line ends.
func closedFolderIDs(log []folderEntry) map[string]string {
	out := map[string]string{}
	for _, e := range log {
		if e.Kind == "close" {
			out[e.Closes] = e.Outcome
		}
	}
	return out
}

// folderAsk is an open question for the owner.
type folderAsk struct {
	ID     string `json:"id"`
	Folder string `json:"folder"`
	Fact   string `json:"fact,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// openFolderAsks are the log's questions with no close line.
func openFolderAsks(dir string) []folderAsk {
	log := readFolderLog(dir)
	closed := closedFolderIDs(log)
	var out []folderAsk
	seen := map[string]bool{}
	for _, e := range log {
		if e.Kind == "ask" && closed[e.ID] == "" && !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, folderAsk{ID: e.ID, Folder: e.Folder, Fact: e.Fact, To: e.To, Reason: e.Reason, At: e.At})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}
