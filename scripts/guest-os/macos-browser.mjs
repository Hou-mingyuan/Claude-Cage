// Older hosts send one action per line. Route supported actions to the same GUI
// service as current hosts, so a legacy caller cannot start another browser.
import { connect } from "node:net";
import { homedir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { once } from "node:events";

const address = path.join(homedir(), "Library", "Caches", "dev.cage.GuestBrowser", "browser.sock");
const socket = connect(address);
try {
  await once(socket, "connect");
  const replies = createInterface({ input: socket })[Symbol.asyncIterator]();
  for await (const line of createInterface({ input: process.stdin })) {
    let result;
    try {
      const input = JSON.parse(line);
      if (!["navigate", "audit", "editor"].includes(input.action)) throw Error();
      if (input.action === "editor") {
        input.workspace = path.basename(input.folder);
        if (!/^[a-f0-9]{24}$/.test(input.workspace) || input.folder !== path.join(homedir(), "Cage", "Projects", input.workspace)) throw Error();
      }
      socket.write(JSON.stringify({ method: "POST", path: "/" + input.action, input }) + "\n");
      const reply = await replies.next();
      if (reply.done) break;
      result = JSON.parse(reply.value);
    } catch { result = { error: "请更新工作台以使用 Chrome 原生窗口" }; }
    if (!process.stdout.write(JSON.stringify(result) + "\n")) await once(process.stdout, "drain");
  }
} finally { socket.destroy(); }
