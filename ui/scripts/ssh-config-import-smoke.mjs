// Real desktop client/API/browser test. Uses a synthetic local user profile
// and SSH config; never reads the developer's .ssh directory or starts a VM.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtemp, mkdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const windows = process.platform === "win32";
const executable = process.env.CAGE_CLIENT_BINARY;
if (!executable) throw new Error("Set CAGE_CLIENT_BINARY to the newly built cage-client");
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const profile = await mkdtemp(join(process.env.RUNNER_TEMP || tmpdir(), "cage-ssh-config-"));
const artifacts = resolve(process.env.CAGE_REMOTE_QA_DIR || join(profile, "artifacts"));
const userProfile = join(profile, "fixture-user");
const config = join(userProfile, ".ssh", "config");
await mkdir(join(userProfile, ".ssh"), { recursive: true });
await mkdir(artifacts, { recursive: true });
const configText = "Host dev prod\r\n HostName %h.example.com\r\n User developer\r\n IdentityFile C:/never-read/SECRET_KEY\r\nHost jump\r\n ProxyCommand command SECRET_COMMAND\r\n";
await writeFile(config, configText);
const service = spawn(executable, ["serve"], {
  env: {
    ...process.env,
    ...(windows ? { USERPROFILE: userProfile, USERNAME: "fixture-user" } : { HOME: userProfile }),
    CLAUDE_CAGE_CONFIG: join(profile, "config.json"),
  },
  stdio: "ignore",
  windowsHide: true,
});
let browser;
let page;
try {
  let control;
  for (let i = 0; i < 200; i++) {
    try { control = JSON.parse(await readFile(join(profile, "control.json"), "utf8")); break; }
    catch { if (service.exitCode !== null) throw new Error("Client exited before publishing its control record"); await new Promise((resolve) => setTimeout(resolve, 50)); }
  }
  assert.ok(control, "client service did not start");
  const origin = `http://localhost:${control.port}`;
  const launched = await fetch(`${origin}/api/launch`, {
    method: "POST", headers: { "Content-Type": "application/json", Origin: origin }, body: JSON.stringify({ token: control.token }),
  });
  assert.equal(launched.status, 200);
  const entry = await launched.json();
  browser = await chromium.launch({ headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH });
  page = await browser.newPage({ viewport: { width: 1280, height: 850 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const mutations = [];
  page.on("request", (request) => { if (request.method() === "POST") mutations.push(new URL(request.url()).pathname); });
  await page.goto(entry.url);
  const open = () => page.getByRole("button", { name: "导入本机 SSH 配置", exact: true }).click();
  await open();
  const pathHint = windows ? "%USERPROFILE%\\.ssh\\config" : "~/.ssh/config";
  await page.getByRole("dialog").getByText(pathHint, { exact: false }).waitFor();
  await page.getByRole("checkbox").first().waitFor();
  assert.equal(await page.getByRole("checkbox").count(), 2);
  assert.equal(await page.getByText("SECRET_KEY", { exact: false }).count(), 0);
  assert.equal(await page.getByText("SECRET_COMMAND", { exact: false }).count(), 0);
  await assert.rejects(readFile(join(profile, "remote-hosts.json")), { code: "ENOENT" });
  await page.getByText("已跳过的主机", { exact: false }).click();
  await page.getByText("jump", { exact: true }).waitFor();
  await page.screenshot({ path: join(artifacts, "ssh-config-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 375, height: 850 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await page.screenshot({ path: join(artifacts, "ssh-config-mobile.png"), fullPage: true });
  await page.setViewportSize({ width: 1280, height: 850 });
  // Verify keyboard selection and cancellation without storing any hosts.
  const boxes = page.getByRole("checkbox");
  await boxes.first().focus();
  await page.keyboard.press("Space");
  assert.equal(await boxes.first().isChecked(), false);
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await assert.rejects(readFile(join(profile, "remote-hosts.json")), { code: "ENOENT" });
  await open();
  await page.getByRole("checkbox").first().waitFor();
  await writeFile(config, configText + "# changed\r\n");
  await page.getByRole("button", { name: "导入所选主机（2）", exact: true }).click();
  await page.getByRole("alert").filter({ hasText: "SSH 配置已变化" }).waitFor();
  await page.getByRole("button", { name: "重新读取", exact: true }).click();
  await page.getByRole("checkbox").first().waitFor();
  await boxes.nth(1).uncheck();
  await page.getByRole("button", { name: "导入所选主机（1）", exact: true }).click();
  await page.getByText("已导入 1 台主机，跳过 0 台已有主机。", { exact: true }).waitFor();
  await page.getByRole("button", { name: "完成并关闭", exact: true }).click();
  const saved = JSON.parse(await readFile(join(profile, "remote-hosts.json"), "utf8"));
  assert.equal(saved.hosts.length, 1);
  assert.equal(saved.hosts[0].name, "dev");
  assert.deepEqual(saved.hosts[0].auth, { kind: "agent" });
  assert.equal(JSON.stringify(saved).includes("SECRET"), false);
  await page.reload();
  await page.getByRole("button", { name: /dev developer@dev.example.com:22/ }).waitFor();
  await open();
  await page.getByRole("checkbox").first().waitFor();
  await boxes.nth(1).uncheck();
  await page.getByRole("button", { name: "导入所选主机（1）", exact: true }).click();
  await page.getByText("已导入 0 台主机，跳过 1 台已有主机。", { exact: true }).waitFor();
  assert.deepEqual(JSON.parse(await readFile(join(profile, "remote-hosts.json"), "utf8")), saved);
  assert.equal(mutations.some((path) => /connect|candidate|start|deploy/.test(path)), false, "import must not start connections/workloads");
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ ok: true, platform: process.platform, checks: ["real-desktop-api", "platform-path-hint", "no-private-material", "preview-cancel", "keyboard", "mobile-layout", "stale-config", "selected-import", "persistence", "idempotence", "no-connect-or-vm"], artifacts }));
} catch (error) {
  if (page) await page.screenshot({ path: join(artifacts, "failure.png"), fullPage: true }).catch(() => {});
  throw error;
} finally {
  await browser?.close();
  service.kill("SIGTERM");
}
