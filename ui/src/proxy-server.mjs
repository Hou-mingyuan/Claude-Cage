// Recognize numeric loopback spellings and localhost while the user types.
// The backend also checks addresses assigned to the Cage host itself.
export function isLocalListener(server) {
  const host = server.trim().replace(/^\[|\]$/g, "").replace(/\.$/, "").toLowerCase();
  if (host === "localhost" || host.endsWith(".localhost")) return true;
  try {
    const parsed = new URL(`http://${host.includes(":") ? `[${host}]` : host}`).hostname;
    return /^127\.\d+\.\d+\.\d+$/.test(parsed) ||
      parsed === "0.0.0.0" || parsed === "[::]" || parsed === "[::1]" ||
      /^\[::ffff:7f[0-9a-f]{2}:/.test(parsed) || parsed === "[::ffff:0:0]";
  } catch {
    return false;
  }
}
