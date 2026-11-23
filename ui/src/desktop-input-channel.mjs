// A pane owns one private stdin/stdout relay. Never replay a failed commit.
export function desktopInputChannel(connect) {
  let current, disposed = false;
  const unavailable = () => new Error("此实例的输入连接已变化，请重新点击输入框");
  const close = (connection, error = unavailable()) => {
    if (connection.closed) return;
    connection.closed = true;
    if (current === connection) current = undefined;
    for (const job of connection.jobs.splice(0)) { clearTimeout(job.timer); job.reject(error); }
    connection.socket.close();
  };
  const open = () => {
    const socket = connect();
    socket.binaryType = "arraybuffer";
    const connection = { socket, jobs: [], data: "", decoder: new TextDecoder(), closed: false };
    const flush = () => {
      if (disposed || connection.closed) return;
      try {
        for (const job of connection.jobs) {
          if (!job.sent) { socket.send(new TextEncoder().encode(job.line)); job.sent = true; }
        }
      } catch { close(connection); }
    };
    connection.flush = flush;
    socket.addEventListener("open", flush);
    socket.addEventListener("error", () => close(connection));
    socket.addEventListener("close", () => close(connection));
    socket.addEventListener("message", event => {
      if (connection.closed || disposed) return;
      try {
        connection.data += typeof event.data === "string" ? event.data : connection.decoder.decode(event.data, { stream: true });
        for (let end; (end = connection.data.indexOf("\n")) >= 0;) {
          if (end > 32768 || !connection.jobs.length) throw unavailable();
          const value = JSON.parse(connection.data.slice(0, end));
          connection.data = connection.data.slice(end + 1);
          const job = connection.jobs.shift();
          clearTimeout(job.timer);
          if (value.error) job.reject(new Error(value.error));
          else job.resolve(value);
        }
        if (connection.data.length > 32768) throw unavailable();
      } catch (error) { close(connection, error); }
    });
    return connection;
  };
  return {
    request(value) {
      if (disposed) return Promise.reject(unavailable());
      return new Promise((resolve, reject) => {
        const line = JSON.stringify(value) + "\n";
        if (new TextEncoder().encode(line).length > 32768) { reject(new Error("输入内容过长")); return; }
        const connection = current || (current = open());
        connection.jobs.push({ line, resolve, reject, timer: setTimeout(() => close(connection), 5000) });
        if (connection.socket.readyState === 1) connection.flush();
      });
    },
    dispose() { disposed = true; if (current) close(current); },
  };
}
