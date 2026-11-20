import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import { createWorkbench } from "./workbench-fixture.mjs";

function desktopExitSignal() {
  const signals = new EventEmitter();
  return {
    wait: () => once(signals, "quit", { signal: AbortSignal.timeout(5000) }),
    notify: (request) => {
      if (request.action === "quitNow") signals.emit("quit");
    },
  };
}

const calls = [];
const exitSignal = desktopExitSignal();
const view = {
  mode: "local", desktop_gateway: true, connection_id: "test-local-view",
  connection: { state: "connected" },
};
const { page, browser, errors } = await createWorkbench({
  clientContext: view,
  nativeRequest: async (request) => { calls.push(request); exitSignal.notify(request); return null; },
});
const quit = () => page.evaluate(() => window.cageNativeAction("quit"));
const exits = () => calls.filter(({ action }) => action === "quitNow").length;
const refused = (route) => route.abort("connectionrefused");
const stops = [];
page.on("request", (request) => {
  if (new URL(request.url()).pathname === "/api/stop") stops.push(request);
});
try {
  // Lose the gateway while the workbench still shows the previous environment.
  await page.route("**/api/**", refused);
  await page.locator(".connection-banner").waitFor();
  await quit();
  const dialog = page.getByRole("dialog", { name: "退出 Cage" });
  await dialog.getByRole("button", { name: "取消" }).click();
  assert.equal(exits(), 0);
  await quit();
  await Promise.all([exitSignal.wait(), dialog.getByRole("button", { name: "仅退出桌面应用" }).click()]);
  assert.equal(exits(), 1);

  // Initial context cannot be loaded on reopening the page.
  await page.reload();
  await page.getByRole("alert").getByRole("button", { name: "重新连接" }).waitFor();
  await Promise.all([exitSignal.wait(), quit()]);
  assert.equal(exits(), 2);

  // Gateway is alive, but its selected CLI controller has exited.
  view.connection = { state: "failed", error_code: "connection_lost" };
  await page.unroute("**/api/**", refused);
  await page.reload();
  await page.getByRole("heading", { name: "重新连接本机工作台" }).waitFor();
  await Promise.all([exitSignal.wait(), quit()]);
  assert.equal(exits(), 3);
  assert.equal(stops.length, 0);
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
}

for (const stopError of ["", "测试：停止环境超时"]) {
  const job = { id: "requested-stop", kind: "stop", running: !stopError, error: stopError };
  const exitSignal = desktopExitSignal();
  const actions = [];
  let stopRequests = 0;
  const fixture = await createWorkbench({
    nativeRequest: async (request) => { actions.push(request.action); exitSignal.notify(request); return null; },
    beforeNavigate: async (page, state) => {
      state.status.vm_running = true;
      await page.route("**/api/stop", (route) => {
        stopRequests++;
        state.status.job = job;
        return route.fulfill({ json: job });
      });
    },
  });
  try {
    const { page, state, errors } = fixture;
    await page.getByRole("button", { name: "设置", exact: true }).first().waitFor();
    await page.evaluate(() => window.cageNativeAction("quit"));
    await page.getByRole("button", { name: "停止环境并退出", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: stopError ? "未能完成退出" : "正在停止环境" });
    assert.equal(actions.includes("quitNow"), false);
    await Promise.all([exitSignal.wait(), dialog.getByRole("button", { name: "仅退出桌面应用", exact: true }).click()]);
    assert.deepEqual(actions.filter((action) => action === "quitNow"), ["quitNow"]);
    assert.equal(stopRequests, 1);
    assert.equal(state.status.vm_running, true);
    assert.deepEqual(errors, []);
  } finally {
    await fixture.browser.close();
  }
}
