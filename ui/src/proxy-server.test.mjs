import assert from "node:assert/strict";
import test from "node:test";
import { isLocalListener } from "./proxy-server.mjs";

test("manual node entry identifies local addresses across numeric spellings", () => {
  for (const server of [
    "localhost", " LOCALHOST. ", "api.localhost", "127.0.0.12",
    "127.1", "2130706433", "0.0.0.0", "::", "::1", "[0:0:0:0:0:0:0:1]",
    "[::ffff:127.0.0.1]", "::ffff:7f01:1234",
  ]) assert.equal(isLocalListener(server), true, server);
});

test("manual entry leaves other hosts and remote LAN addresses to the backend", () => {
  for (const server of ["", "127.proxy.example", "localhost.example", "192.168.1.47",
    "203.0.113.10", "2001:db8::1", "[::ffff:192.168.1.1]"]) {
    assert.equal(isLocalListener(server), false, server);
  }
});
