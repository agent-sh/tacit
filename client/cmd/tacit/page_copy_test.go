package main

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// runPage runs the page's own script (app.js) under node with stub DOM and
// fetch, then the checks, whose return value is printed as JSON.
func runPage(t *testing.T, checks string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	script := `
const src = require("fs").readFileSync(process.argv[1], "utf8");
const mk = () => new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => "" : k === "then" ? undefined : mk()), apply: () => mk(), set: () => true });
const els = {};
const el = (id) => els[id] || (els[id] = { id, innerHTML: "", textContent: "", value: "", dataset: {}, removeAttribute() {}, querySelector: () => null, querySelectorAll: () => [], addEventListener() {}, setAttribute() {} });
globalThis.Element = class {};
const document = { querySelector: () => ({ content: "tok" }), getElementById: el, addEventListener(type, f) { ((els.__on = els.__on || {})[type] = els.__on[type] || []).push(f); }, documentElement: { dataset: {} }, body: { style: {}, dataset: {}, matches: () => false, closest: () => null } };
const fetchStub = async (url, opts) => {
  if (opts && opts.body) (els.__posts = els.__posts || []).push(JSON.parse(opts.body));
  return String(url).startsWith("/api/items") ? { ok: true, json: async () => ({ items: [], host: "h" }) } : String(url).startsWith("/api/decide") ? { ok: true, json: async () => ({ ok: true }) } : { ok: true, json: async () => ({ repos: [], terms: [], events: [] }) };
};
const body = src + ";return (async () => {" + process.argv[2] + "})()";
new Function("document", "localStorage", "matchMedia", "fetch", "els", body)(document, { setItem() {}, getItem() {} }, () => ({ matches: false }), fetchStub, els).then((o) => console.log(o), (e) => { console.error(e); process.exit(1); });
`
	b, err := exec.Command(node, "-e", script, "reviewui/app.js", checks).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	if err := json.Unmarshal(b[strings.LastIndex(strings.TrimSpace(string(b)), "\n")+1:], out); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
}

// Every finding is asked as a plain question, its choices name the facts by
// what they are (older, newer, your rule, the number from git), and the
// agent's A and B become those names.
func TestPageAsksEachFindingInPlainWords(t *testing.T) {
	var got map[string]any
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name, created, text, more) => Object.assign({ name, created, description: text, body: "", scope: "projects/memra", type: "design_decision", kind: "observed" }, more || {});
  const pair = (kind, a, b) => ({ kind, a, b, allowed: ["left", "right", "both", "query"] });
  const old = f("old", "2026-10-08", "Workers retry three times, as of 2026-08-03."), neu = f("neu", "2026-10-08", "Workers retry five times since 2026-08-17.");
  const out = { q: {}, labels: {} };
  for (const k of Object.keys(KINDS)) out.q[k] = question(pair(k, old, neu));
  // Same written day: the dates in the text decide which is older; listed newer first.
  const c = pair("contradiction", neu, old);
  out.labels.contradiction = orderFor(c.kind, c).map((m) => choiceLabel(c, m));
  out.heads = sides(c);
  const tie = pair("contradiction", f("x", "2026-10-08", "a"), f("y", "2026-10-08", "b"));
  out.labels.tie = ["left", "right"].map((m) => choiceLabel(tie, m));
  for (const k of ["result-superseded", "ruling-change", "supersede-applied"]) { const p = pair(k, old, neu); out.labels[k] = orderFor(k, p).map((m) => choiceLabel(p, m)); }
  const st = { kind: "stale", single: true, a: old, b: old, allowed: ["current", "retire"] };
  out.labels.stale = orderFor("stale", st).map((m) => choiceLabel(st, m));
  out.hint = choiceHint(c, "right");
  // The newer fact is on the data's left here: the older card is drawn first, and the left arrow keeps it.
  c.id = "c"; c.needs_owner = true; items = [c]; view = [c]; cur = 0; sheet();
  const html = els["sheet-inner"].innerHTML;
  out.olderFirst = html.indexOf('id="g-b"') < html.indexOf('id="g-a"');
  out.leftKey = (/data-mark="right"[^]*?<kbd[^>]*>(.)<\/kbd>/.exec(html) || [])[1];
  out.evidence = plainEvidence("B's rule postdates A (2026-08-03). B also says the model stays. A is moot.", sides(c));
  const asked = Object.assign(pair("supersede-proposed", old, neu), { ask: { proposed: "neu", evidence: "A was replaced." } });
  out.word = agentWord(asked, sides(asked)).text;
  out.stale = staleSummary({ n: 3, since: "2026-10-01", detail: ["commits count from 2026-10-01 (last current); 3 deleted something the fact names", "2026-10-02 bcd2345 memra: move capture - capture.rs", "2026-10-03 abc1234 memra: drop the rename worker - rename_worker.rs, RenameWorker"] });
  out.where = ["projects/memra", "machines/host-a", "user", "system", "derived/memra"].map(whereWords);
  return JSON.stringify(out);
`, &got)
	want := map[string]string{
		"contradiction":      "These two facts disagree. Which is true now?",
		"supersede-proposed": "Does the newer fact replace the older one?",
		"result-superseded":  "A newer number exists for this measurement. Use it?",
		"ruling-change":      "A newer fact may change your rule. Does your rule still stand?",
		"supersede-applied":  "The curator replaced the older fact with the newer one. Keep it that way?",
		"duplicate":          "These two facts say the same thing. Keep one?",
		"link":               "Your rule links to a fact that does not exist.",
		"stale":              "Git says something this fact names was deleted. Is it still true?",
		"check":              "The curator checked this fact against what tacit knows now and reads it differently. Is it still true?",
		"question":           "The reviewer had this question for you about an agent's note. An agent closed it.",
	}
	q := got["q"].(map[string]any)
	for k, w := range want {
		if q[k] != w {
			t.Errorf("%s asks %q, want %q", k, q[k], w)
		}
	}
	if len(q) != len(want) {
		t.Errorf("kinds %v", q)
	}
	labels := func(k string) string { b, _ := json.Marshal(got["labels"].(map[string]any)[k]); return string(b) }
	for k, w := range map[string]string{
		"contradiction":     `["Keep the newer","Keep the older","Both are true","Mark as conflicting"]`,
		"tie":               `["Keep the first","Keep the second"]`,
		"result-superseded": `["Use the number from git","Keep the saved number","Both are true","Mark as conflicting"]`,
		"ruling-change":     `["Keep your rule","Use the newer fact","Both are true","Mark as conflicting"]`,
		"supersede-applied": `["Keep it replaced","Undo, both are true","Keep the older instead","Mark as conflicting"]`,
		"stale":             `["Still true","Outdated, retire it"]`,
	} {
		if labels(k) != w {
			t.Errorf("%s choices %s, want %s", k, labels(k), w)
		}
	}
	if h, _ := json.Marshal(got["heads"]); !strings.Contains(string(h), `"aHead":"Newer"`) || !strings.Contains(string(h), `"bHead":"Older"`) {
		t.Errorf("the fact dated 17 Aug in its text is the newer one: %s", h)
	}
	if got["olderFirst"] != true || got["leftKey"] != "←" {
		t.Errorf("older card first %v, the key that keeps it %v", got["olderFirst"], got["leftKey"])
	}
	if got["hint"] != "Keeps the older fact. The newer fact stops reaching agents; it stays in history." {
		t.Errorf("hint %q", got["hint"])
	}
	if w := "The older fact's rule postdates the newer fact (3 Aug). The older fact also says the model stays. The newer fact is moot."; got["evidence"] != w {
		t.Errorf("evidence:\n%q\nwant\n%q", got["evidence"], w)
	}
	if w := "An agent suggests: keep the newer. Its reason: The older fact was replaced."; got["word"] != w {
		t.Errorf("suggestion %q, want %q", got["word"], w)
	}
	if s, _ := json.Marshal(got["stale"]); string(s) != `["rename_worker.rs and RenameWorker were deleted in memra on 3 Oct (“drop the rename worker”).","2 more commits since 1 Oct deleted something it names."]` {
		t.Errorf("stale summary %s", s)
	}
	if s, _ := json.Marshal(got["where"]); string(s) != `["in memra","on host-a","about you, in every repo","for every agent, everywhere","in memra, computed from git"]` {
		t.Errorf("where %s", s)
	}
}

// What the owner reads on a finding never uses tacit's internal words.
func TestPageSpeaksNoInternalWords(t *testing.T) {
	var got map[string]string
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name, created, kind) => ({ name, created, description: "Workers retry three times.", body: "Since the pool rewrite.", scope: "projects/memra", type: "user_standard", kind: kind || "user", ruling: !kind, path: "projects/memra/" + name + ".md" });
  const out = {};
  for (const k of ["contradiction", "supersede-proposed", "result-superseded", "ruling-change", "supersede-applied", "duplicate"]) {
    const it = { id: k, kind: k, a: f("docs-review-every-change", "2026-08-03"), b: f("docs-work-paused", "2026-08-17", "observed"), allowed: ["left", "right", "both", "query"], needs_owner: true,
      ask: { proposed: "docs-work-paused", evidence: "A predates B.", question: "An agent would change the relations of your ruling docs-review-every-change (right). What stands?" } };
    items = [it]; view = [it]; cur = 0; sheet(); out[k] = els["sheet-inner"].innerHTML;
  }
  const st = { id: "s", kind: "stale", single: true, a: f("engine-retries", "2026-10-01"), b: null, flag: { n: 1, since: "2026-10-01", detail: ["2026-10-03 abc1234 memra: drop - rename_worker.rs"] }, allowed: ["current", "retire"], needs_owner: true, state: "live" };
  st.b = st.a; items = [st]; view = [st]; cur = 0; sheet(); out.stale = els["sheet-inner"].innerHTML;
  items = []; view = []; sheet(); out.done = els["sheet-inner"].innerHTML;
  return JSON.stringify(out);
`, &got)
	tags := regexp.MustCompile(`<[^>]*>`)
	banned := regexp.MustCompile(`(?i)\b(proofs?|galley|stet|dele|slug|folio|supersede[ds]?|contradiction|frontmatter|relations|sheet|ruling|to go)\b|\d+ of \d+ marked`)
	for k, html := range got {
		// Details hold the fact's name and file on purpose; the rest is what the owner reads.
		text := tags.ReplaceAllString(regexp.MustCompile(`(?s)<details class="more">.*?</details>`).ReplaceAllString(html, ""), " ")
		if m := banned.FindAllString(text, -1); len(m) > 0 {
			t.Errorf("%s says %v:\n%s", k, m, text)
		}
		if strings.Contains(text, "docs-review-every-change") {
			t.Errorf("%s uses a fact's name outside its details:\n%s", k, text)
		}
	}
	if !strings.Contains(got["done"], "Nothing needs you.") {
		t.Errorf("done state:\n%s", got["done"])
	}
}

// Neither is true is a plain answer on the real queue (w, an x), and every
// open finding takes a note: sent with the answer, or on its own; an answered
// one shows the note under the answer.
func TestPageOffersNeitherAndANote(t *testing.T) {
	var got map[string]any
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name, created) => ({ name, created, description: "d " + name, body: "", scope: "projects/memra", type: "design_decision", kind: "observed", path: "projects/memra/" + name + ".md" });
  const it = { id: "p1", kind: "contradiction", a: f("a", "2026-09-01"), b: f("b", "2026-09-19"), allowed: ["left", "right", "both", "query", "wrong"], needs_owner: true, notes: [{ at: "2026-10-11T06:00:00Z", text: "a holds on the old boxes" }] };
  items = [it]; view = [it]; cur = 0; sheet();
  const out = { open: els["sheet-inner"].innerHTML, hint: choiceHint(it, "wrong"), past: pastFor(it, "wrong") };
  // An answer carries the note; a note alone is the "note" action.
  noteDrafts.set("p1", "  both are stale now  "); await decide("wrong");
  items = [it]; view = [it]; cur = 0; noteDrafts.set("p1", "check the old boxes"); await commentOnly(it);
  items = [it]; view = [it]; cur = 0; await decide("both");
  out.posts = els.__posts;
  const done = Object.assign({}, it, { needs_owner: false, mark: "wrong", allowed: ["reopen"], decision: { action: "wrong", at: "2026-10-11T06:00:00Z", comment: "both are stale now" } });
  items = [done]; view = [done]; cur = 0; sheet(); out.done = els["sheet-inner"].innerHTML;
  return JSON.stringify(out);
`, &got)
	open := got["open"].(string)
	for _, want := range []string{`data-mark="wrong"`, "Neither is true", "<kbd>w</kbd>", "Add a note (optional)", "Comment without answering", "Your notes", "a holds on the old boxes"} {
		if !strings.Contains(open, want) {
			t.Errorf("open finding lacks %q:\n%s", want, open)
		}
	}
	if got["hint"] != "Neither is true: both facts stop reaching agents. They stay in history, and Change this answer brings both back." || got["past"] != "Neither is true, both retired" {
		t.Errorf("hint %q past %q", got["hint"], got["past"])
	}
	posts, _ := json.Marshal(got["posts"])
	for _, want := range []string{`{"Action":"wrong","Comment":"both are stale now","ID":"p1","OwnerOnly":false}`, `{"Action":"note","Comment":"check the old boxes","ID":"p1","OwnerOnly":false}`, `{"Action":"both","Comment":"","ID":"p1","OwnerOnly":false}`} {
		if !strings.Contains(string(posts), want) {
			t.Errorf("posts %s lack %s", posts, want)
		}
	}
	if done := got["done"].(string); !strings.Contains(done, "Your note:</b> both are stale now") || strings.Contains(done, "Add a note") {
		t.Errorf("answered finding:\n%s", done)
	}
}

// The review of 12699d6: the names agree with the dates the cards print, the
// agent's own verdict is shown and suggested even when it named no fact, the
// hints name their rule when it goes, the evidence rewrite keeps fact names and
// English, and the arrow keys keep the card on that side of the screen.
func TestPageNamesAgreeWithDatesAndVerdicts(t *testing.T) {
	var got map[string]any
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name, created, text, more) => Object.assign({ name, created, description: text, body: "", scope: "projects/x", type: "design_decision", kind: "observed", path: "projects/x/" + name + ".md" }, more || {});
  const out = {};
  // 1. The curator's role says A may be replaced by B, but A is dated later.
  const rule = f("release-post-changelog", "2026-10-08", "Post the changelog at every release.", { type: "user_standard", kind: "user", ruling: true });
  const b = f("site-no-blog", "2026-10-08", "The site has no blog since 2026-09-18.");
  const sp = { id: "sp", kind: "supersede-proposed", a: rule, b, allowed: ["left", "right", "both", "query", "wrong"], needs_owner: true,
    ask: { proposed: "site-no-blog", evidence: "The blog was removed 2026-09-18.", question: "Does your ruling release-post-changelog still hold? An agent reads site-no-blog as the current one." } };
  items = [sp]; view = [sp]; cur = 0; sheet();
  const html = els["sheet-inner"].innerHTML;
  out.sp = { q: question(sp), heads: [/id="g-a"[^]*?<span class="who">([^<]*)/.exec(html)[1], /id="g-b"[^]*?<span class="who">([^<]*)/.exec(html)[1]], olderFirst: html.indexOf('id="g-b"') < html.indexOf('id="g-a"'),
    word: agentWord(sp, sides(sp)).text, right: choiceHint(sp, "right"), primary: (/class="btn primary"[^>]*data-mark="([a-z]+)"/.exec(html) || [])[1] };
  // Keys follow the screen: the older card is on the left, so the left arrow keeps it (data: right).
  els.__posts = [];
  for (const h of els.__on.keydown) h({ key: "ArrowLeft", target: document.body, preventDefault() {} });
  await new Promise((r) => setTimeout(r, 30));
  out.leftPost = els.__posts.map((p) => p.Action);
  // 2. Verdicts without a named fact.
  const stale = { id: "st", kind: "stale", single: true, a: f("engine", "2026-10-01", "Retries three times.", { type: "user_standard", kind: "user", ruling: true }), flag: { n: 1, since: "2026-10-01", detail: [] }, allowed: ["current", "retire"], needs_owner: true, state: "live",
    ask: { question: "Does your ruling engine still hold? Something it names was deleted after 2026-10-01 (1 commit(s), newest abc1234); an agent read it as: it would look outdated.", evidence: "abc1234 removed the retry loop." } };
  stale.b = stale.a;
  out.stale = agentWord(stale, { a: "fact", b: "fact" });
  const rel = { id: "q", kind: "contradiction", a: rule, b, allowed: ["left", "right", "both", "query"], ask: { question: "An agent would change the relations of your ruling release-post-changelog (query). What stands?", evidence: "Both cite live docs." } };
  out.rel = agentWord(rel, sides(rel));
  const own = Object.assign({}, rel, { ask: { question: "Which of these do you want for CI watching?", evidence: "" } });
  out.own = agentWord(own, sides(own)).text;
  // 3. A ruling change: their rule by name in the hints and the record.
  const rc = { id: "rc", kind: "ruling-change", a: rule, b: f("new", "2026-10-09", "The blog is back."), allowed: ["left", "right", "both", "query"] };
  out.rc = [choiceHint(rc, "left"), choiceHint(rc, "right"), pastFor(rc, "left"), question(rc)];
  // 4. Fact names and English stay as written.
  out.ev = [plainEvidence("cache-policy-2026-08-04 was replaced on 2026-09-10. A related fix landed.", { a: "older", b: "newer" }), plainEvidence("A predates B.", { a: "older", b: "newer" }),
    plainEvidence("B forbids npm audit fix as 'house style', and nothing backs it.", { a: "first", b: "second" }), plainEvidence("memory github.md:47 tells agents to watch. B bans it. A, by contrast, allows it.", { a: "first", b: "second" })];
  return JSON.stringify(out);
`, &got)
	sp := got["sp"].(map[string]any)
	if sp["q"] != "Does the older fact replace the newer one?" {
		t.Errorf("question %q", sp["q"])
	}
	if h, _ := json.Marshal(sp["heads"]); string(h) != `["Newer, 8 Oct","Older, 18 Sep"]` || sp["olderFirst"] != true {
		t.Errorf("cards %s, older first %v", h, sp["olderFirst"])
	}
	if !strings.HasPrefix(sp["word"].(string), "An agent suggests: keep the older. Only you can change your rule. Its reason: The blog was removed 18 Sep.") || sp["primary"] != "right" {
		t.Errorf("suggestion %q primary %v", sp["word"], sp["primary"])
	}
	if sp["right"] != "Keeps the older fact. Your rule, the newer fact, stops reaching agents; it stays in history." {
		t.Errorf("hint %q", sp["right"])
	}
	if p, _ := json.Marshal(got["leftPost"]); string(p) != `["right"]` {
		t.Errorf("the left arrow on a swapped pair posted %s, want right (keep the left card)", p)
	}
	st := got["stale"].(map[string]any)
	if st["mark"] != "retire" || !strings.HasPrefix(st["text"].(string), "An agent suggests: outdated, retire it. Only you can change your rule.") {
		t.Errorf("stale verdict %v", st)
	}
	if rel := got["rel"].(map[string]any); rel["mark"] != "query" || !strings.HasPrefix(rel["text"].(string), "An agent suggests: mark as conflicting.") {
		t.Errorf("relations verdict %v", rel)
	}
	if got["own"] != "An agent asks: Which of these do you want for CI watching?" {
		t.Errorf("own question %q", got["own"])
	}
	if rc, _ := json.Marshal(got["rc"]); string(rc) != `["Keeps your rule. The newer fact stops reaching agents; it stays in history.","Keeps the newer fact. Your rule stops reaching agents; it stays in history.","Kept your rule","A newer fact may change your rule. Does your rule still stand?"]` {
		t.Errorf("ruling change %s", rc)
	}
	if ev, _ := json.Marshal(got["ev"]); string(ev) != `["cache-policy-2026-08-04 was replaced on 10 Sep. A related fix landed.","The older fact predates the newer fact.","The second fact forbids npm audit fix as 'house style', and nothing backs it.","memory github.md:47 tells agents to watch. The second fact bans it. The first fact, by contrast, allows it."]` {
		t.Errorf("evidence %s", ev)
	}
}

// A pair the curator closed because its facts were replaced since says so in
// plain words, with the reason (and the facts' names) in its details, and
// offers the owner only to change it; before v0.9.16 there was no such record.
func TestPageShowsAFindingTheCuratorClosedAsMoot(t *testing.T) {
	var got map[string]string
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name, created) => ({ name, created, description: "d " + name, body: "", scope: "projects/memra", type: "design_decision", kind: "observed", path: "projects/memra/" + name + ".md" });
  const out = {};
  for (const [k, repointed, why] of [["moved", "c0ffee000001", "re-pointed to c / b, finding c0ffee000001: a is replaced by c; b stands"], ["moot", "", "moot: f is replaced by g, which is retired (2026-10-27), and no live fact replaces it"]]) {
    const it = { id: k, kind: "contradiction", a: f("a", "2026-09-01"), b: f("b", "2026-09-19"), allowed: ["reopen"], needs_owner: false, decision: { action: "moot", by: "curator", at: "2026-10-30T06:00:00Z", evidence: why, repointed } };
    items = [it]; view = [it]; cur = 0; sheet(); out[k] = els["sheet-inner"].innerHTML;
  }
  return JSON.stringify(out);
`, &got)
	tags := regexp.MustCompile(`<[^>]*>`)
	for k, want := range map[string]string{"moved": "so it is asked again about the facts that replaced them.", "moot": "and nothing is left to decide."} {
		html := got[k]
		for _, s := range []string{"The curator closed this: no longer a question", want, "Change this answer", "<details class=\"more\"><summary>Details</summary>"} {
			if !strings.Contains(html, s) {
				t.Errorf("%s lacks %q:\n%s", k, s, html)
			}
		}
		text := tags.ReplaceAllString(regexp.MustCompile(`(?s)<details class="more">.*?</details>`).ReplaceAllString(html, ""), " ")
		if strings.Contains(text, "You answered") || strings.Contains(text, "re-pointed") || strings.Contains(text, "retired (2026") {
			t.Errorf("%s reads as the owner's answer or shows the raw reason outside its details:\n%s", k, text)
		}
	}
}
