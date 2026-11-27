const vmLifecycleJobKinds = new Set([
  "start",
  "stop",
  "setup",
  "restore",
  "settings",
  "resources",
  "storage",
  "storage-recovery",
  "device-reset",
  "device-reset-restore",
  "guest-install",
  "environment-update",
]);

const sessionAllowedGlobalJobKinds = new Set([
  "claude-desktop-install",
  "claude-desktop-update",
  "claude-desktop-start",
  "claude-desktop-stop",
  "desktop-instance-start",
  "desktop-instance-stop",
  "environment-tools",
  "tailscale-install",
  "safety",
  "network-recovery",
  "proxy",
  "proxy-update",
  "proxy-network",
  "project",
]);

export function desktopAppTargetKey(plugin, appID) {
  return `${plugin}/${appID}`;
}

export function isVMLifecycleJobRunning(job) {
  return Boolean(job?.running && vmLifecycleJobKinds.has(job.kind));
}

export function isOwnDesktopAppJob(job, plugin, appID, trackedJobID = "") {
  if (!job?.kind?.startsWith("desktop-app-")) return false;
  const target = desktopAppTargetKey(plugin, appID);
  if (job.target) return job.target === target;
  return Boolean(trackedJobID && job.id === trackedJobID);
}

export function isDesktopAppSessionAllowedDuringJob(job, plugin, appID) {
  if (!job?.running) return true;
  if (
    job.kind === "desktop-app-install" ||
    job.kind === "desktop-app-update" ||
    job.kind === "desktop-app-remove"
  ) {
    return Boolean(job.target && job.target !== desktopAppTargetKey(plugin, appID));
  }
  return sessionAllowedGlobalJobKinds.has(job.kind);
}

export function derivePluginDesktopAppState({
  plugin,
  appID,
  status,
  state,
  submitting = false,
  jobID = "",
  visible = true,
  windowVisible = true,
}) {
  const job = status?.job;
  const vmRunning = Boolean(status?.vm_running);
  const globalJobRunning = Boolean(job?.running);
  const ownJob = isOwnDesktopAppJob(job, plugin, appID, jobID);
  const ownGlobalJobRunning = globalJobRunning && ownJob;
  const ownSessionBusy = Boolean(state?.busy);
  const sessionBlockedByGlobalJob =
    globalJobRunning && !isDesktopAppSessionAllowedDuringJob(job, plugin, appID);
  const trackingGlobalJob = Boolean(jobID);
  const sessionBusy =
    submitting || ownSessionBusy || sessionBlockedByGlobalJob || trackingGlobalJob;
  const maintenanceBusy =
    submitting || globalJobRunning || ownSessionBusy || trackingGlobalJob;
  const otherGlobalJobRunning = globalJobRunning && !ownJob;
  const canPollStatus = Boolean(
    visible && windowVisible && vmRunning && !isVMLifecycleJobRunning(job),
  );
  const pollIntervalMs = ownSessionBusy ? 800 : 5000;
  const progressMessage = ownGlobalJobRunning
    ? job?.message || ""
    : ownSessionBusy
      ? state?.message || ""
      : "";
  const progressHasDetails = Boolean(ownGlobalJobRunning && progressMessage);
  const refreshDisabled = submitting || !vmRunning || isVMLifecycleJobRunning(job);

  return {
    ownJob,
    ownGlobalJobRunning,
    ownSessionBusy,
    otherGlobalJobRunning,
    sessionBlockedByGlobalJob,
    sessionBusy,
    maintenanceBusy,
    canPollStatus,
    pollIntervalMs,
    progressMessage,
    progressHasDetails,
    refreshDisabled,
  };
}
