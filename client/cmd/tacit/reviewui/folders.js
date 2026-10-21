// tacit app: Folders. What each project, area and topic folder is (its
// record), and the owner's channel for it: make a folder, set its status
// with a reason, add another name, fold it into another folder, set an
// area's tools. Every action is one commit of the owner's; an agent's
// changes to a status you set only ever ask for your review.
"use strict";
(function () {
  const call = globalThis.tacitCall;
  const e = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const el = (id) => document.getElementById(id);
  let data = null;
  const list = (s) => String(s || "").split(",").map((x) => x.trim()).filter(Boolean);
  const say = (msg, error) => { const s = el("slip"); if (s) { s.textContent = msg; s.classList.toggle("error", !!error); s.classList.add("on"); setTimeout(() => s.classList.remove("on"), 4000); } };

  function statusSelect(id, cur) {
    return `<select id="${id}" aria-label="Status">${(data.statuses || []).map((s) => `<option value="${e(s)}"${s === cur ? " selected" : ""}>${e(s)}</option>`).join("")}</select>`;
  }

  function rowHTML(r, i) {
    const who = r.by === "owner" ? "you" : r.by === "opus" ? "the curator" : "";
    const head = r.record
      ? `<b>${e(r.folder)}</b> <span class="tag">${e(r.kind)}, ${e(r.status)}</span>${r.suppressed_by ? ` <span class="tag">folded into ${e(r.suppressed_by)}</span>` : ""}`
      : `<b>${e(r.folder)}</b> <span class="tag">no record yet</span>`;
    const meta = r.record
      ? `<p>${e(r.description)}</p><p class="small">${r.facts} fact(s)${r.aliases && r.aliases.length ? `; also known as ${e(r.aliases.join(", "))}` : ""}${r.date ? `; ${e(r.date)}${who ? ", " + who : ""}: ${e(r.why)}` : ""}</p>`
      : `<p class="small">${r.facts} fact(s) and no record.</p>`;
    if (data.view_only || data.format) return `<li class="folder">${head}${meta}</li>`;
    const forms = r.record
      ? `<div class="folder-forms">
          <label>Status ${statusSelect("st-" + i, r.status)}</label> <input id="why-${i}" type="text" placeholder="Why (kept in its history)" aria-label="Why">
          <button type="button" class="btn" data-act="status" data-i="${i}">Set status</button>
          <br><input id="al-${i}" type="text" placeholder="Another name" aria-label="Another name"> <button type="button" class="btn" data-act="alias" data-i="${i}">Add the name</button>
          <br><input id="in-${i}" type="text" placeholder="projects/<name> it folds into" aria-label="Folds into"> <button type="button" class="btn" data-act="suppress" data-i="${i}">Fold into</button>
          ${r.kind === "area" ? `<br><input id="tl-${i}" type="text" value="${e((r.tools || []).join(", "))}" placeholder="Tool patterns, comma separated" aria-label="Tools"> <input id="cm-${i}" type="text" value="${e((r.commands || []).join(", "))}" placeholder="Command prefixes" aria-label="Commands"> <button type="button" class="btn" data-act="tools" data-i="${i}">Set tools</button>` : ""}
        </div>`
      : `<div class="folder-forms"><select id="kd-${i}" aria-label="Kind"><option value="repo">repository</option><option value="folder">folder (no git)</option></select>
          <input id="ds-${i}" type="text" placeholder="One line: what it is" aria-label="Description"> ${statusSelect("st-" + i, "active")}
          <button type="button" class="btn" data-act="create" data-i="${i}">Make its record</button></div>`;
    return `<li class="folder"><details><summary>${head}</summary>${meta}${forms}</details></li>`;
  }

  function paint() {
    const host = el("folders-inner");
    if (!host) return;
    const qs = (data.questions || []).map((q) => `<li><b>${e(q.folder)}</b>${q.fact ? ` (${e(q.fact)})` : ""}: ${e(q.reason)}</li>`).join("");
    const note = data.view_only ? `<p class="intro">${e(data.view_only)}</p>` : data.format ? `<p class="intro">${e(data.format)}</p>` : "";
    const create = data.view_only || data.format ? "" : `<div class="folder-forms"><input id="new-folder" type="text" placeholder="areas/<name>, topics/<name> or areas/<area>/topics/<name>" aria-label="New folder">
      <input id="new-desc" type="text" placeholder="One line: what it is" aria-label="Description"> <button type="button" class="btn primary" data-act="create-new">Make the folder</button></div>`;
    host.innerHTML = `<p class="intro">What each folder is. Agents file facts by these records; the curator decides a folder from its evidence and you can always change it. Your status is never replaced by an agent, only flagged for your review.</p>${note}
      ${qs ? `<h2>Questions for you</h2><ul class="rows">${qs}</ul>` : ""}${create}
      <ul class="rows folders-list">${data.rows.map(rowHTML).join("")}</ul>`;
  }

  async function act(kind, i) {
    const r = data.rows[i];
    let d;
    switch (kind) {
      case "status": d = { action: "status", folder: r.folder, status: el("st-" + i).value, why: el("why-" + i).value }; break;
      case "alias": d = { action: "alias", folder: r.folder, alias: el("al-" + i).value }; break;
      case "suppress": d = { action: "suppress", folder: r.folder, into: el("in-" + i).value, why: el("why-" + i) ? el("why-" + i).value : "" }; break;
      case "tools": d = { action: "tools", folder: r.folder, tools: list(el("tl-" + i).value), commands: list(el("cm-" + i).value) }; break;
      case "create": d = { action: "create", folder: r.folder, kind: el("kd-" + i).value, description: el("ds-" + i).value, status: el("st-" + i).value }; break;
      case "create-new": {
        const f = el("new-folder").value.trim();
        d = { action: "create", folder: f, kind: /^topics\/|\/topics\//.test(f) ? "topic" : "area", description: el("new-desc").value };
        break;
      }
      default: return;
    }
    try {
      const res = await call("FolderSave", d);
      if (res && res.lint && res.lint.length) { say("Not saved: " + res.lint.join("; "), true); return; }
      say(res && res.saved ? "Saved." : (res && res.note) || "Nothing to save.");
      await show();
    } catch (err) { say(err.message, true); }
  }

  async function show() {
    try { data = await call("Folders"); } catch (err) { say(err.message, true); return; }
    data.rows = data.rows || [];
    paint();
  }

  document.addEventListener("click", (ev) => {
    const b = ev.target instanceof Element && ev.target.closest("#folders [data-act]");
    if (!b) return;
    ev.preventDefault();
    act(b.dataset.act, +b.dataset.i);
  });
  globalThis.tacitFolders = { show };
})();
