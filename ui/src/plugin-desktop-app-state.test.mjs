import assert from "node:assert/strict";
import test from "node:test";
import {
  derivePluginDesktopAppState,
  isDesktopAppSessionAllowedDuringJob,
  isOwnDesktopAppJob,
  isVMLifecycleJobRunning,
} from "./plugin-desktop-app-state.mjs";

test("another plugin desktop app install does not block Antigravity status polling, display, or session controls", () => {
  const status = {
    vm_running: true,
    job: {
      id: "codex-job-1",
      kind: "desktop-app-install",
      target: "codex/codex",
      running: true,
      message: "正在下载并安装 Codex，可在操作详情查看进度…",
    },
  };
  const antigravity = derivePluginDesktopAppState({
    plugin: "antigravity",
    appID: "antigravity",
    status,
    state: { id: "aaaa", installed: true, running: true, busy: false },
  });
  assert.equal(antigravity.ownJob, false);
  assert.equal(antigravity.ownGlobalJobRunning, false);
  assert.equal(antigravity.otherGlobalJobRunning, true);
  assert.equal(antigravity.canPollStatus, true);
  assert.equal(antigravity.sessionBusy, false);
  assert.equal(antigravity.refreshDisabled, false);
  assert.equal(antigravity.progressMessage, "");
  assert.equal(antigravity.progressHasDetails, false);
  // Package installation / update / removal still waits for the active install.
  assert.equal(antigravity.maintenanceBusy, true);
});

test("the plugin app currently being installed owns the progress banner and blocks its own session start", () => {
  const status = {
    vm_running: true,
    job: {
      id: "codex-job-1",
      kind: "desktop-app-install",
      target: "codex/codex",
      running: true,
      message: "正在下载并安装 Codex，可在操作详情查看进度…",
    },
  };
  const codex = derivePluginDesktopAppState({
    plugin: "codex",
    appID: "codex",
    status,
    state: { id: "bbbb", installed: false, running: false, busy: false },
  });
  assert.equal(codex.ownJob, true);
  assert.equal(codex.ownGlobalJobRunning, true);
  assert.equal(codex.sessionBlockedByGlobalJob, true);
  assert.equal(codex.sessionBusy, true);
  assert.equal(codex.maintenanceBusy, true);
  assert.equal(codex.progressMessage, status.job.message);
  assert.equal(codex.progressHasDetails, true);
});

test("per-app start/stop shows its own progress and speeds up polling even during another app install", () => {
  const status = {
    vm_running: true,
    job: {
      id: "codex-job-1",
      kind: "desktop-app-install",
      target: "codex/codex",
      running: true,
      message: "正在下载并安装 Codex，可在操作详情查看进度…",
    },
  };
  const opening = derivePluginDesktopAppState({
    plugin: "antigravity",
    appID: "antigravity",
    status,
    state: {
      id: "aaaa",
      installed: true,
      running: false,
      busy: true,
      action: "start",
      message: "正在打开 Antigravity…",
    },
  });
  assert.equal(opening.ownSessionBusy, true);
  assert.equal(opening.sessionBusy, true);
  assert.equal(opening.canPollStatus, true);
  assert.equal(opening.pollIntervalMs, 800);
  assert.equal(opening.progressMessage, "正在打开 Antigravity…");
  assert.equal(opening.progressHasDetails, false);
});

test("VM lifecycle jobs pause polling and block session controls", () => {
  const stopping = {
    vm_running: true,
    job: { id: "stop-1", kind: "stop", running: true, message: "正在停止 Cage 环境…" },
  };
  assert.equal(isVMLifecycleJobRunning(stopping.job), true);
  assert.equal(isDesktopAppSessionAllowedDuringJob(stopping.job, "antigravity", "antigravity"), false);
  const view = derivePluginDesktopAppState({
    plugin: "antigravity",
    appID: "antigravity",
    status: stopping,
    state: { id: "aaaa", installed: true, running: true },
  });
  assert.equal(view.canPollStatus, false);
  assert.equal(view.sessionBusy, true);
  assert.equal(view.refreshDisabled, true);
});

test("legacy jobs without target only match when tracked by jobID and fail closed for concurrent sessions", () => {
  const untargeted = { id: "old-1", kind: "desktop-app-install", running: true };
  assert.equal(isOwnDesktopAppJob(untargeted, "antigravity", "antigravity", ""), false);
  assert.equal(isOwnDesktopAppJob(untargeted, "antigravity", "antigravity", "old-1"), true);
  assert.equal(isDesktopAppSessionAllowedDuringJob(untargeted, "antigravity", "antigravity"), false);
});
