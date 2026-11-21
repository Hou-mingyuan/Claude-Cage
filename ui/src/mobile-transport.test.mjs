import test from "node:test";
import assert from "node:assert/strict";

globalThis.location = { pathname: "/" };

const { request, accessDenied } = await import("./mobile-cli/transport.mjs");

test("mobile transport preserves actionable 423 account guidance and normalizes generic lock", async () => {
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(
      "当前电脑处于免密码本机工作区，暂不支持手机访问。请在电脑的「设置 → 账户」设置账户密码（并可开启「记住登录」），然后点击重新读取。\n",
      { status: 423, headers: { "Content-Type": "text/plain; charset=utf-8", "X-Cage-Recovery": "desktop-account-mobile-access" } },
    );
    await assert.rejects(
      () => request("/projects"),
      error => {
        assert.equal(error.status, 423);
        assert.equal(accessDenied(error), true);
        assert.equal(error.recovery, "desktop-account-mobile-access");
        assert.match(error.message, /免密码本机工作区/);
        return true;
      },
    );

    globalThis.fetch = async () => new Response(
      "Cage 已锁定，请先登录\n",
      { status: 423, headers: { "Content-Type": "text/plain; charset=utf-8" } },
    );
    await assert.rejects(
      () => request("/projects"),
      error => {
        assert.equal(error.status, 423);
        assert.equal(error.recovery, "");
        assert.equal(error.message, "Cage 已锁定。请在电脑上解锁，再重新读取。");
        return true;
      },
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});
