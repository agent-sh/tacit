package derive

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug makes a repo name usable inside a fact name.
func Slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// HotName and MapName are the fact names for a repo.
func HotName(repo string) string { return Slug(repo) + "-hot-spots" }
func MapName(repo string) string { return Slug(repo) + "-module-map" }

// Result is the facts built from one snapshot. Hot is nil when no file
// reached MinTouches; Map is nil when the tree has fewer than two entries
// (a one-line map restates `ls`).
type Result struct {
	Sha, Date string
	Hot, Map  *fact.Fact
}

// Facts builds the derived facts of a snapshot. host is the curator's host,
// written to source.host. Dates are the commit's, so the output is a
// function of the commit and nothing else.
func Facts(ctx context.Context, s *Snapshot, host string) *Result {
	res := &Result{Sha: s.Sha, Date: s.Date}
	if hot := HotSpots(s.Window); len(hot) > 0 {
		res.Hot = hotFact(s, hot, host)
	}
	m := ModuleMap(s.Tree, s.Window)
	if len(m.Entries) >= 2 {
		for i := range m.Entries {
			if !m.Entries[i].Own {
				m.Entries[i].Last = s.lastChange(ctx, strings.TrimSuffix(m.Entries[i].Path, "/"))
			}
		}
		res.Map = mapFact(s, m, host)
	}
	return res
}

func newFact(s *Snapshot, host, name, typ, desc, body string, ap fact.AppliesTo) *fact.Fact {
	return &fact.Fact{
		Path: "derived/" + s.Repo + "/" + name + ".md",
		Meta: fact.Meta{
			Name:        name,
			Description: fact.ClipDescription(desc, 200),
			Type:        typ,
			Scope:       "derived/" + s.Repo,
			AppliesTo:   ap,
			Source:      fact.Source{Harness: "curator", Host: host, Kind: "derived"},
			Created:     s.Date,
			Updated:     s.Date,
			DerivedFrom: s.Sha,
		},
		Body: body,
	}
}

// where says which ref the facts were read at, so a reader knows whether it
// is the remote default branch, a branch the config names, or whatever the
// checkout had.
func (s *Snapshot) where() string {
	switch {
	case s.Ref == "HEAD" && s.OddBranch:
		return "the checkout's HEAD (branch name not shown; no remote default branch found)"
	case s.Ref == "HEAD" && s.Branch == "":
		return "the checkout's HEAD (detached; no remote default branch found)"
	case s.Ref == "HEAD":
		return fmt.Sprintf("the checkout's HEAD (branch `%s`; no remote default branch found)", s.Branch)
	case s.Local:
		return fmt.Sprintf("local branch `%s` (the branch configured for this repo; the remote has none)", s.Ref)
	case s.Configured:
		return fmt.Sprintf("`%s` (the branch configured for this repo)", s.Ref)
	}
	return "`" + s.Ref + "`"
}

func bulk(win []Commit) int {
	n := 0
	for _, c := range win {
		if c.Total > MaxChangeset {
			n++
		}
	}
	return n
}

func hotFact(s *Snapshot, hot []Hot, host string) *fact.Fact {
	var b strings.Builder
	fmt.Fprintf(&b, "Most-changed code files in `%s`, counted over its last %d code-touching commits on %s, read at commit %s of %s.", s.Repo, len(s.Window), s.where(), s.Sha, s.Date)
	b.WriteString(" Merges and commits that touched more than ")
	fmt.Fprintf(&b, "%d code files are not counted", MaxChangeset)
	if n := bulk(s.Window); n > 0 {
		fmt.Fprintf(&b, " (%d of the %d)", n, len(s.Window))
	}
	b.WriteString(".\n\n")
	var desc []string
	var paths []string
	for _, h := range hot {
		fmt.Fprintf(&b, "- %s: %d commits, last %s.", h.Path, h.Touches, h.Last)
		if len(h.With) > 0 {
			var ps []string
			for _, p := range h.With {
				ps = append(ps, fmt.Sprintf("%s (%d)", p.Path, p.Co))
			}
			b.WriteString(" Changes with " + strings.Join(ps, ", ") + ".")
		}
		b.WriteString("\n")
		desc = append(desc, fmt.Sprintf("%s %d", h.Path, h.Touches))
		paths = append(paths, h.Path)
	}
	if s.OmittedTouched > 0 {
		fmt.Fprintf(&b, "\n%d files the window touched have names outside the safe character set and are not shown.\n", s.OmittedTouched)
	}
	fmt.Fprintf(&b, "\nA partner is a file that was in the same commit at least %d times and in at least half of this file's commits; the number is how many commits. Data, logs, lockfiles and prose are not counted, and a renamed file counts from its rename.\n\n**How to apply**: before changing one of these files, read its partners; parallel work collides here most.\n", MinCoChanges)
	return newFact(s, host, HotName(s.Repo), "hot_spot",
		fitList(fmt.Sprintf("Hot spots in %s, last %d code commits (path, commits): ", s.Repo, len(s.Window)), desc),
		b.String(), fact.AppliesTo{Repos: []string{s.Repo}, Paths: paths})
}

func mapFact(s *Snapshot, m Map, host string) *fact.Fact {
	var b strings.Builder
	fmt.Fprintf(&b, "Layout of `%s` on %s, read at commit %s of %s: %d tracked files", s.Repo, s.where(), s.Sha, s.Date, len(s.Tree)+s.Omitted)
	if s.Omitted > 0 {
		fmt.Fprintf(&b, ", %d of them with names outside the safe character set, not listed or counted below", s.Omitted)
	}
	fmt.Fprintf(&b, ". Each line gives the files (code files), the most common extensions, how many of the last %d code-touching commits touched code in the directory, and the date of its last change.\n\n", len(s.Window))
	var active []Entry
	for _, e := range m.Entries {
		if e.Own {
			fmt.Fprintf(&b, "- %s (files directly in it) - %s (%d code): %s\n", e.Path, nFiles(e.Files), e.Code, extList(e.Exts))
			continue
		}
		fmt.Fprintf(&b, "- %s - %s (%d code): %s; commits in window %d", e.Path, nFiles(e.Files), e.Code, extList(e.Exts), e.Commits)
		if e.Last != "" {
			b.WriteString("; last change " + e.Last)
		}
		b.WriteString("\n")
		active = append(active, e)
	}
	if m.More > 0 {
		fmt.Fprintf(&b, "\n%d smaller directories are not listed (%d files).\n", m.More, m.MoreFiles)
	}
	if n := len(m.RootFiles); n > 0 {
		shown := m.RootFiles
		more := ""
		if len(shown) > 12 {
			more = fmt.Sprintf(", and %d more", len(shown)-12)
			shown = shown[:12]
		}
		fmt.Fprintf(&b, "\nRoot files (%d): %s%s.\n", n, strings.Join(shown, ", "), more)
	}
	// The description names the directories where the work is: most commits
	// in the window first, then most code.
	slices.SortStableFunc(active, func(a, b Entry) int {
		if a.Commits != b.Commits {
			return b.Commits - a.Commits
		}
		if a.Code != b.Code {
			return b.Code - a.Code
		}
		return strings.Compare(a.Path, b.Path)
	})
	var names []string
	for _, e := range active {
		names = append(names, e.Path)
	}
	return newFact(s, host, MapName(s.Repo), "structure",
		fitList(fmt.Sprintf("Layout of %s, busiest first: ", s.Repo), names),
		b.String(), fact.AppliesTo{Repos: []string{s.Repo}})
}

// fitList is prefix plus as many whole items as fit in a 200-character
// description, so the line never ends in the middle of a path.
func fitList(prefix string, items []string) string {
	out := prefix
	for i, it := range items {
		sep := ", "
		if i == 0 {
			sep = ""
		}
		if len([]rune(out+sep+it)) > 200 {
			break
		}
		out += sep + it
	}
	return out
}

func nFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func extList(xs []ExtCount) string {
	var out []string
	for _, x := range xs {
		out = append(out, fmt.Sprintf("%s %d", x.Ext, x.N))
	}
	return strings.Join(out, ", ")
}
