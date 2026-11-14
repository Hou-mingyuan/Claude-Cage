export async function* lines(input, limit = 128 << 10) {
  let buffer = Buffer.alloc(0);
  for await (const chunk of input) {
    buffer = Buffer.concat([buffer, chunk]);
    let end;
    while ((end = buffer.indexOf(10)) >= 0) {
      if (end > limit) throw Object.assign(new Error("浏览器请求过大"), { code: "message_limit", safe: true });
      yield buffer.subarray(0, end).toString("utf8");
      buffer = buffer.subarray(end + 1);
    }
    if (buffer.length > limit) throw Object.assign(new Error("浏览器请求过大"), { code: "message_limit", safe: true });
  }
  if (buffer.length) throw Object.assign(new Error("浏览器请求没有完整收到"), { code: "message_invalid", safe: true });
}
