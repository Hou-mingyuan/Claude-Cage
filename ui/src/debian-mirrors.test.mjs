import assert from "node:assert/strict";
import test from "node:test";
import { beginInstall, formatSpeed } from "./debian-mirrors.mjs";

function fakeApi(responses) {
  const calls = [];
  const api = async (path, body) => {
    calls.push(body === undefined ? `GET ${path}` : `POST ${path}`);
    const reply = responses[path];
    if (reply instanceof Error) throw reply;
    return typeof reply === "function" ? reply(body) : reply ?? {};
  };
  return { api, calls };
}

test("a failed save stops the install before /start", async () => {
  const { api, calls } = fakeApi({ "/host/debian-mirrors": new Error("保存软件源选择失败，请重试") });
  await assert.rejects(beginInstall(api, { mirrorChoice: "ustc" }), /保存软件源选择失败/);
  assert.deepEqual(calls, ["POST /host/debian-mirrors"]);
});

test("a saved choice starts the install with the chosen source", async () => {
  let saved;
  const { api, calls } = fakeApi({ "/host/debian-mirrors": body => { saved = body; return { decided: true }; }, "/start": { id: "job" } });
  const result = await beginInstall(api, { mirrorChoice: "ustc", checkMirrors: true, startOptions: { guest_system: "debian" } });
  assert.deepEqual(saved, { preference: "ustc" });
  assert.deepEqual(result, { accepted: { id: "job" } });
  assert.deepEqual(calls, ["POST /host/debian-mirrors", "POST /environment/initialization", "POST /start"]);
});

test("an undecided source shows the measurement instead of starting", async () => {
  const probe = { decided: false, preference: "auto", samples: [] };
  const { api, calls } = fakeApi({ "/host/debian-mirrors": probe });
  assert.deepEqual(await beginInstall(api, { checkMirrors: true }), { mirrors: probe });
  assert.deepEqual(calls, ["GET /host/debian-mirrors"]);
});

test("a decided source or a resumed install starts directly", async () => {
  const decided = fakeApi({ "/host/debian-mirrors": { decided: true }, "/start": {} });
  await beginInstall(decided.api, { checkMirrors: true });
  assert.deepEqual(decided.calls, ["GET /host/debian-mirrors", "POST /environment/initialization", "POST /start"]);
  const resumed = fakeApi({ "/start": {} });
  await beginInstall(resumed.api, {});
  assert.deepEqual(resumed.calls, ["POST /environment/initialization", "POST /start"]);
});

test("speeds are readable", () => {
  assert.equal(formatSpeed(0), "—");
  assert.equal(formatSpeed(58 * 1024), "58 KB/s");
  assert.equal(formatSpeed(3.5 * (1 << 20)), "3.5 MB/s");
});
