import assert from "node:assert/strict";
import test from "node:test";
import {
  desktopClipboardState,
  desktopMergeNoticeTarget,
  desktopTitle,
  mergedDefaultInstance,
  reconcileAccountDesktopPanes,
} from "./account-desktop-state.mjs";

const instanceA = { id: "a".repeat(24), number: 1, label: "工作账号", running: true };
const instanceB = { id: "b".repeat(24), number: 2, label: "备用账号", running: true };

test("isolated desktop clipboard stays connected across window occlusion and pauses polling only", () => {
  const active = desktopClipboardState({
    ready: true,
    focused: true,
    visible: true,
    windowVisible: true,
    native: true,
  });
  assert.deepEqual(active, { connected: true, pollingVisible: true });

  // Switching to another full-screen/occluding macOS window must not tear down the clipboard socket.
  const occluded = desktopClipboardState({
    ready: true,
    focused: true,
    visible: true,
    windowVisible: false,
    native: true,
  });
  assert.deepEqual(occluded, { connected: true, pollingVisible: false });

  // Opening settings pauses polling while keeping the slot warm.
  const settingsOpen = desktopClipboardState({
    ready: true,
    focused: true,
    visible: false,
    windowVisible: true,
    native: true,
  });
  assert.deepEqual(settingsOpen, { connected: true, pollingVisible: false });

  // Losing pane focus or stopping the VM disconnects cleanly.
  assert.deepEqual(
    desktopClipboardState({ ready: true, focused: false, visible: true, windowVisible: true, native: true }),
    { connected: false, pollingVisible: false },
  );
  assert.deepEqual(
    desktopClipboardState({ ready: false, focused: true, visible: true, windowVisible: true, native: true }),
    { connected: false, pollingVisible: false },
  );
});

test("reconcileAccountDesktopPanes switches off retired default Desktop and emits merge notice once", () => {
  const catalog = {
    instances: [instanceA, instanceB],
    redirects: { default: instanceA.id },
  };
  assert.equal(desktopTitle(instanceA), "#01 工作账号");
  assert.deepEqual(mergedDefaultInstance(catalog), instanceA);
  assert.deepEqual(reconcileAccountDesktopPanes([], catalog, true), [instanceA]);
  assert.deepEqual(reconcileAccountDesktopPanes([], catalog, false), []);

  // Redirecting an account pane back to default clears account panes.
  assert.deepEqual(
    reconcileAccountDesktopPanes([instanceB], { instances: [instanceA], redirects: { [instanceB.id]: "default" } }, false),
    [],
  );

  // Merge notice fires only when default redirect changes to a new target.
  assert.deepEqual(desktopMergeNoticeTarget("", catalog), instanceA);
  assert.equal(desktopMergeNoticeTarget(instanceA.id, catalog), null);
});
