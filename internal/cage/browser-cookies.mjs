import { join } from "node:path";
import { link, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { randomUUID, randomBytes, createCipheriv, createDecipheriv, createHash } from "node:crypto";

const key = cookie => createHash("sha256").update(JSON.stringify([
  cookie.name, cookie.domain, cookie.path, cookie.partitionKey || null, Boolean(cookie.partitionKeyOpaque),
])).digest("hex");
const forHost = (cookie, hostname) => cookie.domain.startsWith(".")
  ? hostname === cookie.domain.slice(1) || hostname.endsWith(cookie.domain)
  : hostname === cookie.domain;
// The stored attributes of one cookie, used to restore what a write changed.
const state = cookie => JSON.stringify([cookie.value, Boolean(cookie.secure), Boolean(cookie.httpOnly),
  cookie.sameSite || "", Boolean(cookie.session), cookie.session ? 0 : cookie.expires]);
const cookieParam = cookie => ({
  name: cookie.name, value: cookie.value, domain: cookie.domain, path: cookie.path,
  secure: Boolean(cookie.secure), httpOnly: Boolean(cookie.httpOnly),
  ...(cookie.sameSite ? { sameSite: cookie.sameSite } : {}),
  ...(cookie.session ? {} : { expires: cookie.expires }),
  ...(cookie.priority ? { priority: cookie.priority } : {}),
  ...(cookie.sourceScheme ? { sourceScheme: cookie.sourceScheme } : {}),
  ...(Number.isInteger(cookie.sourcePort) && cookie.sourcePort > 0 ? { sourcePort: cookie.sourcePort } : {}),
  ...(cookie.partitionKey ? { partitionKey: cookie.partitionKey } : {}),
});
const control = /[\u0000-\u001f\u007f;]/;
const ipAddress = hostname => hostname.startsWith("[") || /^\d+(\.\d+){3}$/.test(hostname);

// Validates one cookie for the tab's own site and returns the CDP parameter.
// A bare domain is host-only and a leading dot names the host or a parent
// domain, matching how the list reports both kinds.
function siteCookie(input, url) {
  if (!input || typeof input !== "object" || Array.isArray(input)) throw new Error("Cookie 内容无效");
  if (input.partitioned) throw new Error("暂不支持新增或编辑分区 Cookie");
  const { name, value, path } = input;
  const sameSite = input.sameSite || "", expires = input.expires || 0;
  if (typeof name !== "string" || !name || name.trim() !== name || control.test(name) || name.includes("="))
    throw new Error("Cookie 名称不能为空，且不能包含空格开头结尾、控制字符、分号或等号");
  if (typeof value !== "string" || value.trim() !== value || control.test(value))
    throw new Error("Cookie 值不能以空格开头结尾，且不能包含控制字符或分号");
  if (Buffer.byteLength(name + value) > 4096) throw new Error("Cookie 名称与值合计不能超过 4096 字节");
  if (typeof path !== "string" || !path.startsWith("/") || Buffer.byteLength(path) > 1024 || control.test(path))
    throw new Error("Cookie 路径必须以 / 开头，且不超过 1024 字节");
  if (!["", "Strict", "Lax", "None"].includes(sameSite)) throw new Error("SameSite 取值无效");
  if (typeof input.secure !== "boolean" && input.secure !== undefined || typeof input.httpOnly !== "boolean" && input.httpOnly !== undefined)
    throw new Error("Cookie 内容无效");
  const secure = Boolean(input.secure), httpOnly = Boolean(input.httpOnly);
  if (typeof expires !== "number" || !Number.isFinite(expires) || expires < 0 || expires && expires <= Date.now() / 1000)
    throw new Error("过期时间必须晚于当前时间；删除 Cookie 请使用删除");
  const hostname = url.hostname;
  const domain = typeof input.domain === "string" ? input.domain.trim().toLowerCase() : "";
  const parent = domain.startsWith(".") ? domain.slice(1) : "";
  if (domain !== hostname && !(parent && parent.includes(".") && !ipAddress(hostname) &&
      (hostname === parent || hostname.endsWith("." + parent))))
    throw new Error("只能为当前网站的主机名或其上级域名设置 Cookie");
  if (sameSite === "None" && !secure) throw new Error("SameSite=None 的 Cookie 必须勾选 Secure");
  const prefix = name.toLowerCase();
  if ((prefix.startsWith("__secure-") || prefix.startsWith("__http-")) && !secure)
    throw new Error("__Secure- 与 __Http- 前缀的 Cookie 必须勾选 Secure");
  if ((prefix.startsWith("__host-")) && (!secure || parent || path !== "/"))
    throw new Error("__Host- 前缀的 Cookie 必须勾选 Secure、路径为 /，且不能设置上级域名");
  if ((prefix.startsWith("__http-") || prefix.startsWith("__host-http-")) && !httpOnly)
    throw new Error("__Http- 与 __Host-Http- 前缀的 Cookie 必须勾选 HttpOnly");
  return {
    name, value, domain, path, secure, httpOnly, ...(sameSite ? { sameSite } : {}),
    ...(expires ? { expires } : {}), sourceScheme: secure || url.protocol === "https:" ? "Secure" : "NonSecure",
  };
}

export async function browserCookies(cdp, session, tab, input) {
  if (!tab || tab.login) throw new Error("当前标签不可管理 Cookie");
  // The host always forwards "url"; an empty value means the caller named no page.
  if (input.url !== undefined && input.url !== "" && input.url !== tab.url) throw new Error("网页已切换，请重新打开 Cookie 管理");
  const { mode = "list", scope = "site", query = "", page = 0 } = input;
  if (!["list", "details", "delete", "delete-selected", "clear", "set"].includes(mode) || !["site", "all"].includes(scope) ||
      typeof query !== "string" || query.length > 200 || !Number.isInteger(page) || page < 0)
    throw new Error("Cookie 操作无效");
  if (mode === "delete-selected" && (!Array.isArray(input.cookieKeys) || !input.cookieKeys.length ||
      !input.cookieKeys.every(value => typeof value === "string" && /^[a-f0-9]{64}$/.test(value))))
    throw new Error("请选择要删除的 Cookie");
  const matches = cookie => `${cookie.domain}\n${cookie.name}`.toLowerCase().includes(query.toLowerCase());
  let hostname = "";
  if (scope === "site") {
    const url = new URL(tab.url);
    if (!["http:", "https:"].includes(url.protocol)) throw new Error("当前页面没有网站 Cookie，请选择所有网站");
    hostname = url.hostname;
  }
  if (mode === "set" && input.cookieKey !== undefined && input.cookieKey !== "" &&
      (typeof input.cookieKey !== "string" || !/^[a-f0-9]{64}$/.test(input.cookieKey)))
    throw new Error("Cookie 已变化，请刷新列表");
  let site;
  if (mode === "set") {
    site = new URL(tab.url);
    if (!["http:", "https:"].includes(site.protocol)) throw new Error("只能在网站页面中新增或编辑该网站的 Cookie");
  }
  // Every read and write stays in the tab's own normal or private context.
  const context = tab.browserContextId ? { browserContextId: tab.browserContextId } : {};
  const read = async () => {
    const { cookies } = await cdp("Storage.getCookies", context);
    return cookies.filter(cookie => !hostname || forHost(cookie, hostname));
  };
  let cookies = await read();
  const remove = async cookie => {
    // Opaque partition keys cannot be addressed exactly by Network.deleteCookies.
    if (cookie.partitionKeyOpaque) throw new Error("此 Cookie 的分区不可单独定位，请使用清空所有网站 Cookie");
    await cdp("Network.deleteCookies", {
      name: cookie.name, domain: cookie.domain, path: cookie.path,
      ...(cookie.partitionKey ? { partitionKey: cookie.partitionKey } : {}),
    }, await session(tab.targetId));
  };
  if (mode === "delete" || mode === "details") {
    const cookie = cookies.find(cookie => key(cookie) === input.cookieKey);
    if (!cookie) throw new Error("Cookie 已变化，请刷新列表");
    if (mode === "details") return { value: cookie.value };
    await remove(cookie);
    cookies = await read();
  } else if (mode === "delete-selected") {
    const available = new Map(cookies.filter(matches).map(cookie => [key(cookie), cookie]));
    const selected = [...new Set(input.cookieKeys)].map(value => available.get(value));
    if (selected.some(cookie => !cookie)) throw new Error("所选 Cookie 已变化，请刷新列表后重新选择");
    if (selected.some(cookie => cookie.partitionKeyOpaque)) throw new Error("所选 Cookie 含不可单独定位的分区，请使用清空所有网站 Cookie");
    try {
      for (const cookie of selected) await remove(cookie);
    } catch (error) {
      throw new Error("批量删除未完成，部分 Cookie 可能已删除；请刷新列表后重新选择。", { cause: error });
    }
    cookies = await read();
  } else if (mode === "set") {
    const param = siteCookie(input.cookie, site);
    const all = async () => (await cdp("Storage.getCookies", context)).cookies;
    const before = await all();
    const original = input.cookieKey ? before.find(cookie => key(cookie) === input.cookieKey) : undefined;
    if (input.cookieKey && (!original || !forHost(original, site.hostname))) throw new Error("Cookie 已变化，请刷新列表");
    if (original && (original.partitionKey || original.partitionKeyOpaque)) throw new Error("暂不支持新增或编辑分区 Cookie");
    if (original?.priority) param.priority = original.priority;
    const target = key(param);
    if (before.some(cookie => key(cookie) === target && cookie !== original))
      throw new Error("已存在名称、域名和路径相同的 Cookie，请编辑该条");
    try { await cdp("Storage.setCookies", { cookies: [param], ...context }); }
    catch (error) { throw new Error("浏览器拒绝了此 Cookie，请检查名称、域名、Secure 与 SameSite 设置", { cause: error }); }
    const after = await all();
    const saved = after.find(cookie => key(cookie) === target);
    // Chromium caps the expiry, so only the session flag is compared for it.
    const accepted = Boolean(saved) && saved.value === param.value && Boolean(saved.secure) === param.secure &&
      Boolean(saved.httpOnly) === param.httpOnly && (saved.sameSite || "") === (param.sameSite || "") &&
      Boolean(saved.session) === !param.expires;
    // Undo what the write changed besides the accepted target: a rejected
    // target, or a cookie Chromium moved to another host such as a public
    // suffix. Only the written name and path are considered, so cookies the
    // page itself sets meanwhile are left alone.
    const previous = new Map(before.map(cookie => [key(cookie), cookie]));
    for (const cookie of after.filter(cookie => cookie.name === param.name && cookie.path === param.path)) {
      const id = key(cookie), earlier = previous.get(id);
      if (accepted && id === target) continue;
      if (!earlier) await remove(cookie);
      else if (state(earlier) !== state(cookie)) await cdp("Storage.setCookies", { cookies: [cookieParam(earlier)], ...context });
    }
    if (!accepted) throw new Error("浏览器未接受此 Cookie；公共后缀域名（如 co.uk）及不符合 Secure、SameSite 规则的设置会被拒绝");
    if (original && key(original) !== target) {
      try { await remove(original); }
      catch (error) { throw new Error("已保存新的 Cookie，但未能删除原来的 Cookie；请刷新列表后手动删除", { cause: error }); }
    }
    cookies = await read();
  } else if (mode === "clear") {
    if (scope === "all") await cdp("Storage.clearCookies", context);
    else {
      if (cookies.some(cookie => cookie.partitionKeyOpaque)) throw new Error("此网站含不可单独定位的分区 Cookie，请使用清空所有网站 Cookie");
      for (const cookie of cookies) await remove(cookie);
    }
    cookies = await read();
  }
  cookies = cookies.filter(matches)
    .sort((a, b) => a.domain.localeCompare(b.domain) || a.name.localeCompare(b.name) || a.path.localeCompare(b.path));
  const current = Math.min(page, Math.max(0, Math.ceil(cookies.length / 50) - 1));
  return { total: cookies.length, page: current, hostname, incognito: Boolean(tab.incognito),
    selectableKeys: cookies.filter(cookie => !cookie.partitionKeyOpaque).map(key),
    cookies: cookies.slice(current * 50, (current + 1) * 50).map(({ value, ...cookie }) => ({
      key: key(cookie), name: cookie.name, domain: cookie.domain, path: cookie.path,
      expires: cookie.expires, session: cookie.session, secure: cookie.secure, httpOnly: cookie.httpOnly,
      sameSite: cookie.sameSite || "", partitionKey: cookie.partitionKey,
      partitionKeyOpaque: Boolean(cookie.partitionKeyOpaque), size: cookie.size,
    })),
  };
}

// The browser process keeps opted-in sign-in cookies in the guest user's
// private profile. The account ID authenticates each encrypted record, so a
// record moved to another account cannot be restored there.
export class AccountLoginCookies {
  constructor(directory) { this.directory = directory; }

  async file(account) {
    if (!/^[a-f0-9]{24}$/.test(account)) throw new Error("授权账号无效");
    await mkdir(this.directory, {recursive: true, mode: 0o700});
    return join(this.directory, account);
  }

  async key() {
    const filename = join(this.directory, ".key");
    try { return await readFile(filename); }
    catch (error) { if (error.code !== "ENOENT") throw error; }
    const key = randomBytes(32), temporary = filename + "." + randomUUID();
    await writeFile(temporary, key, {flag: "wx", mode: 0o600});
    try {
      await link(temporary, filename);
      return key;
    } catch (error) {
      if (error.code !== "EEXIST") throw error;
      return readFile(filename);
    } finally { await unlink(temporary); }
  }

  async load(account) {
    const file = await this.file(account);
    let envelope;
    try { envelope = await readFile(file); }
    catch (error) { if (error.code === "ENOENT") return []; throw error; }
    const decipher = createDecipheriv("aes-256-gcm", await this.key(), envelope.subarray(0,12));
    decipher.setAAD(Buffer.from(account)); decipher.setAuthTag(envelope.subarray(12,28));
    return JSON.parse(Buffer.concat([decipher.update(envelope.subarray(28)),decipher.final()]).toString("utf8"));
  }

  async save(account, cookies) {
    const file = await this.file(account);
    const iv = randomBytes(12), cipher = createCipheriv("aes-256-gcm", await this.key(), iv);
    cipher.setAAD(Buffer.from(account));
    const body = Buffer.concat([cipher.update(JSON.stringify(cookies)), cipher.final()]);
    const temporary = file + "." + randomUUID();
    try {
      await writeFile(temporary, Buffer.concat([iv,cipher.getAuthTag(),body]), {flag:"wx", mode:0o600});
      await rename(temporary, file);
    } finally { await unlink(temporary).catch(() => {}); }
  }
}
