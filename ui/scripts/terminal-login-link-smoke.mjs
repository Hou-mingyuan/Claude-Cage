// Click both link formats emitted by command-line tools in the production xterm.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

for (const [format, base] of [
  ["printed", "https://claude.ai/oauth/authorize"],
  ["printed", "https://claude.com/cai/oauth/authorize"],
  ["OSC 8", "https://claude.com/cai/oauth/authorize"],
]) {
  const url = base + "?state=" + "fixture".repeat(22);
  const opened = [];
  const { browser, page, errors } = await createWorkbench({ mockWebSockets: false });
  try {
    await page.route("**/api/terminals/*/login-link", route => {
      opened.push(route.request().postDataJSON());
      return route.fulfill({ json: { id: "login-tab" } });
    });
    await page.routeWebSocket("**/api/**", socket => {
      if (!socket.url().includes("/terminals/")) return socket.close();
      socket.send(Buffer.from(format === "printed" ? url : `\x1b]8;;${url}\x07Open Claude\x1b]8;;\x07`));
    });
    await page.reload();
    await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
    await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
    await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
    const row = page.locator(".xterm-rows > div").first();
    await row.filter({ hasText: format === "printed" ? new URL(url).host : "Open Claude" }).waitFor();
    const rect = await row.boundingBox();
    await page.mouse.move(rect.x + 20, rect.y + rect.height / 2);
    await page.waitForFunction(() => !!document.querySelector(".xterm-cursor-pointer"));
    assert.equal(opened.length, 0);
    await page.mouse.down();
    await page.mouse.up();
    await page.getByRole("tab", { name: /账号授权/ }).waitFor();
    assert.deepEqual(opened, [{ url }]);
    assert.equal(page.context().pages().length, 1);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
}
console.log("Printed and OSC 8 authorization links open the guest login tab on click.");

// A native CLI can ask for a specific existing browser tab while the
// workbench is displaying settings. A repeated status poll must not steal
// focus again after the user has moved on.
const { browser, page, state, errors } = await createWorkbench();
try {
  await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
  const settings = page.getByRole("button", { name: "设置", exact: true });
  const groups = page.getByRole("navigation", { name: "设置分组" });
  await settings.click();
  await groups.waitFor();
  state.tabs.push({ id: "cli-login", kind: "web", title: "Claude", url: "https://claude.ai" });
  state.active = "cli-login";
  state.status.host_cli_browser = { id: "from-terminal", tab: "cli-login" };
  await groups.waitFor({ state: "hidden" });
  await page.getByRole("tab", { name: /Claude/, selected: true }).waitFor();
  await settings.click();
  await groups.waitFor();
  await page.waitForResponse(response => new URL(response.url()).pathname === "/api/browser/tabs");
  assert.ok(await groups.isVisible());
  state.tabs.push({ id: "cli-auth", kind: "web", title: "账号授权", url: "", login: true });
  state.active = "cli-auth";
  state.status.host_cli_browser = { id: "new-authorization" };
  await groups.waitFor({ state: "hidden" });
  await page.getByRole("tab", { name: /账号授权/, selected: true }).waitFor();
  assert.equal(page.context().pages().length, 1);
  assert.deepEqual(errors, []);
} finally { await browser.close(); }
console.log("Native CLI login focuses the guest tab once per request.");
