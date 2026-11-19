import path from "node:path";
import { fileURLToPath } from "node:url";
import { homedir } from "node:os";
import { readFile } from "node:fs/promises";
process.stderr.write("remote-browser: loading components\n");
const { createWindowsBrowserService, lines } = await import("./windows-browser.mjs");
const { appContainerChromium } = await import("./windows-appcontainer-spawn.mjs");
const { chromium } = await import("./playwright/index.mjs");
process.stderr.write("remote-browser: components loaded\n");

// Launched only by the paired headless bridge with its AppContainer token and
// explicit environment. No listening CDP port or administrative browser.
const root = path.dirname(fileURLToPath(import.meta.url));
const region = JSON.parse(await readFile(path.join(root, "remote-browser-settings.json"), "utf8"));
if (typeof region.timezone !== "string" || !region.timezone || region.timezone.length > 128 ||
    typeof region.language !== "string" || !/^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8}){0,3}$/.test(region.language))
  throw new Error("remote_browser_region_invalid");
const executable = path.join(root, "chrome-headless-shell-win64", "chrome-headless-shell.exe");
const proxy = new URL(process.env.HTTPS_PROXY);
if (proxy.protocol !== "http:" || proxy.hostname !== "127.0.0.1" || proxy.port !== "17897" ||
    !["proxy", "cage"].includes(proxy.username) || !/^[a-f0-9]{64}$/.test(proxy.password)) throw new Error("remote_proxy_invalid");
const service = createWindowsBrowserService({
  remoteHeadless: true,
  executablePath: executable,
  chromium: appContainerChromium(chromium, executable),
  directory: path.join(homedir(), "Browser"),
  downloadDirectory: path.join(homedir(), "Downloads"),
  homepage: "about:blank",
  proxyCredentials: { username: proxy.username, password: proxy.password },
});
await service.request({ operation: "configure", input: {
  // Match the Windows browser density advertised by the controller. Using 1
  // with a 2x framebuffer request doubles the CSS viewport and halves text.
  proxy: proxy.origin, displayScale: 2, timezone: region.timezone, language: region.language,
} });
process.stderr.write("remote-browser: configured\n");
try {
  for await (const line of lines(process.stdin)) {
    let result;
    try {
      const request = JSON.parse(line);
      process.stderr.write("remote-browser: request received\n");
      if (request.operation === "release-viewer") { await service.releaseInput(); result = { ok: true }; }
      else if (request.operation || request.path === "/tailnet" || request.path?.startsWith("/editor"))
        result = { error: "Windows 云端此功能尚未接通", code: "action_unavailable" };
      else result = await service.request(request);
    } catch (error) {
      result = { error: error.safe ? error.message : "Windows 云端浏览器操作未完成", code: error.safe ? error.code : "browser_operation_failed" };
    }
    await new Promise((resolve, reject) => process.stdout.write(JSON.stringify(result) + "\n", error => error ? reject(error) : resolve()));
  }
} finally { await service.close(); }
