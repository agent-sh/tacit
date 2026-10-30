package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A fact of this machine whose description names a path that is not here
// any more is most often stale: on the author's machine, 27 of a fresh
// sample of 30 such facts were (eval/VALUE.md, "Machine facts whose paths
// are gone"). The rotation takes them first and shows the model the paths.
// The rules below were each written for a kind of false flag the samples
// showed; no model is called.
var (
	// An absolute path under a system root, or one under ~, cut at the
	// first character no path in the samples held. A colon ends it (a
	// line number, a list of paths).
	gonePathRx = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_$/.~-])(~/[\p{L}\p{N}_@%+=,./-]+|/(?:home|data|opt|etc|srv|var|usr|mnt|Users)/[\p{L}\p{N}_@%+=,./-]+)`)
	// A placeholder or a pattern, not a path: NN, XXXX, YYYY, %Y, a dot run.
	goneTemplate = regexp.MustCompile(`\.\.\.|N{2,}|X{2,}|YYYY|MMDD|\bHH\b|[a-z]N(?:$|[-_./])|%[A-Za-z]`)
	// What comes before a path a command writes: an output, a scratch dir.
	goneOutput = regexp.MustCompile(`(?i)(?:-o|--out|--output|--target-dir|--local-dir|TMPDIR=|>|written to|writes to|go(?:es)? (?:to|in|into)|sav(?:e|ed|es) (?:to|in)|download(?:ed|s|ing)?(?: [\w.-]+)* to)\s*$`)
	// A fact about a path made on use: it need not exist between uses.
	goneMade = regexp.MustCompile(`(?i)mkdir -p|if missing|if absent|created (?:on|at) |is created|gets created`)
	// An environment variable set to a path: a setting, made on use.
	goneEnv = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=[~/]`)
	// In the path's clause: it says the path is not there, or is an example.
	goneNegation = regexp.MustCompile(`(?i)\b(?:no|not|never|removed|deleted|gone|obsolete|stale|absence|absent|missing|longer|retired|old|was|were|former|previous|instead)\b`)
	goneExample  = regexp.MustCompile(`(?i)\b(?:eg|for example|for instance|such as|like|say)\b`)
	// The words that end the clause before a path's: "eg, ~/x".
	goneExampleEnd = regexp.MustCompile(`(?i)\b(?:eg|for example|for instance|such as|say)$`)
	// Anywhere in the description: the fact itself says something is not there.
	goneSaid = regexp.MustCompile(`(?i)\b(?:gone|deleted|removed|moved|no longer exists?|do(?:es)?(?: not|n't|nt) exist|obsolete)\b`)
	// The fact is about another machine (its ~ is that machine's home).
	goneRemote = regexp.MustCompile(`(?i)\bon (?:the |a |another )?(?:box|server|remote|vm|mac)\b|\bvia ssh\b|\bssh\b[^;.]*\w@[\w.-]+|\b[\w-]+:~/`)
	// Spellings of e.g. and i.e. the clause rules read as words.
	goneEg = regexp.MustCompile(`(?i)\be\.g\.`)
	goneIe = regexp.MustCompile(`(?i)\bi\.e\.`)
)

// goneProbe finds, for the facts of one machine, the paths their
// descriptions name that do not exist under its home. Built once per run:
// each fact is read and its paths are looked up once.
type goneProbe struct {
	host, home string
	elsewhere  *regexp.Regexp // a fact that says it is about one of the store's other machines
	memo       map[*fact.Fact][]string
}

// newGoneProbe is the probe for host, whose home is home. machines are the
// names of the store's machines (machineNames); a description that puts its
// paths on another of them is not this machine's to look up.
func newGoneProbe(host, home string, machines []string) *goneProbe {
	g := &goneProbe{host: host, memo: map[*fact.Fact][]string{}}
	if home != "" {
		g.home = filepath.Clean(home)
	}
	var names []string
	seen := map[string]bool{}
	for _, m := range machines {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] || strings.Contains("-"+strings.ToLower(host)+"-", "-"+m+"-") {
			continue
		}
		seen[m] = true
		names = append(names, regexp.QuoteMeta(m))
	}
	if len(names) > 0 {
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		alt := strings.Join(names, "|")
		g.elsewhere = regexp.MustCompile(`(?i)\bon (?:the )?(?:` + alt + `)s?\b|\bssh (?:-\S+ )*(?:[\w.-]*@)?[\w-]*(?:` + alt + `)|^(?:` + alt + `)\b:`)
	}
	return g
}

// machineNames are the store's machines: the hosts of its machines/<host>
// folders, and the part of a hyphenated host that holds a digit, which is
// how a machine is often called (spark-b23d, b23d). Not applies_to.hosts:
// agents put addresses, GPUs and nicknames there.
func machineNames(facts []*fact.Fact) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range facts {
		h, ok := strings.CutPrefix(f.Meta.Scope, "machines/")
		if !ok || strings.Contains(h, "/") || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
		if parts := strings.Split(h, "-"); len(parts) > 1 {
			for _, p := range parts {
				if len(p) >= 3 && strings.ContainsAny(p, "0123456789") && strings.ContainsAny(p, "abcdefghijklmnopqrstuvwxyz") {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// thisMachine reports a fact of host: its scope is machines/<host>, or it
// applies to host.
func thisMachine(f *fact.Fact, host string) bool {
	if host == "" {
		return false
	}
	if h, ok := strings.CutPrefix(f.Meta.Scope, "machines/"); ok && strings.EqualFold(h, host) {
		return true
	}
	for _, h := range f.Meta.AppliesTo.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// paths are the paths f names in its description that do not exist under
// home: none for another machine's fact or a fact of no machine. The
// description, not the body: a body's paths are often context, examples or
// history.
func (g *goneProbe) paths(f *fact.Fact) []string {
	if out, ok := g.memo[f]; ok {
		return out
	}
	out := g.find(f)
	g.memo[f] = out
	return out
}

func (g *goneProbe) find(f *fact.Fact) []string {
	if g.home == "" || !thisMachine(f, g.host) {
		return nil
	}
	text := goneIe.ReplaceAllString(goneEg.ReplaceAllString(f.Meta.Description, "eg"), "ie")
	// The words around the paths, the paths blanked out: a path's own
	// name ("~/old/x") says nothing about whether it is there.
	prose := []byte(text)
	for _, m := range gonePathRx.FindAllStringSubmatchIndex(text, -1) {
		for i := m[2]; i < m[3]; i++ {
			prose[i] = ' '
		}
	}
	if goneRemote.MatchString(text) || (g.elsewhere != nil && g.elsewhere.MatchString(text)) || goneMade.MatchString(text) || goneEnv.MatchString(text) || goneSaid.Match(prose) {
		return nil
	}
	var out []string
	for _, m := range gonePathRx.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[2], m[3]
		raw := text[start:end]
		p := strings.TrimRight(raw, ".,;:)'\"`")
		end = start + len(p)
		rest := text[end:]
		if strings.HasSuffix(raw, "...") || strings.HasPrefix(rest, "...") || strings.HasPrefix(rest, "…") || strings.Contains(p, ",") {
			continue // a path cut short, or a list of them (a,b,c)
		}
		if start > 0 && text[start-1] == ':' {
			continue // host:/path, or one of a list of paths
		}
		if len(p) < 4 || strings.Contains(p, ".bak") || strings.Contains(strings.ToLower(p), "backup") || strings.HasSuffix(p, "-") || strings.HasSuffix(p, "_") {
			continue
		}
		// A glob or a placeholder right after the path, after its dots
		// (run.*, cfg.{json,yaml}, out.<ext>, run[0-9], run?.log, $VAR).
		if next := strings.TrimLeft(rest, "."); next != "" {
			if strings.IndexByte("*+<{$[%", next[0]) >= 0 || (next[0] == '?' && len(next) > 1 && next[1] != ' ' && next[1] != '\n') {
				continue
			}
		}
		if len(rest) > 2 && rest[0] == ' ' && rest[1] >= 'A' && rest[1] <= 'Z' && rest[2] >= 'a' && rest[2] <= 'z' {
			continue // a path with a space in it, cut at the space
		}
		if goneOutput.MatchString(text[max(0, start-40):start]) {
			continue
		}
		lo, hi := clauseStart(text[:start]), clauseEnd(text, end)
		if clause := prose[lo:hi]; goneNegation.Match(clause) || goneExample.Match(clause) {
			continue
		}
		if goneExampleEnd.MatchString(strings.TrimRight(text[:lo], " ,")) {
			continue // "eg, ~/x": the example marker ends the clause before
		}
		// A quoted path the regex cut short (at a space): the whole quote
		// is the path, looked up and named as such. In a code span the
		// whole span is a path only when it is there (it is most often a
		// command line, its first word the path).
		if q, code, ok := quoted(text, start, end); ok {
			if !code {
				p = q
			} else if !g.gone(q) {
				continue
			}
		}
		if !g.gone(p) || goneTemplate.MatchString(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// gone reports a path under home that does not exist. Another root's paths
// are not this machine's to look up.
func (g *goneProbe) gone(p string) bool {
	full := p
	if strings.HasPrefix(p, "~/") {
		full = filepath.Join(g.home, p[2:])
	}
	if full != g.home && !strings.HasPrefix(full, g.home+string(filepath.Separator)) {
		return false
	}
	_, err := os.Stat(full)
	return os.IsNotExist(err)
}

// quoted is the text from the path at text[start:end] to the quote that
// closes the one right before it, when the quote does not close at end;
// code says the quote is a code span's backtick.
func quoted(text string, start, end int) (q string, code, ok bool) {
	open, _ := utf8.DecodeLastRuneInString(text[:start])
	closer := map[rune]string{'\'': "'", '"': `"`, '`': "`", '“': "”", '‘': "’"}[open]
	if closer == "" || strings.HasPrefix(text[end:], closer) {
		return "", false, false
	}
	i := strings.Index(text[end:], closer)
	if i < 0 {
		return "", false, false
	}
	return text[start : end+i], open == '`', true
}

// clauseStart is where the clause that ends text begins: after its last
// comma, semicolon, newline, opening parenthesis or sentence end. A dot inside
// a word (a path, a file name) ends nothing.
func clauseStart(text string) int {
	for i := len(text) - 1; i >= 0; i-- {
		switch text[i] {
		case ',', ';', '\n', '(':
			return i + 1
		case '.', '?', '!':
			if i+1 < len(text) && text[i+1] == ' ' {
				return i + 1
			}
		}
	}
	return 0
}

// clauseEnd is where the clause that holds text[from] ends: at the next
// comma, semicolon, newline, closing parenthesis or sentence end.
func clauseEnd(text string, from int) int {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case ',', ';', '\n', ')':
			return i
		case '.', '?', '!':
			if i+1 == len(text) || text[i+1] == ' ' {
				return i
			}
		}
	}
	return len(text)
}
