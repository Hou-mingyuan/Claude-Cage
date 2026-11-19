// Only the selected, reviewable text leaves the browser. Never unpack to disk.
import { BlobReader, ZipReader, type FileEntry } from "@zip.js/zip.js";

export type LogAttachment = { name: string; report: string; excerpt: string };
const MiB = 1024 * 1024;
const limitFile = 16 * MiB,
  limitTotal = 128 * MiB;
const limitText = 12000;
const problems =
  /\b(?:error|fatal|panic|warn(?:ing)?|fail(?:ed|ure)?|denied|timeout|timed out|exception|unavailable)\b|失败|错误|异常|拒绝|超时/i;

function scrub(line: string) {
  return line
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(
      /(?:https?|socks5|ss|vless|vmess|trojan):\/\/[^\s"'<>]+/gi,
      "[链接已隐藏]",
    )
    .replace(
      /(["']?(?:authorization|proxy-authorization|set-cookie|cookie|[\w-]{0,64}(?:password|passwd|token|secret|api[_-]?key))["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|(?:bearer\s+|basic\s+)?[^\s,;}]+)/gi,
      "$1[已隐藏]",
    )
    .replace(
      /\b(?:Bearer\s+[\w.~+/=-]+|sk-[\w-]{8,}|gh[pousr]_[\w-]{8,}|github_pat_[\w-]+|eyJ[\w-]+\.[\w-]+\.[\w-]+)\b/gi,
      "[凭据已隐藏]",
    )
    .replace(/[\w.+%-]{1,128}@[\w.-]{1,253}\.[a-z]{2,}/gi, "[邮箱已隐藏]")
    .replace(
      /(?:\/(?:Users|home)\/[^/\s"']+|[a-z]:[\\/]+Users[\\/]+[^\\/\s"']+)/gi,
      "[用户目录]",
    )
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[IP已隐藏]");
}
function sample(text: string, manifest: boolean) {
  if (
    !text.trim() ||
    /[\0\x01-\x08]/.test(text) ||
    /-----BEGIN [^-]*PRIVATE KEY-----/i.test(text)
  )
    return "";
  if (manifest) {
    try {
      const m = JSON.parse(text);
      return [
        "app_version",
        "os",
        "arch",
        "created_at",
        "guest_collection",
        "safety_collection",
      ]
        .filter((k) => ["string", "number", "boolean"].includes(typeof m[k]))
        .map((k) => k + "=" + scrub(String(m[k])).slice(0, 180))
        .join("\n");
    } catch {
      /* Fall through for other log formats. */
    }
  }
  const recent: string[] = [],
    errors: string[] = [];
  for (const [raw] of text.matchAll(/[^\r\n]+/g)) {
    if (raw.length > 65536 || !raw.trim()) continue;
    let line = raw;
    if (raw[0] === "{") {
      try {
        const e = JSON.parse(raw);
        if (typeof e?.message === "string")
          line = [e.time ?? e.timestamp, e.level, e.source, e.message]
            .filter(Boolean)
            .join(" ");
      } catch {
        /* Keep plain-text lines. */
      }
    }
    recent.push(line);
    if (recent.length > 4) recent.shift();
    if (problems.test(line)) {
      errors.push(line);
      if (errors.length > 5) errors.shift();
    }
  }
  return [...new Set([...errors.reverse(), ...recent.reverse()])]
    .map((line) => {
      const safe = scrub(line).trim();
      const pos = safe.search(problems);
      const start = pos > 400 ? pos - 100 : 0;
      return (start ? "…" : "") + safe.slice(start, start + 500);
    })
    .join("\n");
}
function priority(name: string) {
  if (/(^|\/)manifest\.json$/i.test(name)) return 0;
  if (/(^|\/)events\.jsonl$/i.test(name)) return 1;
  if (/(^|\/)(services\.log|latest-report\.json)$/i.test(name)) return 2;
  if (/\/README\.txt$|^README\.txt$/i.test(name)) return 5;
  return 3;
}
export async function readLogArchive(file: File): Promise<LogAttachment> {
  if (file.size > 96 * MiB)
    throw Error("ZIP 超过 96 MiB，请只打包与本次故障有关的日志。");
  const reader = new ZipReader(new BlobReader(file), { useWebWorkers: false });
  const candidates: FileEntry[] = [];
  let count = 0,
    total = 0;
  const skipped: string[] = [];
  const skip = (name: string) =>
    skipped.push(scrub(name.split("/").at(-1) || "文件").slice(0, 60));
  try {
    let entries = 0;
    for await (const entry of reader.getEntriesGenerator()) {
      if (++entries > 2048) throw Error("ZIP 内文件过多，请重新打包相关日志。");
      if (entry.directory) continue;
      count++;
      const name = entry.filename.replaceAll("\\", "/");
      if (
        entry.encrypted ||
        entry.uncompressedSize > limitFile ||
        !/(?:\.log(?:\.\d+)?|\.txt|\.jsonl|\.ndjson|\.json)$/i.test(name) ||
        /(?:^|\/)(?:\.{1,2}|__MACOSX|\.[^/]+)(?:\/|$)/.test(name) ||
        /^(?:\/|[a-z]:)/i.test(name)
      ) {
        skip(name);
        continue;
      }
      candidates.push(entry);
    }
    candidates.sort(
      (a, b) =>
        priority(a.filename) - priority(b.filename) ||
        b.lastModDate.getTime() - a.lastModDate.getTime(),
    );
    const selected: { name: string; text: string }[] = [];
    for (const entry of candidates) {
      if (
        selected.length >= 12 ||
        total + entry.uncompressedSize > limitTotal
      ) {
        skip(entry.filename);
        continue;
      }
      let bytes = 0;
      const chunks: Uint8Array[] = [];
      try {
        await entry.getData(
          new WritableStream<Uint8Array>({
            write(chunk) {
              total += chunk.byteLength;
              bytes += chunk.byteLength;
              if (
                bytes > limitFile ||
                total > limitTotal ||
                bytes > entry.uncompressedSize
              )
                throw Error("日志超过可读取范围");
              chunks.push(chunk);
            },
          }),
          { checkSignature: true },
        );
        const raw = new Uint8Array(bytes);
        let offset = 0;
        for (const chunk of chunks) {
          raw.set(chunk, offset);
          offset += chunk.length;
        }
        const utf16 =
          raw[0] === 255 && raw[1] === 254
            ? "utf-16le"
            : raw[0] === 254 && raw[1] === 255
              ? "utf-16be"
              : "utf-8";
        const text = sample(
          new TextDecoder(utf16, { fatal: true }).decode(raw),
          /\bmanifest\.json$/i.test(entry.filename),
        );
        if (text)
          selected.push({ name: scrub(entry.filename).slice(-100), text });
        else skip(entry.filename);
      } catch {
        skip(entry.filename);
      }
    }
    if (!selected.length)
      throw Error(
        "没有可读的日志。请使用未加密的 ZIP，包含文本日志、TXT、JSON 或 JSONL。",
      );
    const per = Math.min(3600, Math.floor(limitText / selected.length) - 110);
    const excerpt = selected
      .map((s) => "【" + s.name + "】\n" + s.text.slice(0, per))
      .join("\n\n")
      .slice(0, limitText);
    const report =
      "共 " +
      count +
      " 个文件，摘取 " +
      selected.length +
      " 个" +
      (skipped.length
        ? "；跳过 " +
          skipped.length +
          " 个（" +
          skipped.slice(0, 4).join("、") +
          (skipped.length > 4 ? " 等" : "") +
          "）"
        : "");
    return { name: file.name, report, excerpt };
  } finally {
    await reader.close();
  }
}

self.onmessage = async (event: MessageEvent<File>) => {
  try {
    self.postMessage({ log: await readLogArchive(event.data) });
  } catch (error) {
    self.postMessage({
      error:
        error instanceof Error && /[\u4e00-\u9fff]/.test(error.message)
          ? error.message
          : "ZIP 无法读取，请确认文件完整并重新导出。",
    });
  }
};
