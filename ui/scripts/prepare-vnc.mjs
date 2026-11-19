import { copyFileSync, mkdirSync, readdirSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

// The vendored tree has only regular files and directories. Avoid recursive
// fs.cpSync, which can crash older Node 24 builds on Windows.
function copyVendor(source, target) {
  mkdirSync(target, { recursive: true });
  for (const entry of readdirSync(source, { withFileTypes: true })) {
    const from = join(source, entry.name);
    const to = join(target, entry.name);
    if (entry.isDirectory()) copyVendor(from, to);
    else if (entry.isFile()) copyFileSync(from, to);
    else throw new Error(`Unexpected noVNC vendor entry: ${from}`);
  }
}
copyVendor("vendor/novnc", ".generated/novnc");
try {
  execFileSync(
    "uv",
    ["run", "python", "../internal/cage/novnc-patch.py", ".generated/novnc"],
    { stdio: "inherit" },
  );
} catch (error) {
  if (error.code !== "ENOENT") throw error;
  console.error("uv was not found on PATH. Install it from https://docs.astral.sh/uv/getting-started/installation/ and rerun the UI build.");
  process.exitCode = 1;
}
