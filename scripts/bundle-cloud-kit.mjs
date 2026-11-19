// Prepare the Linux server and installation recipe for SSH deployment.
// An explicitly selected offline kit can also be supplied for private builds.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createGzip } from "node:zlib";
import { pipeline } from "node:stream/promises";
import { fetchBuildArtifact, sha256 as digest } from "./fetch-build-artifact.mjs";

const destination = process.argv[2];
const offline = process.env.CAGE_CLOUD_KIT;
async function prepareCore() {
  const {mihomo} = JSON.parse(fs.readFileSync("packaging/linux/dependencies.json"));
  const archive = await fetchBuildArtifact(mihomo.url, mihomo.sha256, path.resolve("_output/linux-deps", mihomo.file));
  return { archive, checksum: mihomo.sha256 };
}
if (destination === "--prefetch") {
  if (!offline) console.log("Verified cloud kit dependency at " + (await prepareCore()).archive);
  process.exit(0);
}
const target = path.resolve(destination, "cloud-linux-amd64.tar");
if (offline) {
  if (await digest(offline) !== fs.readFileSync(offline + ".sha256", "utf8").trim())
    throw Error("Cloud deployment kit checksum mismatch");
  fs.copyFileSync(offline, target);
} else {
  const { archive, checksum } = await prepareCore();
  const stage = fs.mkdtempSync(path.join(os.tmpdir(), "cage-bootstrap-"));
  try {
    const version = fs.readFileSync("VERSION", "utf8").trim();
    const binary = path.join(stage, "cage");
    for (const arch of ["amd64", "arm64"]) {
      if (!fs.existsSync(path.resolve("internal/tailnetassets/assets/cage-tailnet-linux-" + arch + ".gz"))) throw Error("Build Tailscale guest assets before packaging the cloud kit");
    }
    execFileSync("go", ["build", "-trimpath", "-ldflags", "-s -w -X github.com/sylvinus/agent-vm/internal/version.Version=" + version, "-o", binary, "./cmd/cage"], {
      env: {...process.env, CGO_ENABLED:"0", GOOS:"linux", GOARCH:"amd64"}, stdio:"inherit"
    });
    await pipeline(fs.createReadStream(binary), createGzip(), fs.createWriteStream(binary + ".gz"));
    fs.unlinkSync(binary);
    const core = path.join(stage, "mihomo.gz");
    fs.copyFileSync(archive, core);
    if (await digest(core) !== checksum) throw Error("Linux Mihomo checksum mismatch");
    const recipe = path.join(stage, "deploy/cloud");
    fs.mkdirSync(recipe, {recursive:true});
    for (const name of ["Dockerfile.guest","restore-apt-sources.sh","build-guest.sh"])
      fs.copyFileSync(path.join("deploy/cloud",name),path.join(recipe,name));
    fs.mkdirSync(path.join(stage,"internal/cage"),{recursive:true});
    fs.copyFileSync("internal/cage/apt-bootstrap.sh",path.join(stage,"internal/cage/apt-bootstrap.sh"));
    for (const name of ["cage-server.service","cage-container-engine.service"])
      fs.copyFileSync(path.join("deploy/cloud",name),path.join(stage,name));
    const runtime = JSON.parse(fs.readFileSync("deploy/cloud/runtime-artifacts.json"));
    fs.writeFileSync(path.join(stage,"engine-downloads"),runtime.artifacts.map(a=>[a.sha256,a.file,a.url].join(" ")).join("\n")+"\n");
    // Git Bash's GNU tar treats the drive letter in a Windows path as a remote host.
    const tar = process.platform === "win32" ? path.join(process.env.SystemRoot, "System32", "tar.exe") : "tar";
    execFileSync(tar, ["-cf",target,"-C",stage,"."],{env:{...process.env,COPYFILE_DISABLE:"1"},stdio:"inherit"});
  } finally { fs.rmSync(stage,{recursive:true,force:true}); }
}
fs.writeFileSync(target+".sha256",await digest(target)+"\n");
