import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const { browser, page, state, errors } = await createWorkbench();
const desktop = { installed: true, running: true, version: "4.4212.0", failed: false };
const actions = [];
let failUpdate = true;
await page.route("**/api/claude-desktop**", async route => {
  const path = new URL(route.request().url()).pathname;
  if (path.endsWith("/vnc-credentials")) return route.fulfill({ json: { password: "fixture-only" } });
  if (route.request().method() === "GET") return route.fulfill({ json: desktop });
  if (path.endsWith("/resize")) return route.fulfill({ json: { ok: true } });
  const action = path.split("/").at(-1);
  actions.push(action);
  const job = { id: `desktop-${actions.length}`, kind: `claude-desktop-${action}`, running: false, message: "已完成" };
  if (action === "update") {
    desktop.running = false;
    if (failUpdate) job.error = "测试：下载失败，登录资料和已安装版本保留";
    else desktop.version = "4.4213.0";
  }
  if (action === "start") desktop.running = true;
  state.status.job = job;
  return route.fulfill({ json: job });
});
try {
  state.status.vm_running = state.status.ready = true;
  state.status.guest_system = "debian-13";
  await page.reload();
  await page.getByRole("button", { name: "Claude Desktop", exact: true }).click();
  const toolbar = page.locator(".claude-desktop-toolbar");
  await toolbar.getByRole("button", { name: "更新应用", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "取消", exact: true }).click();
  assert.deepEqual(actions, [], "cancelling must preserve the running application");
  assert.equal(desktop.running, true);
  await toolbar.getByRole("button", { name: "更新应用", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "更新应用", exact: true }).click();
  await page.getByRole("alert").filter({ hasText: "下载失败" }).waitFor();
  assert.deepEqual(actions, ["update"], "failed update must not start a partial installation");
  assert.equal(desktop.version, "4.4212.0");
  await page.getByRole("button", { name: "打开 Claude Desktop", exact: true }).waitFor();
  failUpdate = false;
  await toolbar.getByRole("button", { name: "更新应用", exact: true }).click();
  await toolbar.getByText("v4.4213.0", { exact: true }).waitFor();
  await toolbar.getByRole("button", { name: "关闭应用", exact: true }).waitFor();
  assert.deepEqual(actions, ["update", "update", "start"]);
  assert.equal(desktop.installed, true);
  state.status.guest_system = "windows-11";
  await page.reload();
  await page.getByRole("button", { name: "Claude Desktop", exact: true }).click();
  await toolbar.getByText("v4.4213.0", { exact: true }).waitFor();
  assert.equal(await toolbar.getByRole("button", { name: "更新应用", exact: true }).count(), 0, "Windows uses its official in-app updater");
  assert.deepEqual(errors, []);
  console.log("PASS: Linux update confirmation/cancel, failure preservation, retry/reopen, Windows in-app update boundary");
} finally {
  await browser.close();
}
