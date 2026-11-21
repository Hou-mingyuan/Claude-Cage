import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { isMainModule } from "./is-main-module.mjs";

// Syntax-checks every tracked script, including ones no test or workflow
// runs, so a broken helper cannot merge unnoticed. Vendored and third-party
// sources are skipped: they are checked upstream and some are not standalone.
const skipped = /(?:^|\/)(?:node_modules|vendor|third_party|dist)\/|\.min\.js$/;

function shebang(file) {
  try {
    return readFileSync(file, "utf8").slice(0, 80).split("\n", 1)[0];
  } catch {
    return "";
  }
}

export function classify(file) {
  if (skipped.test(file)) return null;
  if (/\.(?:mjs|cjs|js)$/.test(file)) return "node";
  if (/\.py$/.test(file)) return "python";
  if (/\.sh$/.test(file)) return "shell";
  if (/\.[A-Za-z0-9]+$/.test(file.split("/").pop())) return null;
  const line = shebang(file);
  if (!line.startsWith("#!")) return null;
  if (/python/.test(line)) return "python";
  if (/\b(?:ba)?sh\b/.test(line)) return "shell";
  if (/\bnode\b/.test(line)) return "node";
  return null;
}

// Returns one "file: message" entry per script that fails to parse.
export function checkFiles(files) {
  const failures = [];
  const cache = mkdtempSync(join(tmpdir(), "syntax-"));
  try {
    for (const file of files) {
      const kind = classify(file);
      let result;
      if (kind === "node") {
        result = spawnSync(process.execPath, ["--check", file], { encoding: "utf8" });
      } else if (kind === "python") {
        // Compile into a temporary file so the checkout gains no __pycache__.
        result = spawnSync("python3", ["-c",
          "import py_compile,sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)",
          file, join(cache, "check.pyc")], { encoding: "utf8" });
      } else if (kind === "shell") {
        result = spawnSync("bash", ["-n", file], { encoding: "utf8" });
      } else {
        continue;
      }
      if (result.error) throw result.error;
      if (result.status !== 0) {
        failures.push(`${file}: ${(result.stderr || result.stdout).trim().split("\n").slice(0, 6).join("\n")}`);
      }
    }
  } finally {
    rmSync(cache, { recursive: true, force: true });
  }
  return failures;
}

if (isMainModule(import.meta.url)) {
  const args = process.argv.slice(2);
  const files = args.length ? args : execFileSync("git", ["ls-files", "-z"], { encoding: "utf8" })
    .split("\0").filter(Boolean);
  const failures = checkFiles(files);
  if (failures.length) {
    console.error(`Script syntax errors:\n${failures.join("\n\n")}`);
    process.exit(1);
  }
  console.log(`Checked script syntax for ${files.filter(classify).length} files.`);
}
