"""Package only the compiled runtime, public boot files and corresponding source."""
import hashlib
import json
from pathlib import Path
import re
import shutil
import ssl
import subprocess
import sys
import urllib.request

cache, dest = map(lambda s: Path(s).resolve(), sys.argv[1:3])
identity = sys.argv[3]
if dest.exists():
    raise SystemExit("Runtime destination must not exist")
prefix = cache / "prefix"
runtime = dest / "runtime"
licenses = dest / "licenses"
for folder in (runtime / "bin", runtime / "lib", runtime / "share/qemu", licenses / "sources"):
    folder.mkdir(parents=True, exist_ok=True)


def run(*args):
    return subprocess.check_output(list(map(str, args)), text=True).strip()


def dependencies(path):
    result = []
    for line in run("otool", "-L", path).splitlines()[1:]:
        name = line.strip().split(" (", 1)[0]
        if name.startswith(("/System/", "/usr/lib/")) or name == str(path):
            continue
        if name.startswith("@"):
            resolved = next((p for p in (path.parent / Path(name).name, prefix / "lib" / Path(name).name, cache / "dependencies/lib" / Path(name).name) if p.is_file()), None)
            if resolved is None:
                raise RuntimeError("Unresolved runtime dependency: " + name)
        else:
            resolved = Path(name)
        if resolved.resolve() != path.resolve():
            result.append((name, resolved.resolve()))
    return result


files = {}
pending = [prefix / "bin/qemu-system-x86_64", prefix / "bin/qemu-img"]
formulae = {}
while pending:
    source = pending.pop()
    if source in files:
        continue
    target = runtime / ("bin" if source.parent.name == "bin" else "lib") / source.name
    if target.exists():
        raise RuntimeError("Conflicting dynamic library names")
    shutil.copy2(source, target)
    target.chmod(0o755)
    references = dependencies(source)
    files[source] = (target, references)
    pending.extend(p for _, p in references)
    if "/Cellar/" in str(source):
        parts = source.parts
        position = parts.index("Cellar")
        keg = Path(*parts[:position + 3])
        formulae[keg] = next((keg / ".brew").glob("*.rb"))

entitlements = cache / "source/qemu-11.1.3/accel/hvf/entitlements.plist"
for source, (target, references) in files.items():
    for previous, dependency in references:
        replacement = ("@executable_path/../lib/" if target.parent.name == "bin" else "@loader_path/") + dependency.name
        run("install_name_tool", "-change", previous, replacement, target)
    if target.suffix == ".dylib":
        run("install_name_tool", "-id", "@loader_path/" + target.name, target)
    signing = ["codesign", "--force", "--sign", identity]
    if target.name == "qemu-system-x86_64":
        signing += ["--entitlements", str(entitlements)]
    if identity != "-":
        signing += ["--timestamp", "--options", "runtime"]
    run(*signing, target)

for name in ("edk2-x86_64-code.fd", "edk2-i386-vars.fd", "bios-256k.bin", "kvmvapic.bin", "vgabios-stdvga.bin", "efi-virtio.rom", "linuxboot_dma.bin", "multiboot_dma.bin", "pvh.bin"):
    shutil.copy2(prefix / "share/qemu" / name, runtime / "share/qemu" / name)

provenance = {"qemu": "11.1.3", "dependencies": [], "sources": []}


def https_context():
    # python.org 安装的 Python 自带独立的 OpenSSL，没运行 Install Certificates.command
    # 时一张 CA 都没有；此时改用 macOS 自带的 /etc/ssl/cert.pem，仍然校验证书。
    context = ssl.create_default_context()
    if context.cert_store_stats()["x509_ca"] == 0 and Path("/etc/ssl/cert.pem").is_file():
        context.load_verify_locations("/etc/ssl/cert.pem")
    return context


def source_archive(name, url, digest=None):
    target = licenses / "sources" / name
    cached = cache / "downloads" / name
    if not cached.exists():
        if not url.startswith("https://"):
            raise RuntimeError("Source archive requires HTTPS")
        with urllib.request.urlopen(url, timeout=60, context=https_context()) as response, cached.open("xb") as output:
            shutil.copyfileobj(response, output)
    with cached.open("rb") as stream:
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
    if digest and actual != digest:
        raise RuntimeError("Corresponding source checksum mismatch: " + name)
    shutil.copy2(cached, target)
    provenance["sources"].append(dict(name=name, url=url, sha256=actual))


source_archive("qemu-11.1.3.tar.xz", "https://download.qemu.org/qemu-11.1.3.tar.xz", "5a8289c7940dc77d3873c81a6c29ba3afe1446fc9d36e77fa1658cdbecb84b22")
source_archive("glib-2.88.3-source.tar.xz", "https://download.gnome.org/sources/glib/2.88/glib-2.88.3.tar.xz", "ab24d24e698dfa1e408b7bcdb508f4aafc906185a8b8ce72fdf79bbbdc9b383b")
source_archive("pcre2-10.47.tar.bz2", "https://github.com/PCRE2Project/pcre2/releases/download/pcre2-10.47/pcre2-10.47.tar.bz2", "47fe8c99461250d42f89e6e8fdaeba9da057855d06eb7fc08d9ca03fd08d7bc7")
for keg, formula in formulae.items():
    text = formula.read_text()
    url = re.search(r'^\s*url "([^"]+)"', text, re.M)
    digest = re.search(r'^\s*sha256 "([a-f0-9]{64})"', text, re.M)
    if not url or not digest or "patch do" in text:
        raise RuntimeError("Review corresponding source for patched dependency: " + formula.name)
    suffix = next((s for s in (".tar.xz", ".tar.bz2", ".tar.gz", ".tgz", ".zip") if url[1].endswith(s)), None)
    if suffix is None:
        raise RuntimeError("Unknown dependency source archive format")
    name = keg.parent.name + "-" + keg.name + "-source" + suffix
    source_archive(name, url[1], digest[1])
    shutil.copy2(formula, licenses / "sources" / formula.name)
    provenance["dependencies"].append(dict(name=keg.parent.name, version=keg.name))
shutil.copy2(Path(__file__), licenses / "sources/package-intel-macos-runtime.py")
shutil.copy2(Path(__file__).with_name("build-intel-macos-runtime.sh"), licenses / "sources/build-intel-macos-runtime.sh")
(licenses / "NOTICE").write_text("QEMU is distributed under GPL-2.0. Corresponding source archives and build scripts are included in sources/. Firmware notices are supplied with QEMU's source archive. This runtime contains no guest system image.\n")
(dest / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
for executable in (runtime / "bin").iterdir():
    run("codesign", "--verify", "--strict", executable)
for target, _ in files.values():
    for minimum in re.findall(r"\bminos\s+(\S+)", run("otool", "-l", target)):
        if tuple((list(map(int, minimum.split("."))) + [0, 0])[:3]) > (14, 0, 0):
            raise RuntimeError("Runtime dependency requires a newer macOS: " + target.name)
print("Packaged clean Intel runtime:", dest)
