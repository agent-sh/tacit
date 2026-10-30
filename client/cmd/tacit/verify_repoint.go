package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A pair finding whose fact was replaced by a third fact since it was raised
// cannot take the verifier's "keep the other": the review page refuses to
// retire a fact that already has a successor, by design
// (TestReviewRefusesToOverwriteAThirdPartySupersede). Every attempt failed
// and the finding stranded at the attempt limit. So before it sends pairs,
// tacit verify moves such a finding to the live facts that replaced its
// sides, each side's superseded_by chain followed to its end, as a new
// finding the verifier then judges; and closes it as moot, with the reason,
// when a side has no live successor or both sides now lead to one fact
// (SPEC.md, "Attempts").

// mootMark is the curator's record that closes such a finding. It changes no
// fact; Repointed names the finding that took its place, if any.
const mootMark = "moot"

// replacedSide reports a pair one of whose facts is superseded by a fact
// that is not the pair's other fact. One superseded by the other is the
// pair's own answer (a supersede the curator applied), not this.
func replacedSide(it reviewItem) bool {
	return (it.A.SupersededBy != "" && it.A.SupersededBy != it.B.Name) || (it.B.SupersededBy != "" && it.B.SupersededBy != it.A.Name)
}

// repointable is an open pair finding the verifier would send, with both
// facts in the store and one replaced by a third fact.
func repointable(it reviewItem, owned map[string]bool) bool {
	return it.Decision == nil && it.Ask == nil && !owned[it.ID] && !owned[it.A.Name] && !owned[it.B.Name] &&
		!it.A.Missing && !it.B.Missing && replacedSide(it)
}

// liveEnd follows name's superseded_by chain to its end. It returns the live
// fact there (name itself when nothing replaced it), or "" and why there is
// none: the chain ends in a retired fact, names a fact the store does not
// hold, or loops.
func (rs *reviewServer) liveEnd(name string) (string, string) {
	seen := map[string]bool{}
	chain := []string{}
	for cur := name; ; {
		if seen[cur] {
			return "", fmt.Sprintf("the supersede chain of %s loops (%s -> %s)", name, strings.Join(chain, " -> "), cur)
		}
		seen[cur] = true
		chain = append(chain, cur)
		f := rs.facts[cur]
		if f == nil {
			return "", fmt.Sprintf("%s, which replaces %s, is not in the store", cur, name)
		}
		if f.Meta.SupersededBy == "" {
			switch {
			case f.Meta.Retired != "" && cur == name:
				return "", fmt.Sprintf("%s is retired (%s) and no fact replaces it", name, f.Meta.Retired)
			case f.Meta.Retired != "":
				return "", fmt.Sprintf("%s is replaced by %s, which is retired (%s), and no live fact replaces it", name, via(chain), f.Meta.Retired)
			}
			return cur, ""
		}
		cur = f.Meta.SupersededBy
	}
}

// via names the chain after its first fact: "c", or "c through x".
func via(chain []string) string {
	if len(chain) < 2 {
		return ""
	}
	end := chain[len(chain)-1]
	if len(chain) == 2 {
		return end
	}
	return end + " through " + strings.Join(chain[1:len(chain)-1], ", ")
}

// repointPlan is what becomes of one finding: closed as moot for Why, and,
// when both its sides lead to live facts, those facts as the finding To.
type repointPlan struct {
	NewA, NewB string // the live facts in the finding's place; empty: none
	Kind       string // the new finding's kind
	To         string // the finding on NewA and NewB: the new one, or one already raised
	Line       string // the review line that raises To; empty when To was already raised
	Why        string // the reason the moot record carries
}

// planRepoint works out what becomes of it from the store as it is now.
func (rs *reviewServer) planRepoint(it reviewItem) repointPlan {
	a, whyA := rs.liveEnd(it.A.Name)
	b, whyB := rs.liveEnd(it.B.Name)
	switch {
	case a == "" || b == "":
		why := whyA
		if why == "" {
			why = whyB
		} else if whyB != "" {
			why += "; " + whyB
		}
		return repointPlan{Why: "moot: " + why}
	case a == b:
		return repointPlan{Why: fmt.Sprintf("moot: both facts now lead to %s (%s)", a, movedSides(it.A.Name, a, it.B.Name, b))}
	}
	p := repointPlan{NewA: a, NewB: b, Kind: repointKind(it.Kind, rs.facts[a] != nil && isRuling(rs.facts[a]))}
	for _, x := range rs.items() {
		if (x.A.Name == a && x.B.Name == b) || (x.A.Name == b && x.B.Name == a) {
			p.To = x.ID
			p.Why = fmt.Sprintf("re-pointed to %s / %s, already finding %s: %s", a, b, x.ID, movedSides(it.A.Name, a, it.B.Name, b))
			return p
		}
	}
	p.To = pairID(p.Kind, a, b)
	p.Why = fmt.Sprintf("re-pointed to %s / %s, finding %s: %s", a, b, p.To, movedSides(it.A.Name, a, it.B.Name, b))
	p.Line = reviewLine(p.Kind, a, b, rs.facts) + fmt.Sprintf("\n  re-pointed %s from finding %s (%s / %s): %s\n", time.Now().Format("2006-01-02"), it.ID, it.A.Name, it.B.Name, movedSides(it.A.Name, a, it.B.Name, b))
	return p
}

// movedSides says which side moved where: "a is replaced by c; b stands".
func movedSides(a, a2, b, b2 string) string {
	say := func(x, x2 string) string {
		if x == x2 {
			return x + " stands"
		}
		return x + " is replaced by " + x2
	}
	return say(a, a2) + "; " + say(b, b2)
}

// repointKind is the kind of the finding raised in a moved one's place. A
// supersede the curator applied is a proposal between the live facts, a
// duplicate in one run is a contradiction between facts of different runs,
// and a ruling change stays one while its side is a ruling.
func repointKind(kind string, aRuling bool) string {
	switch kind {
	case kindSupersedeDone:
		return kindSupersedeAsk
	case kindDuplicate:
		return kindContradiction
	case kindRulingChange:
		if !aRuling {
			return kindSupersedeAsk
		}
	}
	return kind
}

// reviewLine is the review file line parseReviewFiles reads as a finding of
// kind on a and b (A the old fact, B the one that may replace it).
func reviewLine(kind, a, b string, facts map[string]*fact.Fact) string {
	switch kind {
	case kindSupersedeAsk:
		return fmt.Sprintf("- supersedes? [[%s]] may replace [[%s]]; owner decides", b, a)
	case kindResultSup:
		return fmt.Sprintf("- result-superseded? [[%s]] may replace [[%s]]", b, a)
	case kindRulingChange:
		return fmt.Sprintf("- RULING CHANGE? new [[%s]] may supersede owner ruling [[%s]]", b, a)
	}
	scope := ""
	if f := facts[b]; f != nil {
		scope = " (" + f.Meta.Scope + ")"
	}
	return fmt.Sprintf("- CONTRADICTION?: [[%s]]%s vs [[%s]]", b, scope, a)
}

// applyMoot closes a finding as moot (op.Mark mootMark, by the curator) and,
// when its facts lead to live ones, raises the finding on those in today's
// review file, all planned again on the store as it is under the lock.
func (rs *reviewServer) applyMoot(o *op, it *reviewItem) (string, []string, error) {
	if o.By != "curator" {
		return "", nil, errors.New("only the curator closes a finding as moot")
	}
	if !repointable(*it, rs.ownerTouched()) {
		return "", nil, errors.New("this finding is not open with a fact replaced by a third one")
	}
	p := rs.planRepoint(*it)
	files := map[string][]byte{}
	if p.Line != "" {
		rel := "review/" + time.Now().Format("2006-01-02") + ".md"
		old, err := os.ReadFile(filepath.Join(rs.s.Dir, rel))
		if err != nil && !os.IsNotExist(err) {
			return "", nil, err
		}
		if len(old) > 0 && old[len(old)-1] != '\n' {
			old = append(old, '\n')
		}
		files[rel] = append(old, p.Line...)
	}
	d := decision{ID: o.ID, Action: mootMark, A: it.A.Name, B: it.B.Name, At: time.Now().Format(time.RFC3339), By: "curator", Evidence: p.Why}
	if p.NewA != "" {
		d.Repointed = p.To
	}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, p.Why, files)
}

// repoint closes, as moot, every open pair finding with a fact replaced by a
// third fact, and raises the finding on the live facts in its place, in one
// commit before the run sends anything (SPEC.md, "Attempts"). A replay
// (--ids) moves only the findings it names. A dry run prints the plan.
func (r *verifyRun) repoint(ctx context.Context, rs *reviewServer, owned map[string]bool) error {
	named := map[string]bool{}
	for _, id := range r.only {
		named[id] = true
	}
	var ops []op
	var plans []repointPlan
	for _, it := range rs.items() {
		if !repointable(it, owned) || (len(named) > 0 && !named[it.ID]) {
			continue
		}
		ops = append(ops, op{ID: it.ID, Mark: mootMark, By: "curator", A: it.A.Name, B: it.B.Name})
		plans = append(plans, rs.planRepoint(it))
	}
	if len(ops) == 0 {
		return nil
	}
	if r.dry {
		for i, o := range ops {
			fmt.Printf("verify: would close %s (%s / %s): %s\n", o.ID, o.A, o.B, plans[i].Why)
		}
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	notes, errs, err := rs.applyOps(actx, ops)
	moved, moot := 0, 0
	for i, o := range ops {
		if errs[i] != nil {
			fmt.Fprintf(os.Stderr, "verify: %s (%s / %s) not closed: %v\n", o.ID, o.A, o.B, errs[i])
			continue
		}
		if strings.HasPrefix(notes[i], "re-pointed") {
			moved++
		} else {
			moot++
		}
		fmt.Fprintf(os.Stderr, "verify: closed %s (%s / %s): %s\n", o.ID, o.A, o.B, notes[i])
	}
	if moved+moot > 0 {
		fmt.Fprintf(os.Stderr, "verify: %d finding(s) with a fact replaced by a third one: %d re-pointed to the live facts, %d moot\n", moved+moot, moved, moot)
	}
	return err
}
