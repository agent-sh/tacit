// Applied before the page paints, so a saved light never flashes the other.
"use strict";
try { const t = localStorage.getItem("tacit-theme"); if (t === "light" || t === "dark") document.documentElement.dataset.theme = t; } catch (_) { /* storage off */ }
