// Command tacit is the client: lint, index and import in phase 1.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/settings"
	tstore "github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/tree"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  tacit [--config FILE] [--runner TOOL] <command>
  tacit init   [--store DIR]    initialize a local store (no server or keys)
  tacit maintain [--env-file FILE]   curate, the rotation, then verify on this machine
  tacit doctor [--models] [--role dream|curate|judge|verify] [--json]   offline configuration and dependency checks
  tacit lint   [--store DIR]
  tacit index  [--store DIR] [--check]   MEMORY.md and every folder's _index.md
  tacit format [--set] [--records-complete]   the store's format marker: show it; --set writes this binary's format (once every machine runs it), --records-complete once every fact folder has a record
  tacit tree   [--depth N] [--repo R] [--host H] [path]   facts by scope and type, then the folder indexes from path (default the root) down N levels (default 2 at the root, 3 for a folder); another repo's or host's folder only when named
  tacit import [--store DIR] [--host NAME] claude <memory-dir>...
  tacit import [--store DIR] [--host NAME] codex  <memories-dir>
  tacit clone <url> <harness>     clone the store and set up the credential helper
  tacit clone --read-only [--as reader] <url>   clone that never pushes; --as signs fetches with that key (the server's public path asks one); agents' adds stay on this machine
  tacit sync                       fast-forward the store and refresh the index
  tacit follow [--url U] [--as H]  hold the server's change feed; sync on each push (a systemd user service)
  tacit search [--repo R] [--limit N] [--inbox] [--json] [--scope S] [--type T] [--rulings] [--path P] [--command C] [--offset K] [query...]  with a filter and no query: those facts, 20 a page
  tacit fact <name>                print one fact
  tacit add --harness H --desc "..." [--type T] [--scope S] [--kind K] <name>
  tacit hook session-start|prompt|pre-tool|pre-compact|questions  Claude Code and Codex hooks (JSON on stdin, block on stdout; pre-tool answers PreToolUse as JSON; pre-compact starts a dream of the session and returns; questions prints only the reviewer's question for the owner, for pi's extension)
  tacit mcp [--harness H]          MCP stdio server: tacit_tree, tacit_search, tacit_fact, tacit_add
  tacit install claude|codex|hermes|grok|pi|muse|agy|follow|dream|curate|app
  tacit install claude [--no-mcp]   hooks, and the tacit MCP server in Claude Code's user config (~/.claude.json); --no-mcp: hooks only
  tacit install dream [--env-file F]   the dreamer every 4 h, and the idle job every 10 min (dream --idle 20m, then curate --quick) (systemd timers on Linux, launchd agents on macOS)
  tacit dream [--model M] [--dry-run] [--max-calls N] [--env-file F] [--idle D] [--session T]   transcript deltas -> facts in inbox/dreamer@<host>/; --idle: only sessions quiet for D (alone: 20m) or just compacted; --session: one transcript
  tacit curate [--model M] [--dry-run] [--max-batches N] [--budget D] [--recheck-conflicts --since D] inbox notes -> curated one-fact entries, conflict check, index, push; then the derive step
  tacit curate --quick [--min-interval D] [--env-file F]   the last day's notes only, no derive, results, links or staleness step; nothing within D (30m) of the last quick run
  tacit assess [--model M] [--max N] [--dry-run]   folders with no record (repositories, project folders) and records whose evidence changed -> the curator's model decides kind and status, code checks, records written (a step of curate)
  tacit reviewer [--dry-run] [--max N] [--backfill] [--retrim] [--note PATH]... [--json FILE] [--env-file F]   agents' notes (tacit add, tacit_add, Hermes memory writes, imported memories) -> the deciding model judges each against the session it came from: correct, uncertain or wrong (not curated); an uncertain note whose core holds is trimmed to its cited claims (a trusted note, or the owner's statement for a one-off remark of theirs), and only a doubted rule, decision or past mistake of an active project is a question; --retrim judges uncertain notes from before trims again; a step of every curate, on by default (TACIT_REVIEWER=off turns it off)
  tacit reviewer questions [--json]   the questions waiting for the owner
  tacit questions [--json]   every question waiting for the owner: an agent's ask or query on a finding, the rotation's check?, the reviewer's questions
  tacit questions close ID --harness H --answer A --proof git|file|config|words|gone --evidence E --reason R [--words W [--from REF]] [--dry-run]   an agent closes a question it can prove (never changing a ruling); the owner reopens it in tacit app
  tacit questions moot [--dry-run] [ID...]   the curator closes the questions that went moot: a fact they are about retired, replaced or gone (also a step of tacit verify)
  tacit statements [--model M] [--dry-run]   the owner's statements (their words said once) -> the curator's model reads each new one once, code checks its dry claims and promotes: the same point in another session (decision; preference, then rule), or one statement a check agrees with (decision); a disagreeing check flags it (a step of curate, before assess)
  tacit folder create|status|alias|suppress|tools --folder F [--kind K] [--desc D] [--status S] [--why W] [--alias A] [--into F] [--tools a,b] [--commands c,d]   an agent's change to a folder record, as the curator (by opus; the owner's channel is tacit review)
  tacit migrate [--dry-run] [--max N] [--commits N]   the facts of suppressed folders -> copies in the folder that replaces each (or absorbed by a fact there), the old ones superseded; a ruling moves with its quote, held for the owner only with a reason (a step of curate; no model)
  tacit rescope --plan FILE [--dry-run] [--max N]   move the facts a plan names (JSON lines: name or path, and to) the same way
  tacit derive [--dry-run] [--out DIR] [--repo R] [--force] [--take-over] [--no-fetch] [--projects DIR] [-v]   hot spots, co-change and module map from git -> derived/<repo>/ (no model)
  tacit derive --set-branch <repo>=<branch>   read that repo at a branch other than the remote default (empty branch removes it)
  tacit results [--dry-run] [--out DIR] [--repo R] [--take-over] [--projects DIR] [-v]   measured results committed to repos -> derived/<repo>/ result facts with their commit; saved facts quoting a replaced value -> review/<date>-results.md (no model)
  tacit links [--dry-run] [--restore]   dead [[links]] in curated facts -> rewritten to the one live fact that clearly answers to them, else plain text; an owner ruling gets a LINK? line in review/<date>.md instead (no model; a step of curate, after results); --restore puts back the links to what a fact replaced that runs before v0.9.17 unwrapped (read from the step's commits), and nothing else
  tacit dedupe [--dry-run] [--json FILE]   curated facts that only restate another curated fact of their scope -> superseded_by the fact kept (rulings first; no model; rulings and facts with the owner's words are never retired)
  tacit stale [--dry-run] [--max N] [--fact NAME] [--as-of T] [--json] [-v]   curated facts whose subject changed in git after their date -> stale? findings in review/<date>.md (no model; flags, never retires)
  tacit rulings audit [--dry-run] [--strict] [--recheck] [--digests DIR] [--digests-only] [--sample N] [--seed N] [--json FILE]   keep a ruling only with the owner's own words as evidence (a verified quote, their decision, written by them); the rest -> source.kind observed (no model)
  tacit rulings one-offs [--dry-run] [--digests DIR] [--json FILE]   a ruling resting on words the owner said in one session -> their statement, which the ruling points to (never deleted; no model)
  tacit resolve supersede <old> <new> | conflict <a> <b> | clear <name>   the owner's answer to a review item
  tacit review [--addr 127.0.0.1:7448] [--no-open]   the owner's page in a browser (every build): what needs you, every fact, edit, retire, restore, undo, new rules, folders; loopback only, its link carries a one-run token printed only to a terminal
  tacit review --label SAMPLE.json [--labels OUT.jsonl]   label mode: the owner's ground truth for a sample, written outside the store
  tacit app                        the desktop window: what needs you, every fact, edit, retire, new rulings (a build with -tags desktop)
  tacit app --browser [--addr 127.0.0.1:7450] [--no-open]   the same page as tacit review, on its own port
  tacit review apply VERDICTS.json...   apply a verifier's checked marks (with evidence) in one commit
  tacit rotation [--max N] [--age DAYS] [--fact A,B] [--model M] [--dry-run]   the facts unchecked longest, judged by the curator's model against the proof code gathers; stale ones retired, replaced ones pointed at their successor, changed details noted in place, the owner's facts only asked about; every answered fact stamped last_checked (needs the store format marker; TACIT_ROTATION_MAX, TACIT_ROTATION_AGE_DAYS)
  tacit verify [--model M] [--max N] [--batch N] [--budget USD] [--ids SAMPLE.json] [--dry-run]   settle open findings against their sources; leave the owner only real questions
  tacit timeline [--as-of T] [--json] [--ids SAMPLE.json] [ID...]   the git timeline of review findings: fact dates, commits on their subject, what HEAD shows; no model
  tacit probe <verb> ...           the read-only commands a verifier may run (git-log, git-show, git-blame, git-grep, git-ls, git-tags, grep, find, ls, gh view/list, unit, units, sys)
  tacit keygen <harness>          create the writer key <harness>@<host> and print its public line
  tacit pubkey <harness>          print the public key line
  tacit authcheck <harness> [--url BASE]   does the server take <harness>@<host> as a writer (for reader: as a reader)? exit 0 yes, 1 not yet, 3 waiting will not help
  tacit token  <harness>          print a fresh push token
  tacit credential <harness> get  git credential helper: credential.helper "tacit credential <harness>"

--store defaults to $TACIT_STORE, then ~/.local/share/tacit/store.
exit codes: 0 ok, 1 lint findings or stale index, 2 error.
`)
	os.Exit(2)
}

func storeFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("TACIT_STORE")
	if def == "" {
		if home, err := os.UserHomeDir(); err == nil {
			def = filepath.Join(home, ".local/share/tacit/store")
		}
	}
	return fs.String("store", def, "store root")
}

var explicitRunner string

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("tacit " + version)
		return
	}
	configPath := settings.Path()
	required := os.Getenv("TACIT_SETTINGS") != ""
	for len(os.Args) > 2 && (os.Args[1] == "--config" || os.Args[1] == "--runner") {
		if os.Args[1] == "--config" {
			configPath, _ = filepath.Abs(os.Args[2])
			required = true
			os.Setenv("TACIT_SETTINGS", configPath)
		} else {
			explicitRunner = os.Args[2]
			setExplicitRunner()
		}
		os.Args = append(os.Args[:1], os.Args[3:]...)
	}
	configPath, _ = filepath.Abs(configPath)
	if required {
		os.Setenv("TACIT_SETTINGS", configPath)
	}
	if err := settings.Load(configPath, required); err != nil {
		if len(os.Args) > 1 && os.Args[1] == "hook" {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "tacit:", err)
		os.Exit(2)
	}
	refreshModels()
	// Finder starts an app bundle's executable with no arguments (older
	// macOS adds -psn_...): from ~/Applications/tacit.app that means the window.
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-psn_") {
		if exe, err := os.Executable(); err == nil && launchedFromBundle(exe) {
			os.Exit(runApp(nil))
		}
		usage()
	}
	switch os.Args[1] {
	case "doctor":
		os.Exit(runDoctor(os.Args[2:]))
	case "maintain":
		os.Exit(runMaintain(os.Args[2:]))
	case "init":
		os.Exit(runInit(os.Args[2:]))
	case "lint":
		os.Exit(runLint(os.Args[2:]))
	case "format":
		os.Exit(runFormat(os.Args[2:]))
	case "assess":
		os.Exit(runAssess(os.Args[2:]))
	case "statements":
		os.Exit(runStatements(os.Args[2:]))
	case "folder":
		os.Exit(runFolder(os.Args[2:]))
	case "migrate":
		os.Exit(runMigrate(os.Args[2:]))
	case "rescope":
		os.Exit(runRescope(os.Args[2:]))
	case "tree":
		os.Exit(runTree(os.Args[2:]))
	case "index":
		os.Exit(runIndex(os.Args[2:]))
	case "import":
		os.Exit(runImport(os.Args[2:]))
	case "reviewer":
		os.Exit(runReviewer(os.Args[2:]))
	case "questions":
		os.Exit(runQuestions(os.Args[2:]))
	case "keygen", "pubkey", "token", "credential":
		os.Exit(runKey(os.Args[1], os.Args[2:]))
	case "clone":
		os.Exit(runClone(os.Args[2:]))
	case "sync":
		os.Exit(runSync(os.Args[2:]))
	case "authcheck":
		os.Exit(runAuthcheck(os.Args[2:]))
	case "follow":
		os.Exit(runFollow(os.Args[2:]))
	case "search":
		os.Exit(runSearch(os.Args[2:]))
	case "fact":
		os.Exit(runFact(os.Args[2:]))
	case "add":
		os.Exit(runAdd(os.Args[2:]))
	case "hook":
		os.Exit(runHook(os.Args[2:]))
	case "mcp":
		os.Exit(runMCP(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "dream":
		os.Exit(runDream(os.Args[2:]))
	case "curate":
		os.Exit(runCurate(os.Args[2:]))
	case "rulings":
		os.Exit(runRulings(os.Args[2:]))
	case "derive":
		os.Exit(runDerive(os.Args[2:]))
	case "results":
		os.Exit(runResults(os.Args[2:]))
	case "links":
		os.Exit(runLinks(os.Args[2:]))
	case "dedupe":
		os.Exit(runDedupe(os.Args[2:]))
	case "stale":
		os.Exit(runStale(os.Args[2:]))
	case "resolve":
		os.Exit(runResolve(os.Args[2:]))
	case "probe":
		os.Exit(runProbe(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "rotation":
		os.Exit(runRotation(os.Args[2:]))
	case "review":
		os.Exit(runReview(os.Args[2:]))
	case "app":
		os.Exit(runApp(os.Args[2:]))
	case "timeline":
		os.Exit(runTimeline(os.Args[2:]))
	default:
		usage()
	}
}

func runLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	store := storeFlag(fs)
	fs.Parse(args)
	findings, err := lint.Run(*store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint:", err)
		return 2 // could not lint; 1 means findings
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "%d finding(s)\n", len(findings))
		return 1
	}
	return 0
}

func runIndex(args []string) int {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	store := storeFlag(fs)
	check := fs.Bool("check", false, "exit 1 if MEMORY.md or a folder index differs instead of writing them")
	fs.Parse(args)
	facts, perrs, err := fact.Walk(*store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		return 1
	}
	if len(perrs) > 0 {
		for _, e := range perrs {
			fmt.Fprintln(os.Stderr, "index:", e)
		}
		return 1
	}
	want := index.Generate(facts)
	recs, _ := record.Walk(*store)
	gen := tree.GenerateWith(facts, recs, "")
	dst := filepath.Join(*store, "MEMORY.md")
	on := index.On() // the memory_md setting: off, MEMORY.md is not generated and is removed
	if *check {
		stale := false
		if have, err := os.ReadFile(dst); on && string(have) != want {
			fmt.Fprintln(os.Stderr, "MEMORY.md is stale")
			stale = true
		} else if !on && err == nil {
			fmt.Fprintln(os.Stderr, "MEMORY.md is generated no more (memory_md off); tacit index removes it")
			stale = true
		}
		write, remove, err := tree.Plan(*store, gen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
		stalePaths := make([]string, 0, len(write))
		for rel := range write {
			stalePaths = append(stalePaths, rel)
		}
		sort.Strings(stalePaths)
		for _, rel := range stalePaths {
			fmt.Fprintln(os.Stderr, rel, "is stale")
			stale = true
		}
		for _, rel := range remove {
			fmt.Fprintln(os.Stderr, rel, "is in a folder with no facts")
			stale = true
		}
		if stale {
			return 1
		}
		return 0
	}
	if !on {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
	} else {
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
		if err := os.Rename(tmp, dst); err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
	}
	// The folder indexes (tacit tree, tacit_tree). A curated write commits
	// them with its facts; this rebuilds them in the clone, and the next
	// curated write of the store carries them.
	write, remove, err := tree.Plan(*store, gen)
	if err == nil {
		err = tree.Apply(*store, write, remove)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		return 1
	}
	return 0
}

func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	store := storeFlag(fs)
	hostname, _ := os.Hostname()
	host := fs.String("host", shortHost(hostname), "short host name for inbox/import@<host>")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 2 {
		usage()
	}
	home, _ := os.UserHomeDir()
	o := importer.Options{Store: *store, Host: *host, Home: home}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		return 2
	}
	// Import writes inbox files: never into a read-only clone.
	if (&tstore.Store{Dir: *store}).ReadOnly(context.Background()) {
		fmt.Fprintln(os.Stderr, "import: nothing done:", tstore.ErrReadOnly)
		return 2
	}
	// A memory filed as the owner's is kind user only with their words in it,
	// checked against their turns on this machine (SPEC.md, Rulings). Without
	// them nothing is imported (fail closed).
	corpus, cerr := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
	if cerr != nil {
		fmt.Fprintln(os.Stderr, "import: the owner's words cannot be read:", cerr)
		return 2
	}
	aud := newRulingAuditor(nil, nil, nil, nil, corpus)
	w := whereFor(aud)
	o.OwnerWords = func(q, session string) string { from, _ := w(q, session); return from }
	var r importer.Result
	switch rest[0] {
	case "claude":
		r = importer.Claude(o, rest[1:]...)
	case "codex":
		if len(rest) != 2 {
			usage()
		}
		r = importer.Codex(o, rest[1])
	default:
		usage()
	}
	fmt.Println("import " + rest[0] + ": " + r.String())
	for _, e := range r.Errors {
		fmt.Fprintln(os.Stderr, "import:", e)
	}
	if len(r.Errors) > 0 {
		return 2
	}
	return 0
}

// runKey covers keygen, pubkey, token and the git credential helper. The
// key id is <harness>@<host>, host being this machine's short name, which
// the server checks against the tailnet node that makes the request.
func runKey(cmd string, args []string) int {
	if len(args) < 1 || !fact.IsSlug(args[0]) {
		usage()
	}
	hostname, _ := os.Hostname()
	harness := args[0]
	// A push made on behalf of another writer (dreamer, curator, import) sets
	// TACIT_WRITER; the credential helper then signs with that key.
	if cmd == "credential" {
		if w := os.Getenv("TACIT_WRITER"); w != "" && fact.IsSlug(w) {
			harness = w
		}
	}
	id := harness + "@" + shortHost(hostname)
	dir, err := auth.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, cmd+":", err)
		return 2
	}
	switch cmd {
	case "keygen":
		k, err := auth.Generate(dir, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "keygen:", err)
			return 2
		}
		fmt.Println(k.PublicOpenSSH())
		return 0
	case "credential":
		// Git calls `<helper> get|store|erase` and feeds key=value lines on stdin.
		if len(args) < 2 || args[1] != "get" {
			return 0
		}
		_, _ = io.ReadAll(os.Stdin)
	}
	k, err := auth.Load(dir, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, cmd+": no key for "+id+"; run `tacit keygen "+harness+"` and enroll it:", err)
		return 2
	}
	switch cmd {
	case "pubkey":
		fmt.Println(k.PublicOpenSSH())
	case "token":
		fmt.Println(k.Token(time.Now()))
	case "credential":
		fmt.Printf("username=%s\npassword=%s\n", id, k.Token(time.Now()))
	}
	return 0
}

func shortHost(h string) string {
	h, _, _ = strings.Cut(h, ".")
	return importer.Slug(h, 40)
}
