package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// splitCacheDays is how long a paid split answer is kept for a note that
// stays pending: past it the note is split again (its store has moved on).
const splitCacheDays = 14

// splitCache keeps the curator's paid answers for notes whose batch failed
// after them (curate-splits.json, curate-local-splits.json in the state
// directory): the split's entries per note, and the rulings pass's once it
// answered. A batch that failed in its rulings pass or conflict judge used
// to be split again, and paid again, in every run until the route came back
// (the rig's 2026-10-22/24 quick runs, about $0.80 each). Now the next run
// takes the answers it already has, checks them again (the checks' world may
// have moved; an entry they refuse sends the batch to the split again), and
// pays only for the step that failed. A note's answers go when its batch
// completes, when the note changes, or after splitCacheDays; all of them go
// when the split or rulings prompt changes.
type splitCache struct {
	path   string
	Prompt string                  `json:"prompt"` // the prompts' hash the answers were given to
	Notes  map[string]*cachedSplit `json:"notes"`
	dirty  bool
}

type cachedSplit struct {
	Hash    string         `json:"hash"` // the note's content (noteHash)
	At      string         `json:"at"`
	Model   string         `json:"model,omitempty"`
	Entries []curate.Entry `json:"entries"`
	// Rulings is the rulings pass's entries of the note, once the pass
	// answered for its batch (RulingsDone).
	Rulings     []curate.Entry `json:"rulings,omitempty"`
	RulingsDone bool           `json:"rulings_done,omitempty"`
}

// splitPromptsHash names the prompts an answer was given to.
func splitPromptsHash() string {
	h := sha256.Sum256([]byte(curate.SplitPrompt + "\x00" + curate.RulingsPrompt))
	return hex.EncodeToString(h[:8])
}

// noteHash is a note's content as the split reads it.
func noteHash(n *fact.Fact) string {
	s := n.Meta.Source
	h := sha256.Sum256([]byte(n.Meta.Scope + "\x00" + s.Kind + "\x00" + n.Meta.Description + "\x00" + n.Body + "\x00" + s.OwnerQuote + "\x00" + s.OwnerWords))
	return hex.EncodeToString(h[:12])
}

func loadSplitCache(path string, now time.Time) *splitCache {
	c := &splitCache{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, c)
	}
	if c.Prompt != splitPromptsHash() {
		c.Notes, c.dirty = nil, len(c.Notes) > 0
		c.Prompt = splitPromptsHash()
	}
	if c.Notes == nil {
		c.Notes = map[string]*cachedSplit{}
	}
	cut := now.AddDate(0, 0, -splitCacheDays).Format("2006-01-02")
	for k, v := range c.Notes {
		if v == nil || v.At < cut {
			delete(c.Notes, k)
			c.dirty = true
		}
	}
	return c
}

// has reports whether every note of the batch has a split answer for its
// current content.
func (c *splitCache) has(batch []*fact.Fact) bool {
	if c == nil || len(batch) == 0 {
		return false
	}
	for _, n := range batch {
		e := c.Notes[n.Meta.Name]
		if e == nil || e.Hash != noteHash(n) {
			return false
		}
	}
	return true
}

// split is the batch's cached split entries, in note order, and its rulings
// pass's when every note has them.
func (c *splitCache) split(batch []*fact.Fact) (entries, rulings []curate.Entry, rulingsDone bool) {
	rulingsDone = true
	for _, n := range batch {
		e := c.Notes[n.Meta.Name]
		entries = append(entries, e.Entries...)
		rulings = append(rulings, e.Rulings...)
		rulingsDone = rulingsDone && e.RulingsDone
	}
	if !rulingsDone {
		rulings = nil
	}
	return entries, rulings, rulingsDone
}

// putSplit keeps a valid split answer for the batch: each note's entries
// (none, for a note the split made nothing of).
func (c *splitCache) putSplit(batch []*fact.Fact, entries []curate.Entry, model string, now time.Time) {
	if c == nil {
		return
	}
	by := map[string][]curate.Entry{}
	for _, e := range entries {
		by[e.Origin] = append(by[e.Origin], e)
	}
	for _, n := range batch {
		c.Notes[n.Meta.Name] = &cachedSplit{Hash: noteHash(n), At: now.Format("2006-01-02"), Model: model, Entries: by[n.Meta.Name]}
	}
	c.dirty = true
	c.save()
}

// putRulings keeps the rulings pass's valid answer for the batch.
func (c *splitCache) putRulings(batch []*fact.Fact, entries []curate.Entry) {
	if c == nil {
		return
	}
	by := map[string][]curate.Entry{}
	for _, e := range entries {
		by[e.Origin] = append(by[e.Origin], e)
	}
	for _, n := range batch {
		if e := c.Notes[n.Meta.Name]; e != nil {
			e.Rulings, e.RulingsDone = by[n.Meta.Name], true
		}
	}
	c.dirty = true
	c.save()
}

// drop forgets the batch's answers: it completed, or its answers no longer
// stand.
func (c *splitCache) drop(batch []*fact.Fact) {
	if c == nil {
		return
	}
	for _, n := range batch {
		if _, ok := c.Notes[n.Meta.Name]; ok {
			delete(c.Notes, n.Meta.Name)
			c.dirty = true
		}
	}
}

// batches is curate.Batches of the pending notes, the notes with a cached
// answer in batches of their own and first: those cost no split. The second
// result is how many notes have one.
func (c *splitCache) batches(pending []*fact.Fact) ([][]*fact.Fact, int) {
	if c == nil || len(c.Notes) == 0 {
		return curate.Batches(pending), 0
	}
	var cached, fresh []*fact.Fact
	for _, n := range pending {
		if c.has([]*fact.Fact{n}) {
			cached = append(cached, n)
		} else {
			fresh = append(fresh, n)
		}
	}
	return append(curate.Batches(cached), curate.Batches(fresh)...), len(cached)
}

func (c *splitCache) save() {
	if c == nil || !c.dirty {
		return
	}
	os.MkdirAll(filepath.Dir(c.path), 0o700)
	if len(c.Notes) == 0 {
		os.Remove(c.path)
	} else {
		b, _ := json.Marshal(c)
		tmp := c.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, c.path)
		}
	}
	c.dirty = false
}

// cachedSplitFor is the batch's cached split answer when the cache has one
// for every note and the checks still pass it; an answer they refuse is
// dropped, and the batch is split again. rulingsCached says the rulings
// pass's answer is cached too (its entries still face their check).
func cachedSplitFor(c *splitCache, batch []*fact.Fact, check curate.Check) (entries, rulings []curate.Entry, rulingsCached, ok bool) {
	if !c.has(batch) {
		return nil, nil, false, false
	}
	entries, rulings, rulingsCached = c.split(batch)
	if check != nil && len(check(entries)) > 0 {
		c.drop(batch)
		return nil, nil, false, false
	}
	return entries, rulings, rulingsCached, true
}
