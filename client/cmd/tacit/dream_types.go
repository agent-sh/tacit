package main

// The dreamer's quiet types (SPEC.md, Dreamer, "Quiet types"), measured
// from this machine's delivery log at most once a day.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
)

const (
	// quietMinDays is the fewest days of delivery log a measurement needs.
	quietMinDays = 7
	// quietMinLive is the fewest live facts a type needs to be measured.
	quietMinLive = 50
	// quietShare: a type is quiet when its delivered lines per live fact
	// are under this share of the median type's.
	quietShare = 0.5
)

// quietable are the dreamer's types a measurement may make quiet: the two
// the prompt asks for by no class of its own. The owner's standards, the
// failures with their fixes, machine facts and decisions each have a class
// line ("Extract EVERY durable fact ..."); with one of them out of the
// list a model files the same facts under another type, so they are never
// quiet.
var quietable = map[string]bool{"structure": true, "history": true}

// typeYield is one type's measurement.
type typeYield struct {
	Live      int     `json:"live"`
	Delivered int     `json:"delivered"`
	Rate      float64 `json:"rate"`
}

// quietMeasure is the cached measurement, dream-types.json in the state
// directory.
type quietMeasure struct {
	Day    string               `json:"day"`
	Store  string               `json:"store"`
	Days   int                  `json:"days"`
	Median float64              `json:"median"`
	Types  map[string]typeYield `json:"types"`
	Quiet  []string             `json:"quiet"`
}

// quietDreamTypes are the types the dreamer does not write this run:
// $TACIT_DREAM_QUIET "off" for none, a comma-separated list for those,
// else (unset or "auto") the measurement of the delivery log, kept for the
// day (not by a dry run).
func quietDreamTypes(storeDir, stateDir string, dry bool) dream.Quiet {
	out := dream.Quiet{}
	switch v := strings.TrimSpace(os.Getenv("TACIT_DREAM_QUIET")); v {
	case "off":
		return out
	case "", "auto":
	default:
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); quietable[t] {
				out[t] = true
			}
		}
		return out
	}
	path := filepath.Join(stateDir, "dream-types.json")
	today := time.Now().UTC().Format("2006-01-02")
	var m quietMeasure
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &m) == nil && m.Day == today && m.Store == storeDir {
		for _, t := range m.Quiet {
			if quietable[t] {
				out[t] = true
			}
		}
		return out
	}
	m = measureQuiet(storeDir, filepath.Join(stateDir, "delivery"))
	m.Day, m.Store = today, storeDir
	// A dry run writes nothing: it measures and keeps no reading.
	if b, err := json.Marshal(m); err == nil && !dry {
		_ = os.WriteFile(path+".tmp", b, 0o600)
		_ = os.Rename(path+".tmp", path)
	}
	for _, t := range m.Quiet {
		out[t] = true
	}
	return out
}

// measureQuiet reads the delivery log's lines (each candidate's type and
// whether a hook printed it; this machine's own notes, which the store does
// not hold, are not counted) and the store's live facts by type: a type's
// rate is its delivered lines per live fact, and the median is over the
// dreamer's types. With fewer than quietMinDays days' files read whole,
// nothing is quiet.
func measureQuiet(storeDir, logDir string) quietMeasure {
	m := quietMeasure{Types: map[string]typeYield{}}
	files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	delivered := map[string]int{}
	for _, p := range files {
		if !rePlainDay(strings.TrimSuffix(filepath.Base(p), ".jsonl")) {
			continue
		}
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		day := map[string]int{}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.Contains(line, []byte(`"delivered":true`)) {
				continue
			}
			var e struct {
				Cands []struct {
					Type      string `json:"type"`
					Delivered bool   `json:"delivered"`
					Local     bool   `json:"local"`
				} `json:"cands"`
			}
			if json.Unmarshal(line, &e) != nil {
				continue
			}
			for _, c := range e.Cands {
				if c.Delivered && !c.Local && c.Type != "" {
					day[c.Type]++
				}
			}
		}
		err = sc.Err()
		fh.Close()
		if err != nil {
			continue // a day read in part is no day's measurement
		}
		m.Days++
		for t, n := range day {
			delivered[t] += n
		}
	}
	if m.Days < quietMinDays {
		return m
	}
	facts, _, err := fact.Walk(storeDir)
	if err != nil {
		return m
	}
	live := map[string]int{}
	for _, f := range facts {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		live[f.Meta.Type]++
	}
	var rates []float64
	for _, t := range dream.Types {
		n := live[t]
		if n < quietMinLive {
			continue
		}
		y := typeYield{Live: n, Delivered: delivered[t], Rate: float64(delivered[t]) / float64(n)}
		m.Types[t] = y
		rates = append(rates, y.Rate)
	}
	if len(rates) == 0 {
		return m
	}
	sort.Float64s(rates)
	if n := len(rates); n%2 == 1 {
		m.Median = rates[n/2]
	} else {
		m.Median = (rates[n/2-1] + rates[n/2]) / 2
	}
	for t, y := range m.Types {
		if quietable[t] && y.Rate < quietShare*m.Median {
			m.Quiet = append(m.Quiet, t)
		}
	}
	sort.Strings(m.Quiet)
	return m
}
