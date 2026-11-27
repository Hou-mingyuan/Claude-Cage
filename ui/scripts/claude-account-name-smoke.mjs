import assert from "node:assert/strict";
import { mkdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { createWorkbench } from "./workbench-fixture.mjs";

const { browser, page, state, errors } = await createWorkbench();
const id = "abcdef0123456789abcdef01";
const original = {
  id, number: 3, name: "sarah", label: "旧备注", email: "sarah.billing@example.test", source: "managed",
  status: "ready", enabled: true, plan: "Pro", handoff_group: "support", proxy_node_id: "fixed",
  quota: { five_hour: { used_percentage: 18, observed_at: Math.floor(Date.now() / 1000) } },
};
const profiles = [
  structuredClone(original),
  { id: "b".repeat(24), number: 8, name: "ops", label: "另一个账号", email: "ops@example.test", status: "ready" },
  { id: "c".repeat(24), number: 11, label: "旧版记录", status: "pending" },
];
const mutations = [];
const conflict = "该账号命令名已被使用，请选择其他名称";

try {
  await page.route("**/api/claude-accounts*", async route => {
    const body = route.request().postDataJSON();
    if (body?.action === "update") {
      mutations.push(body);
      if ("name" in body && profiles.some(account => account.id !== body.id && account.name?.toLowerCase() === body.name.toLowerCase())) {
        return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: conflict });
      }
      const { action, id: accountID, ...fields } = body;
      assert.equal(action, "update");
      const account = profiles.find(account => account.id === accountID);
      if (account.id !== profiles[2].id || !("name" in fields)) Object.assign(account, fields);
    }
    return route.fulfill({ json: { accounts: profiles, detection_status: "ready", scheduling: { mode: "sequential", switch_policy: "ask" } } });
  });
  state.status.ready = state.status.vm_running = true;
  const openPanel = async () => {
    await page.reload();
    await page.getByRole("button", { name: "Claude 账号管理", exact: true }).click();
    await page.getByRole("main", { name: "Claude 账号管理", exact: true }).waitFor();
  };
  await openPanel();
  const card = () => page.getByRole("article", { name: `#03 ${profiles[0].label}`, exact: true });
  const settings = () => card().getByRole("button", { name: "设置", exact: true });
  const name = () => card().getByRole("textbox", { name: "账号命令名", exact: true });
  const valueFor = label => card().locator("dt").filter({ hasText: label }).locator("..").locator("dd");
  await card().getByText("cage sarah", { exact: true }).waitFor();
  await page.getByRole("article", { name: "#11 旧版记录", exact: true }).getByText("未设置", { exact: true }).waitFor();
  await settings().focus();
  await settings().press("Enter");
  // Each field has its own visible label, including the full fixed ID.
  for (const [label, expected] of [["Cage 编号", "#03"], ["Cage ID", id], ["Claude 邮箱", original.email]]) {
    assert.equal(await valueFor(label).textContent(), expected);
  }
  assert.equal(await name().inputValue(), "sarah");
  assert.equal(await card().getByRole("button", { name: "保存名称" }).isDisabled(), true);
  const capture = process.env.CAGE_TEST_CAPTURE;
  if (capture) {
    await mkdir(capture, { recursive: true });
    await page.screenshot({ path: join(capture, "account-name-desktop.png"), animations: "disabled" });
  }
  await name().fill("will-not-save");
  await name().press("Escape");
  assert.equal(await name().inputValue(), "sarah");
  assert.equal(mutations.length, 0);

  await name().fill("OPS");
  await name().press("Enter");
  await page.getByRole("alert").filter({ hasText: conflict }).waitFor();
  assert.equal(await name().inputValue(), "OPS");
  await card().getByText("cage sarah", { exact: true }).waitFor();
  assert.deepEqual(profiles[0], original);
  const renamed = "研发-é_2";
  await name().fill(renamed);
  await name().press("Enter");
  await card().getByText("cage " + renamed, { exact: true }).waitFor();
  assert.equal(await name().evaluate(input => input === document.activeElement), true);
  assert.equal(await page.getByRole("alert").filter({ hasText: conflict }).count(), 0);
  assert.deepEqual(mutations, [{ action: "update", id, name: "OPS" }, { action: "update", id, name: renamed }]);
  assert.deepEqual(profiles[0], { ...original, name: renamed });
  assert.equal(await card().getByRole("checkbox", { name: "参与自动分配" }).isChecked(), true);
  await card().getByText("18%", { exact: true }).waitFor();
  // Closing the settings discards only the unsaved draft.
  await name().fill("discarded");
  await settings().click();
  await settings().click();
  assert.equal(await name().inputValue(), renamed);

  await page.setViewportSize({ width: 375, height: 760 });
  await openPanel();
  await page.getByRole("button", { name: "收起侧栏", exact: true }).click();
  await settings().click();
  assert.equal(await valueFor("Claude 邮箱").textContent(), original.email);
  const layout = await card().evaluate(el => ({ width: el.clientWidth, scroll: el.scrollWidth, viewport: innerWidth, document: document.documentElement.scrollWidth }));
  assert.ok(layout.scroll <= layout.width + 1 && layout.document <= layout.viewport + 1, JSON.stringify(layout));
  if (capture) await page.screenshot({ path: join(capture, "account-name-narrow.png"), animations: "disabled" });
  const maximumName = "𐐀".repeat(64);
  await name().fill(maximumName);
  await name().press("Enter");
  await card().getByText("cage " + maximumName, { exact: true }).waitFor();
  assert.ok(await card().evaluate(el => el.scrollWidth <= el.clientWidth + 1));
  await name().fill("sarah.work");
  await card().getByRole("button", { name: "保存名称" }).click();
  await card().getByText("cage sarah.work", { exact: true }).waitFor();
  await card().getByRole("button", { name: "修改 旧备注 的备注" }).click();
  await card().getByRole("textbox", { name: "账号备注" }).fill("值班备注");
  await card().getByRole("button", { name: "保存备注" }).click();
  await page.getByRole("article", { name: "#03 值班备注", exact: true }).getByText("cage sarah.work", { exact: true }).waitFor();
  assert.deepEqual(mutations.slice(-2), [{ action: "update", id, name: "sarah.work" }, { action: "update", id, label: "值班备注" }]);
  assert.deepEqual(profiles[0], { ...original, name: "sarah.work", label: "值班备注" });
  const enabled = card().getByRole("checkbox", { name: "参与自动分配" });
  await enabled.click();
  await page.waitForFunction(input => !input.disabled && !input.checked, await enabled.elementHandle());
  const group = card().getByRole("textbox", { name: "值班备注 的接续分组" });
  await group.fill("weekend");
  const groupSaved = page.waitForResponse(response => response.request().method() === "POST" &&
    response.url().endsWith("/api/claude-accounts") && response.request().postDataJSON()?.handoff_group === "weekend");
  await group.press("Enter");
  assert.ok((await groupSaved).ok());
  assert.deepEqual(mutations.slice(-2), [{ action: "update", id, enabled: false }, { action: "update", id, handoff_group: "weekend" }]);
  assert.deepEqual(profiles[0], { ...original, name: "sarah.work", label: "值班备注", enabled: false, handoff_group: "weekend" });
  // An older controller can return HTTP 200 while ignoring an unknown field.
  const legacy = page.getByRole("article", { name: "#11 旧版记录", exact: true });
  await legacy.getByRole("button", { name: "设置", exact: true }).click();
  const legacyName = legacy.getByRole("textbox", { name: "账号命令名" });
  await legacyName.fill("upgrade-first");
  await legacyName.press("Enter");
  await page.getByRole("alert").filter({ hasText: "当前后台没有提供所需功能" }).waitFor();
  assert.equal(await legacyName.inputValue(), "upgrade-first");
  assert.equal(profiles[2].name, undefined);
  await page.setViewportSize({ width: 1280, height: 850 });
  await page.getByRole("button", { name: "展开侧栏", exact: true }).first().click();
  const fr = JSON.parse(await readFile(new URL("../src/locales/fr.json", import.meta.url), "utf8"));
  await page.route("**/api/client/language", route => route.fulfill({ json: { language: "fr", active: "fr" } }));
  await page.reload();
  await page.getByRole("button", { name: fr["Claude 账号管理"], exact: true }).click();
  await card().getByRole("button", { name: fr["设置"], exact: true }).click();
  const frenchName = card().getByRole("textbox", { name: fr["账号命令名"] });
  await frenchName.fill("ops");
  await frenchName.press("Enter");
  await page.getByRole("alert").filter({ hasText: fr[conflict] }).waitFor();
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ savedByKeyboard: true, conflictPreservedDraft: true, updateRequests: mutations.length, narrowViewport: layout.viewport, maximumNameCharacters: 64, localizedConflict: "fr", unchangedEmailIDAndQuota: true }));
} finally { await browser.close(); }
