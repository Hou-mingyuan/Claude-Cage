import test from "node:test";
import assert from "node:assert/strict";
import {
  buildNotificationContent,
  createNotificationDeduplicator,
  defaultNotificationPreferences,
  dispatchDesktopNotification,
  normalizeNotificationPreferences,
  shouldDeliverNotification,
} from "./notifications.mjs";

test("normalizeNotificationPreferences fills defaults and rejects invalid modes", () => {
  assert.deepEqual(normalizeNotificationPreferences(null), defaultNotificationPreferences);
  assert.deepEqual(normalizeNotificationPreferences({ turn_completion: "bogus", approval: "always", sound: false }), {
    turn_completion: "off",
    approval: "always",
    sound: false,
  });
  assert.equal(defaultNotificationPreferences.turn_completion, "off");
  assert.equal(defaultNotificationPreferences.approval, "off");
});

test("shouldDeliverNotification honors unfocused, always, and off modes", () => {
  const event = { id: "c:turn:1", kind: "turn_completed", terminal_id: "0123456789abcdef" };
  assert.equal(shouldDeliverNotification({ turn_completion: "off" }, event, { windowFocused: false, viewingTerminal: false }), false);
  assert.equal(shouldDeliverNotification({ turn_completion: "always" }, event, { windowFocused: true, viewingTerminal: true }), true);
  // In unfocused mode: suppress only when window is focused AND viewing the exact terminal.
  assert.equal(shouldDeliverNotification({ turn_completion: "unfocused" }, event, { windowFocused: true, viewingTerminal: true }), false);
  assert.equal(shouldDeliverNotification({ turn_completion: "unfocused" }, event, { windowFocused: true, viewingTerminal: false }), true);
  assert.equal(shouldDeliverNotification({ turn_completion: "unfocused" }, event, { windowFocused: false, viewingTerminal: true }), true);
  assert.equal(shouldDeliverNotification({ turn_completion: "unfocused" }, event, { windowFocused: false, viewingTerminal: false }), true);
});

test("buildNotificationContent uses generic completion copy without leaking transcript data", () => {
  const turn = buildNotificationContent({ kind: "turn_completed", workspace_name: "demo", terminal_title: "Claude 1" });
  assert.equal(turn.title, "Claude Code 已完成回复");
  assert.equal(turn.body, "demo · Claude 1 · 点击返回终端");

  const approval = buildNotificationContent({ kind: "approval_requested" });
  assert.equal(approval.title, "Claude Code 等待确认");
  assert.equal(approval.body, "终端有操作等待确认，点击返回终端");
});

test("dispatchDesktopNotification sends once per event id and passes sound and terminal target", async () => {
  const calls = [];
  const dedup = createNotificationDeduplicator();
  const event = { id: "c:turn:2", kind: "turn_completed", terminal_id: "0123456789abcdef", terminal_title: "Claude 1" };
  const opts = {
    prefs: { turn_completion: "unfocused", approval: "unfocused", sound: false },
    viewState: { windowFocused: false, viewingTerminal: true },
    nativeAvailable: () => true,
    native: async (action, payload) => {
      calls.push({ action, payload });
      return { delivered: true, permission: "authorized" };
    },
    connectionId: "local",
    dedup,
  };
  const first = await dispatchDesktopNotification(event, opts);
  const second = await dispatchDesktopNotification(event, opts);
  assert.equal(first.delivered, true);
  assert.equal(second.delivered, false);
  assert.equal(second.reason, "duplicate");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].action, "showNotification");
  assert.equal(calls[0].payload.terminal_id, "0123456789abcdef");
  assert.equal(calls[0].payload.connection_id, "local");
  assert.equal(calls[0].payload.sound, false);
});
