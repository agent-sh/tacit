package main

import (
	"os/exec"
	"testing"
)

// Exercise the page's actual click handler and the FolderSave payload, not a
// second copy of its classification expression.
func TestFoldersUICreateNewKind(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `
const assert = require("node:assert/strict");
const vm = require("node:vm");
const fs = require("node:fs");
const els = { "new-folder": { value: "" }, "new-desc": { value: "test" } };
let click;
const saved = [];
class Element { closest() { return this; } }
const context = {
  Element,
  document: {
    getElementById: (id) => els[id],
    addEventListener: (name, handler) => { if (name === "click") click = handler; },
  },
  tacitCall: async (method, draft) => {
    if (method === "FolderSave") { saved.push(draft); return { saved: true }; }
    return { rows: [] };
  },
};
vm.runInNewContext(fs.readFileSync("reviewui/folders.js", "utf8"), context);
(async () => {
  await context.tacitFolders.show();
  for (const [folder, kind] of [
    ["topics/security", "topic"],
    ["areas/work/topics/security", "topic"],
    ["areas/work", "area"],
    ["areas/topics", "area"],
    ["projects/topics/security", "area"],
    ["prefix/areas/work/topics/security", "area"],
    ["areas/work/nested/topics/security", "area"],
    ["not-topics/security", "area"],
    ["  topics/security  ", "topic"],
  ]) {
    els["new-folder"].value = folder;
    const target = new Element();
    target.dataset = { act: "create-new" };
    click({ target, preventDefault() {} });
    await new Promise(setImmediate);
    const draft = saved.pop();
    assert.ok(draft, folder + ": no save");
    assert.equal(draft.folder, folder.trim());
    assert.equal(draft.kind, kind, folder);
  }
})().catch((err) => { console.error(err); process.exitCode = 1; });
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("folders.js: %v\n%s", err, out)
	}
}
