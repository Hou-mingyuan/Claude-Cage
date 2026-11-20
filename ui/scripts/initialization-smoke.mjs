import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

const evidence = process.env.CAGE_INITIALIZATION_EVIDENCE;
if (evidence) await fs.mkdir(evidence, { recursive: true });

// A changed exit or environment configuration needs a full restart. The page
// must offer that as the only next action, keep the save-work confirmation and
// start only after the accepted stop has finished.
{
  const restartMessage = "出口地址或环境设置变化需要重启环境";
  let stops = 0, starts = 0, disconnectOnStop = false, disconnected = false;
  const f = await createWorkbench({
    initialization: { required: true, tracked: true, started: true },
    beforeNavigate: async (page, state) => {
      state.account.guest_system = "debian-13";
      Object.assign(state.status, { guest_system: "debian-13", ready: true, vm_running: true,
        job: { id: "proxy-1", kind: "proxy", running: false, error: restartMessage },
        safety: { error: restartMessage, gate: { mode: "closed" } } });
      await page.route("**/api/status", route => disconnected
        ? route.fulfill({ status: 503, contentType: "text/plain; charset=utf-8", body: "测试：停止后连接中断" })
        : route.fulfill({ json: { ...state.status, workspace: state.settings.workspace } }));
      await page.route("**/api/safety/check", () => assert.fail("a required restart must not be replaced by a network retry"));
      await page.route("**/api/stop", route => {
        stops++;
        state.status.job = { id: `stop-${stops}`, kind: "stop", running: true, message: "正在停止环境" };
        if (disconnectOnStop) disconnected = true;
        return route.fulfill({ json: state.status.job });
      });
      await page.route("**/api/start", route => {
        assert.equal(state.status.vm_running, false, "start must wait for the accepted stop to finish");
        assert.equal(state.status.job.kind, "stop");
        assert.equal(state.status.job.running, false);
        assert.ok(!state.status.job.error);
        starts++;
        Object.assign(state.status, { ready: true, vm_running: true, safety: { gate: { mode: "closed" } },
          job: { id: `start-${starts}`, kind: "start", running: true, message: "正在启动并检查环境" } });
        // The backend accepted the start, but its response did not reach the UI.
        return route.fulfill({ status: 502, contentType: "text/plain; charset=utf-8", body: "启动请求的响应传输中断" });
      });
    },
  });
  const { page, state } = f;
  page.setDefaultTimeout(5000);
  const restart = () => page.getByRole("button", { name: /^(重启环境|启动环境)并重新检测$/ });
  const confirm = async (name) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name, exact: true }).click();
  };
  try {
    await page.getByRole("heading", { name: "下一步：重启环境并重新检测", exact: true }).waitFor();
    assert.equal(await restart().count(), 1);
    assert.equal(await page.getByRole("button", { name: /^(重试|运行)网络与隔离检查$/ }).count(), 0,
      "a retry must not compete with the required restart");
    assert.equal(await page.getByRole("button", { name: /^(继续安装|重新启动并检查)$/ }).count(), 0);
    assert.equal(await page.getByRole("button", { name: "进入工作台", exact: true }).count(), 0);
    if (evidence) await page.screenshot({ path: `${evidence}/environment-restart.png`, fullPage: true });

    await restart().click();
    await page.getByRole("dialog").getByText(/请先保存文件/).waitFor();
    await confirm("取消");
    assert.equal(stops + starts, 0, "cancel must not stop the environment");

    await restart().click(); await confirm("重启并检查");
    await page.getByText("正在停止环境", { exact: true }).waitFor();
    assert.equal(stops, 1);
    assert.equal(await page.getByRole("button", { name: "进入工作台", exact: true }).count(), 0);
    await page.setViewportSize({ width: 600, height: 650 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    await page.setViewportSize({ width: 1280, height: 850 });
    assert.equal(starts, 0, "a running stop job must not start the environment");
    state.status.job = { ...state.status.job, running: false, error: "停止环境失败" };
    await page.getByRole("alert").filter({ hasText: "停止环境失败" }).first().waitFor();
    assert.equal(starts, 0, "stop failure must not start the environment");

    disconnectOnStop = true;
    await restart().click(); await confirm("重启并检查");
    await page.getByRole("alert").filter({ hasText: "停止后连接中断" }).first().waitFor();
    assert.equal(starts, 0, "lost status must not authorize a start");
    disconnected = false;
    disconnectOnStop = false;
    state.status.job = { id: "stop-2", kind: "stop", running: false, error: "停止请求中断，环境仍在运行" };
    await page.waitForFunction(() => [...document.querySelectorAll("button")].some(button => button.textContent === "重启环境并重新检测" && !button.disabled));
    await restart().click(); await confirm("重启并检查");
    await page.getByText("正在停止环境", { exact: true }).waitFor();
    assert.equal(starts, 0);
    Object.assign(state.status, { ready: false, vm_running: false, job: { id: "stop-3", kind: "stop", running: false } });
    await page.getByText("正在启动并检查环境", { exact: true }).waitFor();
    assert.equal(starts, 1);
    assert.equal(stops, 3);
    await page.getByRole("alert").filter({ hasText: "响应传输中断" }).first().waitFor();
    assert.equal(await page.getByRole("button", { name: "进入工作台", exact: true }).count(), 0);
    state.status.job = { ...state.status.job, running: false };
    state.status.safety = { gate: { mode: "open", until: new Date(Date.now() + 3600000).toISOString() } };
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    assert.equal(await restart().count(), 0, "a verified open gate must not keep the restart action");
    assert.deepEqual(f.errors, []);
    console.log("PASS environment restart: single next action, save-work confirmation, cancellation, stop-before-start, stop failure, disconnect recovery, lost start response and completion gate");
  } finally { await f.browser.close(); }
}

for (const platform of ["darwin", "windows", "linux"]) {
  // Every host installs the same system; there is nothing to choose.
  const system = "debian-13";
  let starts = 0, rejectComplete = true;
  let directory = "/preview/cage-state";
  const initialDirectory = platform === "windows" ? "/preview/Cage/CageData" : directory;
  const fixture = await createWorkbench({
    initialization: { required: true, tracked: true, started: false },
    clientContext: { mode: "local", connection_id: "local", client_platform: platform, server_platform: platform },
    beforeNavigate: async (page, state) => {
      // Audio is outside this initialization flow; avoid unrelated in-flight audio requests during reload.
      await page.addInitScript(() => localStorage.setItem("cage.audio", JSON.stringify({ muted: true })));
      state.status.local_vm_storage = true;
      await page.route("**/api/environment/storage", route => {
        if (route.request().method() === "POST") {
          assert.equal(route.request().postDataJSON().previous, directory);
          directory = route.request().postDataJSON().directory;
          state.status.job = { id: "storage-default", kind: "storage", running: false };
          return route.fulfill({ json: state.status.job });
        }
        return route.fulfill({ json: { directory, has_environment: false, suggested_directory: directory === "/preview/cage-state" ? initialDirectory : undefined } });
      });
      await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: false } }));
      await page.route("**/api/start", route => {
        starts++;
        const submitted = route.request().postDataJSON();
        assert.equal(submitted.guest_system, system);
        assert.deepEqual(Object.keys(submitted).filter(key => key !== "guest_system" && key !== "guest_profile"), []);
        state.status.job = { id: "install-" + starts, running: true, kind: "start", setup_progress: { system, phase: "image", operation: "download", label: "正在下载系统镜像", received: 25 * 1048576, total: 100 * 1048576, bytes_per_second: 1048576 }, log: "镜像下载已开始" };
        return route.fulfill({ json: state.status.job });
      });
      await page.route("**/api/environment/initialization", route => {
        const action = route.request().method() === "POST" ? route.request().postDataJSON()?.action : "";
        if (action === "complete" && rejectComplete) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "环境网络检查尚未通过，请重新检查" });
        if (action === "install") state.initialization.started = true;
        if (action === "complete") state.initialization.required = false;
        return route.fulfill({ json: state.initialization });
      });
    },
  });
  const { page, state, errors, browser } = fixture;
  try {
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0);
    const storage = page.getByRole("region", { name: "环境存储目录" });
    await storage.getByLabel("存储目录", { exact: true }).waitFor();
    assert.equal(await storage.getByLabel("存储目录", { exact: true }).inputValue(), initialDirectory);
    await storage.getByText("这是 Cage 的环境数据目录。更新、重装或卸载应用会保留这里的数据；请勿手动删除。").waitFor();
    assert.equal(await storage.getByText("当前环境目录", { exact: true }).count(), 0);
    assert.equal(await storage.getByText("新存储位置", { exact: true }).count(), 0);
    await page.locator(".initialization-content").evaluate(async element => {
      await Promise.all(element.getAnimations({ subtree: true })
        .filter(animation => animation.animationName === "initialization-enter")
        .map(animation => animation.finished));
    });
    assert.equal(await page.getByRole("combobox", { name: "客户机系统" }).count(), 0, "a new environment offers no system choice");
    assert.equal(await page.locator(".guest-system-picker").count(), 0);
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-system.png`, fullPage: true });
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    await page.getByRole("heading", { name: "连接你的网络代理" }).waitFor();
    await page.getByRole("button", { name: "上一步", exact: true }).click();
    await storage.getByLabel("存储目录", { exact: true }).waitFor();
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    await page.getByRole("button", { name: "开始安装", exact: true }).click();
    const progress = page.getByRole("region", { name: "环境安装进度" });
    await progress.getByText("25 MiB / 100 MiB", { exact: true }).waitFor();
    assert.equal(await progress.getByRole("progressbar").getAttribute("aria-valuenow"), "25");
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0);
    await page.reload();
    await progress.getByText("25 MiB / 100 MiB", { exact: true }).waitFor();
    assert.equal(starts, 1, "refresh must not restart the accepted installation");
    state.status.job = { ...state.status.job, running: false, error: "下载中断，已保存部分文件" };
    await progress.getByRole("alert").waitFor();
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-failed.png`, fullPage: true });
    await progress.getByRole("button", { name: "继续安装", exact: true }).click();
    await progress.getByRole("progressbar").waitFor();
    assert.equal(starts, 2);
    await page.setViewportSize({ width: 600, height: 650 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-progress-compact.png`, fullPage: true });
    state.status.job = { id: "install-2", kind: "start", running: false, message: "环境已启动，联网检查未通过", error: "联网检查未通过：浏览器检测通道返回 HTTP 503，外网保持关闭" };
    Object.assign(state.status, { ready: true, vm_running: true, safety: { gate: { mode: "closed" } } });
    await page.getByRole("heading", { name: "系统已安装，完成使用检查", exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "继续安装", exact: true }).count(), 0);
    let networkChecks = 0;
    await page.route("**/api/safety/check", route => {
      networkChecks++;
      state.status.job = { id: "retry-network", kind: "safety", running: true, message: "正在检查联网条件" };
      return route.fulfill({ json: state.status.job });
    });
    await page.getByRole("button", { name: "运行网络与隔离检查", exact: true }).click();
    await page.getByText("正在检查联网条件", { exact: true }).waitFor();
    assert.equal(networkChecks, 1);
    assert.equal(starts, 2, "network recovery must not restart installation");
    state.status.job = { id: "install-2", kind: "start", running: false };
    Object.assign(state.status, { ready: true, vm_running: true, safety: { gate: { mode: "open", until: new Date(Date.now() + 3600000).toISOString() } } });
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("alert").filter({ hasText: "网络检查" }).waitFor();
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0, "server completion rejection must keep initialization visible");
    rejectComplete = false;
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    await page.evaluate(() => window.cageNativeAction("settings"));
    await page.getByRole("region", { name: "设置", exact: true }).waitFor();
    await page.reload();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    await page.evaluate(() => window.cageNativeAction("quit"));
    const quitDialog = page.getByRole("dialog", { name: "退出 Cage", exact: true });
    await quitDialog.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(await page.getByRole("heading", { name: "为 Cage 准备独立环境" }).count(), 0);
    assert.deepEqual(errors, []);
    console.log(`PASS ${platform}: independent initialization without a system choice, back navigation, progress, refresh, retry, completion gate and reopen`);
  } finally { await browser.close(); }
}

{
  let location = { directory: "/preview/default-state", has_environment: false }, writes = 0, fail = true;
  const f = await createWorkbench({ initialization: { required: true, tracked: true }, beforeNavigate: async (page, state) => {
    state.status.local_vm_storage = true;
    await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: false } }));
    await page.route("**/api/environment/storage", route => {
      if (route.request().method() === "GET") return route.fulfill({ json: location });
      const body = route.request().postDataJSON();
      writes++;
      assert.equal(body.previous, "/preview/default-state");
      assert.equal(body.migrate, false);
      if (fail) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "请选择专用空文件夹" });
      location = { ...location, directory: body.directory };
      state.status.job = { id: "save-storage", kind: "storage", running: false };
      return route.fulfill({ json: state.status.job });
    });
  } });
  try {
    const input = f.page.getByLabel("存储目录", { exact: true });
    assert.equal(await input.inputValue(), "/preview/default-state");
    await input.fill("/preview/chosen-state");
    assert.equal(writes, 0, "editing must not save before Next");
    await f.page.getByRole("button", { name: "下一步：配置代理" }).click();
    await f.page.getByRole("alert").filter({ hasText: "请选择专用空文件夹" }).waitFor();
    assert.equal(await input.inputValue(), "/preview/chosen-state");
    assert.equal(await f.page.getByRole("heading", { name: "连接你的网络代理" }).count(), 0);
    fail = false;
    await f.page.getByRole("button", { name: "下一步：配置代理" }).click();
    await f.page.getByRole("heading", { name: "连接你的网络代理" }).waitFor();
    assert.equal(location.directory, "/preview/chosen-state");
    assert.equal(writes, 2);
    await f.page.getByRole("button", { name: "上一步", exact: true }).click();
    assert.equal(await input.inputValue(), "/preview/chosen-state");
    assert.deepEqual(f.errors, []);
    console.log("PASS storage: one default-filled directory, save on Next, rejected path preserved, retry and back navigation");
  } finally { await f.browser.close(); }
}

{
  let imported = false, applied = false;
  const f = await createWorkbench({ initialization: { required: true, tracked: true }, beforeNavigate: async (page, state) => {
    state.status.local_vm_storage = true;
    await page.route("**/api/environment/storage", route => route.fulfill({ json: { directory: "/preview/cage-state", has_environment: false } }));
    await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: false } }));
    const nodes = [{ id: "node", name: "本机测试代理", type: "socks5" }, ...Array.from({ length: 244 }, (_, i) => ({ id: `other-${i}`, name: `其他节点 ${i}`, type: "socks5" }))];
    await page.route("**/api/proxy", route => route.fulfill({ json: { active: applied, selected: applied ? "node" : "", sources: imported ? [{ id: "source", name: "自有节点", nodes }] : [], port: 17896, core_available: true } }));
    for (const action of ["import", "route"]) await page.route(`**/api/proxy/${action}`, route => {
      if (action === "import") imported = true;
      else applied = true;
      state.status.job = { id: action, kind: "proxy", running: false, message: "已完成" };
      return route.fulfill({ json: state.status.job });
    });
  } });
  try {
    const { page } = f;
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    const start = page.getByRole("button", { name: "开始安装", exact: true });
    assert.ok(await start.isDisabled());
    await page.getByRole("button", { name: "添加来源", exact: true }).click();
    await page.getByRole("button", { name: "手动节点", exact: true }).click();
    await page.getByLabel("来源名称", { exact: true }).fill("自有节点");
    await page.getByLabel("节点名称", { exact: true }).fill("本机测试代理");
    await page.getByLabel("服务器", { exact: true }).fill("127.0.0.1");
    await page.getByRole("button", { name: "导入并验证", exact: true }).click();
    await page.getByRole("tab", { name: "节点", exact: true }).click();
    await page.locator('.proxy-node-list').waitFor();
    const nodeList = await page.locator('.proxy-node-list').evaluate(el => ({ height: el.clientHeight, content: el.scrollHeight }));
    assert.ok(nodeList.height <= 384 && nodeList.content > nodeList.height, "large node imports must scroll inside a bounded list");
    await page.getByRole("button", { name: "选择节点 本机测试代理", exact: true }).click();
    await page.getByRole("button", { name: "应用代理", exact: true }).click();
    await page.getByText("已完成", { exact: true }).waitFor();
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === '开始安装' && !button.disabled));
    assert.deepEqual(f.errors, []);
    console.log("PASS first proxy: manual import, node selection and explicit apply unlock installation inside initialization");
  } finally { await f.browser.close(); }
}
