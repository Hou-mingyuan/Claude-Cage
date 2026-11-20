import assert from "node:assert/strict";
import { test } from "node:test";
import { TERMINAL_LIMIT, liveTerminalCount, projectSessionRows, sessionKey, unassignedTerminalTabs } from "./session-order.mjs";

const sessions = [
  { id: "one", title: "第一段", pinned: true },
  { id: "two", title: "第二段", active: true },
  { id: "three", title: "第三段" },
];
const keys = rows => rows.map(sessionKey);
test("external running state blocks only history without an owned live terminal", () => {
  const running = [{id:"external",running:true}, {id:"owned",running:true,claude_desktop:true}, {id:"stopped",running:false,claude_desktop:true}, {id:"desktop",running:true,claude_desktop:true}];
  const tabs = [{id:"terminal:owned",cli_session_id:"owned"}, {id:"terminal:old",cli_session_id:"external",closed:true}];
  const rows = projectSessionRows(tabs, running, {}, undefined, new Map());
  assert.equal(rows.find(row=>row.cli_session_id==="external").sessionRunningElsewhere,true);
  assert.equal(rows.find(row=>row.cli_session_id==="owned").sessionRunningElsewhere,false);
  assert.equal(rows.find(row=>row.cli_session_id==="stopped").sessionRunningElsewhere,false);
  assert.equal(rows.find(row=>row.cli_session_id==="desktop").sessionClaudeDesktop,true);
  assert.equal(rows.filter(row=>row.sessionClaudeDesktop).length,1,"only confirmed live Desktop history gets a desktop indicator");
  const exited = projectSessionRows(tabs, running.map(session=>({...session,running:false})), {}, undefined, new Map());
  assert.equal(exited.some(row=>row.sessionRunningElsewhere),false);
  assert.equal(exited.some(row=>row.sessionClaudeDesktop),false);
});
test("default activity, stable clicks and terminal attachment", () => {
  const baseline = new Map();
  const initial = projectSessionRows([], sessions, {}, undefined, baseline);
  assert.deepEqual(keys(initial), ["session:two", "session:one", "session:three"]);
  const open = [{ id: "terminal:new", cli_session_id: "three", kind: "terminal" }];
  assert.deepEqual(keys(projectSessionRows(open, [...sessions].reverse(), {}, undefined, baseline)), keys(initial));
  const ended = [{ ...open[0], closed: true }];
  assert.equal(projectSessionRows(ended, sessions, {}, undefined, baseline).length, 3);
});
test("manual order overrides activity and pinning, survives terminal ID changes", () => {
  const manual = ["session:three", "session:one", "session:two"];
  for (const id of ["terminal:first", "terminal:second"]) {
    const rows = projectSessionRows([{ id, cli_session_id: "three" }], sessions, {}, manual, new Map());
    assert.deepEqual(keys(rows), manual);
  }
  const rows = projectSessionRows([], [...sessions, { id: "new", active: true }], {}, manual, new Map());
  assert.deepEqual(keys(rows), [...manual, "session:new"]);
});
test("only running terminals count toward the host limit", () => {
  const tabs = [
    { id: "terminal:a", kind: "terminal" },
    { id: "terminal:b", kind: "terminal", closed: true, cli_session_id: "ended" },
    { id: "editor", kind: "editor" },
  ];
  assert.equal(liveTerminalCount(tabs), 1);
  assert.equal(liveTerminalCount(Array.from({ length: TERMINAL_LIMIT }, (_, i) => ({ id: `t${i}`, kind: "terminal" }))), TERMINAL_LIMIT);
});
test("terminals outside every project folder stay reachable", () => {
  const tabs = [
    { id: "terminal:in", kind: "terminal", workspace: "/p/one" },
    { id: "terminal:grouped", kind: "terminal", workspace: "/p/extra" },
    { id: "terminal:removed", kind: "terminal", workspace: "/p/gone", closed: true, cli_session_id: "old" },
    { id: "page", kind: "web", workspace: "/p/gone" },
  ];
  const groups = [{ workspace: "/p/one", folders: ["/p/one", "/p/extra"] }];
  assert.deepEqual(unassignedTerminalTabs(tabs, ["/p/one"], groups).map(tab => tab.id), ["terminal:removed"]);
  assert.deepEqual(unassignedTerminalTabs(tabs, []).map(tab => tab.id), ["terminal:in", "terminal:grouped", "terminal:removed"]);
  const rows = projectSessionRows(unassignedTerminalTabs(tabs, ["/p/one"], groups), [], {}, undefined, new Map());
  assert.deepEqual(rows.map(row => row.id), ["terminal:removed"], "an ended conversation terminal is listed as itself, not as history");
});
