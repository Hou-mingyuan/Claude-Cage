import assert from "node:assert/strict";
import test from "node:test";
import { formatMarketAuthor, localizedPluginText, pickPluginLocale } from "./plugin-i18n.mjs";

test("pickPluginLocale matches exact, case-insensitive, and primary subtag", () => {
  const i18n = {
    "zh-CN": { displayName: "浏览器", description: "中文说明" },
    fr: { displayName: "Navigateur", description: "Description française" },
  };
  assert.deepEqual(pickPluginLocale(i18n, "zh-CN"), i18n["zh-CN"]);
  assert.deepEqual(pickPluginLocale(i18n, "zh-cn"), i18n["zh-CN"]);
  assert.deepEqual(pickPluginLocale(i18n, "fr-CA"), i18n.fr);
  assert.equal(pickPluginLocale(i18n, "ja"), null);
});

test("localizedPluginText falls back to listing i18n when installed plugin lacks i18n", () => {
  const installed = { name: "browser", description: "English" };
  const listing = { i18n: { "zh-CN": { displayName: "浏览器", description: "简体中文说明" } } };
  assert.equal(localizedPluginText(installed, "displayName", "zh-CN", listing), "浏览器");
  assert.equal(localizedPluginText(installed, "description", "zh-CN", listing), "简体中文说明");
  assert.equal(localizedPluginText(installed, "description", "ja", listing), "");
});

test("formatMarketAuthor shows official label, raw GitHub login, or fallback", () => {
  const tr = s => `[${s}]`;
  assert.equal(formatMarketAuthor({ name: "Lee-xeo", official: true }, tr), "[Cage 官方]");
  assert.equal(formatMarketAuthor({ official: true }, tr), "[Cage 官方]");
  assert.equal(formatMarketAuthor({ name: "SLEEPYBQ" }, tr), "SLEEPYBQ");
  assert.equal(formatMarketAuthor({ name: " caihao373-star " }, tr), "caihao373-star");
  assert.equal(formatMarketAuthor(null, tr), "[作者未注明]");
  assert.equal(formatMarketAuthor({ name: "  " }, tr), "[作者未注明]");
});
