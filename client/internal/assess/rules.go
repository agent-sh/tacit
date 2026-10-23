package assess

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/record"
)

// Checked is what Enforce reads besides the candidate and its decision: the
// check of a name (a suppression target's) and the record a folder has or
// gets in this run.
type Checked struct {
	Repo   func(name string) check.RepoCheck
	Record func(folder string) *record.Record
}

// sameGroup reports a folder among the names code proved are c's
// repository (Same, the forge's current name, the canonical name).
func sameGroup(c Candidate, folder string) bool {
	n, ok := strings.CutPrefix(folder, "projects/")
	if !ok {
		return false
	}
	for _, r := range c.Same {
		if strings.EqualFold(r.Value, n) {
			return true
		}
	}
	return c.Name != nil && c.Name.Renamed == n || n == c.Canonical
}

// Enforce is code's refusal of a decision a check refutes (the owner's
// rule: dry claims are checked, not judged). It returns the problems, each
// with the check output that refutes the decision; none when no check
// does. It never changes the decision.
func Enforce(c Candidate, d Decision, env Checked) []string {
	if c.Kind != "" { // a proposed area or topic: no repository name
		return nil
	}
	var ps []string
	name := c.Folder[strings.LastIndex(c.Folder, "/")+1:]
	into := strings.TrimSpace(d.SuppressedBy)
	// The name exists nowhere, or is an old name the forge renamed: no
	// status of its own under it.
	proposed := c.Proposed()
	if c.Absent() && into == "" && d.Status != record.StatusNeedsReview && !(d.Kind == KindNone && len(proposed) == 0) {
		what := "no checkout or other name of one on any machine, no remote, and the forge has no repository of that name"
		if c.Name.Renamed != "" {
			what = "the forge says it was renamed: it is " + c.Name.Forge + " now"
		}
		var cands []string
		for _, r := range proposed {
			cands = append(cands, "projects/"+r.Name)
		}
		if c.Name.Renamed != "" && !contains(cands, "projects/"+c.Name.Renamed) {
			cands = append([]string{"projects/" + c.Name.Renamed}, cands...)
		}
		var fix string
		if len(cands) > 0 {
			fix = "set suppressed_by to the real repository its facts belong to (code's candidates: " + strings.Join(cands, ", ") + "), or status needs-review"
		} else {
			fix = "no candidate is in the evidence: status needs-review, or kind none"
		}
		ps = append(ps, fmt.Sprintf("the name %s exists nowhere as a repository (%s; check: %s): it cannot be %s %s; %s", name, what, strings.Join(c.Name.Lines(), " | "), d.Kind, d.Status, fix))
	}
	// One repository, one live folder: a name code proved is the same
	// repository as a folder that is live (and not suppressed into this one)
	// folds into it, or waits for review.
	if env.Record != nil && len(c.Same) > 0 {
		winners := map[string]bool{}
		var lines []string
		for _, r := range c.Same {
			w := liveWinner(env.Record, "projects/"+r.Value)
			if w != "" && w != c.Folder {
				winners[w] = true
				lines = append(lines, r.Line())
			}
		}
		if len(winners) > 0 && !winners[into] && d.Status != record.StatusNeedsReview {
			var ws []string
			for w := range winners {
				ws = append(ws, w)
			}
			sort.Strings(ws)
			ps = append(ps, fmt.Sprintf("%s is the same repository as %s, which has a live record (check: %s): one repository has one live folder: set suppressed_by to %s, or status needs-review", name, strings.Join(ws, ", "), strings.Join(lines, " | "), ws[0]))
		}
	}
	// The canonical name among one repository's names is code's: the owner's
	// decision, else the main checkout's directory, else the forge's name. The
	// other names fold into it, never it into them.
	if canon := c.Canonical; canon != "" && env.Record != nil {
		why := strings.Join(c.CanonicalWhy, " | ")
		target := liveWinner(env.Record, "projects/"+canon)
		switch {
		case canon != name && d.Status != record.StatusNeedsReview && into != "projects/"+canon && (target == "" || into != target):
			ps = append(ps, fmt.Sprintf("%s is another name of %s, the canonical name of this repository (check: %s): set suppressed_by to projects/%s, or status needs-review", name, canon, why, canon))
		case canon == name && into != "" && sameGroup(c, into):
			ps = append(ps, fmt.Sprintf("%s is the canonical name of this repository (check: %s): the other names fold into it, never it into %s", name, why, into))
		}
	}
	// Find the ancestor: a folder related to others is presumed part of one;
	// it stands alone only on the independence evidence code found, cited.
	if len(c.Ancestors) > 0 && into == "" && strings.TrimSpace(d.OwnerQuote) == "" {
		var anc []string
		for _, an := range c.Ancestors {
			anc = append(anc, an.Folder)
		}
		keys := map[string]bool{}
		for _, in := range c.Independence {
			keys[in.Key] = true
		}
		cited := 0
		for _, k := range d.Independent {
			if keys[strings.TrimSpace(k)] {
				cited++
			}
		}
		switch {
		case len(c.Independence) == 0:
			ps = append(ps, fmt.Sprintf("%s is related to %s: it is presumed part of one of code's ancestor candidates (%s), and code found no evidence of an identity of its own (no own commits, no remote or forge repository of its own, no work in a folder of its own name), so it cannot be %s %s on its own: set suppressed_by to %s, or to another of them the evidence supports", name, anc[0], strings.Join(anc, ", "), d.Kind, d.Status, anc[0]))
		case cited == 0:
			var ks []string
			for _, in := range c.Independence {
				ks = append(ks, in.Key)
			}
			ps = append(ps, fmt.Sprintf("%s is related to %s (code's ancestor candidates: %s): it stands alone only on evidence of its own identity: cite the independence item(s) it rests on in independent (code's keys: %s), or set suppressed_by to %s, or to another of them the evidence supports", name, anc[0], strings.Join(anc, ", "), strings.Join(ks, ", "), anc[0]))
		}
	}
	// The other way: a related folder that is a repository of its own (its
	// own commits, and a remote or forge repository of its own that no
	// ancestor has) is another repository than its ancestors, a checked
	// fact: it does not fold into one of them (only into a folder code proved
	// is the same repository).
	if len(c.Ancestors) > 0 && into != "" && !sameGroup(c, into) && ownRepository(c) {
		for _, an := range c.Ancestors {
			if an.Folder == into {
				ps = append(ps, fmt.Sprintf("%s is a repository of its own (%s), not part of %s: it stands alone (cite those keys in independent), or folds only into a folder code proved is the same repository", name, strings.Join(independenceKeys(c), ", "), into))
				break
			}
		}
	}
	if strings.HasPrefix(into, "projects/") && env.Repo != nil {
		t := strings.TrimPrefix(into, "projects/")
		tc := env.Repo(t)
		tr := (*record.Record)(nil)
		if env.Record != nil {
			tr = env.Record(into)
		}
		switch {
		case tc.Renamed != "" && tc.Renamed == name:
			ps = append(ps, fmt.Sprintf("suppressed_by %s: the forge says %s was renamed to %s (check: %s): %s is the current name; fold %s into this folder, not this one into it", into, t, name, strings.Join(tc.Lines(), " | "), name, into))
		case tc.Renamed != "":
			ps = append(ps, fmt.Sprintf("suppressed_by %s: the forge says %s was renamed to %s (check: %s): fold into projects/%s", into, t, tc.Renamed, strings.Join(tc.Lines(), " | "), tc.Renamed))
		case tc.Verdict == check.Fails && (tr == nil || tr.Kind == record.KindRepo):
			ps = append(ps, fmt.Sprintf("suppressed_by %s: that name exists nowhere as a repository (check: %s): fold into a repository that exists", into, strings.Join(tc.Lines(), " | ")))
		}
	}
	return ps
}

// ownRepository reports independence evidence of a repository of its own:
// own commits, and a remote or forge repository of its own.
func ownRepository(c Candidate) bool {
	commits, remote := false, false
	for _, in := range c.Independence {
		switch {
		case in.Key == "commits":
			commits = true
		case strings.HasPrefix(in.Key, "remote:"), strings.HasPrefix(in.Key, "forge:"):
			remote = true
		}
	}
	return commits && remote
}

func independenceKeys(c Candidate) []string {
	var out []string
	for _, in := range c.Independence {
		out = append(out, in.Key)
	}
	return out
}

// liveWinner is the live folder that holds folder's content (folder itself,
// or the end of its suppression), "" when it has no record.
func liveWinner(get func(string) *record.Record, folder string) string {
	for i := 0; i < 8; i++ {
		r := get(folder)
		if r == nil {
			return ""
		}
		if r.SuppressedBy == "" {
			return folder
		}
		folder = r.SuppressedBy
	}
	return ""
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var digitsRx = regexp.MustCompile(`[0-9]+`)

// NamePattern is a folder name with every run of digits as "#": the folders
// one tool or task makes (kv-013-g1, kv-140-g2: kv-#-g#) share it. A name
// with no digit is its own pattern.
func NamePattern(name string) string {
	return digitsRx.ReplaceAllString(strings.ToLower(name), "#")
}

// Near reports name and other as near names (a typo, a suffix, a renamed
// word): an edit distance of at most a quarter of the shorter name (at
// least 2), one name the other's start up to a dash (sharing at least 6
// characters), or the same first word with the rest starting alike (five
// characters or more). It returns the distance.
func Near(name, other string) (int, bool) {
	a, b := strings.ToLower(name), strings.ToLower(other)
	if a == b || a == "" || b == "" {
		return 0, false
	}
	d := levenshtein(a, b)
	short := min(len(a), len(b))
	if d <= max(2, short/4) {
		return d, true
	}
	if short >= 6 && (strings.HasPrefix(b, a+"-") || strings.HasPrefix(a, b+"-")) {
		return d, true
	}
	// The same first word and a word grown or cut after it (reviewer,
	// review-bot): the rest shares a start of five characters or more.
	fa, ra, okA := strings.Cut(a, "-")
	fb, rb, okB := strings.Cut(b, "-")
	if okA && okB && fa == fb {
		n := 0
		for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
			n++
		}
		if n >= 5 {
			return d, true
		}
	}
	return d, false
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Shape is a candidate's evidence in a few words, for finding folders alike
// in a run: what the name check says, the first proposed repository, and
// whether a checkout or folder exists.
func (c Candidate) Shape() string {
	v := "-"
	if c.Name != nil {
		v = c.Name.Verdict
	}
	real := "-"
	if p := c.Proposed(); len(p) > 0 {
		real = p[0].Name
	}
	return fmt.Sprintf("name %s, absent %v, belongs to %s", v, c.Absent(), real)
}

// Fingerprint is a hash of the evidence the model reads of a candidate:
// the same fingerprint is the same question (a refused folder, or one left
// for review, waits until it changes). Why it was asked and the record it
// had are left out, and so is what moves with nothing about the folder
// changed: the owner's newest turns that mention it (a history that grows
// with every session; their words reach the quote ledger, whose lines
// count), of code's checks and proposals the wording (which owners the
// forge was asked under, how many facts name a candidate) where only the
// verdicts and the names matter, and of the proposals, all but the names
// stable evidence gives (ProposedKey: not the ones only the live file system
// makes, not the cut to the best, not the checks of today's checkouts).
func Fingerprint(c Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n%s\n", c.Folder, c.Kind, strings.Join(c.Sessions, " "))
	for _, e := range c.Evidence {
		if l := evidenceLine(e); l != "" {
			b.WriteString("evidence " + l + "\n")
		}
	}
	if c.Name != nil {
		fmt.Fprintf(&b, "name %s %s %s\n", c.Name.Verdict, c.Name.Renamed, c.Name.Forge)
		for _, r := range c.Name.Results {
			fmt.Fprintf(&b, "check %s %s %s\n", r.Source, r.Verdict, r.Value)
		}
	}
	for _, r := range c.Same {
		b.WriteString("same " + r.Value + "\n")
	}
	if c.Canonical != "" {
		b.WriteString("canonical " + c.Canonical + "\n")
	}
	names := append([]string(nil), c.ProposedKey...)
	for _, r := range c.Alike {
		names = append(names, "alike:"+r.Name)
	}
	sort.Strings(names)
	b.WriteString("proposed " + strings.Join(names, " ") + "\n")
	for _, an := range c.Ancestors {
		fmt.Fprintf(&b, "ancestor %s %s\n", an.Folder, strings.Join(an.Sources, "+"))
	}
	for _, in := range c.Independence {
		b.WriteString("independence " + in.Key + "\n")
	}
	if c.NoEvidence {
		b.WriteString("no evidence\n")
	}
	for _, l := range c.Context {
		if !strings.HasPrefix(l, "owner's turn (") {
			b.WriteString(l + "\n")
		}
	}
	for _, l := range c.Similar {
		b.WriteString(l + "\n")
	}
	// What a re-attempt gathered, by its items: a search that found nothing
	// new leaves the fingerprint as it was.
	for _, k := range c.ExtraKeys() {
		b.WriteString("extra " + k + "\n")
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:12])
}

// SimilarLines summarizes the decisions already made on folders alike: by
// outcome, how many, and up to three examples with their why.
func SimilarLines(what string, recs []*record.Record) []string {
	if len(recs) == 0 {
		return nil
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Folder < recs[j].Folder })
	type group struct {
		n   int
		ex  []string
		key string
	}
	groups := map[string]*group{}
	var keys []string
	for _, r := range recs {
		k := r.Kind + ", " + r.Status
		if r.SuppressedBy != "" {
			k = "suppressed into " + r.SuppressedBy + " (" + k + ")"
		}
		g := groups[k]
		if g == nil {
			g = &group{key: k}
			groups[k] = g
			keys = append(keys, k)
		}
		g.n++
		if len(g.ex) < 3 && len(r.Was) > 0 {
			g.ex = append(g.ex, fmt.Sprintf("%s: %s", r.Folder, oneLine(r.Was[0].Why, 200)))
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return groups[keys[i]].n > groups[keys[j]].n })
	out := []string{fmt.Sprintf("decided before on %d folder(s) alike (%s):", len(recs), what)}
	for _, k := range keys {
		g := groups[k]
		out = append(out, fmt.Sprintf("  - %d %s; e.g. %s", g.n, g.key, strings.Join(g.ex, " | ")))
	}
	return out
}

// NoEvidenceOutcome is code's outcome for a folder nothing says anything of
// (Candidate.NoEvidence) when d is not a plain needs-review and rests on no
// evidence code showed: the owner's words (owner_quote), or a note:<name>
// the folder's rendered evidence (shown) lists, as [[name]] or note:name. ok is false when
// d stands as it is.
func NoEvidenceOutcome(c Candidate, d Decision, shown string) (Decision, bool) {
	if !c.NoEvidence || c.Kind != "" || len(c.Proposed()) > 0 {
		return d, false
	}
	if d.Status == record.StatusNeedsReview && strings.TrimSpace(d.SuppressedBy) == "" && d.Kind != KindNone {
		return d, false
	}
	if strings.TrimSpace(d.OwnerQuote) != "" {
		return d, false
	}
	for _, cite := range record.Cites(d.Why) {
		if n, ok := strings.CutPrefix(cite, "note:"); ok && n != "" && (strings.Contains(shown, "[["+n+"]]") || record.WholeWord(shown, "note:"+n)) {
			return d, false
		}
	}
	kind := record.KindFolder
	if c.GitRoot {
		kind = record.KindRepo
	}
	was := d.Kind + " " + d.Status
	if into := strings.TrimSpace(d.SuppressedBy); into != "" {
		was += ", suppressed by " + into
	}
	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		desc = "A folder whose evidence does not yet say what it is."
	}
	return Decision{Folder: c.Folder, Kind: kind, Status: record.StatusNeedsReview, Description: desc,
		Why: "code: no ancestor candidate, no candidate real repository, no identity of its own (no own commits, no remote or forge repository of its own, no work in a folder of its name) and no session worked in it or filed its facts: the evidence does not say what it is or whether it is worked, so it waits for review and is asked again with wider evidence (the answer " + was + " cited no evidence code showed)"}, true
}
