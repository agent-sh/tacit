package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// reviewReply writes the fixture CLI's reply to the reviewer's prompt for a
// note: the model's JSON verdict wrapped as the CLI prints it.
func reviewReply(t *testing.T, dir, name string, v map[string]any) {
	t.Helper()
	inner, _ := json.Marshal(v)
	out, _ := json.Marshal(map[string]any{"result": string(inner), "total_cost_usd": 0.05})
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lastReviewReport(t *testing.T, home string) reviewReport {
	t.Helper()
	f, err := os.Open(filepath.Join(platform.StateDir(home), "reviewer.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var last reviewReport
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r reviewReport
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Candidates > 0 {
			last = r
		}
	}
	return last
}

// The reviewer's trims on a writer: an uncertain note whose core holds is cut
// to its cited claims, and the trimmed note is curated in its place as a
// trusted agent's note while the note stays in the inbox, replaced, its
// removed claims and their reasons in the ledger. A rule kept on one remark
// of the owner's is their statement (their words, the day they typed them,
// the session, a line of context), never a rule. A trim that adds a claim,
// or keeps a rule on the owner's words without their remark, is refused by
// code and the note stays uncertain. Only a doubted decision of an active
// project is a question; the rest stay silent.
func TestReviewerTrimsEndToEnd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(root, "cache"),
		"TACIT_STORE": filepath.Join(root, "facts"), "TACIT_INDEX": filepath.Join(root, "index"), "TACIT_LOCAL": filepath.Join(root, "overlay"),
		"TACIT_PROJECTS": filepath.Join(root, "projects"), "TACIT_SETTINGS": "", "TACIT_SESSION": "", "CODEX_THREAD_ID": "", "PI_SESSION_ID": ""} {
		t.Setenv(k, v)
	}
	os.MkdirAll(home, 0o700)
	reviewerCLI(t, root)
	stubScrump(t)
	replies := filepath.Join(root, "replies")
	t.Setenv("FIXTURE_REPLIES", replies)
	if err := os.MkdirAll(filepath.Join(root, "projects", "demo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init: %d", code)
	}
	s := &store.Store{Dir: filepath.Join(root, "facts")}
	putRecordFile(t, s.Dir, testRec(record.KindRepo, "projects/demo"))
	git(t, s.Dir, "add", "-A")
	git(t, s.Dir, "commit", "-q", "-m", "demo's record")
	ctx := context.Background()

	notes := []struct{ name, typ, desc string }{
		{"note-over", "design_decision", "demo builds its release with make release and always ships it on fridays"},
		{"note-once", "user_standard", "demo releases must always be tagged by hand"},
		{"note-bad", "design_decision", "demo deploys go through the staging bucket"},
		{"note-norule", "user_standard", "demo always deploys from main"},
		{"note-q", "design_decision", "demo pins its toolchain in the release image"},
		{"note-fact", "design_decision", "demo keeps release notes in the changelog folder"},
		{"note-drop", "user_standard", "demo releases are tagged by hand"},
	}
	// Messages: [1] the session's first turn, [2] and [3] the owner's turns
	// (the third typed on 2026-10-20), then one save each.
	lines := []string{
		ownerTurn("sess-t", "demo builds its release with make release, deploys from main"),
		`{"type":"user","entrypoint":"cli","sessionId":"sess-t","origin":{"kind":"human"},"timestamp":"2026-10-20T10:00:00.000Z","message":{"content":"tag this release by hand, the bot is broken today"}}`,
	}
	for _, n := range notes {
		lines = append(lines, addCall("sess-t", n.name, n.desc))
	}
	claudeSession(t, home, "sess-t", "/home/u/projects/demo", lines...)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-t")
	for _, n := range notes {
		if _, err := addFact(ctx, s, "claude", fact.Meta{Name: n.name, Description: n.desc, Type: n.typ, Scope: "projects/demo"}, n.desc); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	reviewReply(t, replies, "note-over", map[string]any{"verdict": "uncertain", "reason": "the fridays part was never said", "doubt": "overreach", "kind": "fact",
		"trim": map[string]any{"claims": []any{map[string]any{"text": "demo builds its release with make release", "cites": []int{2}}},
			"removed": []any{map[string]any{"text": "always ships it on fridays", "reason": "nothing in the session says so"}}}})
	reviewReply(t, replies, "note-once", map[string]any{"verdict": "uncertain", "reason": "a one-time call made a standing rule", "doubt": "overreach", "kind": "rule",
		"trim": map[string]any{"claims": []any{map[string]any{"text": "demo releases tagged by hand", "cites": []int{3}}},
			"removed": []any{map[string]any{"text": "must always", "reason": "he said it for this release, while the bot was broken"}},
			"one_off": map[string]any{"words": "tag this release by hand", "line": 3, "context": "cutting a demo release while the release bot was broken",
				"relevance": map[string]any{"keep": true, "category": "instruction", "reason": "how releases are tagged while the bot is broken"}}}})
	reviewReply(t, replies, "note-bad", map[string]any{"verdict": "uncertain", "reason": "the bucket is not named", "doubt": "overreach", "kind": "fact",
		"trim": map[string]any{"claims": []any{map[string]any{"text": "demo deploys run on kubernetes clusters", "cites": []int{2}}}}})
	reviewReply(t, replies, "note-norule", map[string]any{"verdict": "uncertain", "reason": "said once", "doubt": "overreach", "kind": "fact",
		"trim": map[string]any{"claims": []any{map[string]any{"text": "demo always deploys from main", "cites": []int{2}}}}})
	// The owner's remark judged not to matter: no statement, the note stays
	// uncertain with the judgement on its line.
	reviewReply(t, replies, "note-drop", map[string]any{"verdict": "uncertain", "reason": "one release", "doubt": "overreach", "kind": "rule",
		"trim": map[string]any{"claims": []any{map[string]any{"text": "demo releases tagged by hand", "cites": []int{3}}},
			"one_off": map[string]any{"words": "tag this release by hand", "line": 3, "context": "one demo release",
				"relevance": map[string]any{"keep": false, "category": "steering", "reason": "about that one release"}}}})
	reviewReply(t, replies, "note-q", map[string]any{"verdict": "uncertain", "reason": "nothing says the toolchain is pinned", "doubt": "core", "kind": "decision"})
	reviewReply(t, replies, "note-fact", map[string]any{"verdict": "uncertain", "reason": "the folder is not named", "doubt": "core", "kind": "fact"})

	if code := runReviewer(nil); code != 0 {
		t.Fatalf("reviewer exit %d", code)
	}
	host := store.ShortHost()
	inbox := "inbox/claude@" + host + "/"
	reviews := ledger.ParseReviews(readBytes(t, filepath.Join(s.Dir, ledger.ReviewsPath)))

	// The overreach trimmed to a trusted note.
	l, _ := reviews.ForNote(inbox + "note-over.md")
	if l.Verdict != ledger.VerdictReplaced || l.As != ledger.TrimNote || l.Into != inbox+"note-over-trimmed.md" || len(l.Kept) != 1 || l.Kept[0].Cites[0] != 2 ||
		len(l.Removed) != 1 || l.Removed[0].Reason == "" || l.Doubt != "overreach" {
		t.Fatalf("note-over: %+v", l)
	}
	tl, _ := reviews.ForNote(inbox + "note-over-trimmed.md")
	if tl.Verdict != ledger.VerdictTrimmed || tl.Of != inbox+"note-over.md" || tl.By != ledger.ByTacit {
		t.Fatalf("the trimmed note's line: %+v", tl)
	}
	tn, err := fact.Parse(inbox+"note-over-trimmed.md", readBytes(t, filepath.Join(s.Dir, filepath.FromSlash(inbox+"note-over-trimmed.md"))))
	if err != nil || tn.Meta.Description != "demo builds its release with make release" || tn.Meta.Source.Harness != "claude" || tn.Meta.Source.Session != "sess-t" || strings.Contains(tn.Body, "fridays") {
		t.Fatalf("the trimmed note: %+v %v", tn, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, filepath.FromSlash(inbox+"note-over.md"))); err != nil {
		t.Fatalf("the original left the inbox: %v", err)
	}

	// The one-off: the owner's statement, dated by their turn.
	l, _ = reviews.ForNote(inbox + "note-once.md")
	if l.Verdict != ledger.VerdictReplaced || l.As != ledger.TrimStatement || l.Into != "owner-said-once-note-once" {
		t.Fatalf("note-once: %+v", l)
	}
	sp := filepath.Join(s.Dir, "projects", "demo", "owner-said-once-note-once.md")
	sf, err := fact.Parse("projects/demo/owner-said-once-note-once.md", readBytes(t, sp))
	if err != nil || sf.Meta.Type != fact.StatementType || sf.Meta.Source.OwnerWords != "tag this release by hand" || sf.Meta.Source.OwnerWordsFrom != "claude:sess-t" ||
		sf.Meta.Created != "2026-10-20" || !strings.HasPrefix(strings.TrimLeft(sf.Body, "\n"), `You said once (2026-10-20): "tag this release by hand"`) || !strings.Contains(sf.Body, "Context: cutting a demo release") {
		t.Fatalf("statement: %+v %v", sf, err)
	}
	if q := string(readBytes(t, filepath.Join(s.Dir, ledger.QuotesPath))); !strings.Contains(q, `"owner-said-once-note-once"`) || !strings.Contains(q, `"words":true`) {
		t.Fatalf("quote ledger: %s", q)
	}

	// Refused trims: the notes stay uncertain, with why.
	rep := lastReviewReport(t, home)
	for _, n := range []string{"note-bad", "note-norule"} {
		if l, _ := reviews.ForNote(inbox + n + ".md"); l.Verdict != ledger.VerdictUncertain {
			t.Fatalf("%s: %+v", n, l)
		}
		if len(rep.TrimProblems[inbox+n+".md"]) == 0 {
			t.Fatalf("%s: no trim problem in %+v", n, rep.TrimProblems)
		}
	}
	if p := strings.Join(rep.TrimProblems[inbox+"note-bad.md"], "; "); !strings.Contains(p, "kube") {
		t.Fatalf("note-bad's problems: %s", p)
	}
	if p := strings.Join(rep.TrimProblems[inbox+"note-norule.md"], "; "); !strings.Contains(p, "one_off is required") {
		t.Fatalf("note-norule's problems: %s", p)
	}
	if rep.Trimmed != 1 || rep.TrimStatement != 1 || rep.TrimRefused != 2 || rep.TrimDropped != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if l, _ := reviews.ForNote(inbox + "note-drop.md"); l.Verdict != ledger.VerdictUncertain || l.Relevance == nil || l.Relevance.Keep || l.Relevance.Category != "steering" {
		t.Fatalf("note-drop: %+v", l)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "demo", "owner-said-once-note-drop.md")); err == nil {
		t.Fatal("a dropped remark made a statement")
	}

	// Questions: only the doubted decision of the active project.
	q := readQueue(t, home)
	if open := q.Open(); len(open) != 1 || open[0].Name != "note-q" {
		t.Fatalf("queue: %+v", q.Items)
	}
	if rep.Silent[agentreview.NotCoreDoubt] != 3 || rep.Silent[agentreview.NotWeighty] != 1 {
		t.Fatalf("silent: %+v", rep.Silent)
	}

	// Curate: the trimmed note is curated, trusted; the replaced one is not.
	var entries []map[string]any
	for _, n := range []string{"note-over-trimmed", "note-bad", "note-norule", "note-q", "note-fact"} {
		d := "demo builds its release with make release"
		for _, x := range notes {
			if x.name == n {
				d = x.desc
			}
		}
		entries = append(entries, map[string]any{"origin": n, "name": n + "-fact", "description": d, "type": "design_decision", "scope": "projects/demo", "kind": "observed", "body": d})
	}
	b, _ := json.Marshal(entries)
	out, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	t.Setenv("FIXTURE_SPLIT", string(out))
	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "demo", "note-over-fact.md")); err == nil {
		t.Fatal("the replaced note was curated")
	}
	facts, _, err := fact.Walk(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var trimmedFact *fact.Fact
	for _, f := range facts {
		if f.Meta.Name == "note-over-trimmed-fact" {
			trimmedFact = f
		}
	}
	if trimmedFact == nil || trimmedFact.Review != ledger.VerdictTrimmed || trimmedFact.Meta.Source.Review != ledger.VerdictTrimmed {
		t.Fatalf("the trimmed note's fact: %+v", trimmedFact)
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}
	ix := exactIndex(t, s)
	hits, err := ix.Query(ctx, search.Situation{Repo: "demo", Query: "demo release make release fridays"}, 20, true)
	ix.Close()
	if err != nil {
		t.Fatal(err)
	}
	var sawTrim, sawOrig bool
	for _, h := range hits {
		switch h.Name {
		case "note-over-trimmed-fact":
			sawTrim = true
			if b := search.Block([]search.Hit{h}, "# t"); !strings.Contains(b, "trimmed to what its session supports") {
				t.Fatalf("label:\n%s", b)
			}
		case "note-over":
			sawOrig = true
			if !h.Held() {
				t.Fatalf("the replaced note is delivered: %+v", h)
			}
		}
	}
	if !sawTrim || !sawOrig {
		t.Fatalf("hits: %+v", hits)
	}
}

// A one-off kept with claims the owner's words do not hold: their statement
// holds the claims that cite their messages, and a trimmed note beside it
// the rest (also).
func TestApplyTrimSplitsTheirWordsFromTheAgentsClaims(t *testing.T) {
	stubScrump(t)
	note := &fact.Fact{Path: "inbox/claude@h/buildbox-cap.md", Meta: fact.Meta{Name: "buildbox-cap", Description: "buildbox builds always run make -j8; measured 3% slower than -j16", Type: "design_decision", Scope: "machines/buildbox",
		Source: fact.Source{Harness: "claude", Host: "h", Kind: "observed", Session: "s1"}, Created: "2026-10-20"}, Body: "buildbox builds always run make -j8. Measured 3% slower than -j16."}
	msgs := []agentreview.Msg{
		{Role: "user", Owner: true, Text: "cap the builds at -j8 for now, the box runs too hot", At: "2026-10-19T08:00:00Z"},
		{Role: "assistant", Text: "Measured: -j8 is about 3% slower than -j16."},
	}
	tr := &agentreview.Trim{
		Claims:  []ledger.Claim{{Text: "buildbox builds run make -j8", Cites: []int{1}}, {Text: "Measured 3% slower than -j16", Cites: []int{2}}},
		Removed: []ledger.Removed{{Text: "always", Reason: "he said for now"}},
		OneOff:  &agentreview.OneOff{Words: "cap the builds at -j8 for now", Line: 1, Context: "the box ran too hot"},
	}
	res := agentreview.Result{Line: ledger.Review{Note: note.Path, Name: note.Meta.Name, Verdict: ledger.VerdictUncertain, By: ledger.ByTacit, Session: "claude:s1", At: "2026-10-25T00:00:00Z"},
		Conv: agentreview.Conversation{Harness: "claude", Session: "s1"}, Called: true, Trim: tr, Msgs: msgs}
	w := &reviewWork{supersede: map[string]string{}}
	curated := []*fact.Fact{{Path: "machines/buildbox/buildbox-cap-fact.md", Meta: fact.Meta{Name: "buildbox-cap-fact"}}}
	into, as, ok := applyTrim(w, note, res, curated, false, true)
	if !ok || as != ledger.TrimStatement || into != "owner-said-once-buildbox-cap" || len(w.statements) != 1 || len(w.trimmed) != 1 || len(w.quotes) != 1 {
		t.Fatalf("%s %s %v: %+v", into, as, ok, w)
	}
	st, tn := w.statements[0], w.trimmed[0]
	if st.Meta.Created != "2026-10-19" || st.Meta.Scope != "machines/buildbox" || !strings.Contains(st.Body, "Gloss (the curator's reading, not your words): buildbox builds run make -j8") || strings.Contains(st.Body, "Measured") {
		t.Fatalf("statement: %+v %q", st.Meta, st.Body)
	}
	if tn.Path != "inbox/claude@h/buildbox-cap-trimmed.md" || tn.Meta.Description != "Measured 3% slower than -j16" || strings.Contains(tn.Body, "-j8\n") {
		t.Fatalf("trimmed note: %+v %q", tn.Meta, tn.Body)
	}
	var replaced, trimmed ledger.Review
	for _, l := range w.lines {
		switch l.Verdict {
		case ledger.VerdictReplaced:
			replaced = l
		case ledger.VerdictTrimmed:
			trimmed = l
		}
	}
	if replaced.Into != st.Meta.Name || replaced.Also != tn.Path || len(replaced.Kept) != 2 || trimmed.Of != note.Path || !ledger.ValidReview(replaced) || !ledger.ValidReview(trimmed) {
		t.Fatalf("lines: %+v", w.lines)
	}
	if w.supersede["machines/buildbox/buildbox-cap-fact.md"] != st.Meta.Name {
		t.Fatalf("the note's curated fact: %+v", w.supersede)
	}
	// A store whose format predates statements: no trim, the note stays uncertain.
	if _, _, ok := applyTrim(&reviewWork{supersede: map[string]string{}}, note, res, nil, false, false); ok {
		t.Fatal("a statement in a store before statements")
	}
}

// The owner's words settle a note only when the note holds them, or most of
// their content words: a few shared words of the same folder do not.
func TestOwnerSettlesNeedsTheirWords(t *testing.T) {
	ruling := &fact.Fact{Path: "projects/x/py.md", Meta: fact.Meta{Name: "latest-python-only", Description: "Use the latest stable Python", Type: "user_standard", Scope: "projects/x",
		Source: fact.Source{Harness: "curator", Kind: "user", OwnerQuote: "always use the latest stable python release", OwnerQuoteFrom: "claude:s1"}}, Audited: true, QuoteBacked: true}
	other := &fact.Fact{Path: "projects/x/ci.md", Meta: fact.Meta{Name: "ci-cost", Description: "CI runs cost money", Type: "statement", Scope: "projects/x",
		Source: fact.Source{Harness: "curator", Kind: "observed", OwnerWords: "the ci runner costs too much money per release run", OwnerWordsFrom: "claude:s2"}}, WordsBacked: true, WordsRecorded: true}
	settles := ownerSettles([]*fact.Fact{ruling, other})
	hold := &fact.Fact{Path: "inbox/claude@h/n.md", Meta: fact.Meta{Name: "n", Description: "Owner: always use the latest stable python release for new venvs", Scope: "projects/x"}}
	if got := settles(hold); !strings.Contains(got, "latest-python-only") {
		t.Fatalf("their words in the note: %q", got)
	}
	near := &fact.Fact{Path: "inbox/claude@h/m.md", Meta: fact.Meta{Name: "m", Description: "release runs on the ci runner pick the money-saving tier", Scope: "projects/x"}}
	if got := settles(near); got == "" {
		t.Fatal("most of their words in the description did not settle it")
	}
	far := &fact.Fact{Path: "inbox/claude@h/f.md", Meta: fact.Meta{Name: "f", Description: "python release notes go in the changelog", Scope: "projects/x"}}
	if got := settles(far); got != "" {
		t.Fatalf("a few shared words settled it: %q", got)
	}
	elsewhere := &fact.Fact{Path: "inbox/claude@h/e.md", Meta: fact.Meta{Name: "e", Description: "always use the latest stable python release", Scope: "projects/y"}}
	if got := settles(elsewhere); got != "" {
		t.Fatalf("another folder's words settled it: %q", got)
	}
}

// On this machine's layer an imported memory sits in its agent's folder
// (local/claude@h/memory/...): the trimmed note is written beside it, and the
// statement and the ledger's verdicts are written too (a refused trim once
// cost the whole step's verdicts).
func TestWriteReviewLocalWritesATrimInTheAgentsFolder(t *testing.T) {
	stubScrump(t)
	root := t.TempDir()
	dir, storeDir := filepath.Join(root, "local"), filepath.Join(root, "store")
	os.MkdirAll(dir, 0o700)
	os.MkdirAll(storeDir, 0o700)
	note := &fact.Fact{Path: "local/claude@h/memory/users-x/buildbox-cap.md", Meta: fact.Meta{Name: "buildbox-cap", Description: "buildbox builds always run make -j8; measured 3% slower than -j16", Type: "design_decision", Scope: "machines/buildbox",
		Source: fact.Source{Harness: "claude", Host: "h", Kind: "observed", Session: "s1"}, Created: "2026-10-20"}, Body: "buildbox builds always run make -j8. Measured 3% slower than -j16."}
	msgs := []agentreview.Msg{
		{Role: "user", Owner: true, Text: "cap the builds at -j8 for now, the box runs too hot", At: "2026-10-19T08:00:00Z"},
		{Role: "assistant", Text: "Measured: -j8 is about 3% slower than -j16."},
	}
	tr := &agentreview.Trim{
		Claims:  []ledger.Claim{{Text: "buildbox builds run make -j8", Cites: []int{1}}, {Text: "Measured 3% slower than -j16", Cites: []int{2}}},
		Removed: []ledger.Removed{{Text: "always", Reason: "he said for now"}},
		OneOff:  &agentreview.OneOff{Words: "cap the builds at -j8 for now", Line: 1, Context: "the box ran too hot"},
	}
	res := agentreview.Result{Line: ledger.Review{Note: note.Path, Name: note.Meta.Name, Verdict: ledger.VerdictUncertain, By: ledger.ByTacit, Session: "claude:s1", At: "2026-10-25T00:00:00Z"},
		Conv: agentreview.Conversation{Harness: "claude", Session: "s1"}, Called: true, Trim: tr, Msgs: msgs}
	w := &reviewWork{supersede: map[string]string{}}
	if _, _, ok := applyTrim(w, note, res, nil, true, true); !ok || len(w.trimmed) != 1 || len(w.statements) != 1 {
		t.Fatalf("applyTrim: %+v", w)
	}
	if err := writeReviewLocal(dir, storeDir, w); err != nil {
		t.Fatalf("writeReviewLocal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude@h", "memory", "users-x", "buildbox-cap-trimmed.md")); err != nil {
		t.Fatalf("the trimmed note: %v", err)
	}
	rel := strings.TrimPrefix(w.statements[0].Path, overlay.Prefix)
	if !strings.HasPrefix(rel, overlay.CuratedDir+"/") {
		t.Fatalf("statement path %s", rel)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("the statement: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.ReviewsRel)))
	if err != nil || !strings.Contains(string(b), `"verdict":"trimmed"`) || !strings.Contains(string(b), `"verdict":"replaced"`) {
		t.Fatalf("the ledger: %v %s", err, b)
	}
	// A trim that cannot be written (here a path out of the agents' folders)
	// is an error, and the verdicts are written all the same.
	w2 := &reviewWork{lines: []ledger.Review{{Note: "local/claude@h/x.md", Name: "x", Verdict: ledger.VerdictCorrect, By: ledger.ByTacit, At: "2026-10-25T00:00:00Z"}},
		trimmed: []*fact.Fact{{Path: "local/x-trimmed.md", Meta: fact.Meta{Name: "x-trimmed", Description: "d", Type: "design_decision", Scope: "user"}, Body: "b"}}}
	if err := writeReviewLocal(dir, storeDir, w2); err == nil {
		t.Fatal("a trim out of the agents' folders was written")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(overlay.ReviewsRel))); !strings.Contains(string(b), `"name":"x","verdict":"correct"`) {
		t.Fatalf("the verdict was lost with the trim: %s", b)
	}
}
