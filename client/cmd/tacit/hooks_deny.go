package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Rules before the mistake (SPEC.md, "Command rules"): a rule, standard or
// past mistake keyed to a command (applies_to.commands) stops that command
// once, before it runs, when the command about to run is precisely that
// command. The PreToolUse hook denies the call with the fact's text, so the
// agent reads it and decides again; the same fact never stops a session
// twice, so running the command again goes through. Precision over recall:
//
//   - the fact forbids the command as a whole (forbids): its name or
//     description says never, not, no, avoid, without or instead of right
//     before the key's words, and nothing after them but the clause's end
//     ("never gh pr merge: land by push"). A mistake keyed to a command it
//     only sometimes breaks ("sort needs LC_ALL=C with comm", "no sed for
//     JSX edits") names no situation the command alone shows, and a key of
//     one word never denies;
//   - the command is read as the shell reads it (denyCommands): a key in a
//     quoted string, a comment, a here-document or an argument does not
//     match; only a simple command's own program does, after assignments
//     and the wrappers that run their argument (sudo, env, nice, timeout...);
//   - the key's program is compared by base name, its other words in order
//     with the command's leading non-option words, its options anywhere in
//     the command;
//   - the fact is this session's: tied to its repository, or of no project
//     (the user's, this machine's), not stopped or demoted by outcomes.

// denyBodyCap bounds the fact's text a deny carries.
const denyBodyCap = 600

// denyTableAge is how long the table of keys is used before it is built
// again from the index: a fact keyed to a command denies within this of its
// write.
const denyTableAge = 10 * time.Minute

// denyFact is one fact of the deny table.
type denyFact struct {
	Key     []string `json:"key"`
	Name    string   `json:"name"`
	Scope   string   `json:"scope"`
	Repos   []string `json:"repos,omitempty"`
	Hosts   []string `json:"hosts,omitempty"`
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	Created string   `json:"created,omitempty"`
}

// denyTable is the specific keys' facts, built from the index.
type denyTable struct {
	Built string     `json:"built"`
	Facts []denyFact `json:"facts"`
}

func denyTablePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(platform.StateDir(home), "deny-keys.json")
}

// denyEligible: an owner's rule or ruling, a standard, or a past mistake.
func denyEligible(h search.Hit) bool {
	return h.Ruling || h.Type == "user_standard" || h.Type == "past_mistake"
}

// buildDenyTable reads the index's facts with applies_to and keeps the
// eligible ones whose command keys are specific.
func buildDenyTable(ctx context.Context, s *store.Store, now time.Time) (*denyTable, error) {
	ix, err := openHookIndex(ctx, s)
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	all, err := ix.MetaFacts(ctx)
	if err != nil {
		return nil, err
	}
	return denyTableOf(all, now), nil
}

// denyTableOf is the table of these facts: each eligible fact's keys that
// it forbids.
func denyTableOf(all []search.Hit, now time.Time) *denyTable {
	t := &denyTable{Built: now.UTC().Format(time.RFC3339)}
	for _, h := range all {
		if !denyEligible(h) || h.HeldBack() || h.Quiet {
			continue
		}
		for _, c := range h.Applies.Commands {
			k := denyKey(c)
			if len(k) == 0 || !forbids(h, k) {
				continue
			}
			t.Facts = append(t.Facts, denyFact{Key: k, Name: h.Name, Scope: h.Scope, Repos: h.Applies.Repos, Hosts: h.Applies.Hosts,
				Type: h.Type, Text: denyText(h), Created: h.Created})
		}
	}
	return t
}

// negations are the words that, right before a command in a fact's text,
// say the command is not to be run.
var negations = map[string]bool{"never": true, "not": true, "don't": true, "dont": true, "no": true, "avoid": true, "without": true}

var textToken = regexp.MustCompile("[a-z0-9][a-z0-9'._/=+:-]*[a-z0-9]|[a-z0-9]|--?[a-z0-9][a-z0-9._=-]*|[`\"]|[;:,!?()]|\\.(?:\\s|$)")

// quoteToken: a mark around a command, no word of the sentence.
func quoteToken(t string) bool { return t == "`" || t == `"` }

// boundary: a token that ends a clause.
func boundary(t string) bool {
	return strings.ContainsAny(t, ";:,!?().") && len(strings.TrimSpace(t)) == 1
}

// fillers may stand between a negation and the command it forbids ("do
// not run X", "never use X"); any other word makes the command an object of
// something else ("never pipe X", "not plain X").
var fillers = map[string]bool{"run": true, "use": true, "call": true, "do": true, "to": true, "ever": true}

// forbids reports whether the fact forbids the key as a whole command: its
// description holds the key's words in a row, a negation right before them
// (never, not, don't, no, avoid, without, instead of; "run", "use", "call",
// "do", "to" and "ever" may stand between), and nothing after them but the
// clause's end, so "never gh pr merge: land by push" forbids gh pr merge and
// "no sed for JSX edits" or "never pipe git remote -v through head" does
// not. Its name counts too when it starts with the negation
// ("never-gh-pr-merge"). A key of one word (a bare program) is never
// forbidden as a whole: a rule about a program is about how it is used.
func forbids(h search.Hit, key []string) bool {
	if len(key) < 2 {
		return false
	}
	name := textToken.FindAllString(strings.ToLower(strings.ReplaceAll(h.Name, "-", " ")), -1)
	if len(name) > 0 && negations[name[0]] && forbidsIn(name, key) {
		return true
	}
	return forbidsIn(textToken.FindAllString(strings.ToLower(h.Description), -1), key)
}

func forbidsIn(w, key []string) bool {
	for i := 0; i+len(key) <= len(w); i++ {
		if filepath.Base(w[i]) != key[0] || !slicesEqual(w[i+1:i+len(key)], key[1:]) {
			continue
		}
		end := i + len(key)
		for end < len(w) && quoteToken(w[end]) {
			end++
		}
		if end < len(w) && !boundary(w[end]) {
			continue
		}
		j, filled := i-1, 0
		for j >= 0 && (quoteToken(w[j]) || fillers[w[j]] && filled < 2) {
			if !quoteToken(w[j]) {
				filled++
			}
			j--
		}
		if j < 0 {
			continue
		}
		if negations[w[j]] && w[j] != "instead" || w[j] == "of" && j > 0 && w[j-1] == "instead" {
			return true
		}
	}
	return false
}

// denyText is the fact's text as a deny carries it: its description, and
// its body's start when it adds to it.
func denyText(h search.Hit) string {
	d := strings.TrimSpace(h.Description)
	b := strings.Join(strings.Fields(h.Body), " ")
	if len([]rune(b)) > denyBodyCap {
		b = string([]rune(b)[:denyBodyCap]) + "..."
	}
	switch {
	case b == "" || strings.Contains(b, d):
		if b == "" {
			return d
		}
		return b
	case d == "":
		return b
	}
	return d + " " + b
}

// loadDenyTable is the table on disk. One older than denyTableAge, or none,
// starts a rebuild in the background (startDenyBuild, at most one at a time)
// and this call uses what is there: the hook's budget is no time to scan the
// store's facts.
func loadDenyTable(now time.Time) *denyTable {
	t := readDenyTable()
	if t != nil {
		if at, err := time.Parse(time.RFC3339, t.Built); err == nil && now.Sub(at) < denyTableAge && now.Sub(at) >= 0 {
			return t
		}
	}
	marker := denyTablePath() + ".building"
	if fi, err := os.Stat(marker); err == nil && now.Sub(fi.ModTime()) < 2*time.Minute {
		return t
	}
	os.Remove(marker) // a build that died
	if os.MkdirAll(filepath.Dir(marker), 0o700) != nil {
		return t
	}
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return t
	}
	f.Close()
	startDenyBuild()
	if fresh := readDenyTable(); fresh != nil {
		return fresh // a build that finished at once (the tests run it in this process)
	}
	return t
}

func readDenyTable() *denyTable {
	b, err := os.ReadFile(denyTablePath())
	if err != nil {
		return nil
	}
	var t denyTable
	if json.Unmarshal(b, &t) != nil {
		return nil
	}
	return &t
}

// writeDenyTable is `tacit hook deny-table`: the table built from the
// index and saved, the building marker removed.
func writeDenyTable(s *store.Store) {
	defer os.Remove(denyTablePath() + ".building")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t, err := buildDenyTable(ctx, s, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tacit hook deny-table:", err)
		return
	}
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	p := denyTablePath()
	tmp := fmt.Sprintf("%s.%d", p, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

// startDenyBuild starts `tacit hook deny-table` detached, its output in
// index-build.log. Tests substitute it.
var startDenyBuild = func() {
	exe, err := hookExe()
	if err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	log, err := os.OpenFile(filepath.Join(platform.StateDir(home), "index-build.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer log.Close()
	cmd := exec.Command(exe, "hook", "deny-table")
	cmd.Stdout, cmd.Stderr = log, log
	if p, err := platform.StartDetached(cmd); err == nil {
		p.Release()
	}
}

// denyKey is a key's words, lowercase, up to its first placeholder
// (<pr>, $X, {n}, ...): what the command must start with.
func denyKey(c string) []string {
	cmds := denyCommands(c)
	if len(cmds) != 1 {
		return nil // a key of two commands, or none, is no prefix of one
	}
	var out []string
	for _, w := range cmds[0] {
		if strings.ContainsAny(w, "<>${}*?[]|") || w == "..." {
			break
		}
		out = append(out, strings.ToLower(w))
	}
	if len(out) == 0 {
		return nil
	}
	out[0] = filepath.Base(out[0])
	return out
}

// wrappers run their argument as a command: their options (and a
// duration or niceness) come before it.
var wrappers = map[string]bool{"sudo": true, "env": true, "nice": true, "ionice": true, "time": true, "nohup": true, "exec": true, "command": true, "timeout": true, "stdbuf": true, "doas": true}

var wrapperArg = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)

// gitGlobalArg are git's global options that take the next word.
var gitGlobalArg = map[string]bool{"-c": true, "-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// program is a simple command's words from its program on: assignments
// and wrappers (with their options) dropped, git's global options too.
func program(w []string) []string {
	for len(w) > 0 {
		switch {
		case isAssignment(w[0]):
			w = w[1:]
		case wrappers[filepath.Base(w[0])]:
			w = w[1:]
			for len(w) > 0 && (strings.HasPrefix(w[0], "-") || isAssignment(w[0]) || wrapperArg.MatchString(w[0])) {
				w = w[1:]
			}
		default:
			if filepath.Base(w[0]) == "git" {
				rest := w[1:]
				for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
					if gitGlobalArg[rest[0]] && len(rest) > 1 {
						rest = rest[2:]
					} else {
						rest = rest[1:]
					}
				}
				return append([]string{w[0]}, rest...)
			}
			return w
		}
	}
	return nil
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for _, r := range w[:i] {
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// denyMatch reports whether the simple command w (its words, as
// denyCommands reads them) runs the key: the same program by base name,
// the key's non-option words as the command's first non-option words, the
// key's options among the command's words.
func denyMatch(key, w []string) bool {
	w = program(w)
	if len(key) == 0 || len(w) == 0 || filepath.Base(strings.ToLower(w[0])) != key[0] {
		return false
	}
	var args, opts []string
	for _, x := range w[1:] {
		x = strings.ToLower(x)
		if strings.HasPrefix(x, "-") {
			opts = append(opts, x)
		} else {
			args = append(args, x)
		}
	}
	i := 0
	for _, k := range key[1:] {
		if strings.HasPrefix(k, "-") {
			found := false
			for _, o := range opts {
				if o == k || strings.HasPrefix(o, k+"=") {
					found = true
					break
				}
			}
			if !found {
				return false
			}
			continue
		}
		if i >= len(args) || args[i] != k {
			return false
		}
		i++
	}
	return true
}

// denyCommands reads a shell command line into its simple commands' words
// as the shell would run them: words split on blanks outside quotes, quotes
// removed (a quoted string is one word, never a command), commands split at
// ; & | && || newlines and parentheses, $( and backticks starting one of
// their own, comments and here-documents' bodies dropped.
func denyCommands(line string) [][]string {
	var cmds [][]string
	var cur []string
	var word strings.Builder
	inWord := false
	var heredocs []string // delimiters whose bodies start at the next newline
	var nested [][]string // the commands of substitutions inside double quotes
	endWord := func() {
		if inWord {
			cur = append(cur, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
			cur = nil
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			i++
			if rs[i] != '\n' {
				word.WriteRune(rs[i])
				inWord = true
			}
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			word.WriteString(string(rs[i+1 : min(j, len(rs))]))
			inWord, i = true, j
		case r == '"':
			// A command substitution inside the quotes runs its own
			// commands, and its quotes nest.
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				switch {
				case rs[j] == '\\' && j+1 < len(rs):
					word.WriteRune(rs[j+1])
					j += 2
				case rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(':
					k := closingParen(rs, j+1)
					nested = append(nested, denyCommands(string(rs[j+2:k]))...)
					j = min(k+1, len(rs))
				default:
					word.WriteRune(rs[j])
					j++
				}
			}
			inWord, i = true, j
		case r == '#' && !inWord:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i-- // the newline ends the command below
		case r == '<' && i+1 < len(rs) && rs[i+1] == '<' && !(i+2 < len(rs) && rs[i+2] == '<'):
			endWord()
			j := i + 2
			if j < len(rs) && rs[j] == '-' {
				j++
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			k := j
			for k < len(rs) && !unicode.IsSpace(rs[k]) && !strings.ContainsRune(";&|()<>", rs[k]) {
				k++
			}
			if d := strings.Trim(string(rs[j:k]), `'"\`); d != "" {
				heredocs = append(heredocs, d)
			}
			i = k - 1
		case r == '\n':
			endCmd()
			for _, d := range heredocs {
				// The body: every line up to the delimiter's own.
				for i+1 < len(rs) {
					j := i + 1
					for j < len(rs) && rs[j] != '\n' {
						j++
					}
					l := strings.TrimLeft(string(rs[i+1:j]), "\t")
					i = j
					if strings.TrimSpace(l) == d {
						break
					}
				}
			}
			heredocs = nil
		case r == ';' || r == '&' || r == '|' || r == '(' || r == ')' || r == '`':
			endCmd()
		case r == '$' && i+1 < len(rs) && rs[i+1] == '(':
			endCmd()
			i++
		case unicode.IsSpace(r):
			endWord()
		case r == '>' || r == '<':
			// A redirection: its target is no word of the command.
			endWord()
			j := i + 1
			for j < len(rs) && (rs[j] == '>' || rs[j] == '&' || rs[j] == '<') {
				j++
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			for j < len(rs) && !unicode.IsSpace(rs[j]) && !strings.ContainsRune(";&|()", rs[j]) {
				j++
			}
			i = j - 1
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	endCmd()
	return append(cmds, nested...)
}

// closingParen is the index of the parenthesis that closes rs[open], quotes
// and nested parentheses skipped; len(rs) when none does.
func closingParen(rs []rune, open int) int {
	depth := 0
	for i := open; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'':
			for i++; i < len(rs) && rs[i] != '\''; i++ {
			}
		case '"':
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(rs)
}

// deniedFile holds the facts that stopped a command in the session.
func deniedFile(id string) string {
	if p := sessionFile(id); p != "" {
		return p + ".denied"
	}
	return ""
}

func deniedSet(id string) map[string]bool {
	out := map[string]bool{}
	if p := deniedFile(id); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if l != "" {
					out[l] = true
				}
			}
		}
	}
	return out
}

func addDenied(id string, names []string) {
	p := deniedFile(id)
	if p == "" {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strings.Join(names, "\n") + "\n")
}

// denyScope is what the session is, for a fact's fit: its repository's
// names (none outside one) and this machine.
type denyScope struct {
	repos []string
	host  string
}

// fits: the fact is this session's: tied to its repository, or of no
// project and no other machine.
func (sc denyScope) fits(f denyFact) bool {
	if len(f.Hosts) > 0 {
		ok := false
		for _, h := range f.Hosts {
			ok = ok || strings.EqualFold(h, sc.host)
		}
		if !ok {
			return false
		}
	}
	if len(f.Repos) > 0 {
		for _, r := range f.Repos {
			for _, n := range sc.repos {
				if strings.EqualFold(r, n) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(f.Scope, "projects/"):
		for _, n := range sc.repos {
			if strings.EqualFold(f.Scope, "projects/"+n) {
				return true
			}
		}
		return false
	case strings.HasPrefix(f.Scope, "machines/"):
		return strings.EqualFold(f.Scope, "machines/"+sc.host)
	}
	return f.Scope == "user" || f.Scope == "system"
}

// denyFor is the facts the command stops: the table's facts that fit the
// session, match one of the command's simple commands, were not stopped by
// outcomes in the scope and have not stopped this session before, nor has
// their key. One a key, at most two, the repository's first, then the
// newest.
func denyFor(t *denyTable, command string, sc denyScope, denied map[string]bool, oc outcomes) []denyFact {
	if t == nil || len(t.Facts) == 0 {
		return nil
	}
	cmds := denyCommands(command)
	var out []denyFact
	seen := map[string]bool{}
	for _, f := range t.Facts {
		if denied[f.Name] || denied[denyKeyLine(f.Key)] || seen[f.Name] || !sc.fits(f) {
			continue
		}
		if c := oc.class(f.Name); c == outcomeStopped || c == outcomeDemoted {
			continue
		}
		for _, w := range cmds {
			if denyMatch(f.Key, w) {
				seen[f.Name] = true
				out = append(out, f)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := len(out[i].Repos) > 0 || strings.HasPrefix(out[i].Scope, "projects/"), len(out[j].Repos) > 0 || strings.HasPrefix(out[j].Scope, "projects/")
		if a != b {
			return a
		}
		return out[i].Created > out[j].Created
	})
	// One fact a key: many notes may forbid the same command.
	var one []denyFact
	keys := map[string]bool{}
	for _, f := range out {
		if k := denyKeyLine(f.Key); !keys[k] && len(one) < 2 {
			keys[k] = true
			one = append(one, f)
		}
	}
	return one
}

// denyKeyLine is a key's line in the session's denied set: a key that
// stopped a command once stops it no more, whichever fact forbids it.
func denyKeyLine(key []string) string { return "key:" + strings.Join(key, " ") }

// denyReason is what the agent reads in place of the command's output.
func denyReason(fs []denyFact, mcp bool) string {
	var b strings.Builder
	b.WriteString("tacit stopped this command once, before it ran: a rule of this owner is keyed to it.\n")
	for _, f := range fs {
		fmt.Fprintf(&b, "- %s (%s): %s\n", f.Name, strings.ReplaceAll(f.Type, "_", " "), f.Text)
	}
	open := "`tacit fact <name>`"
	if mcp {
		open = "`mcp__tacit__tacit_fact`"
	}
	fmt.Fprintf(&b, "If the command is still right given this, run it again unchanged: tacit does not stop it again for these. %s opens one in full.", open)
	return b.String()
}
