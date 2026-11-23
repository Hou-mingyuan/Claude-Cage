import { test } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DownloadManager } from "./browser-downloads.mjs";

function fixture(t, save) {
  const directory = mkdtempSync(join(tmpdir(), "cage-download-test-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const options = { directory, save };
  const manager = new DownloadManager(options);
  const begin = (name = "report.txt") => {
    const id = randomUUID();
    manager.begin(id, name);
    writeFileSync(join(directory, "Downloads", id), "complete source");
    return id;
  };
  return { directory, options, manager, begin };
}

function completed(manager, id) {
  manager.progress({ guid: id, state: "completed", receivedBytes: 15, totalBytes: 15 });
  return manager.queue;
}

test("failed saves persist and can be retried after controller restart", async t => {
  let attempts = 0;
  const f = fixture(t, async (id, name) => {
    if (++attempts === 1) throw new Error("共享连接暂时不可用");
    unlinkSync(join(f.directory, "Downloads", id));
    return { ok: true, path: `/shared/Downloads/${name}`, name, bytes: 15 };
  });
  const id = f.begin();
  await completed(f.manager, id);
  assert.equal(f.manager.snapshot()[0].state, "failed");
  assert.equal(f.manager.snapshot()[0].retryable, true);
  assert.equal(statSync(join(f.directory, "download-state.json")).mode & 0o777, 0o600);
  const reopened = new DownloadManager(f.options);
  assert.equal(reopened.snapshot()[0].id, id);
  assert.equal(reopened.snapshot()[0].retryable, true);
  assert.deepEqual(reopened.retry(id), { ok: true });
  assert.deepEqual(reopened.retry(id), { ok: true }, "duplicate retry should not enqueue another copy");
  await reopened.queue;
  assert.equal(attempts, 2);
  assert.equal(reopened.snapshot()[0].state, "saved");
  assert.equal(reopened.snapshot()[0].path, "/shared/Downloads/report.txt");
  assert.equal(new DownloadManager(f.options).snapshot()[0].state, "saved");
});

test("interrupted copies are retryable only with a complete source, interrupted downloads are not", async t => {
  let release;
  const f = fixture(t, () => new Promise(resolve => { release = resolve; }));
  const saving = f.begin();
  completed(f.manager, saving);
  await Promise.resolve();
  const downloading = f.begin("partial.txt");
  const reopened = new DownloadManager(f.options);
  const records = reopened.snapshot();
  assert.equal(records.find(r => r.id === saving).retryable, true);
  assert.equal(records.find(r => r.id === downloading).retryable, false);
  assert.ok(records.every(r => r.state === "failed"));
  unlinkSync(join(f.directory, "Downloads", saving));
  assert.throws(() => reopened.retry(saving), /源文件/);
  release({ ok: false, error: "interrupted" });
  await f.manager.queue;
});

test("finished download history stays bounded without losing current transfers", async t => {
  const f = fixture(t, async (_id, name) => ({ ok: true, path: `/shared/Downloads/${name}`, name }));
  const active = f.begin("still-downloading.txt");
  for (let i = 0; i < 60; i++) await completed(f.manager, f.begin(`${i}.txt`));
  const records = f.manager.snapshot();
  assert.equal(records.filter(r => r.state === "saved").length, 50);
  assert.ok(records.some(r => r.id === active && r.state === "downloading"));
  assert.equal(JSON.parse(readFileSync(join(f.directory, "download-state.json"))).length, 51);
  assert.ok(records.every(r => !("sourceReady" in r) && !("url" in r)));
});

test("recoverable failed downloads survive ordinary history pruning and restart until saved or discarded", async t => {
  let failFirst = true;
  const f = fixture(t, async (id, name) => {
    if (name === "precious.zip" && failFirst) throw new Error("共享目录暂时不可写");
    unlinkSync(join(f.directory, "Downloads", id));
    return { ok: true, path: `/shared/Downloads/${name}`, name, bytes: 15 };
  });
  const failedId = f.begin("precious.zip");
  await completed(f.manager, failedId);
  assert.equal(f.manager.snapshot()[0].retryable, true);

  // Save 60 newer downloads (exceeding the 50-item ordinary history limit).
  for (let i = 0; i < 60; i++) await completed(f.manager, f.begin(`other-${i}.txt`));

  // Recoverable failed download remains visible, retryable, and accessible across restarts.
  const reopened = new DownloadManager(f.options);
  const retained = reopened.snapshot().find(r => r.id === failedId);
  assert.ok(retained, "recoverable failed download was pruned out of history");
  assert.equal(retained.state, "failed");
  assert.equal(retained.retryable, true);
  assert.equal(reopened.sourceFile(failedId), join(f.directory, "Downloads", failedId));
  assert.equal(reopened.snapshot().filter(r => r.state === "saved").length, 50);

  // Once retried and saved, it enters ordinary history and can be pruned after 50 newer saves.
  failFirst = false;
  assert.deepEqual(reopened.retry(failedId), { ok: true });
  await reopened.queue;
  assert.equal(reopened.snapshot().find(r => r.id === failedId).state, "saved");
  for (let i = 0; i < 55; i++) {
    const id = randomUUID();
    reopened.begin(id, `after-${i}.txt`);
    writeFileSync(join(f.directory, "Downloads", id), "complete source");
    await completed(reopened, id);
  }
  assert.equal(reopened.snapshot().find(r => r.id === failedId), undefined);
});

test("discarding a recoverable failed download removes its staged file and returns it to normal history", async t => {
  const f = fixture(t, async () => { throw new Error("共享目录暂时不可写"); });
  const id = f.begin("stale.bin");
  await completed(f.manager, id);
  const staged = join(f.directory, "Downloads", id);
  assert.equal(f.manager.sourceFile(id), staged);
  assert.deepEqual(f.manager.discard(id), { ok: true });
  assert.throws(() => statSync(staged), { code: "ENOENT" });
  const record = f.manager.snapshot().find(r => r.id === id);
  assert.equal(record.state, "canceled");
  assert.equal(record.retryable, false);
  assert.throws(() => f.manager.retry(id), /源文件/);
  assert.throws(() => f.manager.sourceFile(id), /源文件/);
  assert.throws(() => f.manager.discard(id), /仅支持丢弃/);
});

test("recoverable failed download capacity rejects new downloads without discarding existing files", async t => {
  const canceled = [];
  const f = fixture(t, async () => { throw new Error("共享目录暂时不可写"); });
  f.manager.cancel = id => canceled.push(id);
  const ids = [];
  for (let i = 0; i < 50; i++) {
    const id = f.begin(`pending-${i}.bin`);
    ids.push(id);
    await completed(f.manager, id);
  }
  const extra = randomUUID();
  f.manager.begin(extra, "overflow.bin");
  assert.deepEqual(canceled, [extra]);
  const overflow = f.manager.snapshot().find(r => r.id === extra);
  assert.equal(overflow.state, "canceled");
  assert.match(overflow.error, /失败下载过多/);
  // All 50 previously received recoverable files remain intact after restart.
  const reopened = new DownloadManager(f.options);
  assert.equal(reopened.snapshot().filter(r => r.retryable).length, 50);
  for (const id of ids) {
    assert.equal(reopened.sourceFile(id), join(f.directory, "Downloads", id));
  }
});
