package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runResolve is the owner's answer to a review item:
//
//	tacit resolve supersede <old-name> <new-name>   old gets superseded_by new
//	tacit resolve conflict  <a-name> <b-name>       both get conflicts_with (one side is dropped at delivery)
//	tacit resolve clear     <name>                  remove superseded_by and conflicts_with from a fact
//
// Commits and pushes as curator. Only names in the curated tree are accepted.
func runResolve(args []string) int {
	if len(args) < 2 {
		usage()
	}
	for _, n := range args[1:] {
		if !fact.IsSlug(n) {
			usage()
		}
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "resolve: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if refuseWriter(context.Background(), s, "curator", "resolve") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "resolve: sync:", err)
		return 1
	}
	load := func(name string) (*fact.Fact, string, error) {
		files := factFiles(s.Dir, "", name)
		if len(files) == 0 {
			return nil, "", fmt.Errorf("no fact named %s", name)
		}
		// A name two curated files hold is not one fact to change: say
		// which, and change neither.
		var curated []string
		for _, f := range files {
			if f.rank <= rankRetired {
				curated = append(curated, f.rel)
			}
		}
		if len(curated) > 1 {
			return nil, "", fmt.Errorf("%d curated files hold the name %s (%s); resolve changes neither", len(curated), name, strings.Join(curated, ", "))
		}
		p := files[0].path
		rel := files[0].rel
		if strings.HasPrefix(rel, "inbox/") {
			return nil, "", fmt.Errorf("%s is an inbox note, not a curated fact", name)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, "", err
		}
		f, err := fact.Parse(rel, b)
		if err == nil {
			// Without the store's state no fact can be told from a ruling: refuse.
			err = fact.WithStore(s.Dir, f)
		}
		return f, rel, err
	}
	// Agents can run this command. A ruling, and a fact that holds words of
	// theirs (fact.OwnerWordsOf), are changed only by the owner, in tacit app or
	// the review page: no agent retires or conflicts them without asking them.
	guard := func(fs ...*fact.Fact) bool {
		for _, f := range fs {
			if fact.IsRuling(f) {
				fmt.Fprintf(os.Stderr, "resolve: %s is the owner's rule; only the owner changes it, in tacit app\n", f.Meta.Name)
				return false
			}
			if w := fact.OwnerWordsOf(f); w != "" {
				fmt.Fprintf(os.Stderr, "resolve: %s holds the owner's words (%q); only the owner changes it, in tacit app\n", f.Meta.Name, w)
				return false
			}
		}
		return true
	}
	save := func(f *fact.Fact, rel string) error {
		f.Meta.Updated = time.Now().Format("2006-01-02")
		b, err := f.Encode()
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(s.Dir, rel), b, 0o644)
	}
	var changed []string
	var msg string
	switch args[0] {
	case "supersede":
		if len(args) != 3 {
			usage()
		}
		old, orel, err := load(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve:", err)
			return 1
		}
		if _, _, err := load(args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "resolve:", err)
			return 1
		}
		if !guard(old) {
			return 1
		}
		old.Meta.SupersededBy = args[2]
		if err := save(old, orel); err != nil {
			return 2
		}
		changed, msg = []string{orel}, fmt.Sprintf("resolve: %s superseded by %s", args[1], args[2])
	case "conflict":
		if len(args) != 3 || args[1] == args[2] {
			usage()
		}
		a, arel, err := load(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve:", err)
			return 1
		}
		b, brel, err := load(args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve:", err)
			return 1
		}
		if !guard(a, b) {
			return 1
		}
		a.Meta.ConflictsWith = addUnique(a.Meta.ConflictsWith, args[2])
		b.Meta.ConflictsWith = addUnique(b.Meta.ConflictsWith, args[1])
		if err := save(a, arel); err != nil {
			return 2
		}
		if err := save(b, brel); err != nil {
			return 2
		}
		changed, msg = []string{arel, brel}, fmt.Sprintf("resolve: %s conflicts with %s", args[1], args[2])
	case "clear":
		if len(args) != 2 {
			usage()
		}
		f, rel, err := load(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve:", err)
			return 1
		}
		if !guard(f) {
			return 1
		}
		f.Meta.SupersededBy, f.Meta.ConflictsWith = "", nil
		if err := save(f, rel); err != nil {
			return 2
		}
		changed, msg = []string{rel}, fmt.Sprintf("resolve: %s cleared", args[1])
	default:
		usage()
	}
	// A superseded fact leaves MEMORY.md and its folder's index.
	if all, _, err := fact.Walk(s.Dir); err == nil {
		ix, err := regenIndexes(ctx, s.Dir, all, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve: index:", err)
			s.Restore(append(changed, ix...))
			return 2
		}
		changed = append(changed, ix...)
	}
	if err := s.CommitPush(ctx, changed, msg, "curator"); err != nil {
		fmt.Fprintln(os.Stderr, "resolve: push:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

func addUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}
