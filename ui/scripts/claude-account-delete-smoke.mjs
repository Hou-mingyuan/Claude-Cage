// Real workbench UI with isolated Claude accounts fixture. No request reaches personal data.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const base = process.env.CAGE_UI_URL || "http://127.0.0.1:5178";
const { browser, page, state, errors } = await createWorkbench({ url: base });

const idA = "a".repeat(24);
const idB = "b".repeat(24);
const idDraft = "c".repeat(24);
const desktopA = "d".repeat(24);

const profiles = [
  {
    number: 1,
    id: idA,
    source: "managed",
    label: "工作账号",
    email: "work@example.test",
    plan: "Pro",
    enabled: true,
    status: "ready",
    desktop_instance_id: desktopA,
    desktop_status: "ready",
  },
  {
    number: 2,
    id: idB,
    source: "default",
    label: "personal@example.test",
    email: "personal@example.test",
    plan: "Pro",
    enabled: false,
    status: "ready",
  },
  {
    number: 3,
    id: idDraft,
    source: "managed",
    label: "未完成草稿",
    enabled: true,
    status: "pending",
  },
];

let failDelete = false;
const deletedIDs = [];
const cancelledIDs = [];

try {
  state.status.ready = state.status.vm_running = true;
  state.terminals = [
    {
      id: "term-1",
      kind: "claude",
      workspace: state.settings.workspace,
      title: "终端 1",
      claude_account_id: idA,
      claude_account_source: "managed",
      closed: false,
    },
  ];

  await page.route("**/api/claude-accounts*", async (route) => {
    const req = route.request();
    const body = req.headers()["content-type"]?.includes("application/json") ? req.postDataJSON() : undefined;
    if (body?.action === "cancel") {
      cancelledIDs.push(body.id);
      const idx = profiles.findIndex((p) => p.id === body.id);
      if (idx >= 0) profiles.splice(idx, 1);
    }
    if (body?.action === "delete") {
      if (failDelete) {
        return route.fulfill({
          status: 409,
          contentType: "text/plain; charset=utf-8",
          body: "请先关闭该账号的 Desktop，再删除账号",
        });
      }
      deletedIDs.push(body.id);
      const idx = profiles.findIndex((p) => p.id === body.id);
      if (idx >= 0) profiles.splice(idx, 1);
    }
    return route.fulfill({
      json: {
        accounts: profiles,
        scheduling: { mode: "balanced", switch_policy: "ask" },
        detection_status: "signed_out",
      },
    });
  });

  await page.reload();
  await page.getByRole("button", { name: "Claude 账号管理", exact: true }).click();
  await page.getByRole("main", { name: "Claude 账号管理", exact: true }).waitFor();

  // Draft account keeps "撤销添加" instead of "删除账号".
  const draftCard = page.getByRole("article", { name: "#03 未完成草稿", exact: true });
  await draftCard.getByRole("button", { name: "撤销添加", exact: true }).waitFor();
  assert.equal(await draftCard.getByRole("button", { name: "删除账号", exact: true }).count(), 0);
  await draftCard.getByRole("button", { name: "撤销添加", exact: true }).click();
  const cancelDialog = page.getByRole("dialog", { name: "撤销添加「未完成草稿」？" });
  await cancelDialog.getByRole("button", { name: "撤销添加", exact: true }).click();
  await cancelDialog.waitFor({ state: "hidden" });
  assert.deepEqual(cancelledIDs, [idDraft]);

  // Bound account with active terminal has disabled delete button.
  const workCard = page.getByRole("article", { name: "#01 工作账号", exact: true });
  assert.equal(await workCard.getByRole("button", { name: "删除账号", exact: true }).isEnabled(), false);

  // Once terminal closes, delete button becomes enabled; supports cancel, error retry and removal.
  state.terminals[0].closed = true;
  await page.reload();
  await page.getByRole("button", { name: "Claude 账号管理", exact: true }).click();
  await page.getByRole("main", { name: "Claude 账号管理", exact: true }).waitFor();
  assert.equal(await workCard.getByRole("button", { name: "删除账号", exact: true }).isEnabled(), true);

  await workCard.getByRole("button", { name: "删除账号", exact: true }).click();
  const deleteWorkDialog = page.getByRole("dialog", { name: "删除账号「工作账号」？" });
  await deleteWorkDialog.getByText(/从账号管理、调度和 Desktop 列表中移除/).waitFor();
  await deleteWorkDialog.getByRole("button", { name: "取消", exact: true }).click();
  await deleteWorkDialog.waitFor({ state: "hidden" });
  assert.deepEqual(deletedIDs, []);

  failDelete = true;
  await workCard.getByRole("button", { name: "删除账号", exact: true }).click();
  await deleteWorkDialog.getByRole("button", { name: "删除账号", exact: true }).click();
  await deleteWorkDialog.getByRole("alert").filter({ hasText: "请先关闭该账号的 Desktop，再删除账号" }).waitFor();
  assert.deepEqual(deletedIDs, []);

  failDelete = false;
  await deleteWorkDialog.getByRole("button", { name: "删除账号", exact: true }).click();
  await deleteWorkDialog.waitFor({ state: "hidden" });
  assert.deepEqual(deletedIDs, [idA]);
  assert.equal(await page.getByRole("article", { name: "#01 工作账号", exact: true }).count(), 0);

  // Deleting the last (default-sourced) account shows the last-account warning and leaves empty state.
  const personalCard = page.getByRole("article", { name: "#02 personal@example.test", exact: true });
  await personalCard.getByRole("button", { name: "删除账号", exact: true }).click();
  const deleteLastDialog = page.getByRole("dialog", { name: "删除账号「personal@example.test」？" });
  await deleteLastDialog.getByText(/这是列表中的最后一个账号/).waitFor();
  await deleteLastDialog.getByRole("button", { name: "删除账号", exact: true }).click();
  await deleteLastDialog.waitFor({ state: "hidden" });
  assert.deepEqual(deletedIDs, [idA, idB]);
  await page.getByText("尚未添加账号", { exact: true }).waitFor();

  assert.deepEqual(errors, []);
  console.log("PASS: Claude account deletion (draft cancel preserved, active terminal guard, Desktop warning, cancel, error retry, last-account empty state)");
} finally {
  await browser.close();
}
