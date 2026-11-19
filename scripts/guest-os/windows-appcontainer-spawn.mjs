import childProcess from "node:child_process";
import { openSync, closeSync } from "node:fs";
import { createServer } from "node:net";
import { randomUUID } from "node:crypto";
import path from "node:path";

// libuv <1.53 retries non-LOCAL pipe names forever inside AppContainer
// (nodejs/node#66374). Create the stdio pairs in the permitted package-local
// namespace, then let Node inherit numeric descriptors instead of creating its
// own pipes. This never grants access outside the package or opens a TCP port.
async function localPipe() {
  const name = "\\\\.\\pipe\\LOCAL\\" + randomUUID();
  const server = createServer();
  let descriptor, socket;
  try {
    await new Promise((resolve, reject) => { server.once("error", reject); server.listen(name, resolve); });
    const connected = new Promise((resolve, reject) => { server.once("connection", resolve); server.once("error", reject); });
    descriptor = openSync(name, "r+");
    socket = await connected;
    server.close();
    return { descriptor, socket, closeDescriptor() { if (descriptor !== undefined) { closeSync(descriptor); descriptor = undefined; } },
      close() { this.closeDescriptor(); socket.destroy(); } };
  } catch (error) {
    if (descriptor !== undefined) closeSync(descriptor);
    socket?.destroy(); server.close(); throw error;
  }
}

export function appContainerChromium(chromium, executable) {
  return { async launchPersistentContext(directory, options) {
    const pipes = [];
    const originalSpawn = childProcess.spawn;
    let used = false;
    try {
      // Playwright uses stdin, stdout, stderr, and two CDP pipe descriptors.
      for (let i = 0; i < 5; i++) pipes.push(await localPipe());
      childProcess.spawn = function(file, args, settings) {
        if (path.resolve(file).toLowerCase() !== path.resolve(executable).toLowerCase())
          return originalSpawn.call(this, file, args, settings);
        if (used || !Array.isArray(settings?.stdio) || settings.stdio.length !== 5 || settings.stdio.some((value, i) => value !== "pipe" && !(i === 0 && value === "ignore")))
          throw new Error("remote_browser_stdio_invalid");
        used = true;
        // Even ignored stdin must use the package-local pair: Node implements
        // "ignore" by opening NUL, whose scoped device ACL disappears at reboot.
        // Closing the parent end below gives the child EOF without that grant.
        const child = originalSpawn.call(this, file, args, { ...settings, stdio: pipes.map(pipe => pipe.descriptor) });
        for (const pipe of pipes) pipe.closeDescriptor();
        child.stdio = pipes.map((pipe, i) => { if (settings.stdio[i] === "pipe") return pipe.socket; pipe.close(); return null; });
        [child.stdin, child.stdout, child.stderr] = child.stdio;
        return child;
      };
      const context = await chromium.launchPersistentContext(directory, options);
      context.once("close", () => { for (const pipe of pipes) pipe.close(); });
      return context;
    } catch (error) {
      process.stderr.write("remote-browser launch: " + String(error.message).replace(/[a-f0-9]{64}/g, "[redacted]") + "\n");
      for (const pipe of pipes) pipe.close(); throw error;
    }
    finally { childProcess.spawn = originalSpawn; }
  } };
}
