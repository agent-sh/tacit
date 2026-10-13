package results

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// cacheVersion is bumped whenever extraction, the read limits or the Series
// shape change, so an old cache is never read as if it were current.
const cacheVersion = "results-v3"

// cached is what reading a repo's history yields, kept per (repo, commit):
// the series and the counts. Only matching against the saved facts depends on
// anything else, so the daily run of an unchanged repo costs a file read.
type cached struct {
	V      string
	Ref    string
	Series []*Series
	Stats  Stats
}

func cacheName(dir, repo, ref string) string {
	if len(ref) > 12 {
		ref = ref[:12]
	}
	return filepath.Join(dir, repo+"-"+ref+".json")
}

func cacheLoad(dir, repo, ref string) (*cached, bool) {
	if dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(cacheName(dir, repo, ref))
	if err != nil {
		return nil, false
	}
	var c cached
	if json.Unmarshal(b, &c) != nil || c.V != cacheVersion || c.Ref != ref {
		return nil, false
	}
	for _, s := range c.Series {
		if len(s.Points) == 0 {
			return nil, false
		}
	}
	return &c, true
}

// cacheSave writes the entry (a read that finished, whole) and removes the
// repo's entries for other commits. A failure to write is not an error: the
// next run reads again.
func cacheSave(dir, repo, ref string, series []*Series, st Stats) {
	if dir == "" {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	b, err := json.Marshal(cached{V: cacheVersion, Ref: ref, Series: series, Stats: st})
	if err != nil {
		return
	}
	own := cacheName(dir, repo, ref)
	tmp := own + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if os.Rename(tmp, own) != nil {
		os.Remove(tmp)
		return
	}
	old, _ := filepath.Glob(filepath.Join(dir, repo+"-????????????.json"))
	for _, f := range old {
		if f != own {
			os.Remove(f)
		}
	}
}
