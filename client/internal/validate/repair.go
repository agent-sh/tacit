package validate

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// The repairs below are the ones the curator makes before it checks a
// model's entries (SPEC.md, Metadata checks): each reads a value code can
// make reliable, or drops one no delivery could match. None puts a guess of
// code's in place of a value: a repository name is taken only when the value
// spells a name the checks know, and a path or name that names nothing here
// is dropped, never replaced.

var (
	sshRemoteRx = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:`)
	urlPrefixRx = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/]*/?`)
)

// knownRepoName reports a name Repos accepts: a repository's own name or
// another name of a checkout here.
func (e Env) knownRepoName(n string) bool {
	return fact.IsSlug(n) && (e.KnownRepo(n) || e.Catalog.AliasOf(n) != "")
}

// RepoName reads a repository name the checks know from an
// applies_to.repos value: the value itself in lower case, or the last part
// of an owner/repo pair, a forge URL or SSH remote (".git" cut), or a path,
// when that part is such a name. ok is false when the value spells no known
// repository; the caller drops it then.
func (e Env) RepoName(v string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(v))
	if n == "" {
		return "", false
	}
	if e.knownRepoName(n) {
		return n, true
	}
	s := sshRemoteRx.ReplaceAllString(n, "")
	s = urlPrefixRx.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, `\`, "/")
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s != n && e.knownRepoName(s) {
		return s, true
	}
	return "", false
}

// RepairRepos repairs applies_to.repos: each value read by RepoName, the
// names it cannot read dropped, duplicates dropped. changed reports any
// difference from repos; dropped counts the values dropped for naming no
// known repository.
func (e Env) RepairRepos(repos []string) (out []string, changed bool, dropped int) {
	seen := map[string]bool{}
	for _, r := range repos {
		n, ok := e.RepoName(r)
		switch {
		case !ok:
			dropped++
			changed = true
			continue
		case seen[n]:
			changed = true
			continue
		case n != r:
			changed = true
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, changed, dropped
}

// AbsolutePath reports a path Paths refuses on a fact that is not a
// machine's for being absolute (a leading /, ~, drive letter or UNC prefix).
func AbsolutePath(p string) bool { return absolute(p) }

// ownerLabelRx is a label that opens a sentence and claims the owner's
// authority for what follows: "Owner ruling:", "Owner rule (2026-10-12):",
// "Owner's rules:". It is what is cut; the statement after it is kept, as
// an agent's note.
var ownerLabelRx = regexp.MustCompile(`(?i)\bowner(?:'s|’s)?\s+(?:ruling|rulings|rule|rules)(?:\s*\([^()\n]{0,40}\))?\s*:\s*`)

// labelStartRx ends text after which a label opens a sentence: nothing, a
// sentence's end, a line break, or an opening quote or bracket.
var labelStartRx = regexp.MustCompile(`(?:^|[.!?;]\s+|\n\s*|^\s*["'“‘(\[]|[.!?;]\s+["'“‘(\[])\s*$`)

// StripOwnerLabels cuts each label that opens a sentence and claims the
// owner's authority ("Owner ruling: never push to main" becomes "Never push
// to main"), so the statement reads as what it is, an agent's note. It
// rewrites no other wording: "the owner said ..." or "the owner wants ..."
// has no neutral form code can write without changing what the text says,
// and stays for the model. The result can still claim the owner's authority;
// the caller checks it again (ClaimsOwner).
func StripOwnerLabels(s string) string {
	ms := ownerLabelRx.FindAllStringIndex(s, -1)
	if len(ms) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	// The rune after a cut label is capitalized when the next kept text is
	// written, never by moving the cursor: a label that follows right after
	// ("Owner ruling:\nowner rule: ...") must still start where it matched.
	capNext := false
	write := func(seg string) {
		if capNext && seg != "" {
			if r, size := utf8.DecodeRuneInString(seg); unicode.IsLower(r) {
				b.WriteRune(unicode.ToUpper(r))
				seg = seg[size:]
			}
			capNext = false
		}
		b.WriteString(seg)
	}
	for _, m := range ms {
		// A label right after one just cut opens the same sentence.
		if m[0] < last || !(capNext && m[0] == last) && !labelStartRx.MatchString(s[:m[0]]) {
			continue
		}
		write(s[last:m[0]])
		last = m[1]
		capNext = true
	}
	write(s[last:])
	return b.String()
}
