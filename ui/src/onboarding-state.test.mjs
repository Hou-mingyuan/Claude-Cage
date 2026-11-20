import assert from "node:assert/strict";
import test from "node:test";
import { needsEnvironmentRestart, needsProxySetup } from "./onboarding-state.mjs";

test("only an applied Cage node completes setup", () => {
  assert.equal(needsProxySetup({}, {}, {}), true);
  assert.equal(
    needsProxySetup({}, {}, { active: false, selected: "node" }),
    true,
  );
  assert.equal(
    needsProxySetup({}, {}, { active: true, selected: "node" }),
    false,
  );
});

test("setup offers restart for a changed environment and explicit recovery", () => {
  const base = { guest_system: "debian-13", vm_running: true, safety: { gate: { mode: "closed" } } };
  for (const state of [
    { safety: { error: "出口地址或环境设置变化需要重启环境", gate: { mode: "closed" } } },
    { job: { kind: "proxy", error: "出口地址或环境设置变化需要重启环境" } },
    { job: { kind: "safety", error: "出口地址或环境设置变化需要重启环境" } },
    { job: { error: "容器文件共享需要重新连接", recovery: "environment-restart" } },
  ]) {
    assert.equal(needsEnvironmentRestart({ ...base, ...state }), true);
    assert.equal(needsEnvironmentRestart({ ...base, ...state, vm_running: false }), true);
  }
});

test("ordinary failures and stale restart errors do not ask for a restart", () => {
  const base = { guest_system: "debian-13" };
  for (const status of [undefined, null, base,
    { ...base, network_error: "无法读取物理网卡，请检查主机网络后重试" },
    { ...base, safety: { error: "关键检查尚未完成" } },
    { ...base, job: { error: "联网检查未通过：浏览器检测通道返回 HTTP 503，外网保持关闭" } },
    { ...base, job: { recovery: "environment-restart" } },
    { ...base, job: { error: "environment outdated", recovery: "environment-update" } },
    { ...base, safety: { error: "出口地址或环境设置变化需要重启环境", gate: { mode: "open" } } },
    { ...base, job: { error: "出口地址或环境设置变化需要重启环境" }, safety: { gate: { mode: "open" } } },
  ]) assert.equal(needsEnvironmentRestart(status), false);
});

test("running VM, old endpoint and restore file cannot bypass node setup", () => {
  for (const status of [{ vm_running: true }, { can_restore: true }]) {
    assert.equal(
      needsProxySetup({ proxy_url: "socks5://127.0.0.1:17896" }, status, {}),
      true,
    );
  }
});
