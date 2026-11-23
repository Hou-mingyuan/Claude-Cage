import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, posix } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { verifyBuild } from "./verify-build.mjs";

const repoRoot = fileURLToPath(new URL("../..", import.meta.url));

test("build verification rejects corrupted or missing frontend resources", async (t) => {
  for (const problem of ["none", "zero JS", "invalid JS", "zero CSS", "missing script", "missing entry"]) {
    await t.test(problem, async () => {
      const directory = await mkdtemp(join(tmpdir(), "cage-web-integrity-"));
      try {
        await mkdir(join(directory, "assets"));
        const html = '<div id="root"></div><script type="module" src="/assets/main.js"></script><link rel="stylesheet" href="/assets/main.css">';
        await writeFile(join(directory, "index.html"), html);
        await writeFile(join(directory, "mobile.html"), problem === "missing entry" ? '<div id="root"></div>' : html);
        if (problem !== "missing script") await writeFile(join(directory, "assets/main.js"), problem === "zero JS" ? Buffer.alloc(100) : problem === "invalid JS" ? "export const =;" : "export const valid = true;");
        await writeFile(join(directory, "assets/main.css"), problem === "zero CSS" ? Buffer.alloc(10) : "body { color: black }");
        if (problem !== "none") {
          await assert.rejects(verifyBuild(directory), /(?:NUL bytes|Invalid JavaScript|Missing build resource|Missing entry script)/);
        } else {
          assert.equal(await verifyBuild(directory), 4);
          const manifest = JSON.parse(await readFile(join(directory, "build-integrity.json"), "utf8"));
          assert.match(manifest.files["assets/main.js"], /^[a-f0-9]{64}$/);
        }
      } finally { await rm(directory, { recursive: true, force: true }); }
    });
  }
});

test("npm scripts invoke local CLIs via node rather than Windows .cmd shims (#651)", async () => {
  for (const workspace of ["ui", "www"]) {
    const manifest = JSON.parse(await readFile(join(repoRoot, workspace, "package.json"), "utf8"));
    const lock = JSON.parse(await readFile(join(repoRoot, workspace, "package-lock.json"), "utf8"));
    const localBins = new Map();
    for (const [pkgKey, entry] of Object.entries(lock.packages ?? {})) {
      if (!pkgKey.startsWith("node_modules/") || !entry.bin) continue;
      for (const [binName, target] of Object.entries(entry.bin)) {
        localBins.set(binName, posix.normalize(`${pkgKey}/${target}`));
      }
    }
    for (const [name, command] of Object.entries(manifest.scripts ?? {})) {
      for (const segment of command.split("&&").map((part) => part.trim()).filter(Boolean)) {
        const tokens = segment.split(/\s+/);
        assert.ok(
          !localBins.has(tokens[0]),
          `${workspace}/package.json script "${name}" invokes local .bin shim "${tokens[0]}" directly; use "node ./${localBins.get(tokens[0])}" so checkouts containing "&" work on Windows`,
        );
        if (tokens[0] === "node" && tokens[1]?.startsWith("./node_modules/")) {
          const normalized = posix.normalize(tokens[1]);
          assert.ok(
            [...localBins.values()].includes(normalized),
            `${workspace}/package.json script "${name}" points to "${tokens[1]}", which is not a declared bin entry in ${workspace}/package-lock.json`,
          );
        }
      }
    }
  }
});
