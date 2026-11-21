import assert from "node:assert/strict";
import test from "node:test";
import { cookieEditable, cookieForm, cookieFromForm, localDateTime, newCookieForm, siteHostname } from "./browser-cookie-form.mjs";

test("only cookies of the tab's own website are editable", () => {
  const host = siteHostname("https://sub.example.test/page");
  assert.equal(host, "sub.example.test");
  assert.equal(siteHostname("cage://settings#privacy"), "");
  assert.equal(siteHostname("not a url"), "");
  const cookie = (domain, extra = {}) => ({ name: "a", domain, path: "/", ...extra });
  assert.equal(cookieEditable(cookie("sub.example.test"), host), true);
  assert.equal(cookieEditable(cookie(".example.test"), host), true);
  assert.equal(cookieEditable(cookie("example.test"), host), false);
  assert.equal(cookieEditable(cookie(".notexample.test"), host), false);
  assert.equal(cookieEditable(cookie("sub.example.test", { partitionKey: { topLevelSite: "https://example.test" } }), host), false);
  assert.equal(cookieEditable(cookie("sub.example.test", { partitionKeyOpaque: true }), host), false);
  assert.equal(cookieEditable(cookie("sub.example.test"), ""), false);
});

test("forms round-trip cookies with session and expiry in local time", () => {
  const now = Date.UTC(2026, 10, 19, 12, 0);
  const added = newCookieForm("https://sub.example.test/", now);
  assert.deepEqual({ ...added, expires: undefined }, { name: "", value: "", domain: "sub.example.test", path: "/", secure: true,
    httpOnly: false, sameSite: "Lax", session: true, expires: undefined });
  assert.equal(newCookieForm("http://localhost:3000/", now).secure, false);
  assert.deepEqual(cookieFromForm({ ...added, name: "n", value: "v" }),
    { name: "n", value: "v", domain: "sub.example.test", path: "/", secure: true, httpOnly: false, sameSite: "Lax", expires: 0 });
  const expires = Math.floor(now / 1000) + 3600 * 24 * 7 - 60 * 7 - 13;
  const form = cookieForm({ name: "n", domain: ".Example.test", path: "/a", secure: true, httpOnly: true, sameSite: "", session: false, expires }, "secret", now);
  assert.equal(form.value, "secret");
  assert.equal(form.expires, localDateTime(expires));
  const sent = cookieFromForm({ ...form, domain: " .Example.TEST " });
  assert.equal(sent.domain, ".example.test");
  assert.equal("sameSite" in sent, false);
  assert.equal(sent.expires, expires - (expires % 60));
  assert.equal(cookieFromForm({ ...form, expires: "" }).expires, -1);
});
