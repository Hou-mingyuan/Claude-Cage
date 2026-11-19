import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { appendFile, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fetchBuildArtifact } from "./fetch-build-artifact.mjs";

const content = Buffer.from("verified build input");
const checksum = createHash("sha256").update(content).digest("hex");
const url = "https://build-fixture.invalid/dependency.gz";

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), "cage-build-download-test-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  return { root, destination: join(root, "dependency.gz") };
}

test("an interrupted transfer resumes and the verified file becomes a cache hit", async t => {
  const { root, destination } = await fixture(t);
  const chunk = content.subarray(0, 8);
  const offsets = [];
  await fetchBuildArtifact(url, checksum, destination, async (_url, part, resume) => {
    await assert.rejects(readFile(destination), { code: "ENOENT" });
    if (!resume) {
      offsets.push(0);
      await writeFile(part, chunk);
      return { code: 18, http: "200" };
    }
    offsets.push((await readFile(part)).length);
    await appendFile(part, content.subarray(chunk.length));
    return { code: 0, http: "206" };
  });
  assert.deepEqual(offsets, [0, 8]);
  assert.deepEqual(await readFile(destination), content);
  assert.deepEqual(await readdir(root), ["dependency.gz"]);
  await fetchBuildArtifact(url, checksum, destination, () => assert.fail("warm cache must not download"));
});

test("a server that rejects Range causes a safe full retry", async t => {
  const { destination } = await fixture(t);
  const attempts = [];
  await fetchBuildArtifact(url, checksum, destination, async (_url, part, resume) => {
    attempts.push(resume);
    if (attempts.length === 1) {
      await writeFile(part, "partial");
      return { code: 18, http: "200" };
    }
    if (resume) return { code: 33, http: "200" };
    await assert.rejects(readFile(part), { code: "ENOENT" });
    await writeFile(part, content);
    return { code: 0, http: "200" };
  });
  assert.deepEqual(attempts, [false, true, false]);
  assert.deepEqual(await readFile(destination), content);
});

test("a complete file with a wrong checksum is never published", async t => {
  const { root, destination } = await fixture(t);
  await assert.rejects(fetchBuildArtifact(url, checksum, destination, async (_url, part) => {
    await writeFile(part, "not the reviewed artifact");
    return { code: 0, http: "200" };
  }), /checksum mismatch/);
  assert.deepEqual(await readdir(root), []);
});
