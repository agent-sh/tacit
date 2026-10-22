// Package check verifies dry claims: things with one source of truth that
// code can read (SPEC.md, "Checked claims"). A repository's existence and
// its current name (every machine's catalog of checkouts, the remotes'
// normalized URLs, and the forge when one answers), a path in a checkout at
// HEAD, a version string against the checkout's tags and manifests. A dry
// claim is checked, never judged: the curator's model reads the results, and
// code refuses a decision that a result refutes.
//
// A Result is the one record of a check: what was asked (Kind, Claim,
// Inputs), which source answered (Source), what it said (Output, Value),
// the verdict on the claim (Holds, Fails, Unknown) and when (At). Results
// are plain values, written into a record's evidence and shown in prompts,
// so a later reader (a statement of the owner's, a review) can take one as
// an agreeing or disagreeing check without running it again.
//
// Nothing is guessed: a source that cannot answer (no forge configured, the
// network down, git unable to read the checkout) gives Unknown, and Unknown
// refutes nothing. URL reachability is not checked: an answer depends on the
// network, credentials and the moment, so it is no source of truth.
package check

import (
	"fmt"
	"strings"
	"time"
)

// Kinds of claim.
const (
	KindRepo    = "repo"    // a repository of this name exists (and is its current name)
	KindPath    = "path"    // a path exists in a checkout at HEAD
	KindVersion = "version" // a version string is a tag or a manifest's version
)

// Verdicts.
const (
	Holds   = "holds"   // the source confirms the claim
	Fails   = "fails"   // the source refutes it
	Unknown = "unknown" // no source could answer
)

// Result is one check of one claim against one source (or, for a
// repository, the sources together: Summary).
type Result struct {
	Kind  string `json:"kind" yaml:"kind"`
	Claim string `json:"claim" yaml:"claim"`
	// Source names what answered: "checkouts", "remotes", "forge:github",
	// "git:HEAD", "git:tags", "manifest:<file>", or several joined by "+".
	Source string `json:"source" yaml:"source"`
	// Inputs are what the check was given (name, owner, repo, path,
	// version), sorted by key when rendered.
	Inputs map[string]string `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	// Output is what the source said, in a line.
	Output  string `json:"output" yaml:"output"`
	Verdict string `json:"verdict" yaml:"verdict"`
	// Value is the source's own value where it has one: a repository's
	// current name (owner/name), the manifest's version.
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
	// At is when the source answered (RFC 3339; a cached forge answer keeps
	// the time it was fetched).
	At string `json:"at" yaml:"at"`
}

// Agrees reports a result that confirms its claim.
func (r Result) Agrees() bool { return r.Verdict == Holds }

// Disagrees reports a result that refutes its claim.
func (r Result) Disagrees() bool { return r.Verdict == Fails }

// Line is the result in one line for a prompt or a refusal: claim, verdict,
// source and output. The time is left out, so the same answer renders the
// same line (prompt fingerprints stay stable).
func (r Result) Line() string {
	s := fmt.Sprintf("%s: %s (%s: %s)", r.Claim, r.Verdict, r.Source, r.Output)
	if r.Value != "" && !strings.Contains(r.Output, r.Value) {
		s += "; value " + r.Value
	}
	return s
}

// Valid reports problems with a result's form (lint, a record's evidence).
func (r Result) Valid() []string {
	var ps []string
	switch r.Kind {
	case KindRepo, KindPath, KindVersion, KindAccount:
	default:
		ps = append(ps, fmt.Sprintf("kind %q is not repo, path, version or account", r.Kind))
	}
	switch r.Verdict {
	case Holds, Fails, Unknown:
	default:
		ps = append(ps, fmt.Sprintf("verdict %q is not holds, fails or unknown", r.Verdict))
	}
	if strings.TrimSpace(r.Claim) == "" || strings.TrimSpace(r.Source) == "" {
		ps = append(ps, "claim and source are set")
	}
	if _, err := time.Parse(time.RFC3339, r.At); err != nil {
		ps = append(ps, fmt.Sprintf("at %q is not an RFC 3339 time", r.At))
	}
	return ps
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func inputs(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

// Lines renders results, one line each, in order.
func Lines(rs []Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Line())
	}
	return out
}

// Refutes reports whether the results of the fact that would stay (winner)
// are refuted where those of the fact it would replace (loser) hold: for
// some kind of claim (repositories and paths; versions are left out, since
// a planned version is no tag yet and code cannot tell plan from claim),
// every one of winner's checked claims of that kind fails and at least one
// of loser's holds. The results that show it are returned. A decision that
// keeps the refuted fact over the confirmed one is the misjudgement a check
// catches.
func Refutes(winner, loser []Result) (bool, []Result) {
	for _, k := range []string{KindRepo, KindPath} {
		var wf, lh []Result
		wAll := true
		for _, r := range winner {
			if r.Kind != k || r.Verdict == Unknown {
				continue
			}
			if r.Verdict == Fails {
				wf = append(wf, r)
			} else {
				wAll = false
			}
		}
		for _, r := range loser {
			if r.Kind == k && r.Verdict == Holds {
				lh = append(lh, r)
			}
		}
		if wAll && len(wf) > 0 && len(lh) > 0 {
			return true, append(wf, lh...)
		}
	}
	return false, nil
}
