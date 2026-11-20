import { readdirSync, readFileSync } from "node:fs";
import assert from "node:assert/strict";
import test from "node:test";

const read = path => readFileSync(new URL(path, import.meta.url), "utf8");

test("Cage's desktop language list and first-run translations match the README", () => {
  const readme = read("../../README.md");
  const documented = [...readme.matchAll(/\[([^\]]+)\]\(README(?:\.([\w-]+))?\.md\)/g)]
    .map(([, name, id]) => [id ?? "zh-CN", name]);
  const languages = JSON.parse(read("../../internal/uilocale/languages.json"));
  assert.deepEqual(languages.map(({ id, name }) => [id, name]), documented);
  const native = JSON.parse(read("../../internal/uilocale/native.json"));
  for (const { id } of languages.slice(1)) {
    const catalog = JSON.parse(read(`./locales/${id}.json`));
    for (const source of ["界面语言", "跟随系统", "注册 Cage 账户", "为 Cage 准备独立环境", "连接你的网络代理"]) {
      assert.ok(catalog[source] && catalog[source] !== source, `${id} is missing "${source}"`);
    }
    assert.ok(native[id]?.["正在启动 Cage 工作台…"], `${id} is missing the startup message`);
    for (const [source, value] of Object.entries(catalog)) {
      const placeholders = text => (text.match(/\{\d+\}/g) ?? []).sort();
      assert.deepEqual(placeholders(value), placeholders(source), `${id}: interpolation for "${source}"`);
    }
  }
});

test("every text passed to tr() or translate() is translated in every language", () => {
  const sources = new Set();
  const files = readdirSync(new URL(".", import.meta.url), { recursive: true })
    .filter(name => /\.(jsx|js|mjs)$/.test(name) && !/\.test\.mjs$/.test(name));
  for (const name of files) {
    // Only double-quoted literals are catalog keys; template literals are not.
    for (const [, literal] of read(`./${name}`).matchAll(/\b(?:tr|translate)\(("(?:[^"\\\n]|\\.)*")/g)) {
      sources.add(JSON.parse(literal));
    }
  }
  assert.ok(sources.size > 1000, `found only ${sources.size} translated texts`);
  const languages = JSON.parse(read("../../internal/uilocale/languages.json"));
  const missing = [];
  for (const { id } of languages.slice(1)) {
    const catalog = JSON.parse(read(`./locales/${id}.json`));
    for (const source of sources) {
      if (typeof catalog[source] !== "string" || !catalog[source].trim()) missing.push(`${id}: ${source}`);
    }
  }
  assert.deepEqual(missing, []);
});
