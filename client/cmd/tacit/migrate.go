package main

// The migrate step (SPEC.md, "Suppression"): the facts of a suppressed
// folder move to the folder that replaces it, file by file, by code. Each
// live fact gets a copy in the winner's folder (a new name: names are
// unique store-wide, and the old fact points at the copy), with its dates,
// source, text and applies_to kept and the loser's names in applies_to.repos
// mapped to the winner's; a copy that only restates a live fact of the
// winner (curate.Absorber) is not written, and the old fact points at that
// fact instead. The old fact gets superseded_by and updated, nothing else
// (fact.PatchRelations): never deleted. The owner's rulings and facts that
// hold their words move the same way, their quote ledger lines copied for
// the new name; one is held for the owner, with the reason, only when code
// can state a reason (an open contradiction with a fact of the winner, an
// endorsement bound to the old name, evidence the store does not back).
// tacit rescope --plan FILE moves any fact the same way into the scope a
// plan names.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// Bounds of one migration run: facts per commit, commits per run.
const (
	migrateBatch   = 200
	migrateCommits = 20
)

// moveItem is one fact to move: its name and the scope it goes to, with
// the names its applies_to.repos should read as the winner's.
type moveItem struct {
	Name    string
	To      string
	Renames map[string]string
	Folder  string // the suppressed folder it leaves, "" for a rescope
}

// moveResult is a planned batch.
type moveResult struct {
	files                 map[string][]byte // rel (to the root written) -> bytes
	ledger                []byte            // quote ledger lines to append
	asks                  []folderEntry     // questions for the owner to append
	pairs                 []string          // "old -> new" or "old -> absorbed by x", for the message
	moved, absorbed, held int
}

// planMoves plans up to max items over the facts of the root at dir
// (facts' paths relative to it after prefix): copies, pointers, ledger
// lines and asks. open are the folder log's open question ids, quotes the
// root's quote ledger.
func planMoves(dir, prefix string, facts []*fact.Fact, items []moveItem, quotes []byte, open map[string]bool, today string, max int) moveResult {
	res := moveResult{files: map[string][]byte{}}
	byName := map[string]*fact.Fact{}
	taken := map[string]bool{}
	var curated []*fact.Fact
	for _, f := range facts {
		taken[f.Meta.Name] = true
		if !f.InInbox() && !overlay.IsInbox(f.Path) {
			if byName[f.Meta.Name] == nil {
				byName[f.Meta.Name] = f
			}
			curated = append(curated, f)
		}
	}
	absorber := curate.NewAbsorber(curated)
	ql := ledger.Quotes(quotes)
	liveIn := func(scope string) map[string]*fact.Fact {
		out := map[string]*fact.Fact{}
		for _, f := range curated {
			if f.Meta.Scope == scope && f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
				out[f.Meta.Name] = f
			}
		}
		return out
	}
	targets := map[string]map[string]*fact.Fact{}
	n := 0
	for _, it := range items {
		if n >= max {
			break
		}
		f := byName[it.Name]
		if f == nil || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || f.Meta.Scope == it.To || strings.HasPrefix(f.Meta.Scope, "derived/") {
			continue
		}
		if targets[it.To] == nil {
			targets[it.To] = liveIn(it.To)
		}
		ruling, words := fact.IsRuling(f), fact.OwnerWordsOf(f) != ""
		ask := func(reason string) {
			id := folderID("ask", f.Meta.Name, it.To)
			if open[id] {
				return
			}
			open[id] = true
			res.asks = append(res.asks, folderEntry{Kind: "ask", ID: id, At: today, Folder: f.Meta.Scope, Fact: f.Meta.Name, To: it.To, Reason: reason})
			res.held++
		}
		if ruling || words {
			if reason := holdReason(f, targets[it.To]); reason != "" {
				ask(reason)
				continue
			}
		} else if f.Meta.Source.Kind == "user" && f.Audited && !fact.HasEvidence(f) {
			ask("kind user without the owner's words: lint already flags it, and a copy would carry the claim")
			continue
		}
		n++
		c := &fact.Fact{Meta: f.Meta, Body: f.Body, Audited: f.Audited}
		c.Meta.Scope = it.To
		c.Meta.SupersededBy, c.Meta.Retired, c.Meta.LastChecked = "", "", ""
		c.Meta.Origin = f.Meta.Name
		c.Meta.AppliesTo.Repos = renameRepos(f.Meta.AppliesTo.Repos, it.Renames)
		src := &c.Meta.Source
		var lines []byte
		// Evidence moves only with its ledger line; a claim the store does not
		// back is left on the old fact, not copied.
		if src.OwnerQuote != "" {
			if q, ok := ql[ledger.Quote{Name: f.Meta.Name, Quote: src.OwnerQuote, From: src.OwnerQuoteFrom}.Key()]; ok && f.QuoteBacked {
				lines = append(lines, quoteLine(q, "")...)
			} else {
				src.OwnerQuote, src.OwnerQuoteFrom = "", ""
			}
		}
		if src.OwnerWords != "" {
			if q, ok := ql[ledger.Quote{Name: f.Meta.Name, Quote: src.OwnerWords, From: src.OwnerWordsFrom, Words: true}.Key()]; ok && f.WordsRecorded {
				lines = append(lines, quoteLine(q, "")...)
			} else {
				src.OwnerWords, src.OwnerWordsFrom = "", ""
			}
		}
		if src.Endorsed != "" {
			src.Endorsed = "" // bound to the old name; a ruling with only an endorsement is held above
		}
		c.QuoteBacked, c.WordsBacked, c.WordsRecorded = f.QuoteBacked && src.OwnerQuote != "", f.WordsBacked && src.OwnerWords != "", f.WordsRecorded && src.OwnerWords != ""
		c.Meta.Name = "" // set below; the absorber compares text and scope
		c.Path = it.To + "/x.md"
		by := absorber.Restates(c, ruling)
		old := *f
		old.Meta.Updated = today
		if by != "" {
			old.Meta.SupersededBy = by
			res.absorbed++
			res.pairs = append(res.pairs, f.Meta.Name+" -> absorbed by "+by)
		} else {
			name := copyName(f.Meta.Name, it.To, taken)
			taken[name] = true
			c.Meta.Name = name
			c.Path = it.To + "/" + name + ".md"
			for _, l := range bytes.Split(bytes.TrimSpace(lines), []byte("\n")) {
				var q ledger.Quote
				if json.Unmarshal(l, &q) == nil {
					res.ledger = append(res.ledger, quoteLine(q, name)...)
				}
			}
			b, err := c.Encode()
			if err != nil {
				continue
			}
			res.files[c.Path] = b
			absorber.Add(c, ruling)
			targets[it.To][name] = c
			old.Meta.SupersededBy = name
			res.moved++
			res.pairs = append(res.pairs, f.Meta.Name+" -> "+name)
		}
		rel := strings.TrimPrefix(f.Path, prefix)
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		patched, err := fact.PatchRelations(data, &old)
		if err != nil {
			continue
		}
		res.files[rel] = patched
	}
	return res
}

// holdReason is why a ruling or a fact with the owner's words waits for the
// owner instead of moving: a reason code can state, "" for none.
func holdReason(f *fact.Fact, target map[string]*fact.Fact) string {
	for _, c := range f.Meta.ConflictsWith {
		if target[c] != nil {
			return fmt.Sprintf("it has an open contradiction with [[%s]] in the folder it would move to", c)
		}
	}
	names := make([]string, 0, len(target))
	for n := range target {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, c := range target[n].Meta.ConflictsWith {
			if c == f.Meta.Name {
				return fmt.Sprintf("[[%s]] in the folder it would move to contradicts it", n)
			}
		}
	}
	if f.Meta.Source.Endorsed != "" && f.Meta.Source.OwnerQuote == "" && !fact.OwnerWrote(f) {
		return "it is a ruling by the owner's endorsement, which names this fact: a copy under another name would not be theirs"
	}
	if fact.IsRuling(f) && !fact.OwnerWrote(f) && !f.QuoteBacked && f.Meta.Source.Endorsed == "" {
		return "its evidence is not in the store's quote ledger"
	}
	return ""
}

func quoteLine(q ledger.Quote, name string) []byte {
	if name != "" {
		q.Name = name
	}
	return q.Line()
}

// copyName is the copy's name: the old one with the winner's last part, a
// short hash when that is taken too.
func copyName(old, to string, taken map[string]bool) string {
	base := importer.Slug(old+"-"+path.Base(to), 80)
	if !taken[base] {
		return base
	}
	return importer.Slug(old, 70) + "-" + shortHash(old+"\x00"+to)
}

// renameRepos maps names of applies_to.repos (lower case) to the winner's,
// once each.
func renameRepos(repos []string, renames map[string]string) []string {
	if len(repos) == 0 {
		return repos
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range repos {
		n := r
		if to, ok := renames[strings.ToLower(r)]; ok {
			n = to
		}
		if !seen[strings.ToLower(n)] {
			seen[strings.ToLower(n)] = true
			out = append(out, n)
		}
	}
	return out
}

// migrationItems are the live facts of every suppressed folder, largest
// folder first, each with the scope of the live folder that replaces it.
func migrationItems(facts []*fact.Fact, recs map[string]*record.Record) []moveItem {
	byFolder := map[string][]*fact.Fact{}
	for _, f := range facts {
		if f.InInbox() || overlay.IsInbox(f.Path) || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		if r := recs[f.Meta.Scope]; r != nil && r.SuppressedBy != "" {
			byFolder[f.Meta.Scope] = append(byFolder[f.Meta.Scope], f)
		}
	}
	folders := make([]string, 0, len(byFolder))
	for f := range byFolder {
		folders = append(folders, f)
	}
	sort.Slice(folders, func(i, j int) bool {
		if len(byFolder[folders[i]]) != len(byFolder[folders[j]]) {
			return len(byFolder[folders[i]]) > len(byFolder[folders[j]])
		}
		return folders[i] < folders[j]
	})
	var out []moveItem
	for _, folder := range folders {
		winner, _, err := record.Resolve(recs, folder)
		if err != nil || recs[winner] == nil || !recs[winner].Live() {
			continue
		}
		renames := map[string]string{}
		wname := path.Base(winner)
		for _, n := range recs[folder].Names() {
			renames[strings.ToLower(n)] = wname
		}
		fs := byFolder[folder]
		sort.Slice(fs, func(i, j int) bool { return fs[i].Meta.Name < fs[j].Meta.Name })
		for _, f := range fs {
			out = append(out, moveItem{Name: f.Meta.Name, To: winner, Renames: renames, Folder: folder})
		}
	}
	return out
}

// migrateReport is one run's accounting.
type migrateReport struct {
	Commits, Moved, Absorbed, Held int
	Folders                        map[string][3]int // folder -> moved, absorbed, held
}

// runMigrate implements `tacit migrate [--dry-run] [--max N]`: the
// suppressed folders' facts move to their winners (a step of curate).
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "plan and print, write nothing")
	max := fs.Int("max", migrateBatch, "facts per commit")
	commits := fs.Int("commits", migrateCommits, "commits per run")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "migrate: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if ok, why := fact.FormatWrites(s.Dir); !ok {
		fmt.Fprintln(os.Stderr, "migrate: nothing done:", why)
		return 2
	}
	if !localLayer(ctx, s) && !*dry && refuseWriter(ctx, s, "curator", "migrate") {
		return 2
	}
	rep, err := migrateStep(ctx, s, *max, *commits, *dry, nil)
	fmt.Printf("migrate: %d moved, %d absorbed, %d held for the owner, %d commit(s)\n", rep.Moved, rep.Absorbed, rep.Held, rep.Commits)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	return 0
}

// curateMigrateStep is the full curate's migration; tests substitute it.
var curateMigrateStep = func(ctx context.Context, s *store.Store, dry bool) int {
	if ok, _ := fact.FormatWrites(s.Dir); !ok {
		return 0
	}
	if _, err := migrateStep(ctx, s, migrateBatch, migrateCommits, dry, nil); err != nil {
		fmt.Fprintln(os.Stderr, "curate: migrate:", err)
		return 1
	}
	return 0
}

// migrateStep moves the suppressed folders' facts (or, with plan set, the
// plan's items: tacit rescope) in commits of at most max facts, at most
// commits commits; with the local layer, in the overlay.
func migrateStep(ctx context.Context, s *store.Store, max, commits int, dry bool, plan []moveItem) (migrateReport, error) {
	rep := migrateReport{Folders: map[string][3]int{}}
	local := localLayer(ctx, s)
	for c := 0; c < commits; c++ {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		var res moveResult
		var err error
		if local {
			res, err = migrateLocal(s, max, dry, plan)
		} else if dry {
			res = migratePlan(s.Dir, max, plan)
		} else {
			_, err = curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
				res = migratePlan(s.Dir, max, plan)
				if len(res.files) == 0 && len(res.asks) == 0 {
					return nil, "", nil
				}
				return migrateFiles(s.Dir, res), migrateMessage(res, plan != nil), nil
			})
		}
		if err != nil {
			return rep, err
		}
		if len(res.files) == 0 && len(res.asks) == 0 {
			break
		}
		if dry {
			for _, p := range res.pairs {
				fmt.Println("- " + p)
			}
			for _, a := range res.asks {
				fmt.Printf("- held: %s (%s): %s\n", a.Fact, a.Folder, a.Reason)
			}
		}
		rep.Commits++
		rep.Moved += res.moved
		rep.Absorbed += res.absorbed
		rep.Held += res.held
		if dry {
			break // a dry run plans one batch: nothing moved, the next would be the same
		}
	}
	return rep, nil
}

// migratePlan plans one batch over the store at dir.
func migratePlan(dir string, max int, plan []moveItem) moveResult {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return moveResult{}
	}
	items := plan
	if items == nil {
		recs, _ := record.Walk(dir)
		items = migrationItems(facts, recs)
	}
	quotes, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
	open := map[string]bool{}
	for _, a := range openFolderAsks(dir) {
		open[a.ID] = true
	}
	return planMoves(dir, "", facts, items, quotes, open, time.Now().Format("2006-01-02"), max)
}

// migrateFiles is a batch's files for the store: the facts, the ledger and
// the folder log with their lines appended.
func migrateFiles(dir string, res moveResult) map[string][]byte {
	files := map[string][]byte{}
	for k, v := range res.files {
		files[k] = v
	}
	if len(res.ledger) > 0 {
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
		files[ledger.QuotesPath] = append(append([]byte(nil), cur...), res.ledger...)
	}
	if len(res.asks) > 0 {
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.FoldersPath)))
		for _, a := range res.asks {
			cur = append(cur, a.line()...)
		}
		files[ledger.FoldersPath] = cur
	}
	return files
}

func migrateMessage(res moveResult, rescope bool) string {
	var subj string
	if rescope {
		subj = fmt.Sprintf("curator: rescope: %d fact(s) moved, %d absorbed, %d held", res.moved, res.absorbed, res.held)
	} else {
		from, to := migrateRoute(res)
		subj = fmt.Sprintf("curator: migrate: %s -> %s (%d fact(s): %d moved, %d absorbed, %d held)", from, to, res.moved+res.absorbed+res.held, res.moved, res.absorbed, res.held)
	}
	body := append([]string(nil), res.pairs...)
	for _, a := range res.asks {
		body = append(body, fmt.Sprintf("%s held for the owner: %s", a.Fact, a.Reason))
	}
	return subj + "\n\n" + strings.Join(body, "\n")
}

// migrateRoute names a batch's folders: the one loser and winner, or how
// many when a batch spans several.
func migrateRoute(res moveResult) (string, string) {
	from, to := map[string]bool{}, map[string]bool{}
	for rel, b := range res.files {
		f, err := fact.Parse(rel, b)
		if err != nil {
			continue
		}
		if f.Meta.SupersededBy != "" {
			from[f.Meta.Scope] = true
		} else {
			to[f.Meta.Scope] = true
		}
	}
	for _, a := range res.asks {
		from[a.Folder], to[a.To] = true, true
	}
	name := func(m map[string]bool) string {
		var xs []string
		for k := range m {
			xs = append(xs, k)
		}
		sort.Strings(xs)
		if len(xs) == 1 {
			return xs[0]
		}
		return fmt.Sprintf("%d folders", len(xs))
	}
	return name(from), name(to)
}

// migrateLocal is one batch in the local layer: the overlay's curated facts
// of folders suppressed in the store or the overlay move within the
// overlay, never the clone.
func migrateLocal(s *store.Store, max int, dry bool, plan []moveItem) (moveResult, error) {
	dir := overlay.Dir()
	if !overlay.Outside(dir, s.Dir) {
		return moveResult{}, overlay.ErrInside
	}
	all, _ := overlay.Load(dir)
	var facts []*fact.Fact
	for _, f := range all {
		if overlay.IsCurated(f.Path) {
			facts = append(facts, f)
		}
	}
	items := plan
	if items == nil {
		items = migrationItems(facts, storeRecords(s.Dir))
	}
	curated := filepath.Join(dir, filepath.FromSlash(overlay.CuratedDir))
	quotes, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.QuotesRel)))
	open := map[string]bool{}
	for _, a := range openFolderAsks(curated) {
		open[a.ID] = true
	}
	prefix := overlay.Prefix + overlay.CuratedDir + "/"
	res := planMoves(curated, prefix, facts, items, quotes, open, time.Now().Format("2006-01-02"), max)
	if dry || (len(res.files) == 0 && len(res.asks) == 0) {
		return res, nil
	}
	for _, rel := range sortedFileKeys(res.files) {
		if err := overlay.Write(dir, s.Dir, overlay.CuratedDir+"/"+rel, res.files[rel]); err != nil {
			return res, err
		}
	}
	if len(res.ledger) > 0 {
		if err := overlay.Append(dir, s.Dir, overlay.QuotesRel, res.ledger); err != nil {
			return res, err
		}
	}
	for _, a := range res.asks {
		if err := overlay.Append(dir, s.Dir, overlay.CuratedDir+"/"+ledger.FoldersPath, a.line()); err != nil {
			return res, err
		}
	}
	local, _ := overlay.Load(dir)
	return res, regenLocalIndexes(dir, s.Dir, local)
}

func sortedFileKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runRescope implements `tacit rescope --plan FILE [--dry-run]`: each line
// of FILE, a JSON object with a fact's "name" (or "path") and the scope it
// goes to ("to", or the second item of a "scope" pair of the pipeline
// audit's plans), moves the fact there as the migration does. A target
// scope must pass the metadata checks; a line that does not is skipped and
// counted.
func runRescope(args []string) int {
	fs := flag.NewFlagSet("rescope", flag.ExitOnError)
	planFile := fs.String("plan", "", "the plan, JSON lines")
	dry := fs.Bool("dry-run", false, "plan and print, write nothing")
	max := fs.Int("max", migrateBatch, "facts per commit")
	fs.Parse(args)
	if *planFile == "" {
		fmt.Fprintln(os.Stderr, "rescope: --plan FILE is required")
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "rescope: no store")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if !localLayer(ctx, s) && !*dry && refuseWriter(ctx, s, "curator", "rescope") {
		return 2
	}
	items, skipped, err := readRescopePlan(*planFile, curateEnv(s.Dir, localLayer(ctx, s)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "rescope:", err)
		return 2
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "rescope: %d line(s) name no scope the checks accept; skipped\n", skipped)
	}
	rep, err := migrateStep(ctx, s, *max, 1<<20, *dry, items)
	fmt.Printf("rescope: %d moved, %d absorbed, %d held for the owner, %d commit(s)\n", rep.Moved, rep.Absorbed, rep.Held, rep.Commits)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rescope:", err)
		return 1
	}
	return 0
}

// readRescopePlan reads a plan's moves, each target checked by env.
func readRescopePlan(file string, env validate.Env) ([]moveItem, int, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []moveItem
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var l struct {
			Name  string          `json:"name"`
			Path  string          `json:"path"`
			To    string          `json:"to"`
			Scope json.RawMessage `json:"scope"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		to := l.To
		if to == "" && len(l.Scope) > 0 {
			var pair []string
			if json.Unmarshal(l.Scope, &pair) == nil && len(pair) == 2 {
				to = pair[1]
			}
		}
		name := l.Name
		if name == "" && l.Path != "" {
			name = strings.TrimSuffix(path.Base(l.Path), ".md")
		}
		if name == "" || to == "" {
			continue
		}
		if w, ok := env.Redirect(to); ok {
			to = w
		}
		if p := env.Scope(to); p != nil || !fact.IsSlug(name) {
			skipped++
			continue
		}
		renames := map[string]string{}
		out = append(out, moveItem{Name: name, To: to, Renames: renames})
	}
	return out, skipped, sc.Err()
}
