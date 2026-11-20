import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const fixture = await createWorkbench({
  mockWebSockets: false,
  clientContext: {
    mode: "local",
    connection_id: "local",
    client_platform: "windows",
    server_platform: "linux",
    connection: { state: "disconnected" },
  },
});
const { page, state } = fixture;
const inputs = [];
let socket;
let connections = 0;

const waitForInputs = async (ready) => {
  const deadline = Date.now() + 5000;
  while (!ready() && Date.now() < deadline) await page.waitForTimeout(10);
  assert.ok(ready(), "timed out waiting for terminal input");
};

try {
  await page.route("**/api/terminals", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    const tab = {
      id: "history-test",
      title: "Claude 1",
      kind: "claude",
      workspace: state.settings.workspace,
      cli_session_id: "fixture-cli",
      cli_starting: false,
    };
    state.terminals = [tab];
    return route.fulfill({ json: tab });
  });
  await page.routeWebSocket("**/api/**", (ws) => {
    if (!ws.url().includes("/terminals/")) return ws.close();
    socket = ws;
    connections++;
    ws.onMessage((message) => {
      const value = JSON.parse(message);
      if (value.type === "input") inputs.push(value.data);
    });
    ws.send(JSON.stringify({ type: "cli-startup", pending: false }));
    ws.send(JSON.stringify({ type: "history" }));
    ws.send(Buffer.from(("\x1b[c\x1b]11;?\x07\x1b[6n").repeat(500) + "\r\nRestored history " + connections));
    ws.send(Buffer.from("\r\nLive ready " + connections + "\x1b[c"));
  });
  await page.reload();
  await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).waitFor();
  await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
  await page.getByRole("button", { name: "新建终端", exact: true }).first().waitFor();
  await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
  await page.getByRole("menuitem", { name: /Claude/ }).first().waitFor();
  await page.getByRole("menuitem", { name: /Claude/ }).first().click();
  await page.getByRole("button", { name: "创建 Claude Code 终端", exact: true }).waitFor();
  await page.getByRole("button", { name: "创建 Claude Code 终端", exact: true }).click();
  for (let cycle = 1; cycle <= 10; cycle++) {
    try {
      await page.waitForFunction((n) => document.querySelector(".xterm-rows")?.textContent.includes("Live ready " + n), cycle, { timeout: 10000 });
    } catch (error) {
      console.error("terminal history fixture did not render live output", {
        cycle,
        terminals: state.terminals,
        terminalConnections: state.terminalConnections,
        body: await page.locator("body").innerText(),
        rows: await page.locator(".xterm-rows").allTextContents(),
        errors: fixture.errors,
      });
      throw error;
    }
    await waitForInputs(() => inputs.length > 0);
    assert.equal(inputs.join(""), "\x1b[?1;2c", "only the queued live query is answered, never historical queries");
    inputs.length = 0;
    socket.send(Buffer.from(("\x1b[c\x1b]11;?\x07").repeat(32)));
    await waitForInputs(() => inputs.join("").split("\x1b]11;rgb:").length - 1 === 32);
    assert.ok(inputs.some((value) => value.includes("\x1b[?1;2c")), "live device queries still answered");
    assert.ok(inputs.some((value) => value.includes("\x1b]11;rgb:")), "live color queries still answered");
    assert.equal(inputs.join("").split("\x1b[?1;2c").length - 1, 32, "all live device replies preserved");
    assert.ok(inputs.length < 8, "live reply bursts fit within the remote input queue");
    inputs.length = 0;
    await page.locator(".xterm-helper-textarea").focus();
    await page.keyboard.type("input-check-" + cycle);
    await waitForInputs(() => inputs.join("") === "input-check-" + cycle);
    assert.equal(inputs.join(""), "input-check-" + cycle, "typing works after replay");
    inputs.length = 0;
    if (cycle < 10) {
      socket.close();
      await page.getByRole("button", { name: "重新连接", exact: true }).click();
    }
  }
  assert.deepEqual(fixture.errors, []);
  console.log("PASS: 10 reconnects; historical queries suppressed; live replies and typing preserved");
} finally {
  await fixture.browser.close();
}
