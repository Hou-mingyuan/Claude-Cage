import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { checkFiles, classify } from "./check-script-syntax.mjs";

test("classifies scripts and skips vendored sources", () => {
  assert.equal(classify("scripts/a.mjs"), "node");
  assert.equal(classify("internal/cage/a.py"), "python");
  assert.equal(classify("tests/a.sh"), "shell");
  assert.equal(classify("ui/vendor/novnc/core/rfb.js"), null);
  assert.equal(classify("third_party/x/a.sh"), null);
  assert.equal(classify("README.md"), null);
});

test("reports duplicate declarations and other parse errors", () => {
  const dir = mkdtempSync(join(tmpdir(), "syntax-test-"));
  try {
    const files = {
      "dup.mjs": "import { setTimeout as delay } from 'node:timers/promises';\nimport { setTimeout as delay } from 'node:timers/promises';\n",
      "ok.mjs": "export const a = 1;\n",
      "bad.py": "def f(:\n  pass\n",
      "ok.py": "print(1)\n",
      "bad.sh": "if true; then\n",
      "ok.sh": "echo ok\n",
    };
    for (const [name, body] of Object.entries(files)) writeFileSync(join(dir, name), body);
    const failures = checkFiles(Object.keys(files).map(name => join(dir, name)));
    const failed = failures.map(line => line.split(":")[0].split("/").pop()).sort();
    assert.deepEqual(failed, ["bad.py", "bad.sh", "dup.mjs"]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("every tracked script in this checkout parses", () => {
  // Fails when a script such as browser-last-tab-smoke.mjs carries a
  // duplicate import, even though no workflow runs it.
  const files = execFileSync("git", ["ls-files", "-z"], { encoding: "utf8" }).split("\0").filter(Boolean);
  assert.deepEqual(checkFiles(files), []);
});

