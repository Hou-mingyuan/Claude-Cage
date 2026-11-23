import test from "node:test";
import assert from "node:assert/strict";
import {
  autoSelectionUnavailable,
  defaultTerminalAccount,
  reconcileTerminalAccount,
} from "./terminal-menu-state.mjs";

test("defaultTerminalAccount switches away from auto when refreshed accounts have no assignable candidates", () => {
  assert.equal(defaultTerminalAccount(null), "auto");
  assert.equal(autoSelectionUnavailable(null), false);

  const changedOnly = [{ id: "acc-1", enabled: false, status: "changed" }];
  assert.equal(defaultTerminalAccount(changedOnly), "login");
  assert.equal(autoSelectionUnavailable(changedOnly), true);

  const readyNotEnabled = [{ id: "acc-2", enabled: false, status: "ready" }];
  assert.equal(defaultTerminalAccount(readyNotEnabled), "acc-2");
  assert.equal(autoSelectionUnavailable(readyNotEnabled), true);

  const eligible = [{ id: "acc-3", enabled: true, status: "ready" }];
  assert.equal(defaultTerminalAccount(eligible), "auto");
  assert.equal(autoSelectionUnavailable(eligible), false);
});

test("reconcileTerminalAccount updates untouched auto default after async load or stale cache refresh", () => {
  // Opened while accounts were still null -> refreshed to only a changed account.
  assert.equal(
    reconcileTerminalAccount({
      account: "auto",
      accounts: [{ id: "acc-1", enabled: false, status: "changed" }],
      manual: false,
    }),
    "login",
  );

  // Opened with stale ready+enabled cache -> refreshed to ready-but-disabled account.
  assert.equal(
    reconcileTerminalAccount({
      account: "auto",
      accounts: [{ id: "acc-2", enabled: false, status: "ready" }],
      manual: false,
    }),
    "acc-2",
  );

  // Opened with no accounts -> refreshed to a newly ready+enabled account.
  assert.equal(
    reconcileTerminalAccount({
      account: "login",
      accounts: [{ id: "acc-3", enabled: true, status: "ready" }],
      manual: false,
    }),
    "auto",
  );
});

test("reconcileTerminalAccount preserves user manual account choice across refreshes", () => {
  const refreshed = [
    { id: "acc-1", enabled: false, status: "changed" },
    { id: "acc-2", enabled: true, status: "ready" },
  ];
  assert.equal(
    reconcileTerminalAccount({ account: "acc-1", accounts: refreshed, manual: true }),
    "acc-1",
  );
  assert.equal(
    reconcileTerminalAccount({ account: "login", accounts: refreshed, manual: true }),
    "login",
  );
  // If a manually selected account no longer exists, fall back cleanly.
  assert.equal(
    reconcileTerminalAccount({ account: "removed", accounts: refreshed, manual: true }),
    "auto",
  );
  // If manual "auto" loses all assignable accounts after refresh, fall back instead of submitting 400.
  assert.equal(
    reconcileTerminalAccount({
      account: "auto",
      accounts: [{ id: "acc-1", enabled: false, status: "changed" }],
      manual: true,
    }),
    "login",
  );
});
