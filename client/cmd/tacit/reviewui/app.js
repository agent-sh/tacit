"use strict";
// Needs you: one decision at a time, in plain words. Shared by tacit review
// (index.html) and tacit app (app.html, with desk.js). Design: DESIGN.md.
const TOKEN = document.querySelector('meta[name="tacit-token"]').content;
// tacit app sets tacitCall (api.js): the desktop window's bindings, or its
// HTTP bridge in a browser. The review page talks to its own endpoints.
const CALL = globalThis.tacitCall;

// --- icons: one family, 20 px grid, one stroke weight, round joins ---
const ICONS = {
  check: '<path d="M4.5 10.5l3.5 3.5 7.5-8"/>',
  checks: '<path d="M2 10.8l3 3 6.5-7.3"/><path d="M10 13.6l.2.2 7.3-7.8"/>',
  x: '<path d="M5.5 5.5l9 9M14.5 5.5l-9 9"/>',
  conflict: '<path d="M4.5 8h11M4.5 12h11M12.6 4.2l-5.2 11.6"/>',
  skip: '<path d="M4.5 5.5l5 4.5-5 4.5M10.5 5.5l5 4.5-5 4.5"/>',
  unsure: '<circle cx="10" cy="10" r="7"/><path d="M8 8.2a2 2 0 1 1 2.8 1.8c-.5.3-.8.7-.8 1.3v.3"/><path d="M10 14.1v.1"/>',
  archive: '<rect x="3" y="4" width="14" height="4" rx="1"/><path d="M4.5 8v7a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1V8M8 11h4"/>',
  unarchive: '<rect x="3" y="4" width="14" height="4" rx="1"/><path d="M4.5 8v7a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1V8M10 14.5v-4M8 12.3l2-2 2 2"/>',
  undo: '<path d="M7.5 5.5L4 9l3.5 3.5"/><path d="M4 9h8a4 4 0 0 1 0 8h-2"/>',
  edit: '<path d="M13.4 3.9l2.7 2.7-9.2 9.2-3.4.7.7-3.4z"/><path d="M11.7 5.6l2.7 2.7"/>',
  plus: '<path d="M10 4.5v11M4.5 10h11"/>',
  search: '<circle cx="9" cy="9" r="5"/><path d="M12.8 12.8l3.7 3.7"/>',
  filter: '<path d="M3.5 6h13M6 10h8M8.5 14h3"/>',
  comment: '<path d="M4.5 4.5h11a1.5 1.5 0 0 1 1.5 1.5v6.5a1.5 1.5 0 0 1-1.5 1.5H9.5L6 16.8V14H4.5A1.5 1.5 0 0 1 3 12.5V6a1.5 1.5 0 0 1 1.5-1.5z"/>',
  prev: '<path d="M12 5l-5 5 5 5"/>',
  next: '<path d="M8 5l5 5-5 5"/>',
  link: '<path d="M8.6 11.4l2.8-2.8"/><path d="M9.2 6.6l1.1-1.1a3 3 0 0 1 4.2 4.2l-1.1 1.1M10.8 13.4l-1.1 1.1a3 3 0 0 1-4.2-4.2l1.1-1.1"/>',
  take: '<path d="M15.5 8.5A5.8 5.8 0 0 0 5 6.6M4.5 11.5a5.8 5.8 0 0 0 10.5 1.9"/><path d="M5 3.4v3.2h3.2M15 16.6v-3.2h-3.2"/>',
  moon: '<path d="M15.8 12.6A6.2 6.2 0 0 1 7.4 4.2a6.2 6.2 0 1 0 8.4 8.4z"/>',
  sun: '<circle cx="10" cy="10" r="3.2"/><path d="M10 2.8v1.6M10 15.6v1.6M2.8 10h1.6M15.6 10h1.6M4.9 4.9L6 6M14 14l1.1 1.1M4.9 15.1L6 14M14 6l1.1-1.1"/>',
};
const icon = (n) => ICONS[n] ? `<svg class="ic" viewBox="0 0 20 20" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">${ICONS[n]}</svg>` : "";
// A button: the icon leads the label, the key trails it. Only the primary action takes the accent.
const btn = (id, ic, label, key, opts = {}) => `<button type="button" class="btn${opts.primary ? " primary" : ""}${opts.chosen ? " chosen" : ""}"${id ? ` id="${id}"` : ""}${opts.mark ? ` data-mark="${opts.mark}"` : ""}${opts.off ? " disabled" : ""}${opts.chosen ? ' aria-pressed="true"' : ""}>${icon(ic)}<span class="label">${label}</span>${key ? `<kbd${key === "←" || key === "→" ? ' class="arrow"' : ""}>${key}</kbd>` : ""}</button>`;

// --- plain words ---
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const THIS_YEAR = new Date().getFullYear();
// 2026-10-03 reads "3 Oct"; another year keeps its year.
function plainDate(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(String(s || ""));
  if (!m) return String(s || "");
  return `${+m[3]} ${MON[+m[2] - 1]}${+m[1] !== THIS_YEAR ? " " + m[1] : ""}`;
}
// Where a fact reaches, from its scope.
function whereWords(scope) {
  const s = String(scope || "");
  let m;
  if ((m = /^projects\/(.+)$/.exec(s))) return "in " + m[1];
  if ((m = /^derived\/(.+)$/.exec(s))) return `in ${m[1]}, computed from git`;
  if ((m = /^machines\/(.+)$/.exec(s))) return "on " + m[1];
  if (s === "user") return "about you, in every repo";
  if (s === "system") return "for every agent, everywhere";
  return s;
}
const cap = (s) => s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
// Their rule: their own words, as the server decided (fact.IsRuling). A user_standard
// an agent filed is not one.
const isRule = (f) => !!(f && f.ruling);

// The finding kinds. q is the question (a function of the two facts' names
// when the names decide it), note is said when no agent left a word on it.
// Choice labels come from choiceLabel.
const KINDS = {
  "contradiction":      { label: "Facts that disagree", q: "These two facts disagree. Which is true now?", dated: true,
    note: "The curator's check says an agent following both would act two ways. Often one is a refinement or an exception, and both are true." },
  "supersede-proposed": { label: "Newer may replace older", dated: true, roles: true, q: (s) => `Does the ${s.b} fact replace the ${s.a} one?`,
    note: (s) => `The curator thinks the ${s.b} fact replaces the ${s.a} one.` },
  "result-superseded":  { label: "Newer numbers", q: "A newer number exists for this measurement. Use it?", a: "saved", b: "from git", aHead: "Saved number", bHead: "Number from git",
    note: "A newer commit recorded a different value for what the saved fact quotes. Git holds what the committer, usually an agent, wrote." },
  "ruling-change":      { label: "Changes to your rules", rule: true, q: (s) => `${cap(an(s.b))} fact may change your rule. Does your rule still stand?`,
    note: "The curator never changes your rule by itself." },
  "supersede-applied":  { label: "Replaced by the curator", dated: true, roles: true, q: (s) => `The curator replaced the ${s.a} fact with the ${s.b} one. Keep it that way?`,
    note: "Done by itself because both came from one note or say nearly the same thing." },
  "duplicate":          { label: "Duplicates", q: "These two facts say the same thing. Keep one?", dated: true,
    note: "Two facts from one curator run that read alike." },
  // Two files hold one name: a copy a tool outside tacit left in a second folder.
  "name":               { label: "One name, two files", q: "Two files hold this fact's name. Which one stands?", dated: true,
    note: (s, it) => it && it.crowded
      ? `More files hold this name: ${(it.also || []).join(", ")}. No answer here can say which file replaced which. Open each file from its card and retire the copies.`
      : "A name belongs to one fact. A tool outside tacit left a second file of this one. Keeping one retires the other." },
  // tacit app only: one of your rules links to a fact that is not there.
  "link":               { label: "Broken links in your rules", q: "Your rule links to a fact that does not exist." },
  // One fact, not a pair: git deleted something it names after the day it was last current.
  "stale":              { label: "Maybe outdated", q: "Git says something this fact names was deleted. Is it still true?", qRule: "Git says something your rule names was deleted. Does your rule still hold?" },
  // One fact: the curator's regular check went against one of your facts, and changed nothing in it.
  "check":              { label: "Checked by the curator", q: "The curator checked this fact against what tacit knows now and reads it differently. Is it still true?", qRule: "The curator checked your rule against what tacit knows now and reads it differently. Does your rule still hold?", side: "What the curator found" },
  // A question the reviewer had for you at your prompt, which an agent closed with its proof.
  "question":           { label: "Questions an agent closed", q: "The reviewer had this question for you about an agent's note. An agent closed it." },
};
// How an agent proved the answer to a question it closed (SPEC.md, "Closing questions").
const PROOF = { git: "git history", file: "the current files", config: "a configuration", words: "your own words", gone: "its subject is gone", moot: "a fact it asked about was retired or replaced" };
// Where words of yours were found, said as the harness you typed them in.
const SESSION_WORDS = { claude: "a Claude Code session", codex: "a Codex session", pi: "a pi session", hermes: "a Hermes session", note: "a note of yours" };
const sessionWords = (from) => SESSION_WORDS[String(from || "").split(":")[0]] || "one of your sessions";
const an = (w) => (/^[aeiou]/i.test(w) ? "an " : "a ") + w;
// The day a fact speaks for, the one its card prints: the newest date in its
// text that is not after the day it was written (as the timeline reads it),
// else the day it was written.
function asOf(f) {
  const made = f.created || "9999-12-31";
  const ds = (String(f.description || "") + " " + String(f.body || "")).match(/\b20\d\d-\d\d-\d\d\b/g) || [];
  const ok = ds.filter((d) => d <= made).sort();
  return ok.length ? ok[ok.length - 1] : f.created || "";
}
// The two facts of a pair, named by the dates their cards print: older and
// newer. Where the curator's roles (A may be replaced by B) run against the
// dates, the names still follow the dates and the question says it plainly.
function sides(it) {
  const K = KINDS[it.kind] || {};
  const da = asOf(it.a || {}), db = asOf(it.b || {});
  if (K.rule) {
    const b = da && db && db < da ? "older" : "newer";
    return { a: "your rule", b, aHead: "Your rule", bHead: cap(b) };
  }
  if (!K.dated) return { a: K.a || "first", b: K.b || "second", aHead: K.aHead || cap(K.a || "first"), bHead: K.bHead || cap(K.b || "second") };
  if (da && db && da !== db) return da < db ? { a: "older", b: "newer", aHead: "Older", bHead: "Newer" } : { a: "newer", b: "older", aHead: "Newer", bHead: "Older" };
  return K.roles ? { a: "older", b: "newer", aHead: "Older", bHead: "Newer" } : { a: "first", b: "second", aHead: "First", bHead: "Second" };
}
const question = (it) => { const K = KINDS[it.kind] || {}; return typeof K.q === "function" ? K.q(sides(it)) : K.q || ""; };
const kindNote = (it) => { const K = KINDS[it.kind] || {}; return typeof K.note === "function" ? K.note(sides(it), it) : K.note || ""; };
// The older fact is always the left card; when the data has it on the right, the cards and the arrows swap.
const flipped = (it) => !it.single && !!KINDS[it.kind] && sides(it).a === "newer";
// A fact as a sentence names it: "your rule", "the saved number", "the older fact".
const np = (n) => n === "your rule" ? "your rule" : n === "from git" ? "the number from git" : n === "saved" ? "the saved number" : `the ${n} fact`;
// What a choice says on its button.
function choiceLabel(it, mark) {
  const s = sides(it), k = it.kind;
  if (k === "result-superseded" && mark === "right") return "Use the number from git";
  if (k === "result-superseded" && mark === "left") return "Keep the saved number";
  if (k === "ruling-change" && mark === "left") return "Keep your rule";
  if (k === "ruling-change" && mark === "right") return `Use the ${s.b} fact`;
  if (k === "supersede-applied" && mark === "right") return "Keep it replaced";
  if (k === "supersede-applied" && mark === "left") return `Keep the ${s.a} instead`;
  if (k === "supersede-applied" && mark === "both") return "Undo, both are true";
  return { left: `Keep the ${s.a}`, right: `Keep the ${s.b}`, both: "Both are true", query: "Mark as conflicting", wrong: "Neither is true", skip: "Not sure, skip",
    current: "Still true", retire: "Outdated, retire it", fix: "Detail to fix", later: "Skip for now" }[mark] || mark;
}
// "Your rule stops reaching agents" whenever the fact that goes is their rule.
function goes(name, f) {
  if (name === "your rule") return "Your rule stops reaching agents";
  return isRule(f) ? `Your rule, ${np(name)}, stops reaching agents` : `${cap(np(name))} stops reaching agents`;
}
// What a choice does, said under the choices when it is pointed at. In label
// mode nothing in the store changes, and the hint says so.
function choiceHint(it, mark) {
  const s = sides(it);
  if (labelling && ["left", "right", "both", "query", "wrong"].includes(mark)) return `Recorded as your label: ${choiceLabel(it, mark).toLowerCase()}. The store does not change.`;
  switch (mark) {
    case "left": return `Keeps ${np(s.a)}. ${goes(s.b, it.b)}; it stays in history.`;
    case "right": return `Keeps ${np(s.b)}. ${goes(s.a, it.a)}; it stays in history.`;
    case "both": return it.kind === "supersede-applied" ? "Undoes the replacement: both reach agents again." : "Both stay and both reach agents.";
    case "query": return "Both stay, marked as conflicting: agents are shown one of them at a time.";
    case "wrong": return `Neither is true: both ${it.kind === "name" ? "files" : "facts"} stop reaching agents${isRule(it.a) || isRule(it.b) ? ", your rule too" : ""}. They stay in history, and Change this answer brings both back.`;
    case "skip": return "Left out of the comparison.";
    case "current": return isRule(it.a) ? "Keeps your rule and dates it today." : "Keeps the fact and dates it today.";
    case "retire": return isRule(it.a) ? "Your rule stops reaching agents. It stays in history and can be restored." : "The fact stops reaching agents. It stays in history and can be restored.";
    case "later": return "Moves on. Nothing is saved, and it stays here.";
    default: return "";
  }
}
// What a choice did, for the slip and the record of an answer.
function pastFor(it, mark) {
  const s = sides(it);
  return { left: `Kept ${np(s.a)}`, right: `Kept ${np(s.b)}`, both: "Both are true", query: "Marked as conflicting", wrong: labelling ? "Neither is true" : it.kind === "question" ? "The note is wrong, its facts retired" : "Neither is true, both retired", skip: "Skipped",
    current: "Still true", retire: "Outdated, retired", fix: "A detail to fix", holds: "The note holds", moot: "No longer a question" }[mark] || "Answered";
}
// The agent's evidence calls the two facts A and B. A standalone B is always a
// fact (English has no bare B), and once the text uses B it uses the
// convention, so a standalone A is one too. Without a B, A is a fact only
// before 's, a bracket or one of the verbs a fact takes, so "A related fix"
// stays English. Letters inside words and names are never touched, and only
// dates that stand alone are said as days, never one inside a fact's name.
const A_VERBS = "is|was|says|said|states|stated|has|had|keeps|makes|names|gives|adds|sets|notes|lists|records|describes|covers|quotes|predates|postdates|refines|contradicts|agrees|matches|claims|applies|mentions|cites|replaces|supersedes|wins|holds|stays|still|already|only|also|and|or";
function plainEvidence(text, s) {
  let t = String(text || "");
  const usesB = /(^|[^\w-])B('s)?(?![\w-])/.test(t);
  for (const [L, n] of [["A", s.a], ["B", s.b]]) {
    const ref = L === "B" || usesB ? "(?![\\w-])" : `(?=\\s*\\(|\\s+(?:${A_VERBS})\\b)`;
    t = t.replace(new RegExp(`(^|[^\\w-])${L}'s(?![\\w-])`, "g"), (m, p) => p + np(n) + "'s")
      .replace(new RegExp(`(^|[^\\w-])${L}${ref}`, "g"), (m, p) => p + np(n));
  }
  t = t.replace(/(^|[^\w-])(\d{4}-\d{2}-\d{2})(?![\w-])/g, (m, p, d) => p + plainDate(d));
  return t.replace(/(^|[.!?]\s+)(the|your) /g, (m, p, w) => p + cap(w) + " ");
}
// The one-line summary of a stale finding: what git deleted, where and when.
// Its lines are "2026-10-09 abc1234 repo: subject - NAME, NAME".
function staleSummary(flag) {
  const fl = flag || {}, out = [];
  const rows = (fl.detail || []).map((l) => /^(\d{4}-\d{2}-\d{2}) ([0-9a-f]{4,40}) ([^:\s]+): (.*) - (.+)$/.exec(String(l).trim())).filter(Boolean);
  if (rows.length) {
    const r = rows.reduce((x, y) => (y[1] > x[1] ? y : x));
    const names = r[5].split(/,\s*/).filter(Boolean);
    const named = names.length > 3 ? names.slice(0, 3).join(", ") + ` and ${names.length - 3} more` : names.length > 1 ? names.slice(0, -1).join(", ") + " and " + names[names.length - 1] : names[0];
    out.push(`${named} ${names.length > 1 ? "were" : "was"} deleted in ${r[3]} on ${plainDate(r[1])} (“${r[4]}”).`);
  }
  const more = (fl.n || 0) - (rows.length ? 1 : 0);
  if (more > 0) out.push(`${more} more commit${more === 1 ? "" : "s"} since ${plainDate(fl.since)} deleted something it names.`);
  if (!out.length && fl.n) out.push(`${fl.n} commit${fl.n === 1 ? "" : "s"} since ${plainDate(fl.since)} deleted something it names.`);
  return out;
}

const ORDER = { "contradiction": ["right", "left", "both", "query"], "supersede-applied": ["right", "both", "left", "query"], "ruling-change": ["left", "right", "both", "query"], "name": ["right", "left", "both"], "stale": ["current", "retire"] };
// The choice that keeps the newer fact leads, whichever side the data has it on.
const orderFor = (k, it) => (ORDER[k] || ["right", "left", "both", "query"]).map((m) => (it && flipped(it) && (m === "left" || m === "right") ? (m === "left" ? "right" : "left") : m)).concat(["wrong", "skip"].filter((m) => it && (can(it, m) || it.mark === m)));
// A mark sets what the store says about the pair; the server lists the marks
// it accepts now in it.allowed. it.mark is the recorded mark in this item's
// orientation; it.state is what the store says now. Cards always show the store.
const can = (it, mark) => (it.allowed || []).includes(mark);
const standing = (it) => ["left", "right", "query", "wrong"].includes(it.state) ? it.state : "";
// What each mark does to each card: stays, retired, or conflicting.
const MARKS = {
  left: { left: "stet", right: "dele" }, right: { left: "dele", right: "stet" }, both: { left: "stet", right: "stet" }, query: { left: "query", right: "query" },
  current: { left: "stet" }, retire: { left: "dele" }, fix: { left: "query" }, wrong: { left: "dele", right: "dele" }, skip: {},
};
const WHEN = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

let items = [], view = [], cur = 0, busy = false, lastDecided = null;
// The line shown on a machine that cannot write (no curator key, a read-only clone).
let viewOnly = "";
// Label mode: the owner can say no source settles a pair and only the owner knows.
let labelling = false; const flagged = new Set();
const state = { kind: "", status: "open", scope: "", q: "" };
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function load(keepId, focusMark) {
  const data = CALL ? await CALL("Queue") : await (await fetch("/api/items", { cache: "no-store", headers: { "X-Tacit-Token": TOKEN } })).json();
  items = data.items || [];
  // A one-fact finding is its own other half for the filters.
  for (const it of items) if (it.single && !it.b) it.b = it.a;
  labelling = !!data.labelling;
  const h = $("host"); if (h) h.textContent = data.host ? "on " + data.host : "";
  // View only: every choice comes without a mark the server accepts, so each
  // is drawn disabled, and one plain line says where to answer.
  viewOnly = data.view_only || "";
  if (document.body && document.body.classList) document.body.classList.toggle("view-only", !!viewOnly);
  let vl = $("view-only");
  if (!vl && viewOnly && h && h.insertAdjacentHTML) { h.insertAdjacentHTML("afterend", '<span class="view-only-line" id="view-only"></span>'); vl = $("view-only"); }
  if (vl) vl.textContent = viewOnly;
  filters();
  apply(keepId);
  if (focusMark) firstMark();
}
// Focus moves to the first choice after an answer, without previewing it.
let quiet = false;
function firstMark() { const b = document.querySelector("#sheet-inner .choices .btn:not([disabled])") || $("reopen"); if (b) { quiet = true; b.focus({ preventScroll: true }); quiet = false; } }
function filters() {
  const open = items.filter((i) => i.needs_owner);
  const counts = {}; for (const it of open) counts[it.kind] = (counts[it.kind] || 0) + 1;
  const kinds = $("kinds");
  if (kinds) {
    kinds.innerHTML = `<button type="button" class="chip" data-kind="" aria-pressed="${state.kind === ""}">Every kind</button>` +
      Object.keys(KINDS).filter((k) => counts[k] || state.kind === k || (state.status !== "open" && items.some((i) => i.kind === k))).map((k) => `<button type="button" class="chip" data-kind="${k}" aria-pressed="${state.kind === k}">${KINDS[k].label}<span class="n">${counts[k] || 0}</span></button>`).join("");
    kinds.querySelectorAll(".chip").forEach((b) => b.onclick = () => { state.kind = b.dataset.kind; cur = 0; filters(); apply(); kinds.querySelector(`[data-kind="${b.dataset.kind}"]`).focus(); });
  }
  const scopes = [...new Set(items.flatMap((i) => [i.a.scope, i.b.scope]).filter(Boolean))].sort();
  const sc = $("scope");
  if (sc) sc.innerHTML = '<option value="">Anywhere</option>' + scopes.map((s) => `<option value="${esc(s)}"${s === state.scope ? " selected" : ""}>${esc(cap(whereWords(s)))}</option>`).join("");
  const n = $("needs-n"); if (n) n.textContent = open.length || "";
  const fb = $("filter-btn");
  if (fb) { const on = [state.kind, state.scope, state.q, state.status !== "open"].filter(Boolean).length; const l = fb.querySelector && fb.querySelector(".label"); if (l) l.textContent = on ? `Filter (${on})` : "Filter"; }
}
function leftLine() {
  const open = items.filter((i) => i.needs_owner).length, el = $("left-n");
  if (!el) return;
  if (state.status === "decided") el.textContent = `Past answers, ${view.length ? cur + 1 : 0} of ${view.length}`;
  else if (state.status === "all") el.textContent = `Everything, ${view.length ? cur + 1 : 0} of ${view.length}`;
  else el.textContent = open ? `${open} left` : "";
  for (const id of ["prev", "next"]) { const b = $(id); if (b) b.hidden = view.length < 2; }
}
function apply(keepId) {
  const q = state.q.toLowerCase();
  view = items.filter((it) => (!state.kind || it.kind === state.kind) &&
    (state.status === "all" || (state.status === "open" ? it.needs_owner : !it.needs_owner)) &&
    (!state.scope || it.a.scope === state.scope || it.b.scope === state.scope) &&
    (!q || [it.a.name, it.b.name, it.a.description, it.b.description].join(" ").toLowerCase().includes(q)));
  const i = keepId ? view.findIndex((x) => x.id === keepId) : -1;
  cur = i >= 0 ? i : Math.max(0, Math.min(cur, view.length - 1));
  sheet();
}
// One fact, as the owner reads it: what it says first, where and when after,
// its name and file only under Details.
const TYPE_WORDS = { user_standard: "preference", design_decision: "design decision", past_mistake: "past mistake", machine_fact: "machine fact", code_change: "code change", hot_spot: "hot spot", structure: "module map", history: "history", user: "fact about you", reference: "reference", result: "result", result_ref: "result, search only", unclassified: "note" };
const typeWords = (t) => TYPE_WORDS[t] || String(t || "").replace(/_/g, " ");
const SOURCE_WORDS = { user: "said by you", observed: "seen by an agent", web: "from the web", derived: "computed from git" };
// byPath opens the card's own file: two cards of one name are two files.
function card(f, side, head, byPath) {
  const rule = isRule(f);
  const dates = [f.created ? "written " + plainDate(f.created) : "", f.updated && f.updated !== f.created ? "changed " + plainDate(f.updated) : ""].filter(Boolean).join(", ");
  const name = CALL && !f.missing ? `<button type="button" class="open-fact" data-fact="${esc(byPath && f.path ? f.path : f.name)}">${esc(f.name)}</button>` : esc(f.name);
  return `<article class="card${f.missing ? " missing" : ""}" id="g-${side}" aria-label="${esc(head)}">
    <div class="card-head"><span class="who">${esc(head)}${asOf(f) ? ", " + esc(plainDate(asOf(f))) : ""}</span>${rule && head !== "Your rule" ? '<span class="tag">Your rule</span>' : ""}
      <span class="state s-dele">Stops reaching agents</span><span class="state s-dele-p">Would stop reaching agents</span><span class="state s-stet">Stays</span><span class="state s-query">Marked as conflicting</span></div>
    <p class="text">${f.missing ? "This fact is no longer in the store." : esc(f.description)}</p>
    ${f.missing || !f.body || f.body === f.description ? "" : f.body.length > 320
      ? `<p class="body clamped">${esc(f.body)}</p><button type="button" class="link-btn show-all" aria-expanded="false">Show all</button>`
      : `<p class="body">${esc(f.body)}</p>`}
    ${f.missing ? "" : `<p class="where">${esc(cap(whereWords(f.scope)))}${f.retired ? `, retired ${esc(plainDate(f.retired))}` : ""}${f.superseded_by ? ", replaced" : ""}</p>`}
    ${f.missing ? "" : `<details class="more"><summary>Details</summary><dl class="fields">
      <dt>Name</dt><dd>${name}</dd><dt>Kind</dt><dd>${esc(typeWords(f.type))}${f.kind ? ", " + esc(SOURCE_WORDS[f.kind] || f.kind) : ""}</dd>
      ${dates ? `<dt>Dates</dt><dd>${esc(cap(dates))}</dd>` : ""}<dt>File</dt><dd><code>${esc(f.path || "")}</code></dd></dl></details>`}
  </article>`;
}
// What the agent said, as one plain sentence with its reason.
// The questions the server writes when it turns an agent's verdict into a
// question for the owner; each says which answer the agent would give.
const ASK_TEMPLATES = [
  [/^An agent would change the relations of your ruling .+ \((left|right|both|query)\)\. What stands\?$/, (x) => x[1]],
  [/^Does your ruling .+ still hold\? An agent reads .+ as the current one\.$/, null],
  [/^Does your ruling .+ still hold\? Something it names was deleted .* it would (look outdated|still hold|need a detail fixed)\.$/, (x) => ({ "look outdated": "retire", "still hold": "current", "need a detail fixed": "fix" })[x[1]]],
  [/^You left a note on this\. An agent reads .+ as the current one; does .+ still hold\?$/, null],
  [/^You left a note on this\. An agent reads .+ as outdated; is it\?$/, () => "retire"],
];
// What the agent said, as one plain sentence with its reason, and the choice
// it points to (the primary button).
function agentWord(it, s) {
  const ask = !it.decision && it.ask, d = it.decision;
  const rule = isRule(it.a) || (!it.single && isRule(it.b));
  const why = (e) => (e ? " Its reason: " + plainEvidence(e, s) : "");
  if (ask) {
    let m = "", templ = false;
    // A name finding's two files share the name: its agent proposes a path.
    const ka = it.kind === "name" ? it.a.path : it.a.name, kb = it.kind === "name" ? it.b.path : it.b.name;
    if (ask.proposed) m = ask.proposed === ka ? "left" : ask.proposed === kb ? "right" : "";
    const own = String(ask.question || "").trim();
    for (const [re, f] of ASK_TEMPLATES) { const x = re.exec(own); if (x) { templ = true; if (!m && f) m = f(x) || ""; } }
    if (m === "fix") return { text: "An agent says it is true but a detail changed." + (rule ? " Only you can change your rule." : "") + why(ask.evidence), mark: "" };
    if (m) return { text: `An agent suggests: ${choiceLabel(it, m).toLowerCase()}.` + (rule ? " Only you can change your rule." : "") + why(ask.evidence), mark: m };
    if (own && !templ) return { text: `An agent asks: ${own}` + why(ask.evidence), mark: "" };
    return { text: "An agent checked the sources and could not settle this." + why(ask.evidence), mark: "" };
  }
  if (d && d.by === "agent" && it.needs_owner && d.action === "query") return { text: "An agent marked these as conflicting and left the call to you." + why(d.evidence), mark: "" };
  if (d && d.by === "agent" && d.action === "fix") return { text: "An agent says it is true but a detail changed." + why(d.evidence), mark: "" };
  return { text: "", mark: "" };
}
// The record of an answer already made, with the way to change it.
function recordHTML(it) {
  const d = it.decision; if (!d) return "";
  const who = d.by === "agent" ? "An agent answered" : "You answered";
  const reopen = can(it, "reopen") ? btn("reopen", "undo", "Change this answer", "z") : "";
  // A pair whose fact a third fact replaced: closed by code. The plain words
  // say what became of it; its reason, with the facts' names, is in details.
  // A question an agent left for you, closed: by an agent that proved the
  // answer, or by the curator when a fact of it was retired or replaced.
  const q = d.question || (it.ask && it.ask.question) || "";
  const asked = q && it.kind !== "question" ? `<p class="evidence"><b>The question it closed:</b> ${esc(q)}</p>` : "";
  // Your own answer, typed in a session, which an agent only passed on.
  if (d.by === "agent" && d.proof && d.relayed) {
    return `<div class="record"><p><b>Your answer, relayed by an agent: ${esc(pastFor(it, it.mark || d.action))}</b>, ${esc(WHEN.format(new Date(d.at)))}.</p>${reopen}</div>${asked}`
      + `<p class="evidence"><b>Your words</b> (${esc(plainDate(d.words_on || ""))}, in ${esc(sessionWords(d.words_from))}): “${esc(d.words || "")}”</p>`
      + `<p class="evidence"><b>How the agent read them:</b> ${esc(d.reason || "")}</p>`
      + (d.evidence ? `<details class="more"><summary>Details</summary><p>${esc(plainEvidence(d.evidence, sides(it)))}</p></details>` : "");
  }
  if (d.by === "agent" && d.proof) {
    const how = PROOF[d.proof] || d.proof;
    return `<div class="record"><p><b>An agent closed your question: ${esc(pastFor(it, it.mark || d.action))}</b>, proved by ${esc(how)}, ${esc(WHEN.format(new Date(d.at)))}.</p>${reopen}</div>${asked}`
      + `<p class="evidence"><b>Why:</b> ${esc(d.reason || "")}</p><p class="evidence"><b>What it checked:</b> ${esc(plainEvidence(d.evidence, sides(it)))}</p>`
      + (d.words ? `<p class="evidence"><b>Your words it rests on</b> (${esc(plainDate(d.words_on || ""))}): “${esc(d.words)}”</p>` : "");
  }
  if (d.action === "moot") {
    const what = d.closes ? "A fact it asks about was retired or replaced since, so nothing is left to ask you." + (d.repointed ? " The facts that replaced them are checked again." : "")
      : d.repointed ? "The facts it asks about were replaced since, so it is asked again about the facts that replaced them." : "The facts it asks about were replaced since, and nothing is left to decide.";
    return `<div class="record"><p><b>The curator closed this: no longer a question</b>, ${esc(WHEN.format(new Date(d.at)))}.</p>${reopen}</div>${asked}<p class="evidence">${what}</p>${d.evidence ? `<details class="more"><summary>Details</summary><p>${esc(d.evidence)}</p></details>` : ""}`;
  }
  return `<div class="record"><p><b>${who}: ${esc(pastFor(it, it.mark || d.action))}</b>${d.owner_only ? ", only you could know" : ""}, ${esc(WHEN.format(new Date(d.at)))}.</p>${reopen}</div>${d.comment ? `<p class="your-note"><b>Your note:</b> ${esc(d.comment)}</p>` : ""}${d.evidence && d.by === "agent" && !it.needs_owner ? `<p class="evidence">${esc(plainEvidence(d.evidence, sides(it)))}</p>` : ""}`;
}
// The notes the owner left on a finding without answering it.
function notesHTML(it) {
  const ns = it.notes || [];
  if (!ns.length) return "";
  return `<div class="notes"><p class="flabel">Your notes</p><ul>${ns.map((n) => `<li><time>${esc(plainDate(n.at))}</time><span>${esc(n.text)}</span></li>`).join("")}</ul></div>`;
}
// A note in their words: sent with any answer, or on its own (the finding stays open).
const noteDrafts = new Map(); // finding id -> what the owner has typed, until it is sent
function noteBoxHTML(it) {
  if ((it.decision && !it.needs_owner) || viewOnly) return "";
  return `<div class="note-box"><label class="flabel" for="note">Add a note (optional)</label>
    <textarea id="note" rows="2" placeholder="In your words. It is kept with your answer, and the curator reads it.">${esc(noteDrafts.get(it.id) || "")}</textarea>
    <div class="note-actions">${btn("comment-only", "comment", "Comment without answering", "", { off: !(noteDrafts.get(it.id) || "").trim() })}<span class="tl-sub">Leaves this open for later.</span></div></div>`;
}
function wireNote(it) {
  const t = $("note"), b = $("comment-only");
  if (!t || !t.addEventListener) return;
  t.addEventListener("input", () => { noteDrafts.set(it.id, t.value); if (b) b.disabled = !t.value.trim(); });
  if (b) b.onclick = () => commentOnly(it);
}
const noteOf = (it) => (noteDrafts.get(it.id) || "").trim();
let sideNouns = { A: "left", B: "right", "A,B": "both" };
function sheet() {
  const inner = $("sheet-inner");
  leftLine();
  if (!view.length) {
    const left = items.filter((i) => i.needs_owner).length;
    const filtered = state.kind || state.scope || state.q;
    inner.innerHTML = filtered && left
      ? `<div class="done"><h2>Nothing matches the filter.</h2><p>${left} still need${left === 1 ? "s" : ""} you.</p>${btn("clear-filter", "", "Clear the filter", "")}</div>`
      : state.status === "open" || !items.length
        ? `<div class="done"><h2>Nothing needs you.</h2><p>When the curator finds something only you can answer, it shows up here.</p>${items.length ? btn("see-past", "", "See past answers", "") : ""}</div>`
        : `<div class="done"><h2>No past answers here.</h2>${btn("see-open", "", "Back to what needs you", "")}</div>`;
    const on = (id, fn) => { const b = $(id); if (b) b.onclick = fn; };
    on("clear-filter", () => setFilter({ kind: "", scope: "", q: "", status: "open" }));
    on("see-past", () => setFilter({ status: "decided" })); on("see-open", () => setFilter({ status: "open" }));
    return;
  }
  const it = view[cur];
  if (it.link && typeof linkSheet === "function") { linkSheet(it); return; }
  if (it.kind === "question") { questionSheet(it); return; }
  if (it.single) { staleSheet(it); return; }
  const K = KINDS[it.kind], s = sides(it), d = it.decision, made = standing(it);
  sideNouns = { A: s.a, B: s.b, "A,B": "both" };
  const missing = it.a.missing || it.b.missing;
  const word = agentWord(it, s);
  const third = (f, other) => f.superseded_by && f.superseded_by !== other.name ? f : null;
  const blocked = third(it.a, it.b) || third(it.b, it.a);
  const why = d && it.stale ? "The store changed since this answer. Change it to answer again."
    : missing ? "One fact is gone from the store, so only Both are true is left. It closes this."
    : it.state === "tangled" ? "These two retire each other. Keep one, or say both are true."
    : blocked && !d ? `One of them was already replaced by another fact (${blocked.superseded_by}). Change that answer first to retire it here.`
    : "";
  inner.innerHTML = `
    <h2 class="question">${esc(question(it))}</h2>
    ${word.text ? `<p class="suggest">${esc(word.text)}</p>` : kindNote(it) && !d ? `<p class="suggest quiet">${esc(kindNote(it))}</p>` : ""}
    ${recordHTML(it)}${notesHTML(it)}
    <div class="pair">${flipped(it) ? card(it.b, "b", s.bHead, it.kind === "name") + card(it.a, "a", s.aHead, it.kind === "name") : card(it.a, "a", s.aHead, it.kind === "name") + card(it.b, "b", s.bHead, it.kind === "name")}</div>
    ${choicesHTML(it, word.mark)}
    ${missing ? "" : '<section class="git" id="timeline" aria-labelledby="tl-h" aria-busy="true"><h3 id="tl-h">What git shows</h3><p class="tl-sub">Reading git.</p></section>'}`;
  if (why) { const c = inner.querySelector(".hint"); if (c) { c.textContent = why; c.dataset.fixed = "1"; } }
  if (made) show(made, false);
  if (!missing) timeline(it);
  wire(it, made);
}
// The choices: the agent's suggestion first and in the accent, the rest equal.
function choicesHTML(it, suggested) {
  const d = it.decision;
  let order = orderFor(it.single ? "stale" : it.kind, it);
  if (suggested && order.includes(suggested)) order = [suggested, ...order.filter((m) => m !== suggested)];
  const ICON = { left: "check", right: "check", both: "checks", query: "conflict", wrong: "x", skip: "unsure", current: "check", retire: "archive" };
  const f = flipped(it);
  const KEY = { left: f ? "→" : "←", right: f ? "←" : "→", both: "s", query: "q", wrong: "w", skip: "x", current: "s", retire: "r" };
  const b = order.map((m) => btn("", ICON[m], esc(choiceLabel(it, m)), KEY[m], { mark: m, off: !can(it, m), primary: !d && m === suggested, chosen: d && it.mark === m })).join("");
  const later = !d && !can(it, "skip") ? btn("later", "skip", "Skip for now", "j", { mark: "later" }) : "";
  const only = labelling && !d ? `<button type="button" class="btn ghost" id="only" aria-pressed="${flagged.has(it.id)}">${icon("unsure")}<span class="label">Only I can know</span><kbd>o</kbd></button>` : "";
  const lead = suggested || order.find((m) => can(it, m)) || "";
  return `<div class="choices" role="group" aria-label="Your answer">${b}${later}${only}</div><p class="hint" id="hint" aria-live="polite">${lead && !d ? esc(choiceHint(it, lead)) : ""}</p>${noteBoxHTML(it)}`;
}
function wire(it, made) {
  const inner = $("sheet-inner"), hint = $("hint");
  wireNote(it);
  // A long body is folded to a few lines, so the choices stay near the question.
  inner.querySelectorAll(".show-all").forEach((b) => b.onclick = () => {
    const p = b.previousElementSibling, open = p.classList.toggle("clamped");
    b.textContent = open ? "Show all" : "Show less"; b.setAttribute("aria-expanded", String(!open));
  });
  const fixed = hint && hint.dataset.fixed ? hint.textContent : "";
  const rest = hint ? hint.textContent : "";
  inner.querySelectorAll(".choices .btn[data-mark]").forEach((b) => {
    const m = b.dataset.mark;
    b.onclick = () => (m === "later" ? move(1) : decide(m));
    const point = () => { if (b.disabled) return; if (m !== "later" && !labelling) show(m, true); if (hint && !fixed) hint.textContent = choiceHint(it, m); };
    const leave = () => { show(made, false); if (hint && !fixed) hint.textContent = rest; };
    b.onmouseenter = point; b.onfocus = () => { if (!quiet) point(); };
    b.onmouseleave = b.onblur = leave;
  });
  const ro = $("reopen"); if (ro) ro.onclick = () => decide("reopen");
  const on = $("only"); if (on) on.onclick = toggleOnly;
}
// A question the reviewer had for you at your prompt, which an agent closed:
// the question, the closure with its proof, and the note (or the fact made
// from it). Change this answer reopens it, and it waits at your prompt again.
function questionSheet(it) {
  const inner = $("sheet-inner"), f = it.a;
  sideNouns = { A: "the note", B: "the note", "A,B": "the note" };
  inner.innerHTML = `
    <h2 class="question">${esc(KINDS.question.q)}</h2>
    ${it.question ? `<p class="suggest">${esc(it.question)}</p>` : ""}
    ${recordHTML(it)}
    <div class="pair">${card(f, "a", f.missing ? "The agent's note" : "What agents read of it")}</div>`;
  wire(it, "");
}
// A stale check is about one fact: the card, and beside it what git shows.
function staleSheet(it) {
  const inner = $("sheet-inner"), K = KINDS[it.kind] && KINDS[it.kind].qRule ? KINDS[it.kind] : KINDS.stale, f = it.a, d = it.decision, fl = it.flag || {};
  sideNouns = { A: "this fact", B: "this fact", "A,B": "this fact" };
  const made = it.state === "retired" ? "retire" : d && d.action === "current" ? "current" : "";
  const word = agentWord(it, { a: "fact", b: "fact" });
  if (word.mark && !can(it, word.mark)) word.mark = "";
  const why = d && d.by === "agent" && d.action === "fix" && it.applied ? "The fact was edited after the agent's fix, so this counts as fixed."
    : d && d.by === "agent" && d.action === "fix" ? "Fix the fact and answer Still true, or answer Outdated."
    : d && it.stale ? "The store changed since this answer and the fact is no longer retired. Change the answer to answer again."
    : it.state === "retired" && !d ? "This fact is already retired."
    : it.state === "gone" ? "This fact was replaced or removed, so there is nothing to decide."
    : "";
  const sum = it.kind === "check" ? (fl.detail || []) : staleSummary(fl);
  inner.innerHTML = `
    <h2 class="question">${esc(isRule(f) ? K.qRule : K.q)}</h2>
    ${word.text ? `<p class="suggest">${esc(word.text)}</p>` : ""}
    ${it.prior ? `<p class="suggest quiet">An earlier check left a fix that was never applied: ${esc(it.prior.evidence || "")}</p>` : ""}
    ${recordHTML(it)}${notesHTML(it)}
    <div class="pair">${card(f, "a", isRule(f) ? "Your rule" : "Fact")}
      <aside class="side-col" aria-label="${esc(K.side || "What git shows")}"><h3>${esc(K.side || "What git shows")}</h3><ul class="sum">${sum.map((l) => `<li>${esc(l)}</li>`).join("")}</ul>
        <section class="git inside" id="timeline" aria-busy="true"><p class="tl-sub">Reading git.</p></section></aside></div>
    ${choicesHTML(it, word.mark)}`;
  if (why) { const h = $("hint"); if (h) { h.textContent = why; h.dataset.fixed = "1"; } }
  if (made) show(made, false);
  timeline(it);
  wire(it, made);
}
// What git shows: the plain summary first, the full history folded under it.
// Built on the server without a model; a finding asks for it when it is shown.
const tlCache = new Map(); // id -> { at, ttl, data, error }; a partial answer or a failure is kept only 20 s
let tlAbort = null, tlLoading = null;
async function timeline(it) {
  const hit = tlCache.get(it.id);
  if (hit && Date.now() - hit.at < hit.ttl) { paintTimeline(hit.data, hit.error); return; }
  if (tlAbort && tlLoading === it.id) return;
  if (tlAbort) tlAbort.abort();
  const ctl = new AbortController(); tlAbort = ctl; tlLoading = it.id;
  const current = () => view[cur] && view[cur].id === it.id && $("timeline");
  try {
    let data;
    if (CALL) {
      data = await CALL("Timeline", it.id);
      if (ctl.signal.aborted) return;
    } else {
      const r = await fetch("/api/timeline?id=" + encodeURIComponent(it.id), { cache: "no-store", headers: { "X-Tacit-Token": TOKEN }, signal: ctl.signal });
      data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || "no answer");
    }
    // A body without events is not a timeline; it must never read as "nothing to compare".
    if (!Array.isArray(data.events)) throw new Error(data.error || "the answer had no timeline in it");
    tlCache.set(it.id, { at: Date.now(), ttl: data.incomplete ? 20000 : 600000, data });
    if (current()) paintTimeline(data);
  } catch (e) {
    if (e.name === "AbortError") return;
    tlCache.set(it.id, { at: Date.now(), ttl: 20000, error: e.message });
    if (current()) paintTimeline(null, e.message);
  } finally { if (tlAbort === ctl) { tlAbort = null; tlLoading = null; } }
}
function paintTimeline(t, error, el = $("timeline")) {
  if (!el) return;
  const head = el.dataset && el.dataset.head ? `<h3 class="sub">${esc(el.dataset.head)}</h3>` : el.classList && el.classList.contains("inside") ? "" : '<h3 id="tl-h">What git shows</h3>';
  if (error || !t) { el.removeAttribute("aria-busy"); el.querySelector(".tl-sub").textContent = "Git could not be read: " + (error || "no answer") + "."; return; }
  const where = (t.repos || []).length ? `Read ${esc(t.repos.join(", "))}${t.since ? ` from ${esc(plainDate(t.since))}` : ""}${t.as_of ? `, as the repos stood at ${esc(t.as_of)}` : ""}.` : "";
  const rows = (t.events || []).map((e) => {
    const who = e.kind === "fact" ? (sideNouns[e.side] || "") : e.kind === "head" ? `${e.repo || ""} now` : [e.repo, e.ref].filter(Boolean).join(" ");
    const tail = [e.via ? esc(e.via) : "", e.by ? "by " + esc(e.by) : ""].filter(Boolean).join(", ");
    return `<li class="tl-${esc(e.kind)}"><time>${e.when === "HEAD" ? "now" : esc(plainDate(e.when))}</time><span class="on">${esc(who)}</span><span class="what">${esc(e.text)}${tail ? `<span class="tail">${tail}</span>` : ""}</span></li>`;
  }).join("");
  const sum = (t.summary || []).map((l) => `<li>${esc(l)}</li>`).join("");
  el.removeAttribute("aria-busy");
  el.innerHTML = `${head}${sum ? `<ul class="sum">${sum}</ul>` : ""}${t.incomplete && !sum ? '<p class="tl-sub">Part of git could not be read, so this is a partial view.</p>' : ""}
    <details class="full"><summary>Full git history</summary>${where ? `<p class="tl-sub">${where}</p>` : ""}<ol class="tl">${rows}</ol></details>`;
}
// Draw on the cards what an answer does: stays, stops reaching agents, conflicting.
function show(mark, preview) {
  const A = $("g-a"), B = $("g-b"); if (!A) return;
  for (const [el, side] of [[A, "left"], [B, "right"]]) {
    if (!el) continue;
    const m = mark && MARKS[mark] && MARKS[mark][side];
    if (!m) { delete el.dataset.mark; delete el.dataset.preview; continue; }
    el.dataset.mark = m;
    if (preview) el.dataset.preview = "1"; else delete el.dataset.preview;
  }
}
let slipTimer;
function slip(msg, opts = {}) {
  const s = $("slip");
  s.className = "slip show" + (opts.error ? " error" : "");
  s.innerHTML = `<span>${esc(msg)}</span>` + (opts.undo ? `<button type="button" id="slip-undo">${icon("undo")}${esc(opts.label || "Undo (z)")}</button>` : "");
  if (opts.undo) $("slip-undo").onclick = opts.undo;
  clearTimeout(slipTimer); slipTimer = setTimeout(() => s.className = "slip", opts.error ? 9000 : 5000);
}
function toggleOnly() {
  const it = view[cur]; if (!it) return;
  if (flagged.has(it.id)) flagged.delete(it.id); else flagged.add(it.id);
  const on = $("only"); if (on) on.setAttribute("aria-pressed", String(flagged.has(it.id)));
  slip(flagged.has(it.id) ? "Only you can know this one. Now give your answer." : "No longer marked as only you can know.");
}
async function post(id, action, comment) {
  const only = labelling && flagged.has(id) && action !== "reopen";
  comment = action === "reopen" ? "" : comment || "";
  if (CALL) return (await (action === "note" ? CALL("Comment", id, comment) : CALL("Answer", id, action, comment))) || "";
  const r = await fetch("/api/decide", { method: "POST", headers: { "Content-Type": "application/json", "X-Tacit-Token": TOKEN }, body: JSON.stringify({ ID: id, Action: action, OwnerOnly: only, Comment: comment }) });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || "The answer was not saved.");
  return body.note || "";
}
async function decide(mark) {
  if (busy || !view.length) return;
  const it = view[cur];
  if (!can(it, mark)) {
    if (it.decision && mark !== "reopen") slip("This one is answered. Change the answer (z) first.");
    return;
  }
  busy = true; document.body.style.cursor = "progress";
  try {
    const note = await post(it.id, mark, mark === "reopen" ? "" : noteOf(it));
    if (mark !== "reopen") noteDrafts.delete(it.id);
    // A reopened flagged label comes back flagged; any other mark uses the flag up.
    if (mark === "reopen" && it.decision && it.decision.owner_only) flagged.add(it.id); else flagged.delete(it.id);
    if (mark === "reopen") { if (lastDecided === it.id) lastDecided = null; slip(note || "Open again."); await load(it.id, true); }
    else {
      lastDecided = it.id;
      slip(note ? `${pastFor(it, mark)}. ${note}` : pastFor(it, mark) + ".", { undo: reopenLast });
      const next = view.slice(cur + 1).find((x) => x.needs_owner) || view.find((x) => x.needs_owner && x.id !== it.id);
      await load(state.status === "open" && next ? next.id : it.id, true);
    }
  } catch (e) {
    slip(e.message === "Failed to fetch" ? (CALL ? "tacit app is not running. Start it again with tacit app." : "tacit review is not running. Start it again with tacit review.") : e.message, { error: true });
    // The store may have moved under us; show what it says now.
    try { await load(it.id); } catch (_) { /* server gone; the slip says so */ }
  } finally { busy = false; document.body.style.cursor = ""; }
}
// A note on its own: saved in their words, the finding stays open.
async function commentOnly(it) {
  const text = noteOf(it);
  if (busy || !text) return;
  busy = true; document.body.style.cursor = "progress";
  try {
    await post(it.id, "note", text);
    noteDrafts.delete(it.id);
    slip("Note saved. This stays open.");
    await load(it.id);
  } catch (e) { slip(e.message, { error: true }); }
  finally { busy = false; document.body.style.cursor = ""; }
}
async function reopenLast() {
  if (!lastDecided || busy) return;
  busy = true;
  const id = lastDecided;
  const was = items.find((x) => x.id === id);
  try {
    const note = await post(id, "reopen"); lastDecided = null;
    if (was && was.decision && was.decision.owner_only) flagged.add(id);
    slip(note || "Undone. It is open again."); await load(id, true);
  }
  catch (e) { slip(e.message, { error: true }); try { await load(id); } catch (_) { /* server gone */ } }
  finally { busy = false; }
}
function move(n) { if (!view.length) return; cur = (cur + n + view.length) % view.length; sheet(); const s = $("sheet"); if (s) s.scrollTop = 0; }
function setFilter(p) {
  Object.assign(state, p);
  cur = 0;
  if ($("find")) $("find").value = state.q;
  if ($("status")) $("status").value = state.status;
  if (typeof refreshPicks === "function") refreshPicks();
  filters(); apply();
}
function toggleFilter(open) {
  const p = $("filter-panel"), b = $("filter-btn"); if (!p) return;
  p.hidden = open === undefined ? !p.hidden : !open;
  if (b) b.setAttribute("aria-expanded", String(!p.hidden));
  if (!p.hidden) $("find").focus();
}
document.addEventListener("keydown", (e) => {
  // In tacit app the other places (facts, inbox, a form) take their own keys.
  if (globalThis.tacitDesk && !globalThis.tacitDesk.needs()) return;
  const t = e.target instanceof Element ? e.target : document.body;
  if (t.matches("input, select, textarea")) { if (e.key === "Escape") t.blur(); return; }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const it = view[cur];
  switch (e.key) {
    case "j": case "ArrowDown": move(1); break;
    case "k": case "ArrowUp": move(-1); break;
    // The arrows keep the card on that side of the screen.
    case "ArrowLeft": decide(it && flipped(it) ? "right" : "left"); break;
    case "ArrowRight": decide(it && flipped(it) ? "left" : "right"); break;
    case "s": decide(it && it.single ? "current" : "both"); break;
    case "r": if (it && it.single) decide("retire"); else return; break;
    case "q": decide("query"); break;
    case "x": if (can(view[cur] || {}, "skip")) decide("skip"); else return; break;
    case "w": if (can(view[cur] || {}, "wrong")) decide("wrong"); else return; break;
    case "z": if (it && it.decision) decide("reopen"); else reopenLast(); break;
    case "o": if (labelling && it && !it.decision) toggleOnly(); else return; break;
    // The owner's page finds any fact from the box at the top; the review page alone folds out its filter.
    case "/": if ($("top-q")) { $("top-q").focus(); if ($("top-q").select) $("top-q").select(); } else toggleFilter(true); break;
    case "d": lights(); break;
    default: return;
  }
  e.preventDefault();
});
$("find").addEventListener("input", (e) => { state.q = e.target.value; cur = 0; filters(); apply(); });
$("status").addEventListener("change", (e) => { state.status = e.target.value; cur = 0; filters(); apply(); });
$("scope").addEventListener("change", (e) => { state.scope = e.target.value; cur = 0; filters(); apply(); });
{ const on = (id, fn) => { const b = $(id); if (b) b.onclick = fn; }; on("prev", () => move(-1)); on("next", () => move(1)); on("filter-btn", () => toggleFilter()); }
const dark = () => (document.documentElement.dataset.theme || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light")) === "dark";
function paintLights() { const b = $("lights"); if (b) b.innerHTML = dark() ? `${icon("sun")}<span class="label">Light</span>` : `${icon("moon")}<span class="label">Dark</span>`; }
function lights() { const t = dark() ? "light" : "dark"; document.documentElement.dataset.theme = t; try { localStorage.setItem("tacit-theme", t); } catch (_) { /* storage off */ } paintLights(); }
$("lights").onclick = lights; paintLights();
// The system scheme can change while the page is open (WebKitGTK may report it
// late): the page follows it unless the owner chose, and the button says what it will do.
{ const mq = matchMedia("(prefers-color-scheme: dark)"); if (mq && mq.addEventListener) mq.addEventListener("change", paintLights); }
// Static buttons name their icon; the page draws it, so there is one icon family.
// Text fields grow with what they hold, so no toolkit resize grip is needed.
function grow(t) { if (!t || !t.style) return; t.style.height = "auto"; t.style.height = t.scrollHeight + 2 + "px"; }
document.addEventListener("input", (e) => { if (e.target && e.target.tagName === "TEXTAREA") grow(e.target); });
if (typeof document.querySelectorAll === "function") document.querySelectorAll("[data-icon]").forEach((el) => el.insertAdjacentHTML("afterbegin", icon(el.dataset.icon)));
load();
