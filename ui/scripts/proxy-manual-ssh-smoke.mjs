// Actual workbench UI with isolated API fixtures; no real proxy or user data.
import assert from "node:assert/strict";
import { generateKeyPairSync } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { createWorkbench, chooseInline } from "./workbench-fixture.mjs";

const output = path.resolve(process.env.CAGE_SSH_QA_OUTPUT || "_output/qa/manual-ssh");
await fs.mkdir(output, { recursive: true });
const { privateKey } = generateKeyPairSync("ed25519", {
  privateKeyEncoding: { type: "pkcs8", format: "pem" },
  publicKeyEncoding: { type: "spki", format: "pem" },
});
for (const platform of ["darwin", "windows", "linux"]) {
  const f = await createWorkbench({
    viewport: { width: 1280, height: 1100 },
    clientContext: { mode: "local", connection_id: "local", client_platform: platform, server_platform: platform, connection: { state: "disconnected" } },
  });
  const { page, state } = f;
  const saved = { active: false, sources: [], port: 17896, core_available: true };
  const submissions = [];
  let fail = false;
  try {
    await page.route("**/api/proxy", r => r.fulfill({ json: saved }));
    await page.route("**/api/proxy/import", r => {
      const body = r.request().postDataJSON();
      const node = JSON.parse(body.content).proxies[0];
      submissions.push({ body, node });
      const job = { id: "manual-" + submissions.length, running: false, error: fail ? "测试核心拒绝配置" : "", message: "已导入" };
      state.status.job = job;
      if (!fail) saved.sources.push({ id: job.id, name: body.name, nodes: [{ id: job.id, name: node.name, type: node.type }], remote: false, updated_at: new Date().toISOString() });
      return r.fulfill({ json: job });
    });
    await page.getByRole("button", { name: /^设置/ }).click();
    await page.getByRole("button", { name: "网络与代理", exact: true }).click();
    await page.getByRole("tab", { name: "订阅", exact: true }).click();
    const open = async () => {
      await page.getByRole("button", { name: "添加来源", exact: true }).click();
      await page.getByRole("button", { name: "手动节点", exact: true }).click();
    };
    const submit = page.getByRole("button", { name: "导入并验证", exact: true });
    const protocol = page.getByRole("combobox", { name: "协议", exact: true });
    const auth = page.getByRole("combobox", { name: "认证方式", exact: true });
    const server = page.getByLabel("服务器", { exact: true });
    const port = page.getByLabel("端口", { exact: true });
    const username = () => page.getByLabel(/用户名/);
    const password = () => page.getByLabel(/^密码/);
    const key = () => page.getByLabel("私钥内容（必填）", { exact: true });
    const passphrase = () => page.getByLabel("私钥口令（可选）", { exact: true });
    const hostKey = () => page.getByLabel("服务器公钥（可选，建议填写）", { exact: true });
    const top = locator => locator.evaluate(el => el.offsetTop);
    const waitDone = () => page.locator(".proxy-import").waitFor({ state: "hidden" });
    await open();

    // Both pointer and keyboard open an in-flow list and restore the layout.
    const before = await top(server);
    await protocol.click();
    await page.getByRole("listbox", { name: "协议选项", exact: true }).waitFor();
    assert.ok(await top(server) > before, "opening the protocol list must push later fields down");
    assert.equal(await page.getByRole("listbox").evaluate(el => getComputedStyle(el).position), "static");
    await protocol.press("Escape");
    assert.equal(await top(server), before);
    assert.equal(await protocol.evaluate(el => el === document.activeElement), true);
    await protocol.press("ArrowDown");
    await protocol.press("End");
    await protocol.press("Enter");
    assert.equal(await protocol.innerText(), "SSH");
    assert.equal(await port.inputValue(), "22");
    await page.getByLabel("节点名称", { exact: true }).fill("固定出口");
    await server.fill("203.0.113.10");
    assert.equal(await submit.isDisabled(), true, "SSH requires a user and credentials");
    await username().fill("admin");
    assert.equal(await submit.isDisabled(), true);
    await password().fill("fixture-password");
    await port.fill("65536");
    assert.equal(await submit.isDisabled(), true);
    await port.fill("2222");
    const userTop = await top(username());
    await auth.click();
    await page.getByRole("listbox", { name: "认证方式选项" }).waitFor();
    assert.ok(await top(username()) > userTop);
    await auth.press("Escape");
    assert.equal(await top(username()), userTop);
    await hostKey().fill("ssh-ed25519 fixture-first\n\nssh-rsa fixture-second\n");
    await submit.click();
    await waitDone();
    assert.deepEqual(submissions.at(-1).node, { name: "固定出口", type: "ssh", server: "203.0.113.10", port: 2222, username: "admin", password: "fixture-password", "host-key": ["ssh-ed25519 fixture-first", "ssh-rsa fixture-second"] });
    assert.equal(submissions.at(-1).body.interval_hours, 0);

    await open();
    assert.equal(await protocol.innerText(), "SOCKS5");
    assert.equal(await password().inputValue(), "", "successful imports clear credentials");
    await chooseInline(protocol, "ssh");
    await page.getByLabel("节点名称", { exact: true }).fill("私钥出口");
    await server.fill("203.0.113.11");
    await username().fill("key-user");
    await password().fill("must-not-be-sent");
    await chooseInline(auth, "private-key");
    await key().fill("~/.ssh/id_ed25519");
    assert.equal(await submit.isDisabled(), true, "file paths are not inline key content");
    await key().fill(privateKey);
    await passphrase().fill("fixture-passphrase");
    await page.screenshot({ path: path.join(output, `${platform}-private-key.png`), mask: [key(), passphrase()] });
    fail = true;
    await submit.click();
    await page.getByRole("region", { name: "设置", exact: true }).getByRole("alert").filter({ hasText: "测试核心拒绝配置" }).waitFor();
    assert.equal(await key().inputValue(), privateKey, "failed validation keeps the draft for retry");
    assert.equal(await passphrase().inputValue(), "fixture-passphrase");
    assert.equal(saved.sources.length, 1);
    fail = false;
    await submit.click();
    await waitDone();
    assert.deepEqual(submissions.at(-1).node, { name: "私钥出口", type: "ssh", server: "203.0.113.11", port: 22, username: "key-user", "private-key": privateKey, "private-key-passphrase": "fixture-passphrase" });

    // Switching authentication or protocol never submits hidden credentials.
    await open();
    await chooseInline(protocol, "ssh");
    await chooseInline(auth, "private-key");
    assert.equal(await key().inputValue(), "");
    assert.equal(await passphrase().inputValue(), "");
    await key().fill(privateKey);
    await passphrase().fill("discarded-passphrase");
    await hostKey().fill("ssh-ed25519 fixture-unused");
    await chooseInline(auth, "password");
    await password().fill("chosen-password");
    await chooseInline(auth, "password");
    assert.equal(await password().inputValue(), "chosen-password", "reselecting the active method must preserve its draft");
    await page.getByLabel("节点名称", { exact: true }).fill("密码出口");
    await server.fill("203.0.113.12");
    await username().fill("password-user");
    await submit.click();
    await waitDone();
    assert.equal(submissions.at(-1).node.password, "chosen-password");
    assert.equal("private-key" in submissions.at(-1).node, false);
    assert.equal("private-key-passphrase" in submissions.at(-1).node, false);

    for (const type of ["http", "socks5"]) {
      await open();
      await chooseInline(protocol, "ssh");
      await chooseInline(auth, "private-key");
      await key().fill(privateKey);
      await passphrase().fill("not-http-auth");
      await hostKey().fill("ssh-ed25519 not-http-key");
      await port.fill("8080");
      await chooseInline(protocol, type);
      assert.equal(await port.inputValue(), "8080", "custom ports survive protocol switching");
      await page.getByLabel("节点名称", { exact: true }).fill(type + "出口");
      await server.fill("203.0.113.13");
      await username().fill("proxy-user");
      await password().fill("proxy-password");
      await submit.click();
      await waitDone();
      assert.deepEqual(submissions.at(-1).node, { name: type + "出口", type, server: "203.0.113.13", port: 8080, username: "proxy-user", password: "proxy-password" });
    }
    await open();
    await chooseInline(protocol, "ssh");
    await chooseInline(auth, "private-key");
    await key().fill(privateKey);
    const count = submissions.length;
    await page.getByRole("button", { name: "取消", exact: true }).click();
    await open();
    await chooseInline(protocol, "ssh");
    await chooseInline(auth, "private-key");
    assert.equal(await key().inputValue(), "", "cancel discards private key material");
    await page.reload();
    assert.equal(submissions.length, count, "reload must not submit a draft");
    assert.deepEqual(f.errors, []);
    console.log(`PASS ${platform}: password/key payloads, retry, credential isolation, cancel, HTTP/SOCKS5 and inline keyboard controls`);
  } catch (error) {
    await page.screenshot({ path: path.join(output, `${platform}-failure.png`), mask: [page.locator('textarea, input[type="password"]')] });
    throw error;
  } finally {
    await f.browser.close();
  }
}
