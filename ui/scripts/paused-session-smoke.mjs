import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { createWorkbench } from "./workbench-fixture.mjs";

// The desktop and phone share a synthetic host. Hold both continue requests
// until their UI states have been observed; neither client writes to the PTY.
const url = process.env.CAGE_UI_URL || "http://127.0.0.1:5198";
const workspace = "/projects/sample";
const view = {
  id:"shared-session", title:"项目测试记录", workspace, account_label:"项目账号",
  desktop_terminal_id:"terminal-original", terminal_available:true, can_send:false,
  paused:true, running:true, generation:"fixture", revision:1, updated_at:"2026-01-03T10:00:00Z",
  turns:[{id:"turn",status:"completed",prompt:"请检查项目的测试结果。",text:"两项检查均已完成。"}],
};
const account = () => ({session:view,supported:true,candidates:[{id:"other",label:"其他账号"}],switch_policy:"ask"});
const callers = [];
const input = [];
let unblock, automatic = 0;
const rejectConflict = new Set(["desktop","phone"]);
const continuing = new Promise(resolve => { unblock = resolve; });
async function continueRequest(route, client) {
  assert.equal(route.request().method(), "POST");
  assert.deepEqual(route.request().postDataJSON(), {});
  if (rejectConflict.delete(client)) {
    return route.fulfill({status:409,contentType:"text/plain",body:"terminal_conflict"});
  }
  callers.push(client);
  await continuing;
  return route.fulfill({json:view});
}
const fixture = await createWorkbench({
  url, mockWebSockets:false,
  beforeNavigate:async (page, state) => {
    state.status.ready = state.status.vm_running = true;
    state.status.environment_directory = state.settings.workspace = workspace;
    state.status.projects = [workspace];
    state.terminals = [{id:view.desktop_terminal_id,kind:"claude",workspace,title:view.title,cli_session_id:view.id}];
    await page.routeWebSocket("**/api/**", ws => {
      if (!ws.url().includes("/terminals/")) return ws.close();
      ws.send(JSON.stringify({type:"cli-startup",pending:false}));
      ws.send(Buffer.from("\r\nClaude Code\r\n\r\n> 请检查项目的测试结果。\r\n\r\n  两项检查均已完成。\r\n"));
      ws.onMessage(raw => { const msg = JSON.parse(raw); if (msg.type === "input") input.push(msg.data); });
    });
  },
  cliRequest:async (route, endpoint) => {
    if (endpoint === "/cli/sessions") return route.fulfill({json:{sessions:[view]}});
    if (endpoint.endsWith("/account")) return route.fulfill({json:account()});
    if (endpoint.endsWith("/continue")) return continueRequest(route, "desktop");
    assert.fail("unexpected desktop request " + endpoint);
  },
});
const { browser, page:desktop, errors } = fixture;
try {
  const phone = await browser.newPage({viewport:{width:390,height:844},userAgent:"CageMobile/1",isMobile:true,hasTouch:true});
  phone.on("pageerror", error => errors.push(error.message));
  await phone.route("**/api/**", async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint.endsWith("/projects")) return route.fulfill({json:{host:"测试电脑",ready:true,projects:[{name:"sample",path:workspace}]}});
    if (endpoint.endsWith("/sessions")) return route.fulfill({json:{sessions:[view]}});
    if (endpoint.endsWith("/" + view.id)) return route.fulfill({json:view});
    if (endpoint.endsWith("/account")) return route.fulfill({json:account()});
    if (endpoint.endsWith("/open")) {
      assert.deepEqual(route.request().postDataJSON(), {automatic:true});
      automatic++;
      return route.fulfill({json:view});
    }
    if (endpoint.endsWith("/continue")) return continueRequest(route, "phone");
    assert.fail("unexpected phone request " + endpoint);
  });
  await phone.goto(new URL("/mobile.html", url).href);
  const row = phone.getByRole("button",{name:view.title,exact:true});
  await row.getByText("已暂停").waitFor();
  await row.click();
  await phone.getByText(view.turns[0].text).waitFor();
  await phone.reload();
  const mobileButton = phone.getByRole("button",{name:"继续对话"});
  const desktopButton = desktop.getByRole("button",{name:"继续对话"});
  await mobileButton.waitFor();
  await desktop.getByRole("tab",{name:view.title,exact:true}).click();
  await desktopButton.waitFor();
  await desktop.locator(".terminal-startup").waitFor({state:"hidden"});
  const composer = phone.getByRole("textbox",{name:"消息"});
  assert.equal(await composer.isDisabled(), true);
  assert.equal(automatic, 0);
  await desktop.locator(".xterm-helper-textarea").focus();
  await desktop.keyboard.type("paused text");
  assert.deepEqual(input, []);
  if (process.env.CAGE_TEST_CAPTURE) {
    await mkdir(process.env.CAGE_TEST_CAPTURE, {recursive:true});
    await desktop.screenshot({path:path.join(process.env.CAGE_TEST_CAPTURE,"desktop-paused.png")});
    await phone.screenshot({path:path.join(process.env.CAGE_TEST_CAPTURE,"mobile-paused.png")});
  }
  await phone.getByRole("button",{name:/会话绑定/}).click();
  await phone.getByText("Claude 已暂停，原会话仍占用此账号，请先继续对话").waitFor();
  await phone.getByRole("checkbox",{name:"允许将此会话历史交给所选账号"}).check();
  assert.equal(await phone.getByRole("button",{name:"切换并恢复历史"}).isDisabled(), true);
  await phone.getByRole("button",{name:/会话绑定/}).click();
  await Promise.all([mobileButton.click(), desktopButton.click()]);
  for (const page of [phone, desktop])
    await page.getByRole("alert").filter({hasText:"原终端正在处理其他任务"}).waitFor();
  await Promise.all([mobileButton.click(), desktopButton.click()]);
  for (const page of [phone, desktop]) {
    const pending = page.getByRole("button",{name:"正在恢复…"});
    await pending.waitFor();
    assert.equal(await pending.isDisabled(), true);
    await pending.evaluate(button => button.click());
  }
  Object.assign(view,{paused:false,can_send:false,revision:2});
  unblock();
  await desktopButton.waitFor({state:"detached"});
  await mobileButton.waitFor({state:"detached"});
  assert.equal(await composer.isDisabled(), true);
  Object.assign(view,{can_send:true,revision:3});
  await composer.fill("可以继续发送");
  assert.deepEqual(callers.sort(), ["desktop","phone"]);
  assert.deepEqual(input, []);
  assert.deepEqual(errors, []);
  console.log("Desktop and phone paused session, reconnect, conflict, concurrent continue, account switch and channel readiness passed");
} finally {
  unblock();
  await browser.close();
}
