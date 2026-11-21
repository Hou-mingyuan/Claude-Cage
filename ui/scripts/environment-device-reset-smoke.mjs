import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { createWorkbench } from "./workbench-fixture.mjs";

const screenshotDir = process.env.CAGE_SCREENSHOT_DIR || "";

const baseTargets = [
  {
    id: "browser_profile",
    title: "浏览器完整资料与登录态",
    description: "Cookie、LocalStorage、IndexedDB、浏览器设备 ID（Local State）、历史与登录配置",
    affects_identity: true,
    default_selected: true,
    present: true,
    bytes: 48 * 1024 * 1024,
    files: 412,
  },
  {
    id: "claude_code",
    title: "Claude Code 登录与设备状态",
    description: "CLI 认证凭据、用户/设备标识与本地缓存",
    affects_identity: true,
    default_selected: true,
    present: true,
    bytes: 12 * 1024 * 1024,
    files: 68,
  },
  {
    id: "claude_desktop",
    title: "Claude Desktop 登录与应用数据",
    description: "桌面端 Cookie、存储、GPU 缓存与本地设备状态",
    affects_identity: true,
    default_selected: true,
    present: true,
    bytes: 95 * 1024 * 1024,
    files: 230,
  },
  {
    id: "editor_state",
    title: "内置编辑器状态与缓存",
    description: "工作区界面布局、最近打开记录与编辑器缓存（不影响项目文件）",
    affects_identity: false,
    default_selected: true,
    present: true,
    bytes: 6 * 1024 * 1024,
    files: 54,
  },
  {
    id: "system_traces",
    title: "终端历史与系统临时痕迹",
    description: "Shell 命令历史、临时状态与旧控制扩展目录",
    affects_identity: false,
    default_selected: true,
    present: true,
    bytes: 32 * 1024,
    files: 7,
  },
];

for (const runtimeKind of ["vz", "qemu", "container"]) {
  const calls = [];
  let archives = [
    {
      id: "a1b2c3d4e5f60708",
      state: "ready",
      created_at: 1795000000,
      runtime_kind: runtimeKind,
      previous_hostname: "LAT5450-K9M2Q4",
      previous_username: "alex42",
      bytes: 2.35 * 1024 * 1024 * 1024,
    },
  ];
  let batches = [
    {
      id: "b".repeat(32),
      created_at: 1795080000,
      count: 4,
      item_ids: ["browser_profile", "claude_code", "claude_desktop", "system_traces"],
      bytes: 155 * 1024 * 1024,
      previous_hostname: "T14G4-7N3P8X",
    },
  ];
  let lastResult = {
    mode: "quick",
    completed_at: 1795080120,
    irreversible: false,
    batch_id: "b".repeat(32),
    before: {
      hostname: "T14G4-7N3P8X",
      mac_suffix: "..:4a:9c",
      uuid_prefix: "3f8a21c0",
      audio_description: "Focusrite Scarlett Solo 4th Gen",
    },
    after: {
      hostname: "DESKTOP-7Q4N8K2",
      mac_suffix: "..:e2:15",
      uuid_prefix: "9c41d0e7",
      audio_description: "Yamaha AG03MK2",
    },
    cleaned_items: ["browser_profile", "claude_code", "claude_desktop", "system_traces"],
    vz_identifier_reset: runtimeKind === "vz",
    smbios_rotated: runtimeKind === "qemu",
    os_rotated: true,
  };

  const fixture = await createWorkbench({
    viewport: { width: 1280, height: 920 },
    deviceScaleFactor: screenshotDir ? 2 : 1,
    beforeNavigate: async (page, state) => {
      state.status.ready = state.status.vm_running = true;
      await page.route("**/api/jobs/**", async (route) => {
        const id = new URL(route.request().url()).pathname.split("/").at(-1);
        return route.fulfill({ json: { id, kind: "device-reset", running: false, message: "已完成" } });
      });
      await page.route("**/api/environment/device-reset**", async (route) => {
        const url = new URL(route.request().url());
        const suffix = url.pathname.replace(/^\/api\/environment\/device-reset\/?/, "");
        const method = route.request().method();
        const input = method === "POST" ? route.request().postDataJSON() : null;
        calls.push({ method, suffix, input });

        if (method === "GET" && suffix === "") {
          return route.fulfill({
            json: {
              supported: true,
              runtime_kind: runtimeKind,
              host_os: runtimeKind === "vz" ? "darwin" : "linux",
              container: runtimeKind === "container",
              vm_running: true,
              ready: true,
              legacy_profile: false,
              current: {
                hostname: "DESKTOP-7Q4N8K2",
                mac_suffix: "..:e2:15",
                uuid_prefix: "9c41d0e7",
                machine_id_prefix: "9c41d0e7",
                username: "jordan18",
                browser_username: "casey64",
                audio_description: "Yamaha AG03MK2",
                display_size: "2560x1440",
                browser_size: "1440x900",
                runtime_kind: runtimeKind,
                smbios_supported: runtimeKind === "qemu",
                vz_identifier_supported: runtimeKind === "vz",
              },
              targets: baseTargets,
              protected: [
                "宿主机上所有项目文件夹（双向共享，绝不删除或修改）",
                "代理订阅、节点配置与出口规则",
                "密码库、收藏与 Cage 应用通用设置",
              ],
              archives,
              recovery_batches: batches,
              last_result: lastResult,
              audio_models: [{ id: "yamaha-ag03mk2", label: "Yamaha AG03MK2" }],
            },
          });
        }
        if (method === "POST" && suffix === "preview") {
          return route.fulfill({
            json: {
              hostname: "B860MSL-K7R2M9",
              username: "morgan51",
              work_home: "/home/morgan51",
              browser_username: "riley82",
              browser_home: "/home/riley82",
              extension_name: "Window Panels",
              display_width: 2880,
              display_height: 1800,
              browser_width: 1600,
              browser_height: 900,
              audio_description: "Steinberg UR22C",
              webgl: input.webgl || "native",
              canvas: input.canvas || "native",
              graphics_backend: input.graphics_backend || "auto",
              gpu_backend: input.gpu_backend || "default",
            },
          });
        }
        if (method === "POST" && suffix === "execute") {
          return route.fulfill({ json: { id: "job-reset-1", running: false } });
        }
        if (method === "POST" && suffix === "restore") {
          if (input.kind === "batch") {
            batches = [];
            return route.fulfill({ json: { status: "ok", batch_id: input.id, outcomes: [] } });
          }
          archives = [];
          return route.fulfill({ json: { id: "job-restore-1", running: false } });
        }
        if (method === "POST" && suffix === "delete-archive") {
          if (input.kind === "archive") archives = archives.filter((a) => a.id !== input.id);
          if (input.kind === "batch") batches = batches.filter((b) => b.id !== input.id);
          return route.fulfill({ json: { status: "ok", id: input.id } });
        }
        return route.fulfill({ status: 404, body: "not found" });
      });
    },
  });

  const { page, browser, errors } = fixture;
  try {
    await page.getByRole("button", { name: "设置", exact: true }).click();
    await page
      .getByRole("navigation", { name: "设置分组" })
      .getByRole("button", { name: "环境与恢复", exact: true })
      .click();

    const panel = page.getByRole("region", { name: "一键换机与指纹重置" });
    await panel.getByText("DESKTOP-7Q4N8K2").first().waitFor();
    await panel.scrollIntoViewIfNeeded();

    // 1. Test L3 mode + custom profile preview
    const customSummary = panel.locator("summary", { hasText: "自定义新机名称与设备配置" });
    await customSummary.click();
    await panel.getByRole("button", { name: "重新随机生成名称和尺寸", exact: true }).click();
    await panel.locator('input[value="B860MSL-K7R2M9"]').waitFor();

    if (screenshotDir && runtimeKind === "vz") {
      await mkdir(screenshotDir, { recursive: true });
      await panel.locator("h4").first().scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(screenshotDir, "device-reset-l3-top.png") });
      await panel.getByRole("button", { name: "一键换新机", exact: true }).scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(screenshotDir, "device-reset-l3-bottom.png") });
    }

    // 2. Switch to L2 Quick Reset mode, verify all 5 checkboxes default checked, test uncheck warning
    await panel.getByRole("radio", { name: /快速换机（原地重置）/ }).check();
    const browserBox = panel.getByRole("checkbox", { name: "浏览器完整资料与登录态", exact: true });
    await browserBox.waitFor();
    assert.equal(await browserBox.isChecked(), true);
    assert.equal(await panel.getByRole("checkbox", { name: "内置编辑器状态与缓存", exact: true }).isChecked(), true);

    await browserBox.uncheck();
    await panel.getByText(/取消勾选了部分包含登录态或设备标识的资料/).waitFor();
    await browserBox.check();

    if (screenshotDir && runtimeKind === "vz") {
      await panel.locator("h4").first().scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(screenshotDir, "device-reset-l2-quick.png") });
    }

    // 3. Test irreversible checkbox + double confirmation modal (cancel first, then execute)
    const irrev = panel.getByRole("checkbox", { name: /彻底清理旧数据（不可恢复）/ });
    assert.equal(await irrev.isChecked(), false);
    await irrev.check();
    await panel.getByRole("button", { name: "执行快速换机", exact: true }).click();
    let dialog = page.getByRole("dialog", { name: "快速换机并彻底删除所选数据？", exact: true });
    await dialog.waitFor();
    assert.match(await dialog.innerText(), /不可恢复/);

    if (screenshotDir && runtimeKind === "vz") {
      await page.screenshot({ path: path.join(screenshotDir, "device-reset-confirm-dialog.png") });
    }

    await dialog.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(calls.filter((c) => c.suffix === "execute").length, 0);

    await panel.getByRole("button", { name: "执行快速换机", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "快速换机并彻底删除所选数据？", exact: true });
    await dialog.getByRole("button", { name: "确认快速换机（不可恢复）", exact: true }).click();
    await panel.getByText("换机任务已提交，请在下方查看实时进度。").waitFor();

    const quickExec = calls.find((c) => c.suffix === "execute");
    assert.equal(quickExec.input.mode, "quick");
    assert.equal(quickExec.input.irreversible, true);
    assert.equal(quickExec.input.confirm_irreversible, true);
    assert.equal(quickExec.input.items.length, 5);

    // 4. Test L3 rebuild submission with custom profile
    await panel.getByRole("radio", { name: /彻底换新机（模板重建）/ }).check();
    await irrev.uncheck();
    await panel.getByRole("button", { name: "一键换新机", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "重建全新客机并归档旧环境？", exact: true });
    await dialog.getByRole("button", { name: "开始一键换新机", exact: true }).click();
    const rebuildExec = calls.filter((c) => c.suffix === "execute").at(-1);
    assert.equal(rebuildExec.input.mode, "rebuild");
    assert.equal(rebuildExec.input.irreversible, false);
    assert.equal(rebuildExec.input.custom_profile.hostname, "B860MSL-K7R2M9");

    // 5. Test archive deletion & batch restore
    await panel.getByRole("button", { name: "删除归档", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "永久删除这份旧环境归档？", exact: true });
    await dialog.getByRole("button", { name: "确认永久删除", exact: true }).click();
    await panel.getByText("已删除旧归档并释放空间。").waitFor();

    await panel.getByRole("button", { name: "恢复此批资料", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "恢复这批重置前的数据？", exact: true });
    await dialog.getByRole("button", { name: "确认恢复数据", exact: true }).click();
    await panel.getByText("已恢复这批重置前的数据。").waitFor();

    // 6. Test proxy switch shortcut button in post-reset diff banner
    await panel.getByRole("button", { name: "前往切换代理节点", exact: true }).click();
    await page.getByText("测试节点").first().waitFor();

    assert.deepEqual(errors, []);
    console.log(JSON.stringify({ runtimeKind, passed: true, apiCalls: calls.length }));
  } finally {
    await browser.close();
  }
}
