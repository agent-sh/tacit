// tacit app: the places beside Needs you. Your rules, What agents believe you
// prefer, All facts and Inbox are search first; a fact opens with what it says, where it applies and what can
// be done, its details folded. The owner edits, retires, restores, replaces with
// a new rule, undoes their last change and writes new rules. The box at the top
// finds any fact from every place (/), and #fact=<name> opens the page on one. Needs you is app.js, with the broken links
// in their rules added. Design: DESIGN.md.
"use strict";
const desk = {
  view: "needs", mode: "show", // show | edit | ruling
  meta: null, detail: null, back: "needs",
  lists: {
    rules: { q: { q: "", status: "live", where: "curated", scope: "", repo: "", type: "", kind: "", rulings: true }, rows: [], cur: 0, matched: 0, total: 0 },
    believed: { q: { q: "", status: "live", where: "curated", scope: "", repo: "", type: "", kind: "", rulings: false, believed: true }, rows: [], cur: 0, matched: 0, total: 0 },
    facts: { q: { q: "", status: "live", where: "curated", scope: "", repo: "", type: "", kind: "", rulings: false }, rows: [], cur: 0, matched: 0, total: 0 },
    inbox: { q: { q: "", status: "all", where: "inbox", scope: "", repo: "", type: "", kind: "", rulings: false }, rows: [], cur: 0, matched: 0, total: 0 },
  },
  drafts: new Map(), // fact name -> the edit form's values, kept until saved or cancelled
  ruling: null,      // the new-rule form's values
  busy: false,
};
globalThis.tacitDesk = { needs: () => desk.view === "needs" && desk.mode !== "ruling" };
// A machine that cannot write as the curator (no curator key, a read-only
// clone) is view only: Go refuses every write, and the page offers none.
const deskViewOnly = () => (desk.meta && desk.meta.view_only) || "";
function paintViewOnly() { if (document.body && document.body.classList) document.body.classList.toggle("view-only", !!deskViewOnly()); }
const INTRO = {
  rules: "Your own words: rules you wrote here, kept in a review, or said in a session where your words are on record. Agents read them as rules in force, and no agent may change or retire them.",
  believed: "What agents wrote down as your preference, in their words, not yours. Agents read them like any other fact, and a check against the sources may retire them, but never against words of yours one holds without asking you. Nothing here waits for you: if one is yours, say so; if it is not, say Not mine; if it is wrong, retire it.",
  facts: "Everything agents can be shown, newest first.",
  inbox: "Notes agents wrote that the curator has not reviewed yet. Agents are not shown them until they are.",
};
const PLACEHOLDER = { rules: "Search your rules", believed: "Search what agents believe", facts: "Search every fact", inbox: "Search what agents wrote" };
// A name from a description: whole words only, at most 60 characters.
const slugify = (s) => {
  const full = String(s).toLowerCase().normalize("NFKD").replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (full.length <= 60) return full;
  const cut = full.slice(0, 61), i = cut.lastIndexOf("-");
  return (i > 20 ? cut.slice(0, i) : full.slice(0, 60)).replace(/-+$/, "");
};
const repoOfScope = (s) => (/^(projects|derived)\/(.+)$/.exec(s || "") || [])[2] || "";

function isCombo(e) { return (e.ctrlKey || e.metaKey) && e.key === "Enter"; }
async function guarded(fn) {
  if (desk.busy) return;
  desk.busy = true; document.body.style.cursor = "progress";
  try { return await fn(); } finally { desk.busy = false; document.body.style.cursor = ""; }
}
function fail(e) { slip(e.message === "Failed to fetch" ? "tacit app is not running. Start it again with tacit app." : e.message, { error: true }); }

// --- places ---

function setView(v, opts = {}) {
  if (desk.mode !== "show" && !opts.keepMode) desk.mode = "show";
  desk.view = v;
  document.body.dataset.view = v;
  for (const b of document.querySelectorAll(".place")) b.setAttribute("aria-pressed", String(b.dataset.view === v));
  const needs = v === "needs", folders = v === "folders";
  $("needs").hidden = !needs; $("browse").hidden = needs || folders;
  if ($("folders")) $("folders").hidden = !folders;
  $("sheet").scrollTop = 0;
  if (folders) { if (globalThis.tacitFolders) globalThis.tacitFolders.show(); return; }
  if (needs) { const it = view[cur]; load(it && it.id); return; }
  paintFilters();
  if (!opts.noList) browse(opts.then);
}

// --- lists ---

let browseTimer = null, browseSeq = 0;
function browseSoon() { clearTimeout(browseTimer); browseTimer = setTimeout(() => browse(), 160); }
async function browse(then) {
  const L = desk.lists[desk.view]; if (!L) return;
  const seq = ++browseSeq;
  try {
    const r = await tacitCall("Browse", L.q);
    if (seq !== browseSeq) return;
    L.rows = r.rows || []; L.matched = r.matched; L.total = r.total;
    L.cur = Math.min(L.cur, Math.max(0, L.rows.length - 1));
    if (r.note) slip(r.note, { error: true });
    factLedger();
    if (then) then();
    else if (desk.mode === "show") { if (L.rows.length) openFact(L.rows[L.cur].path); else blankSheet(); }
  } catch (e) { fail(e); }
}
// --- lists the page draws itself ---
// A <select> stays as the model (its value and change event drive the filters
// and the form) but is never shown: in the desktop window WebKitGTK would open
// its options as a GTK menu. Each gets a button and a listbox drawn here, keyed
// like a native one: arrows, Home, End, Page keys, type-ahead, Enter or Space
// to choose, Esc to close.
let picks = [];
function upgradeSelect(sel) {
  if (!sel || typeof document.createElement !== "function" || !sel.options || (sel.dataset && sel.dataset.pick)) return null;
  sel.dataset.pick = "1";
  const wrap = document.createElement("div"); wrap.className = "pick";
  const btn = document.createElement("button");
  btn.type = "button"; btn.className = "pick-btn"; btn.setAttribute("aria-haspopup", "listbox"); btn.setAttribute("aria-expanded", "false");
  const lab = sel.getAttribute("aria-label") || (sel.closest("label") && sel.closest("label").querySelector(".flabel") ? sel.closest("label").querySelector(".flabel").textContent : "");
  const list = document.createElement("ul");
  list.className = "pick-list"; list.setAttribute("role", "listbox"); list.tabIndex = -1; list.hidden = true;
  list.id = (sel.id || "pick" + picks.length) + "-list"; btn.setAttribute("aria-controls", list.id);
  if (lab) list.setAttribute("aria-label", lab);
  sel.parentNode.insertBefore(wrap, sel); wrap.append(sel, btn, list);
  sel.hidden = true; sel.tabIndex = -1;
  let active = 0, typed = "", typedAt = 0;
  const label = () => { const o = sel.options[sel.selectedIndex]; btn.textContent = o ? o.textContent : ""; btn.setAttribute("aria-label", (lab ? lab + ": " : "") + btn.textContent); };
  const paint = () => {
    list.innerHTML = [...sel.options].map((o, i) => `<li role="option" id="${list.id}-${i}" data-i="${i}" aria-selected="${i === sel.selectedIndex}"${i === active ? ' class="active"' : ""}>${esc(o.textContent)}</li>`).join("");
    list.setAttribute("aria-activedescendant", `${list.id}-${active}`);
    const a = list.children[active]; if (a) a.scrollIntoView({ block: "nearest" });
  };
  const open = () => { closePicks(); active = Math.max(0, sel.selectedIndex); list.hidden = false; btn.setAttribute("aria-expanded", "true"); wrap.classList.add("open"); paint(); list.focus({ preventScroll: true }); };
  const close = (focus) => { if (list.hidden) return; list.hidden = true; btn.setAttribute("aria-expanded", "false"); wrap.classList.remove("open"); if (focus) btn.focus({ preventScroll: true }); };
  const choose = (i) => {
    if (i !== sel.selectedIndex) { sel.selectedIndex = i; sel.dispatchEvent(new Event("change", { bubbles: true })); }
    label(); close(true);
  };
  btn.addEventListener("click", () => (list.hidden ? open() : close(true)));
  btn.addEventListener("keydown", (e) => {
    if (["ArrowDown", "ArrowUp", "Enter", " "].includes(e.key) && !e.ctrlKey && !e.metaKey && !e.altKey) { e.preventDefault(); e.stopPropagation(); open(); }
  });
  list.addEventListener("mousedown", (e) => e.preventDefault());
  list.addEventListener("click", (e) => { const li = e.target.closest("li[data-i]"); if (li) choose(+li.dataset.i); });
  list.addEventListener("mousemove", (e) => { const li = e.target.closest("li[data-i]"); if (li && +li.dataset.i !== active) { active = +li.dataset.i; paint(); } });
  list.addEventListener("keydown", (e) => {
    const n = sel.options.length, k = e.key;
    if (k === "Tab") { close(false); return; }
    e.stopPropagation(); // the places' own keys wait while a list is open
    if (k === "ArrowDown") active = Math.min(n - 1, active + 1);
    else if (k === "ArrowUp") active = Math.max(0, active - 1);
    else if (k === "Home") active = 0;
    else if (k === "End") active = n - 1;
    else if (k === "PageDown") active = Math.min(n - 1, active + 8);
    else if (k === "PageUp") active = Math.max(0, active - 8);
    else if (k === "Enter" || k === " ") { e.preventDefault(); choose(active); return; }
    else if (k === "Escape") { e.preventDefault(); close(true); return; }
    else if (k.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const now = Date.now();
      typed = (now - typedAt < 700 ? typed : "") + k.toLowerCase(); typedAt = now;
      const os = [...sel.options], from = typed.length === 1 ? active + 1 : active;
      for (let j = 0; j < n; j++) { const i = (from + j) % n; if (os[i].textContent.trim().toLowerCase().startsWith(typed)) { active = i; break; } }
    } else return;
    e.preventDefault(); paint();
  });
  list.addEventListener("blur", () => setTimeout(() => { if (!wrap.contains(document.activeElement)) close(false); }, 0));
  new MutationObserver(label).observe(sel, { childList: true, subtree: true });
  sel.addEventListener("change", label);
  const pk = { sel, label, close, open, choose };
  picks.push(pk); label();
  return pk;
}
function closePicks() { picks = picks.filter((p) => p.sel.isConnected); for (const p of picks) p.close(false); }
function refreshPicks() { for (const p of picks) p.label(); }
document.addEventListener("mousedown", (e) => { if (!(e.target instanceof Element && e.target.closest(".pick"))) closePicks(); });

const FILTER_KEYS = ["status", "where", "scope", "repo", "type", "kind"];
const DEFAULTS = { rules: { status: "live", where: "curated" }, believed: { status: "live", where: "curated" }, facts: { status: "live", where: "curated" }, inbox: { status: "all", where: "inbox" } };
function activeFilters(v) {
  const L = desk.lists[v], d = DEFAULTS[v];
  return FILTER_KEYS.filter((k) => (L.q[k] || "") !== (d[k] || "")).length;
}
function paintFilters() {
  const L = desk.lists[desk.view]; if (!L) return;
  $("intro").textContent = INTRO[desk.view];
  $("q").placeholder = PLACEHOLDER[desk.view]; $("q").value = L.q.q;
  const n = activeFilters(desk.view);
  const fl = $("f-btn").querySelector && $("f-btn").querySelector(".label"); if (fl) fl.textContent = n ? `Filter (${n})` : "Filter";
  const m = desk.meta; if (!m) return;
  const opts = (id, first, xs, sel, label = (x) => x) => { $(id).innerHTML = `<option value="">${first}</option>` + xs.map((x) => `<option value="${esc(x)}"${x === sel ? " selected" : ""}>${esc(label(x))}</option>`).join(""); };
  opts("f-scope", "Anyone", m.scopes, L.q.scope, (s) => cap(whereWords(s)));
  opts("f-repo", "Any repo", m.repos, L.q.repo);
  opts("f-type", "Any kind", m.types, L.q.type, (t) => cap(typeWords(t)));
  opts("f-kind", "Any source", m.kinds, L.q.kind, (k) => cap(SOURCE_WORDS[k] || k));
  $("f-status").value = L.q.status; $("f-where").value = L.q.where === "inbox" ? "curated" : L.q.where;
  { const row = $("f-status").closest && $("f-status").closest(".selects"); if (row) row.hidden = desk.view === "inbox"; }
  refreshPicks();
}
function factLedger() {
  const L = desk.lists[desk.view], ul = $("facts");
  const hadFocus = ul.contains(document.activeElement);
  const n = (x) => Number(x || 0).toLocaleString("en-GB");
  const noun = desk.view === "inbox" ? "notes" : desk.view === "rules" ? "rules" : "facts";
  const empty = { inbox: "The inbox is empty: the curator has read every note.", rules: "No rules yet. New rule writes one.", believed: "Agents have not filed anything as your preference." };
  // Your rules are a filter of every fact, so their count is the whole place.
  const filtered = L.q.q || activeFilters(desk.view);
  $("facts-tally").textContent = !filtered && desk.view !== "inbox" ? `${n(L.matched)} ${noun} in use${L.matched > L.rows.length ? `, first ${n(L.rows.length)} shown` : ""}` : L.matched === L.total ?`${n(L.total)} ${noun}` : `${n(L.matched)} of ${n(L.total)} ${noun}${L.matched > L.rows.length ? `, first ${n(L.rows.length)} shown` : ""}`;
  if (!L.rows.length) {
    ul.innerHTML = `<li class="none">${L.total ? "Nothing matches." : empty[desk.view] || "No facts yet."}</li>`;
    return;
  }
  ul.innerHTML = L.rows.map((r, i) => {
    const meta = [r.inbox ? `${r.harness} on ${r.host}` : cap(whereWords(r.scope)), plainDate(r.created), r.ruling && desk.view !== "rules" ? "your rule" : "", r.retired ? "retired" : r.superseded_by ? "replaced" : ""].filter(Boolean).join(", ");
    const gone = r.retired || r.superseded_by;
    const words = r.ruling && r.quote ? `<span class="row-words">In your words: “${esc(r.quote)}”</span>`
      : !r.ruling && r.words ? `<span class="row-words">Your words in it: “${esc(r.words)}”</span>` : "";
    return `<li><button type="button" class="row${gone ? " gone" : ""}" data-i="${i}" tabindex="${i === L.cur ? 0 : -1}"${i === L.cur ? ' aria-current="true"' : ""}>
      <span class="row-text">${esc(r.description || r.name)}</span>${words}<span class="row-meta">${esc(meta)}</span></button></li>`;
  }).join("");
  ul.querySelectorAll(".row").forEach((b) => b.onclick = () => { L.cur = +b.dataset.i; factLedger(); openFact(L.rows[L.cur].path); });
  const el = ul.querySelector('[aria-current="true"]');
  if (el) { el.scrollIntoView({ block: "nearest" }); if (hadFocus) el.focus({ preventScroll: true }); }
}
function moveFact(n) {
  const L = desk.lists[desk.view]; if (!L || !L.rows.length || desk.mode !== "show") return;
  L.cur = (L.cur + n + L.rows.length) % L.rows.length; factLedger(); openFact(L.rows[L.cur].path);
}
function blankSheet() {
  $("fact-sheet").innerHTML = `<div class="done"><h2>Nothing matches.</h2><p>Change the search or clear the filter.</p></div>`;
  desk.detail = null;
}

// --- one fact ---

let openSeq = 0;
// What to send to act on the fact open: its file when a note or another
// curated file holds its name, else its name (factDetail.ref).
const refOf = (d) => d.ref || (d.inbox ? d.path : d.name);
async function openFact(ref, opts = {}) {
  const seq = ++openSeq;
  try {
    const d = await tacitCall("Fact", ref);
    if (seq !== openSeq) return;
    desk.detail = d;
    if (opts.edit && d.can_edit) { desk.mode = "edit"; editSheet(); } else { desk.mode = "show"; factSheet(); }
    if (opts.focus) { const b = $("fact-sheet").querySelector(".actions .btn:not([disabled]), textarea"); if (b) b.focus({ preventScroll: true }); }
  } catch (e) { if (seq === openSeq) fail(e); }
}
// From anywhere (a name in a card, a link in a body): show that fact.
function jumpTo(name, opts = {}) {
  desk.back = desk.view;
  setView(desk.view === "inbox" && !opts.facts ? "inbox" : desk.view === "rules" && !opts.facts ? "rules" : desk.view === "believed" && !opts.facts ? "believed" : "facts", { noList: true });
  const L = desk.lists[desk.view];
  if (!L.rows.length) browse(() => openFact(name, opts)); else openFact(name, opts);
}
function bodyHTML(body, links) {
  const state = Object.fromEntries((links || []).map((l) => [l.name, l.state]));
  return esc(body).replace(/\[\[([A-Za-z0-9_][A-Za-z0-9_.-]*)(?:[|#]([^\]\n]*))?\]\]/g, (all, target, label) => {
    const st = state[target] || "missing", text = esc(label && !label.startsWith("#") ? label : target);
    return st === "missing" ? `<span class="dead" title="No fact is named ${esc(target)}">${text}</span>`
      : `<button type="button" class="open-fact link-${st}" data-fact="${esc(target)}" title="${esc(target)}${st === "live" ? "" : ", " + st}">${text}</button>`;
  });
}
function factLink(name, state) {
  return state === "missing" ? `<span class="dead">${esc(name)}</span>` : `<button type="button" class="open-fact link-${esc(state || "live")}" data-fact="${esc(name)}">${esc(name)}</button>`;
}
const APPLIES = { paths: "files", symbols: "code names", repos: "repos", hosts: "machines", commands: "commands" };
// Where a fact reaches, in a sentence: its scope, then what it is limited to.
function whereSentence(d) {
  const a = Object.assign({}, d.applies_to || {});
  // A repo the scope already names says nothing new.
  const own = repoOfScope(d.scope);
  a.repos = (a.repos || []).filter((r) => r !== own);
  a.hosts = (a.hosts || []).filter((h) => "machines/" + h !== d.scope);
  const only = Object.keys(APPLIES).filter((k) => (a[k] || []).length).map((k) => `${APPLIES[k]} ${a[k].slice(0, 3).join(", ")}${a[k].length > 3 ? ` and ${a[k].length - 3} more` : ""}`);
  return cap(whereWords(d.scope)) + (only.length ? `, when working on ${only.join("; ")}` : "") + ".";
}
function datesSentence(d) {
  return cap([d.created ? "written " + plainDate(d.created) : "", d.updated && d.updated !== d.created ? "changed " + plainDate(d.updated) : "", d.retired ? "retired " + plainDate(d.retired) : ""].filter(Boolean).join(", ")) + ".";
}
function detailsHTML(d) {
  const row = (k, v) => v ? `<dt>${k}</dt><dd>${v}</dd>` : "";
  const a = d.applies_to || {};
  const applies = Object.keys(APPLIES).map((k) => row(cap(APPLIES[k]), (a[k] || []).map((x) => `<code>${esc(x)}</code>`).join(", "))).join("");
  const hist = (d.history || []).map((h) => `<li class="${h.yours ? "tl-yours" : ""}"><time>${esc(plainDate(h.when))}${esc(String(h.when).slice(10, 16))}</time><span class="on">${h.yours ? "you" : esc(h.who)}</span><span class="what">${esc(h.subject)}</span></li>`).join("");
  return `<dl class="fields">
    ${row("Name", esc(d.name))}${row("File", `<code>${esc(d.path)}</code>`)}${row("Kind", esc(cap(typeWords(d.type))))}
    ${row("Source", esc(cap([SOURCE_WORDS[d.kind] || d.kind, d.harness && d.host ? `${d.harness} on ${d.host}` : d.harness].filter(Boolean).join(", "))))}
    ${row("Reaches", esc(d.scope))}${applies}
    ${row("Replaced by", d.superseded_by ? factLink(d.superseded_by, "live") : "")}
    ${row("Conflicts with", (d.conflicts_with || []).map((c) => factLink(c, "live")).join(", "))}
    ${row("From note", d.origin ? (d.origin_state === "missing" ? esc(d.origin) : factLink(d.origin, d.origin_state)) : "")}
    ${row("Links to", (d.links || []).map((l) => factLink(l.name, l.state) + (l.state !== "live" ? ` <i>(${esc(l.state === "missing" ? "no such fact" : l.state)})</i>` : "")).join(", "))}
    ${row("Linked from", (d.backlinks || []).map((n) => factLink(n, "live")).join(", "))}
    ${row("Derived from", d.derived_from ? `<code>${esc(d.derived_from)}</code>` : "")}
    ${row("Imported from", d.imported_from ? `<code>${esc(d.imported_from)}</code>` : "")}
    ${row("Session", d.session ? `<code>${esc(d.session)}</code>` : "")}
  </dl>
  <h3 class="sub">Changes to this fact</h3>
  <p class="tl-sub">${esc(d.history_note || "Every change to this fact in the store, newest first.")}</p>${hist ? `<ol class="tl hist">${hist}</ol>` : ""}
  <section class="git inside" id="ftimeline" data-head="What git shows" aria-label="What git shows" aria-busy="true"><h3 class="sub">What git shows</h3><p class="tl-sub">Reading git.</p></section>`;
}
// A description that is the body's own start, cut short: the body says it again.
const sameStart = (desc, body) => { const d = String(desc || "").replace(/(\.\.\.|…)\s*$/, "").trim(); return d.length > 40 && String(body || "").trim().startsWith(d); };
function factSheet() {
  const d = desk.detail, el = $("fact-sheet");
  const tags = [d.ruling ? "Your rule" : "", d.believed ? "What agents believe you prefer" : "", d.not_mine ? "You said not yours" : "", d.inbox ? "Inbox note, not reviewed" : "", d.retired ? "Retired" : "", d.superseded_by ? "Replaced" : ""].filter(Boolean);
  const actions = d.inbox
    ? btn("act-promote", "plus", "Make it my rule", "p", { primary: true })
    : (d.can_mine ? btn("act-ismine", "plus", "This is mine", "m", { primary: !!d.believed }) : "") +
      (d.can_not_mine ? btn("act-notmine", "x", "Not mine", "x") : "") +
      (d.not_mine ? btn("act-undonotmine", "undo", "Undo Not mine", "x") : "") +
      btn("act-edit", "edit", "Edit", "e", { primary: d.can_edit && !d.believed, off: !d.can_edit }) +
      (d.can_restore ? btn("act-restore", "unarchive", "Restore", "r") : btn("act-retire", "archive", "Retire", "r", { off: !d.can_retire })) +
      (d.can_replace ? btn("act-replace", "take", "Replace with a new rule", "") : "") +
      (d.undo && d.undo.can ? btn("act-undo", "undo", "Undo last change", "u") : "");
  const why = d.why || (!d.inbox && d.undo && d.undo.can ? `Undo puts back what was there before your last change, ${plainDate(d.undo.when)}.` : "");
  el.innerHTML = `
    ${tags.length ? `<div class="tags">${tags.map((t) => `<span class="tag">${esc(t)}</span>`).join("")}</div>` : ""}
    <h2 class="fact-text${d.retired || d.superseded_by ? " gone" : ""}${[...String(d.description)].length > 140 ? " long" : ""}">${esc(d.description)}</h2>
    ${d.body && d.body !== d.description && !sameStart(d.description, d.body) ? `<div class="fact-body">${bodyHTML(d.body, d.links)}</div>` : ""}
    ${d.ruling ? `<p class="words">${esc(wordsSentence(d))}</p>` : d.owner_words ? `<p class="words">Your words in it: “${esc(d.owner_words)}”. Too few to make it your rule; no agent retires it against them without asking you.</p>` : d.not_mine ? `<p class="words">You said this is not yours${d.recorded_words ? `, so your words in it, “${esc(d.recorded_words)}”, no longer keep agents from retiring it` : ""}. Undo Not mine takes that back.</p>` : d.believed ? `<p class="words">Agents wrote this as your preference; no words of yours were found for it.</p>` : ""}
    ${d.body && d.body !== d.description && sameStart(d.description, d.body) ? `<details class="more whole"><summary>Read the whole note</summary><div class="fact-body">${bodyHTML(d.body, d.links)}</div></details>` : ""}
    <p class="where">${esc(whereSentence(d))} ${esc(datesSentence(d))}${d.superseded_by ? ` Replaced by ${factLink(d.superseded_by, "live")}.` : ""}</p>
    <div class="actions">${actions}</div>
    ${why ? `<p class="hint">${esc(why)}</p>` : ""}
    <details class="more" id="fact-more"><summary>Details</summary>${detailsHTML(d)}</details>`;
  const on = (id, fn) => { const b = $(id); if (b) b.onclick = fn; };
  on("act-edit", () => startEdit()); on("act-retire", () => retire(true)); on("act-restore", () => retire(false));
  on("act-undo", () => undo()); on("act-promote", () => promote()); on("act-replace", () => replaceWith()); on("act-ismine", () => mine()); on("act-notmine", () => notMine()); on("act-undonotmine", () => undoNotMine(refOf(d)));
  // What git shows is read when the details are opened: it is slow, and most visits do not need it.
  const more = $("fact-more");
  if (more && more.addEventListener) more.addEventListener("toggle", () => { if (more.open) factTimeline(d); });
}
const ftCache = new Map();
async function factTimeline(d) {
  const key = d.path + "|" + d.updated + "|" + d.retired, el = () => desk.detail && desk.detail.path === d.path && $("ftimeline");
  const hit = ftCache.get(key);
  sideNouns = { A: "this fact", B: "this fact", "A,B": "this fact" };
  if (hit && Date.now() - hit.at < hit.ttl) { if (el()) paintTimeline(hit.data, hit.error, el()); return; }
  try {
    const t = await tacitCall("FactTimeline", refOf(d));
    if (!t || !Array.isArray(t.events)) throw new Error("the answer had no timeline in it");
    ftCache.set(key, { at: Date.now(), ttl: t.incomplete ? 20000 : 600000, data: t });
    if (el()) paintTimeline(t, null, el());
  } catch (e) {
    ftCache.set(key, { at: Date.now(), ttl: 20000, error: e.message });
    if (el()) paintTimeline(null, e.message, el());
  }
}

// --- edit ---

// Drafts are keyed by the fact's name, which a scope move does not change, so a
// draft stays reachable when another machine moves the file. Each remembers the
// version it was started on (base) and, after Keep my draft, the version it was
// kept over (over).
function draftOf(d) {
  if (desk.drafts.has(d.name)) return desk.drafts.get(d.name);
  const a = d.applies_to || {};
  return { rev: d.rev, base: { description: d.description, body: d.body, scope: d.scope, type: d.type }, over: "", description: d.description, body: d.body, scope: d.scope, type: d.type, applies_to: { paths: a.paths || [], symbols: a.symbols || [], repos: a.repos || [], hosts: a.hosts || [], commands: a.commands || [] } };
}
function startEdit() {
  const d = desk.detail; if (!d || !d.can_edit) return;
  desk.mode = "edit"; editSheet();
}
const scopeNudge = (scope, was) => {
  if ((scope === "user" || scope === "system") && scope !== was) return scope === "system" ? "System is loaded into every session of every agent. Agents tend to widen your remarks; keep it here only if you mean every repo, every machine." : "User reaches every agent in every repo. Agents tend to widen your remarks; keep it here only if it is about you everywhere.";
  if (scope !== was) return `Moves the file to ${scope}/; its history follows it.`;
  return "";
};
// A draft keeps the rev of the version it was started on. When the fact has
// changed since (another machine, a sync), the form shows the new version beside
// the draft and Save waits until the owner chooses: keep their draft over it (the
// draft then carries the new rev), or take the new version.
function editSheet() {
  const d = desk.detail, f = draftOf(d), el = $("fact-sheet");
  desk.drafts.set(d.name, f);
  const changed = f.rev !== d.rev;
  const keptOver = !changed && f.over === d.rev;
  const types = desk.meta ? desk.meta.owned_types.slice() : [];
  if (!types.includes(d.type)) types.unshift(d.type);
  const a = f.applies_to;
  // Folded unless it limits the fact beyond what its scope already says.
  const own = { repos: repoOfScope(d.scope), hosts: (/^machines\/(.+)$/.exec(d.scope) || [])[1] || "" };
  const limited = Object.keys(APPLIES).some((k) => (a[k] || []).some((x) => x !== own[k]));
  // Text fields are as tall as what they hold, within reason.
  const rows = (s, min, max) => Math.max(min, Math.min(max, String(s || "").split("\n").reduce((n, l) => n + Math.ceil((l.length + 1) / 78), 0) + 1));
  el.innerHTML = `
    <p class="crumb">Editing ${d.ruling ? "your rule" : esc(an(typeWords(d.type)))}</p>
    <div class="edit-grid${changed || keptOver ? " two" : ""}">
      <form class="form" id="edit-form" novalidate aria-label="Edit ${esc(d.name)}">
        <label class="field"><span class="flabel">What agents read <i id="e-count"></i></span><textarea id="e-desc" class="text" rows="${rows(f.description, 2, 5)}">${esc(f.description)}</textarea></label>
        <label class="field"><span class="flabel">More detail</span><textarea id="e-body" class="body" rows="${rows(f.body, 4, 14)}">${esc(f.body)}</textarea></label>
        <fieldset class="scopes" id="e-scopes"><legend>Who it reaches, narrowest first</legend>
          ${scopeChoices({ repo: repoOfScope(f.scope) || (a.repos || [])[0] || "", scope: f.scope }, "Where it is now.").map(([s, what]) => `<label class="choice"><input type="radio" name="e-scope-choice" value="${esc(s)}"${s === f.scope ? " checked" : ""}><span><b>${esc(cap(whereWords(s)))}</b> <i>${esc(what)}</i></span></label>`).join("")}
          <label class="field inline"><span class="flabel">Or a scope</span><input id="e-scope" value="${esc(f.scope)}" spellcheck="false" autocomplete="off"></label>
        </fieldset>
        <label class="field kind-field"><span class="flabel">Kind</span><select id="e-type">${types.map((t) => `<option value="${esc(t)}"${t === f.type ? " selected" : ""}>${esc(cap(typeWords(t)))}</option>`).join("")}</select></label>
        <p class="nudge" id="e-nudge"></p>
        <details class="more"${limited ? " open" : ""}><summary>Only when working on</summary>
          <fieldset class="applies"><legend>Comma separated. Empty means everywhere it reaches.</legend>
          ${Object.keys(APPLIES).map((k) => `<label class="field inline"><span class="flabel">${cap(APPLIES[k])}</span><input id="e-${k}" value="${esc((a[k] || []).join(", "))}" spellcheck="false" autocomplete="off"></label>`).join("")}
          </fieldset></details>
        <div class="problem" id="e-problem" role="alert"></div>
        <div class="actions">${changed
          ? btn("act-mine", "edit", "Keep my draft", "m", { primary: true }) + btn("act-theirs", "take", "Take the new version", "t") + btn("act-cancel", "x", "Cancel", "esc")
          : btn("act-save", "check", "Save", "ctrl ↵", { primary: true }) + btn("act-cancel", "x", "Cancel", "esc")}</div>
        <p class="hint">${changed ? "This fact changed after you started editing it. Nothing is saved until you choose." : "Saving makes one commit in your name. If tacit's checks fail, nothing is saved and your draft stays."}</p>
      </form>
      ${changed ? changedHTML(d, f, true) : keptOver ? changedHTML(d, f, false) : ""}
    </div>`;
  const keep = () => { desk.drafts.set(d.name, readEdit()); paintEditHints(); };
  el.querySelectorAll("textarea, input, select").forEach((x) => { x.oninput = keep; x.onchange = keep; });
  // The plain choices and the scope field are one value: choosing writes the
  // field, typing checks the choice it matches.
  const radios = [...el.querySelectorAll('input[name="e-scope-choice"]')];
  radios.forEach((x) => x.onchange = () => { $("e-scope").value = x.value; keep(); });
  $("e-scope").oninput = () => { radios.forEach((x) => { x.checked = x.value === $("e-scope").value.trim(); }); keep(); };
  el.querySelectorAll("textarea").forEach(grow);
  upgradeSelect($("e-type"));
  const on = (id, fn) => { const b = $(id); if (b) b.onclick = fn; };
  on("act-save", saveEdit); on("act-cancel", cancelEdit); on("act-mine", keepMine); on("act-theirs", takeTheirs);
  $("edit-form").onsubmit = (e) => { e.preventDefault(); saveEdit(); };
  paintEditHints();
  // In a conflict the choice comes first: focus is on Keep my draft, so m and t
  // work at once and nothing is typed into the draft by accident.
  (changed ? $("act-mine") : $("e-desc")).focus();
}
// The rev is the draft's own, never the one open now: a kept draft must not
// pass the server's stale-form guard by borrowing the new version's rev.
function readEdit() {
  const list = (id) => $(id).value.split(",").map((x) => x.trim()).filter(Boolean);
  const kept = desk.drafts.get(desk.detail.name) || draftOf(desk.detail);
  return { rev: kept.rev, base: kept.base, over: kept.over, description: $("e-desc").value, body: $("e-body").value, scope: $("e-scope").value.trim(), type: $("e-type").value,
    applies_to: { paths: list("e-paths"), symbols: list("e-symbols"), repos: list("e-repos"), hosts: list("e-hosts"), commands: list("e-commands") } };
}
function paintEditHints() {
  const n = [...$("e-desc").value.replace(/\s+/g, " ").trim()].length;
  $("e-count").textContent = `${n} of 200`; $("e-count").className = n > 200 ? "over" : "";
  $("e-nudge").textContent = scopeNudge($("e-scope").value.trim(), desk.detail.scope);
}
function problem(id, msg, lint) {
  const p = $(id); if (!p) return;
  p.innerHTML = msg ? `<p>${esc(msg)}</p>${(lint || []).length ? `<ul>${lint.map((l) => `<li><code>${esc(l)}</code></li>`).join("")}</ul>` : ""}` : "";
}
async function saveEdit() {
  const d = desk.detail; if (!d || desk.mode !== "edit") return;
  const draft = readEdit(); desk.drafts.set(d.name, draft);
  await guarded(async () => {
    try {
      const { base, over, ...sent } = draft;
      const r = await tacitCall("Edit", refOf(d), sent);
      if (r.lint && r.lint.length) { problem("e-problem", r.note, r.lint); slip(r.note, { error: true }); return; }
      if (r.changed) {
        // Show the new version beside the draft, which keeps its own rev.
        desk.detail = await tacitCall("Fact", refOf(d));
        slip(r.note, { error: true }); editSheet(); return;
      }
      desk.drafts.delete(d.name); desk.mode = "show";
      // A file whose name another file holds is named by its path.
      const saved = d.ref && d.ref !== d.name ? d.ref : (r.name || d.name);
      slip(r.saved ? `Saved. ${r.note || ""}`.trim() : (r.note || "Nothing changed."), r.saved ? { undo: () => undo(saved), label: "Undo (u)" } : {});
      await refreshAfterWrite(saved);
    } catch (e) { problem("e-problem", e.message + ". Your draft is kept."); fail(e); }
  });
}
// What changed: the version in the store now, beside the draft in the form.
// Marked are the other side's changes: what differs from the version the draft
// was started on, not the owner's own edits. Before the choice it stands open;
// after Keep my draft it stays, folded, until the draft is saved, so a line
// from the other machine can still be copied in by hand.
function changedHTML(d, f, choosing) {
  const base = f.base || f;
  const was = new Set(String(base.body).split("\n").map((l) => l.trim()));
  const body = String(d.body).split("\n").map((l) => was.has(l.trim()) ? esc(l) : `<mark class="theirs">${esc(l)}</mark>`).join("\n");
  const fields = ["scope", "type"].filter((k) => d[k] !== base[k]).map((k) => `<dt>${k === "scope" ? "Reaches" : "Kind"}</dt><dd><mark class="theirs">${esc(d[k])}</mark></dd>`).join("");
  const last = d.history && d.history[0] ? `${d.history[0].yours ? "you" : d.history[0].who}, ${plainDate(d.history[0].when)}, ${d.history[0].subject}` : "unknown";
  const inner = `<p class="text">${d.description !== base.description ? `<mark class="theirs">${esc(d.description)}</mark>` : esc(d.description)}</p>
    <p class="body">${body}</p>${fields ? `<dl class="fields">${fields}</dl>` : ""}
    <p class="tl-sub">Underlined: what changed in the store since you started this draft. Last change: ${esc(last)}.</p>`;
  return choosing
    ? `<aside class="card changed" aria-label="The version in the store now"><div class="card-head"><span class="who">In the store now</span></div>${inner}</aside>`
    : `<aside class="card changed kept" aria-label="The version your draft replaces"><details open><summary>The version your draft replaces</summary>${inner}</details></aside>`;
}
function keepMine() {
  const d = desk.detail; if (!d) return;
  const f = readEdit(); f.rev = d.rev; f.over = d.rev; desk.drafts.set(d.name, f);
  editSheet(); slip("Your draft now edits the new version. Save when it reads right.");
}
function takeTheirs() {
  const d = desk.detail; if (!d) return;
  desk.drafts.delete(d.name); editSheet();
}
function cancelEdit() {
  const d = desk.detail; if (d) desk.drafts.delete(d.name);
  desk.mode = "show"; if (d) factSheet();
  const b = $("act-edit"); if (b) b.focus({ preventScroll: true });
}
async function refreshAfterWrite(name) {
  desk.meta = await tacitCall("Meta").catch(() => desk.meta);
  await browse(() => openFact(name, { focus: true }));
}

// --- retire, restore, undo ---

// Why a rule is theirs, in a sentence: their words, their decision, or their writing.
function wordsSentence(d) {
  if (d.owner_quote) return `In your words: “${d.owner_quote}”`;
  if (d.endorsed) return "You said this is yours.";
  if (d.harness === "human") return "You wrote this.";
  return "";
}
// This is mine: a fact the agents wrote becomes their rule, on their word.
async function mine() {
  const d = desk.detail; if (!d || !d.can_mine || desk.mode !== "show") return;
  await guarded(async () => {
    try {
      const r = await tacitCall("Mine", refOf(d));
      slip(r.saved ? "Now your rule. Agents read it as a rule in force." : (r.note || "Nothing changed."));
      await refreshAfterWrite(refOf(d));
    } catch (e) { fail(e); }
  });
}
// Undo of Not mine: agents believe the owner prefers it again.
async function undoNotMine(name) {
  if (deskViewOnly()) { slip(deskViewOnly()); return; }
  await guarded(async () => {
    try {
      const r = await tacitCall("UndoNotMine", name);
      slip(r.saved ? "Undone. It is back in What agents believe you prefer." : (r.note || "Nothing changed."));
      await refreshAfterWrite(name);
    } catch (e) { fail(e); }
  });
}
// Not mine: a fact agents believe the owner prefers is an agent's, not their rule.
async function notMine() {
  const d = desk.detail; if (!d || !d.can_not_mine || desk.mode !== "show") return;
  const name = refOf(d);
  await guarded(async () => {
    try {
      const r = await tacitCall("NotMine", name);
      slip(r.saved ? "Recorded: an agent's, not your rule. Agents treat it like any fact now." : (r.note || "Nothing changed."), r.saved ? { undo: () => undoNotMine(name), label: "Undo (u)" } : {});
      await refreshAfterWrite(name);
    } catch (e) { fail(e); }
  });
}
async function retire(yes) {
  const d = desk.detail; if (!d || d.inbox || desk.mode !== "show" || !(yes ? d.can_retire : d.can_restore)) return;
  await guarded(async () => {
    try {
      const r = await tacitCall(yes ? "Retire" : "Restore", refOf(d));
      slip(r.saved ? (yes ? "Retired. It stays in history and stops reaching agents." : "Restored. It reaches agents again.") : (r.note || "Nothing changed."), r.saved ? { undo: () => undo(refOf(d)), label: "Undo (u)" } : {});
      await refreshAfterWrite(refOf(d));
    } catch (e) { fail(e); }
  });
}
// undo reverts the owner's last change to the named fact: the one a slip was
// shown for, or the one open. It asks the store first, so a slip clicked after
// moving on never undoes another fact, and nothing runs when undo is not open.
async function undo(name) {
  name = name || (desk.detail && !desk.detail.inbox ? refOf(desk.detail) : "");
  if (!name) return;
  await guarded(async () => {
    try {
      const d = await tacitCall("Fact", name);
      if (!d.undo || !d.undo.can) { slip((d.undo && d.undo.why) || "Nothing of yours to undo here."); return; }
      const r = await tacitCall("Undo", name);
      slip(r.saved ? "Undone. It is back to what it was before your last change." : (r.note || "Nothing to undo."));
      await refreshAfterWrite(name);
    } catch (e) { fail(e); }
  });
}

// --- new rule ---

function openRuling(prefill = {}) {
  if (deskViewOnly()) { slip(deskViewOnly()); return; }
  const d = desk.detail;
  const repo = prefill.repo ?? (desk.view !== "needs" ? (desk.lists[desk.view] || {}).q?.repo || (d && !d.inbox ? repoOfScope(d.scope) : "") : "");
  desk.ruling = { body: prefill.body || "", description: prefill.description || "", name: prefill.name || "", repo, scope: prefill.scope || (repo ? "projects/" + repo : ""), origin: prefill.origin || "", replaces: prefill.replaces || "", nameEdited: !!prefill.name, descEdited: !!prefill.description };
  desk.back = desk.view;
  if (desk.view === "needs") setView("rules", { then: () => {} });
  desk.mode = "ruling";
  rulingSheet();
}
function scopeChoices(r, typed = "The one you typed.") {
  const host = desk.meta ? desk.meta.host : "this-machine";
  const out = [];
  if (r.repo) out.push(["projects/" + r.repo, `Agents working in ${r.repo}.`]);
  out.push(["machines/" + host, `Agents on ${host} only.`]);
  out.push(["user", "Every agent in every repo: who you are and how you work."]);
  out.push(["system", "Every session of every agent."]);
  if (r.scope && !out.some(([s]) => s === r.scope)) out.unshift([r.scope, typed]);
  return out;
}
function rulingSheet() {
  const r = desk.ruling, el = $("fact-sheet");
  el.innerHTML = `
    <p class="crumb">${r.replaces ? "New rule, replacing another fact" : "New rule"}</p>
    <h2 class="fact-text">What is the rule?</h2>
    ${r.replaces ? `<p class="where">It replaces ${factLink(r.replaces, "live")}. Its words are here to start from; change them to what holds now.</p>` : ""}
    <p class="hint">Agents read it as a rule in force, and no agent may change or retire it. Keep it as narrow as it is true.</p>
    <form class="form" id="ruling-form" novalidate aria-label="New rule">
      <label class="field"><span class="flabel">In your words</span><textarea id="r-body" class="text" rows="5" placeholder="Say it the way you would say it to an agent.">${esc(r.body)}</textarea></label>
      <label class="field"><span class="flabel">Which repo is it about?</span><input id="r-repo" value="${esc(r.repo)}" placeholder="A repo name, or empty when it is not about one" spellcheck="false" autocomplete="off"></label>
      <fieldset class="scopes" id="r-scopes"><legend>Who it reaches, narrowest first</legend></fieldset>
      <p class="nudge" id="r-nudge"></p>
      <label class="field"><span class="flabel">What agents read <i id="r-count"></i></span><input id="r-desc" value="${esc(r.description)}" placeholder="Your first sentence, unless you write one" autocomplete="off"></label>
      <details class="more"><summary>File name</summary><label class="field"><span class="flabel">Name</span><input id="r-name" value="${esc(r.name)}" spellcheck="false" autocomplete="off"></label><p class="tl-sub" id="r-folio"></p></details>
      <div class="problem" id="r-problem" role="alert"></div>
      <div class="actions">${btn("act-save", "check", "Save rule", "ctrl ↵", { primary: true })}${btn("act-cancel", "x", "Cancel", "esc")}</div>
      <p class="hint">${r.replaces ? `Saving makes one commit in your name: the new rule, and the fact it replaces marked as replaced by it. That fact stops reaching agents and stays in history; Undo on it brings it back.` : r.origin ? `Made from the inbox note ${esc(r.origin)}; the note stays as it is.` : "Saving makes one commit in your name. If tacit's checks fail, nothing is saved and your draft stays."}</p>
    </form>`;
  const sync = () => {
    r.body = $("r-body").value;
    r.repo = slugify($("r-repo").value) === $("r-repo").value.trim() ? $("r-repo").value.trim() : slugify($("r-repo").value);
    if (!r.descEdited) { const s = firstSentence(r.body); $("r-desc").value = s; r.description = s; } else r.description = $("r-desc").value;
    if (!r.nameEdited) { let s = slugify(r.description); if (s && s === r.replaces) s = (s.length > 57 ? s.slice(0, 57).replace(/-+$/, "") : s) + "-v2"; $("r-name").value = s; r.name = s; } else r.name = $("r-name").value.trim();
    paintRulingHints();
  };
  $("r-body").oninput = sync;
  $("r-repo").oninput = () => { const had = r.scope.startsWith("projects/") || !r.scope; sync(); if (had) r.scope = r.repo ? "projects/" + r.repo : ""; paintScopes(); paintRulingHints(); };
  $("r-desc").oninput = () => { r.descEdited = true; sync(); };
  $("r-name").oninput = () => { r.nameEdited = true; sync(); };
  $("act-save").onclick = saveRuling; $("act-cancel").onclick = cancelRuling;
  $("ruling-form").onsubmit = (e) => { e.preventDefault(); saveRuling(); };
  // A form that starts from words (Replace with a new rule) names itself from them.
  if (r.body && !r.nameEdited && !r.name) sync();
  paintScopes(); paintRulingHints();
  el.querySelectorAll("textarea").forEach(grow);
  $("r-body").focus();
}
function firstSentence(body) {
  const line = String(body).replace(/\s+/g, " ").trim();
  const m = /^(.+?[.!?])(\s|$)/.exec(line);
  const s = m ? m[1] : line;
  return [...s].length > 200 ? [...s].slice(0, 199).join("").replace(/\s+\S*$/, "") + "…" : s;
}
function paintScopes() {
  const r = desk.ruling, box = $("r-scopes");
  box.innerHTML = '<legend>Who it reaches, narrowest first</legend>' + scopeChoices(r).map(([s, what]) => `<label class="choice"><input type="radio" name="r-scope" value="${esc(s)}"${s === r.scope ? " checked" : ""}><span><b>${esc(cap(whereWords(s)))}</b> <i>${esc(what)}</i></span></label>`).join("");
  box.querySelectorAll("input").forEach((x) => x.onchange = () => { r.scope = x.value; paintRulingHints(); });
}
function paintRulingHints() {
  const r = desk.ruling;
  const n = [...String(r.description || firstSentence(r.body)).replace(/\s+/g, " ").trim()].length;
  $("r-count").textContent = `${n} of 200`; $("r-count").className = n > 200 ? "over" : "";
  $("r-nudge").textContent = !r.scope ? (r.repo ? "" : "Name the repo it is about, or choose who it reaches.") : scopeNudge(r.scope, r.repo ? "projects/" + r.repo : "");
  $("r-folio").textContent = r.name ? `Saved as ${r.scope || "?"}/${r.name}.md` : "";
}
async function saveRuling() {
  const r = desk.ruling; if (!r || desk.mode !== "ruling") return;
  if (!r.scope) { problem("r-problem", "Choose who it reaches first."); return; }
  await guarded(async () => {
    try {
      const res = await tacitCall("NewRuling", { name: r.name, description: r.description, body: r.body, scope: r.scope, origin: r.origin, replaces: r.replaces });
      if (res.lint && res.lint.length) { problem("r-problem", res.note, res.lint); slip(res.note, { error: true }); return; }
      desk.ruling = null; desk.mode = "show";
      slip(r.replaces ? `Saved your rule. ${r.replaces} is replaced by it and stops reaching agents.` : "Saved your rule.");
      if (desk.view !== "rules") setView("rules", { noList: true });
      await refreshAfterWrite(res.name);
    } catch (e) { problem("r-problem", e.message + ". Your draft is kept."); fail(e); }
  });
}
function cancelRuling() {
  desk.ruling = null; desk.mode = "show";
  const back = desk.back;
  if (back === "needs") setView("needs"); else if (desk.detail) factSheet(); else browse();
}
// Replace with a new rule: the rule form starts from this fact's words and
// scope; saving replaces this fact with the new rule in one commit.
function replaceWith() {
  const d = desk.detail; if (!d || !d.can_replace || desk.mode !== "show") return;
  openRuling({ body: d.body || d.description, scope: d.scope, repo: repoOfScope(d.scope), replaces: refOf(d) });
}
function promote() {
  const d = desk.detail; if (!d || !d.inbox) return;
  openRuling({ body: d.body || d.description, description: d.description, name: d.name, repo: repoOfScope(d.scope), origin: d.name });
}

// --- broken links in rules, in Needs you ---

function linkSheet(it) {
  const inner = $("sheet-inner"), K = KINDS.link, l = it.link || {};
  const open = it.state === "open";
  const fix = !open ? "" : l.would === "plain text" || !l.would ? "The fix keeps the words and drops the link. Nothing else in the rule changes." : `The fix points it to ${l.would.replace(/^\[\[|\]\]$/g, "")} instead. Nothing else in the rule changes.`;
  const why = it.state === "settled" ? "The rule no longer has this link." : it.state === "gone" ? "This rule was retired or replaced, so there is nothing to fix." : fix;
  inner.innerHTML = `
    <h2 class="question">${esc(K.q)}</h2>
    <p class="suggest quiet">The curator never edits your rules, so it left this for you.</p>
    <div class="pair">${card(it.a, "a", "Your rule")}
      <aside class="side-col" aria-label="The link"><h3>The link</h3><ul class="sum"><li>It points to <span class="dead">${esc(l.target)}</span>, ${!l.why || /^no fact is named /.test(l.why) ? "a fact that does not exist" : "and " + esc(l.why)}.</li></ul></aside></div>
    <div class="choices" role="group" aria-label="Your answer">${btn("act-apply", "link", "Apply the fix", "a", { primary: can(it, "apply"), off: !can(it, "apply") })}${btn("act-editruling", "edit", "Edit the rule", "e", { off: !can(it, "edit") })}${btn("later", "skip", "Skip for now", "j")}</div>
    <p class="hint" id="hint">${esc(why)}</p>${notesHTML(it)}${noteBoxHTML(it)}`;
  wireNote(it);
  $("act-apply").onclick = () => applyLink(it);
  $("act-editruling").onclick = () => jumpTo(it.a.name, { edit: true, facts: true });
  $("later").onclick = () => move(1);
}
async function applyLink(it) {
  if (!can(it, "apply")) return;
  await guarded(async () => {
    try {
      const r = await tacitCall("ApplyLinkFix", it.id);
      slip(r.saved ? "Fixed the link." : (r.note || "Nothing changed."));
      const next = view.slice(cur + 1).find((x) => x.needs_owner);
      await load(next ? next.id : it.id, true);
    } catch (e) { fail(e); }
  });
}

// --- find any fact, from every place ---

// The box at the top searches every fact, the owner's rules among them, and
// the retired and replaced ones too when its box is ticked. A result opens in
// All facts with its actions; the list beside it holds the same search.
// The retired-and-replaced box sits at the head of the results, so it is
// drawn with them; finder.gone keeps its state between draws.
const finder = { rows: [], matched: 0, cur: -1, seq: 0, timer: null, gone: false };
const findGone = () => { const g = $("top-gone"); if (g && typeof g.checked === "boolean") finder.gone = g.checked; return finder.gone; };
function findQuery(text, gone, limit) {
  return { q: text, status: gone ? "all" : "live", where: "curated", scope: "", repo: "", type: "", kind: "", rulings: false, believed: false, limit };
}
function focusFind() { const q = $("top-q"); if (!q) return; q.focus(); if (q.select) q.select(); }
function findSoon() { clearTimeout(finder.timer); finder.timer = setTimeout(() => findNow(), 140); }
async function findNow() {
  const q = $("top-q"); if (!q) return;
  const text = q.value.trim(), seq = ++finder.seq;
  if (!text) { finder.rows = []; finder.cur = -1; paintFind(); return; }
  try {
    const r = await tacitCall("Browse", findQuery(text, findGone(), 12));
    if (seq !== finder.seq) return;
    finder.rows = r.rows || []; finder.matched = r.matched || finder.rows.length; finder.cur = finder.rows.length ? 0 : -1;
    paintFind();
  } catch (e) { if (seq === finder.seq) fail(e); }
}
function closeFind() {
  const ul = $("top-results"); if (!ul) return;
  ul.hidden = true; ul.innerHTML = "";
  $("top-q").setAttribute("aria-expanded", "false"); $("top-q").removeAttribute("aria-activedescendant");
}
function paintFind() {
  const ul = $("top-results"), q = $("top-q"); if (!ul || !q) return;
  const text = q.value.trim();
  if (!text) { closeFind(); return; }
  const n = (x) => Number(x || 0).toLocaleString("en-GB");
  const rows = finder.rows.map((r, i) => {
    const meta = [r.name, cap(whereWords(r.scope)), r.ruling ? "your rule" : "", r.retired ? "retired" : r.superseded_by ? "replaced" : ""].filter(Boolean).join(", ");
    return `<li role="option" id="top-r-${i}" data-i="${i}" aria-selected="${i === finder.cur}" class="${i === finder.cur ? "active" : ""}${r.retired || r.superseded_by ? " gone" : ""}"><span class="row-text">${esc(r.description || r.name)}</span><span class="row-meta">${esc(meta)}</span></li>`;
  }).join("");
  const none = finder.rows.length ? "" : `<li class="none">No fact matches “${esc(text)}”${findGone() ? "" : ". Retired and replaced facts are left out; tick Retired and replaced too to search them"}.</li>`;
  const more = finder.matched > finder.rows.length ? `<li class="more" data-all="1">All ${n(finder.matched)} in All facts</li>` : "";
  const opts = `<li class="find-opts"><label class="find-gone"><input type="checkbox" id="top-gone"${finder.gone ? " checked" : ""}> Retired and replaced too</label></li>`;
  const onBox = document.activeElement && document.activeElement.id === "top-gone";
  ul.innerHTML = opts + rows + none + more;
  if (onBox && $("top-gone")) $("top-gone").focus();
  ul.hidden = false;
  q.setAttribute("aria-expanded", "true");
  { const g = $("top-gone"); if (g) g.checked = finder.gone; }
  if (finder.cur >= 0) { q.setAttribute("aria-activedescendant", "top-r-" + finder.cur); const a = ul.querySelector && ul.querySelector(".active"); if (a && a.scrollIntoView) a.scrollIntoView({ block: "nearest" }); }
  else q.removeAttribute("aria-activedescendant");
}
// Open a result (or, with none chosen, the whole search) in All facts.
function pickFind(i) {
  const q = $("top-q"), text = q ? q.value.trim() : "";
  if (!text) return;
  const row = i >= 0 ? finder.rows[i] : null;
  closeFind();
  showFound(text, row ? row.path : "", findGone() || !!(row && (row.retired || row.superseded_by)));
}
// All facts, searched for text; path, when given, is the fact that opens.
function showFound(text, path, gone, then) {
  const L = desk.lists.facts;
  Object.assign(L.q, findQuery(text, gone));
  delete L.q.limit;
  L.cur = 0;
  if (desk.view !== "facts") desk.back = desk.view;
  setView("facts", { then: () => {
    const j = path ? L.rows.findIndex((r) => r.path === path) : -1;
    if (j >= 0) L.cur = j;
    factLedger();
    if (then) then();
    else if (path) openFact(path, { focus: true });
    else if (L.rows.length) openFact(L.rows[L.cur].path);
    else blankSheet();
  } });
}
// #fact=<name> (the link tacit review --fact prints): the page opens on that
// fact, or says plainly that the store has none of that name.
function factOfHash() {
  const h = typeof location !== "undefined" && location ? String(location.hash || "") : "";
  const m = /^#fact=(.+)$/.exec(h);
  if (!m) return "";
  try { return decodeURIComponent(m[1]).trim(); } catch (_) { return m[1].trim(); }
}
async function openFromHash() {
  const name = factOfHash(); if (!name) return;
  let d;
  try { d = await tacitCall("Fact", name); } catch (e) { noSuchFact(name, e.message); return; }
  showFound(d.name, d.path, !!(d.retired || d.superseded_by), () => openFact(d.path, { focus: true }));
}
function noSuchFact(name, why) {
  const missing = /^no fact named /i.test(String(why || ""));
  showFound(name, "", true, () => {
    $("fact-sheet").innerHTML = missing
      ? `<div class="done"><h2>No fact is named ${esc(name)}.</h2><p>The link asked for ${esc(name)}, and the store has no fact of that name: not in use, not retired, not replaced. ${desk.lists.facts.rows.length ? "The facts beside this are the ones the search finds for it." : "The search finds nothing for it either."} Find any fact from the box at the top (/).</p></div>`
      : `<div class="done"><h2>${esc(name)} could not be opened.</h2><p>${esc(why || "No answer from tacit.")}</p></div>`;
    desk.detail = null;
  });
}

// --- keys, start ---

document.addEventListener("click", (e) => {
  const b = e.target instanceof Element && e.target.closest(".open-fact");
  if (!b) return;
  e.preventDefault();
  jumpTo(b.dataset.fact, { facts: true });
});
const PLACES = { 1: "needs", 2: "rules", 3: "believed", 4: "facts", 5: "inbox", 6: "folders" };
document.addEventListener("keydown", (e) => {
  const t = e.target instanceof Element ? e.target : document.body;
  const typing = t.matches("input, select, textarea");
  // The finder at the top keeps its own keys, whatever the place is doing (a draft stays as it is).
  if (t.id === "top-q") {
    if (e.key === "Escape") { if (t.value) { t.value = ""; t.dispatchEvent(new Event("input", { bubbles: true })); } closeFind(); t.blur(); }
    return;
  }
  if (desk.mode === "edit" || desk.mode === "ruling") {
    const conflict = desk.mode === "edit" && $("act-mine");
    if (isCombo(e)) { e.preventDefault(); if (!conflict) desk.mode === "edit" ? saveEdit() : saveRuling(); return; }
    if (conflict && !typing && !e.ctrlKey && !e.metaKey && (e.key === "m" || e.key === "t")) { e.preventDefault(); e.key === "m" ? keepMine() : takeTheirs(); return; }
    if (e.key === "Escape") {
      e.preventDefault();
      if (conflict) { $("act-mine").focus(); slip("Choose first: Keep my draft (m) or Take the new version (t). Your draft is kept."); return; }
      desk.mode === "edit" ? cancelEdit() : cancelRuling(); return;
    }
    if (typing) return;
  }
  if (typing) {
    if (e.key === "Escape") {
      if ((t.id === "q" || t.id === "find") && t.value) { t.value = ""; t.dispatchEvent(new Event("input", { bubbles: true })); }
      t.blur();
    }
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  // Keys every place takes.
  if (PLACES[e.key]) { setView(PLACES[e.key]); e.preventDefault(); return; }
  if (e.key === "/") { focusFind(); e.preventDefault(); return; }
  if (e.key === "n" && desk.mode !== "ruling") { openRuling(); e.preventDefault(); return; }
  if (desk.view === "needs") {
    const it = view[cur];
    if (it && it.link) {
      if (e.key === "a") { applyLink(it); e.preventDefault(); }
      else if (e.key === "e" && can(it, "edit")) { jumpTo(it.a.name, { edit: true, facts: true }); e.preventDefault(); }
    }
    return; // app.js takes the rest
  }
  if (desk.mode !== "show") return;
  const d = desk.detail;
  switch (e.key) {
    case "j": case "ArrowDown": moveFact(1); break;
    case "k": case "ArrowUp": moveFact(-1); break;
    case "Home": if (t.closest("#facts")) { desk.lists[desk.view].cur = 0; moveFact(0); } else return; break;
    case "End": if (t.closest("#facts")) { const L = desk.lists[desk.view]; L.cur = L.rows.length - 1; moveFact(0); } else return; break;
    case "e": startEdit(); break;
    case "r": if (d && d.can_restore) retire(false); else retire(true); break;
    case "u": if (d && d.undo && d.undo.can) undo(refOf(d)); else return; break;
    case "p": promote(); break;
    case "m": if (d && d.can_mine) mine(); else return; break;
    case "x": if (d && d.can_not_mine) notMine(); else if (d && d.not_mine && desk.mode === "show") undoNotMine(refOf(d)); else return; break;
    case "d": lights(); break;
    case "Escape": if (desk.back && desk.back !== desk.view) setView(desk.back); else return; break;
    default: return;
  }
  e.preventDefault();
});
for (const b of document.querySelectorAll(".place")) b.onclick = () => setView(b.dataset.view);
$("new-ruling").onclick = () => openRuling();
const L = () => desk.lists[desk.view];
$("q").addEventListener("input", (e) => { L().q.q = e.target.value; L().cur = 0; browseSoon(); });
$("f-btn").onclick = () => { const p = $("f-panel"); p.hidden = !p.hidden; $("f-btn").setAttribute("aria-expanded", String(!p.hidden)); };
$("f-clear").onclick = () => { Object.assign(L().q, DEFAULTS[desk.view], { scope: "", repo: "", type: "", kind: "" }); L().cur = 0; paintFilters(); browse(); };
for (const [id, k] of [["f-status", "status"], ["f-where", "where"], ["f-scope", "scope"], ["f-repo", "repo"], ["f-type", "type"], ["f-kind", "kind"]]) {
  $(id).addEventListener("change", (e) => { L().q[k] = e.target.value; L().cur = 0; paintFilters(); browse(); });
}
for (const sel of document.querySelectorAll(".filter-panel select")) upgradeSelect(sel);
{
  const q = $("top-q"), ul = $("top-results");
  if (q && ul) {
    q.addEventListener("input", findSoon);
    q.addEventListener("focus", () => { if (q.value.trim()) findNow(); });
    // The results close when focus leaves the box and its results (Tab reaches their checkbox).
    const box = $("find-all") || q;
    box.addEventListener("focusout", () => setTimeout(() => { if (!(box.contains && box.contains(document.activeElement))) closeFind(); }, 0));
    q.addEventListener("keydown", (e) => {
      const n = finder.rows.length;
      if (e.key === "ArrowDown" && n) { finder.cur = (finder.cur + 1) % n; paintFind(); e.preventDefault(); }
      else if (e.key === "ArrowUp" && n) { finder.cur = (finder.cur - 1 + n) % n; paintFind(); e.preventDefault(); }
      else if (e.key === "Enter") { clearTimeout(finder.timer); e.preventDefault(); if (n || !q.value.trim()) pickFind(finder.cur); else findNow().then(() => pickFind(finder.cur)); }
    });
    // Focus stays in the box while a result or the checkbox is clicked.
    ul.addEventListener("mousedown", (e) => e.preventDefault());
    ul.addEventListener("click", (e) => { const li = e.target instanceof Element && e.target.closest("li"); if (!li) return; if (li.dataset.i !== undefined) pickFind(+li.dataset.i); else if (li.dataset.all) pickFind(-1); });
    ul.addEventListener("change", (e) => { if (e.target && e.target.id === "top-gone") { finder.gone = !!e.target.checked; findNow(); } });
    ul.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); closeFind(); q.focus(); } });
  }
}
if (typeof window !== "undefined" && window.addEventListener) window.addEventListener("hashchange", () => openFromHash());
(async () => {
  try { desk.meta = await tacitCall("Meta"); } catch (e) { fail(e); }
  paintViewOnly();
  await openFromHash();
})();
