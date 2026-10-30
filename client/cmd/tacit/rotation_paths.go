package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A fact of this machine whose description names a path that is not here
// any more is most often stale: on the author's machine, 25 of a fresh
// sample of 30 such facts were (eval/VALUE.md, "Machine facts whose paths
// are gone"). The rotation takes them first. The rules below were each
// written for a kind of false flag the samples showed; no model is called.
var (
	// An absolute path under a system root, or one under ~; a path cut at
	// the first character no path in the samples held.
	gonePathRx = regexp.MustCompile(`(?:^|[^\w$/.~-])(~/[\w@%+=:,./-]+|/(?:home|data|opt|etc|srv|var|usr|mnt|Users)/[\w@%+=:,./-]+)`)
	// A placeholder or a pattern, not a path: <id>, {a,b}, *, $VAR, NN.
	goneTemplate = regexp.MustCompile(`[<>{}*?$\[\]]|\.\.\.|N{2,}|X{2,}|YYYY|MMDD|\bHH\b|[a-z]N(?:$|[-_./])`)
	// What comes before a path a command writes: an output, a scratch dir.
	goneOutput = regexp.MustCompile(`(?i)(?:-o|--out|--output|--target-dir|--local-dir|TMPDIR=|>|written to|writes to|go(?:es)? (?:to|in|into)|sav(?:e|ed|es) (?:to|in)|download(?:ed|s|ing)?(?: [\w.-]+)* to)\s*$`)
	// A fact about a path made on use: it need not exist between uses.
	goneMade = regexp.MustCompile(`(?i)mkdir -p|if missing|if absent|[A-Z_]{3,}=[~/]|created (?:on|at) |is created|gets created`)
	// In the path's clause: it says the path is not there, or is an example.
	goneNegation = regexp.MustCompile(`(?i)\b(?:no|not|never|removed|deleted|gone|obsolete|stale|absence|absent|missing|longer|retired|old|was|were|former|previous|instead)\b`)
	goneExample  = regexp.MustCompile(`(?i)\b(?:eg|for example|such as|like|say)\b`)
	// Anywhere in the description: the fact itself says something is not there.
	goneSaid = regexp.MustCompile(`(?i)\b(?:gone|deleted|removed|moved|no longer exists?|do(?:es)?(?: not|n't|nt) exist|obsolete)\b`)
	// The fact is about another machine (its ~ is that machine's home).
	goneElsewhere = regexp.MustCompile(`(?i)\bon (?:the )?(?:wrx|box|spark|sparks|b23d|cb33|mac|vm|vast|nebius|verda|server|remote)\b|\bvia ssh\b|\bssh (?:wrx|box|spark)|^(?:wrx|spark[\w-]*|box|b23d|cb33|mac)\b:|\bssh\b[^;.]*\w@[\w.-]+|\bssh (?:-\S+ )*[\w-]*(?:spark|wrx|box|b23d|cb33)|\b[\w-]+:~/`)
)

// gonePaths are the paths a fact of this machine names in its description
// that do not exist under home: none for another machine's fact or a fact
// of no machine. The description, not the body: a body's paths are often
// context, examples or history.
func gonePaths(f *fact.Fact, host, home string) []string {
	if host == "" || home == "" || !(f.Meta.Scope == "machines/"+host || slices.Contains(f.Meta.AppliesTo.Hosts, host)) {
		return nil
	}
	text := strings.ReplaceAll(strings.ReplaceAll(f.Meta.Description, "e.g.", "eg"), "i.e.", "ie")
	// The words around the paths, the paths blanked out: a path's own
	// name ("~/old/x") says nothing about whether it is there.
	prose := []byte(text)
	for _, m := range gonePathRx.FindAllStringSubmatchIndex(text, -1) {
		for i := m[2]; i < m[3]; i++ {
			prose[i] = ' '
		}
	}
	if goneElsewhere.MatchString(text) || goneMade.MatchString(text) || goneSaid.Match(prose) {
		return nil
	}
	var out []string
	for _, m := range gonePathRx.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[2], m[3]
		raw := text[start:end]
		p := strings.TrimRight(raw, ".,;:)'\"`")
		end = start + len(p)
		if strings.HasSuffix(raw, "...") || strings.HasPrefix(text[end:], "...") || strings.Contains(p, ",") {
			continue // a path cut short, or a list of them (a,b,c)
		}
		if start > 0 && strings.IndexByte("'\"", text[start-1]) >= 0 && !strings.HasPrefix(text[end:], text[start-1:start]) {
			continue // a quoted path that goes on past a space
		}
		if len(p) < 4 || goneTemplate.MatchString(p) || strings.Contains(p, ".bak") || strings.Contains(strings.ToLower(p), "backup") || strings.HasSuffix(p, "-") || strings.HasSuffix(p, "_") {
			continue
		}
		if end < len(text) {
			if next := text[end]; strings.IndexByte("*+<{$", next) >= 0 {
				continue
			}
			if rest := text[end:]; len(rest) > 2 && rest[0] == ' ' && rest[1] >= 'A' && rest[1] <= 'Z' && rest[2] >= 'a' && rest[2] <= 'z' {
				continue // a path with a space in it, cut at the space
			}
		}
		if goneOutput.MatchString(text[max(0, start-40):start]) {
			continue
		}
		lo := clauseStart(text[:start])
		hi := len(text)
		if i := strings.IndexAny(text[end:], ",;\n)"); i >= 0 {
			hi = end + i
		}
		if clause := prose[lo:hi]; goneNegation.Match(clause) || goneExample.Match(clause) {
			continue
		}
		full := p
		if strings.HasPrefix(p, "~/") {
			full = filepath.Join(home, p[2:])
		}
		if full != home && !strings.HasPrefix(full, home+string(filepath.Separator)) {
			continue // another root's paths are not this machine's to stat
		}
		if _, err := os.Stat(full); os.IsNotExist(err) {
			out = append(out, p)
		}
	}
	return out
}

// clauseStart is where the clause that ends text begins: after its last
// comma, semicolon, newline, opening parenthesis or sentence end. A dot inside
// a word (a path, a file name) ends nothing.
func clauseStart(text string) int {
	for i := len(text) - 1; i >= 0; i-- {
		switch text[i] {
		case ',', ';', '\n', '(':
			return i + 1
		case '.':
			if i+1 < len(text) && text[i+1] == ' ' {
				return i + 1
			}
		}
	}
	return 0
}
