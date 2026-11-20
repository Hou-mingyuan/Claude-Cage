// Starting always requires a node applied by the Cage proxy manager.
export function needsProxySetup(_settings, _status, proxy) {
  return !(proxy?.active && proxy?.selected);
}

// The egress gate reports a changed exit or environment configuration as this
// message rather than a recovery code. Keep the exact message here; a generic
// network failure should still offer a retry rather than a disruptive restart.
const restartMessages = new Set(["出口地址或环境设置变化需要重启环境"]);

export function needsEnvironmentRestart(status) {
  if (!status || status.safety?.gate?.mode === "open") return false;
  return Boolean(
    (status.job?.error && status.job.recovery === "environment-restart") ||
    restartMessages.has(status.safety?.error) || restartMessages.has(status.job?.error),
  );
}
