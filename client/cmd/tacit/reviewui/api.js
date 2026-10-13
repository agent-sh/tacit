// How the page of tacit app reaches Go. In the desktop window the methods of
// App are Wails bindings (window.go.main.App): no port, no token. In a browser
// (tacit app --browser) the same methods answer POST /api/call/<Method> with
// the page's token. Either way a failure comes back as an Error with the
// server's words.
"use strict";
globalThis.tacitCall = async function tacitCall(method, ...args) {
  const w = globalThis.window;
  const bound = w && w.go && w.go.main && w.go.main.App;
  if (bound) {
    if (typeof bound[method] !== "function") throw new Error("no method " + method);
    try { return await bound[method](...args); } catch (e) { throw new Error(typeof e === "string" ? e : (e && e.message) || String(e)); }
  }
  const token = document.querySelector('meta[name="tacit-token"]').content;
  const r = await fetch("/api/call/" + method, { method: "POST", cache: "no-store", headers: { "Content-Type": "application/json", "X-Tacit-Token": token }, body: JSON.stringify(args) });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || "No answer from tacit app.");
  return body.result;
};
