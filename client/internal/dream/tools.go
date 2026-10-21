package dream

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ToolsFile is the dreamer's record of the tools each session used, in the
// machine's state directory (never the store): per session, the tool names
// its calls had and the programs its shell commands ran. A folder record's
// tool patterns are checked against it (a pattern must have matched a call
// of a session the record cites), and the assessment shows it to the
// curator's model. It holds names only: no argument, path or output.
const ToolsFile = "tools.json"

// Tool usage is kept for maxToolAge, at most maxToolSessions sessions, the
// oldest dropped first.
const (
	maxToolAge      = 180 * 24 * time.Hour
	maxToolSessions = 5000
	maxToolNames    = 200
)

// SessionTools is one session's tool usage.
type SessionTools struct {
	Seen     string   `json:"seen"`
	Tools    []string `json:"tools,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

// ToolUsage is ToolsFile: session id -> its tools.
type ToolUsage struct {
	Sessions map[string]SessionTools `json:"sessions"`
}

// LoadToolUsage reads the file at path (none: empty).
func LoadToolUsage(path string) *ToolUsage {
	u := &ToolUsage{Sessions: map[string]SessionTools{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, u)
	}
	if u.Sessions == nil {
		u.Sessions = map[string]SessionTools{}
	}
	return u
}

// ToolsOf reads a window's digest lines ("T: <tool> <key>", toolLine):
// the tool names, and for shell tools the program of each command (the
// first word of each part of a pipeline or list, its path dropped, after
// any VAR=value assignments), sorted and once each.
func ToolsOf(lines []string) (tools, programs []string) {
	ts, ps := map[string]bool{}, map[string]bool{}
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, "T: ")
		if !ok {
			continue
		}
		tool, key, _ := strings.Cut(rest, " ")
		if tool == "" || strings.ContainsAny(tool, "\t\r\n") {
			continue
		}
		ts[tool] = true
		if shellTools[tool] {
			for _, p := range Programs(key) {
				ps[p] = true
			}
		}
	}
	return keys(ts), keys(ps)
}

// Programs are the programs a command line runs: the first word of each
// part between |, ;, && and ||, after VAR=value assignments and sudo, env,
// time or nice, with its directory dropped. Quoted text is not parsed: a
// word that is no plain name is left out.
func Programs(cmd string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(cmd, func(r rune) bool { return r == '|' || r == ';' || r == '&' || r == '(' || r == ')' || r == '\n' }) {
		for _, w := range strings.Fields(part) {
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") {
				continue // an assignment
			}
			switch w {
			case "sudo", "env", "time", "nice", "command", "exec", "then", "do", "else":
				continue
			}
			w = path.Base(strings.ReplaceAll(w, `\`, "/"))
			if programRx(w) {
				out = append(out, w)
			}
			break
		}
	}
	return out
}

func programRx(w string) bool {
	if w == "" || len(w) > 40 {
		return false
	}
	for _, r := range w {
		if !(r == '-' || r == '_' || r == '.' || r == '+' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')) {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Add merges a window's usage into session's entry, dated day.
func (u *ToolUsage) Add(session, day string, tools, programs []string) {
	if session == "" || (len(tools) == 0 && len(programs) == 0) {
		return
	}
	e := u.Sessions[session]
	e.Tools = merge(e.Tools, tools)
	e.Commands = merge(e.Commands, programs)
	if day > e.Seen {
		e.Seen = day
	}
	u.Sessions[session] = e
}

func merge(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]string(nil), a...), b...) {
		set[x] = true
	}
	out := keys(set)
	if len(out) > maxToolNames {
		out = out[:maxToolNames]
	}
	return out
}

// Used reports a tool of session's (an id, or a cite like claude:<id>)
// that pattern matches (a glob on the tool name).
func (u *ToolUsage) Used(session, pattern string) bool {
	if _, id, ok := strings.Cut(session, ":"); ok {
		session = id
	}
	for _, t := range u.Sessions[session].Tools {
		if ok, _ := path.Match(pattern, t); ok {
			return true
		}
	}
	return false
}

// Save drops sessions older than maxToolAge (and the oldest past
// maxToolSessions) and writes the file at path atomically.
func (u *ToolUsage) Save(path string, now time.Time) error {
	cut := now.Add(-maxToolAge).Format("2006-01-02")
	ids := make([]string, 0, len(u.Sessions))
	for id, e := range u.Sessions {
		if e.Seen < cut {
			delete(u.Sessions, id)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) > maxToolSessions {
		sort.Slice(ids, func(i, j int) bool { return u.Sessions[ids[i]].Seen < u.Sessions[ids[j]].Seen })
		for _, id := range ids[:len(ids)-maxToolSessions] {
			delete(u.Sessions, id)
		}
	}
	b, err := json.MarshalIndent(u, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
