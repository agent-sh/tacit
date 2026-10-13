package gitev

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// summarize is a timeline in a few plain sentences, for a reader who does not
// read git: what the facts name that is gone, and per repo how much changed
// since the facts were written and the latest change. It is built from the
// structured evidence (already redacted), never from the display lines.
// one: the timeline is about one fact, so the sentences say "it", not "they".
func summarize(evidence []RepoEvidence, repos []string, anchor time.Time, somewhere map[string]bool, one, incomplete bool, now time.Time) []string {
	they, these := "they name", "these facts name"
	if one {
		they, these = "it names", "this fact names"
	}
	if len(repos) == 0 {
		return []string{upper(these) + " no repo, so git has nothing to add."}
	}
	since := ""
	if !anchor.IsZero() {
		since = " since " + plainDay(anchor, now)
	}
	var out, quiet, unread []string
	reported := map[string]bool{}
	// Most changed first: the repo with the most to say leads.
	ev := append([]RepoEvidence{}, evidence...)
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].Matched+ev[i].ChangedN > ev[j].Matched+ev[j].ChangedN })
	for _, e := range ev {
		var gone []string
		for _, st := range e.State {
			if !st.Present && !st.Unknown && !st.Generic && !somewhere[st.Term] && !reported[st.Term] {
				reported[st.Term] = true
				gone = append(gone, plainTerm(st.Term))
			}
		}
		if len(gone) > 0 {
			verb := "is"
			if len(gone) > 1 {
				verb = "are"
			}
			out = append(out, fmt.Sprintf("%s %s gone from %s.", joinAnd(gone, 3), verb, e.Name))
		}
		if e.Head == "" {
			unread = append(unread, e.Name)
			continue
		}
		commits, n := e.Commits, e.Matched
		if len(commits) == 0 && e.ChangedN > 0 {
			commits, n = e.Changed, e.ChangedN
		}
		switch {
		case len(e.State) == 0 && n == 0:
			out = append(out, fmt.Sprintf("In %s, git found nothing %s.", e.Name, these))
		case n == 0:
			quiet = append(quiet, e.Name)
		default:
			count := fmt.Sprint(n)
			if e.Capped {
				count += "+"
			}
			noun := "commits"
			if n == 1 && !e.Capped {
				noun = "commit"
			}
			s := fmt.Sprintf("In %s, %s %s%s changed what %s", e.Name, count, noun, since, these)
			if len(commits) > 0 {
				c := latest(commits)
				s += fmt.Sprintf(", the latest on %s: “%s”", plainDay(c.At, now), strings.TrimRight(clip(c.Subject, 90), "."))
			}
			out = append(out, s+".")
		}
	}
	if len(quiet) > 0 {
		out = append(out, fmt.Sprintf("In %s, nothing %s changed%s.", joinAnd(quiet, 4), they, since))
	}
	if len(unread) > 0 {
		out = append(out, fmt.Sprintf("%s could not be read.", joinAnd(unread, 4)))
	}
	if incomplete {
		out = append(out, "Part of git could not be read, so this may be missing something.")
	}
	return out
}

// latest is the newest commit of a list.
func latest(cs []Commit) Commit {
	c := cs[0]
	for _, x := range cs[1:] {
		if x.At.After(c.At) {
			c = x
		}
	}
	return c
}

// plainDay is a date as a person says it: 3 Oct, with the year only when it is
// not this year.
func plainDay(t, now time.Time) string {
	if now.IsZero() || t.Year() != now.Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// plainTerm drops the glob a path term carries: **/PATCHES.md is PATCHES.md.
func plainTerm(s string) string {
	return strings.TrimPrefix(s, "**/")
}

func joinAnd(xs []string, max int) string {
	if len(xs) > max {
		return strings.Join(xs[:max], ", ") + fmt.Sprintf(" and %d more", len(xs)-max)
	}
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
