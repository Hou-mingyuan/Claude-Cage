import React, { useEffect, useRef, useState } from "react";
import { Download, Monitor, PanelLeftOpen, RefreshCw, Trash2 } from "lucide-react";
import { api, nativeAvailable } from "./api";
import { DesktopPane } from "./account-desktops";
import { BrowserUploads } from "./browser-uploads";
import { ProjectDialog } from "./project-dialog";
import { SafetyMessage } from "./safety-message";
import { useWindowVisibility } from "./window-visibility";
import { useTranslation } from "./interface-language.mjs";
import { derivePluginDesktopAppState } from "./plugin-desktop-app-state.mjs";
import "./claude-desktop.css";

export const desktopAppPath = (plugin, app) =>
  `/desktop-apps/${encodeURIComponent(plugin)}/${encodeURIComponent(app)}`;

// PluginDesktopApp is a vendor's application that a plugin runs in the
// environment: installed from the vendor's source at its latest release,
// updated from here, and shown in a display of its own.
export function PluginDesktopApp({ plugin, app, titleId, sidebarCollapsed = false, onExpand, autoStart = true, refreshSignal = 0, onRunningChange, status, uploads = [], visible = true, keyboard, systemClipboard = true,
  onStatus, onFallback, onConfirm, onDetails, onProjectOpened }) {
  const tr = useTranslation();
  const path = desktopAppPath(plugin, app.id);
  const windowVisible = useWindowVisibility();
  const [state, setState] = useState(null);
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [jobID, setJobID] = useState("");
  const [pollRevision, setPollRevision] = useState(0);
  const [closedChooser, setClosedChooser] = useState("");
  const opened = useRef(false);
  const seenJob = useRef(false);
  const lastRunning = useRef(null);
  const {
    ownJob,
    ownGlobalJobRunning,
    otherGlobalJobRunning,
    sessionBlockedByGlobalJob,
    sessionBusy,
    maintenanceBusy,
    canPollStatus,
    progressMessage,
    progressHasDetails,
    refreshDisabled,
  } = derivePluginDesktopAppState({
    plugin,
    appID: app.id,
    status,
    state,
    submitting,
    jobID,
    visible,
    windowVisible,
  });
  const maintenanceHint = otherGlobalJobRunning ? tr("另一个操作仍在进行，请稍候") : undefined;
  const refresh = async (valid = () => true) => {
    try {
      const value = await api(path + "/status");
      if (valid()) { setState(value); setError(""); }
      return value;
    } catch (e) {
      if (valid()) setError(e.message);
      return null;
    }
  };
  const run = async (action) => {
    setSubmitting(true);
    setError("");
    try {
      const job = await api(`${path}/${action}`, {});
      if (job?.local) {
        setState((prev) => prev
          ? { ...prev, busy: true, action, message: job.message || prev.message, error: "" }
          : prev);
        setPollRevision((n) => n + 1);
      } else if (job?.id) {
        seenJob.current = false;
        setJobID(job.id);
      }
    } catch (e) {
      setError(e.message);
    } finally {
      setSubmitting(false);
    }
  };
  useEffect(() => {
    opened.current = false;
    seenJob.current = false;
    lastRunning.current = null;
    setJobID("");
    setError("");
    setState(null);
  }, [path]);
  useEffect(() => {
    if (!state) return;
    const isRunning = Boolean(state.running);
    if (lastRunning.current !== isRunning) {
      lastRunning.current = isRunning;
      onRunningChange?.(isRunning);
    }
  }, [state, onRunningChange]);
  useEffect(() => {
    if (!refreshSignal || !status?.vm_running) return;
    void refresh();
  }, [refreshSignal, status?.vm_running]);
  useEffect(() => {
    if (ownJob && status?.job?.running && status.job.id && jobID !== status.job.id) {
      seenJob.current = true;
      setJobID(status.job.id);
    }
  }, [ownJob, status?.job?.id, status?.job?.running, jobID]);
  useEffect(() => {
    if (!canPollStatus) return;
    let disposed = false, timer;
    const poll = async () => {
      const value = await refresh(() => !disposed);
      if (disposed) return;
      if (value && !opened.current) {
        if (autoStart && value.installed && !value.running && !value.failed && !value.busy && !sessionBlockedByGlobalJob && !ownGlobalJobRunning && !jobID) {
          opened.current = true;
          await run("start");
        } else if (!autoStart || value.running || value.busy || value.failed || !value.installed) {
          opened.current = true;
        }
      }
      if (!disposed) timer = setTimeout(poll, value?.busy ? 800 : 5000);
    };
    poll();
    return () => { disposed = true; clearTimeout(timer); };
  }, [canPollStatus, path, sessionBlockedByGlobalJob, ownGlobalJobRunning, pollRevision]);
  useEffect(() => {
    if (!jobID) return;
    if (status?.job?.id === jobID) {
      seenJob.current = true;
      if (status.job.running) return;
      setJobID("");
      if (status.job.error) {
        setError(status.job.error);
        return;
      }
      if (["desktop-app-install", "desktop-app-update"].includes(status.job.kind)) run("start");
      else refresh();
      return;
    }
    if (seenJob.current && status?.job?.id && status.job.id !== jobID) {
      seenJob.current = false;
      setJobID("");
      void refresh();
    }
  }, [status?.job, jobID]);
  const appUploads = uploads.filter((item) => state?.id && item.desktop === state.id);
  const directoryRequest = appUploads.find((item) => item.directory && item.id !== closedChooser);
  const update = async () => {
    if (state?.running && !await onConfirm({ title: tr("更新 {0}？", app.title), body: tr("请先保存应用内的工作。更新会关闭应用，安装官方最新版本后重新打开；登录与设置继续保留。"), confirm: tr("更新应用") })) return;
    run("update");
  };
  const remove = async () => {
    if (!await onConfirm({ title: tr("卸载 {0}？", app.title), body: tr("卸载会删除应用文件。登录、设置和项目留在环境中，重新安装后继续使用。"), confirm: tr("卸载应用") })) return;
    run("remove");
  };
  const visibleError = error || state?.error || "";
  return (
    <section className="claude-desktop-view plugin-desktop-app" aria-label={app.title}>
      <h1 id={titleId} className="sr-only" tabIndex={-1}>{app.title}</h1>
      {sidebarCollapsed && onExpand && (
        <div className="desktop-sidebar-reveal">
          <button
            className="icon-button"
            aria-label={tr("展开侧栏")}
            title={tr("展开侧栏")}
            onClick={onExpand}
          >
            <PanelLeftOpen size={15} />
          </button>
        </div>
      )}
      {!nativeAvailable() && <BrowserUploads requests={appUploads} onStatus={onStatus} />}
      {directoryRequest && status && <ProjectDialog
        key={directoryRequest.id}
        status={status}
        activate={false}
        onClose={() => {
          setClosedChooser(directoryRequest.id);
          void api("/input/files/cancel", { target: "desktop", id: directoryRequest.id, desktop: directoryRequest.desktop }).catch((e) => onStatus?.(e.message));
        }}
        onOpened={async (workspace) => {
          await api("/input/files/directory", { target: "desktop", id: directoryRequest.id, desktop: directoryRequest.desktop, workspace });
          setClosedChooser(directoryRequest.id);
          await onProjectOpened?.(workspace);
        }}
      />}
      {(visibleError || state?.failed) && (
        <div className="connection-banner" role="alert">
          <SafetyMessage message={visibleError || (state?.failure_reason === "oom-kill"
            ? tr("环境内存不足，{0} 已被系统关闭。请在设置中检查计算资源后重新打开。", app.title)
            : tr("{0} 未能启动。可重新打开，或点“更新应用”安装官方最新版本；仍失败时可能需要升级插件。", app.title))} />
          {onDetails && <button onClick={onDetails}>{tr("操作详情")}</button>}
        </div>
      )}
      {progressMessage && (
        <div className="desktop-progress" role="status">
          <RefreshCw size={16} className="spin" />
          {progressMessage}
          {progressHasDetails && onDetails && <button className="text-button" onClick={onDetails}>{tr("查看进度")}</button>}
        </div>
      )}
      <div className="desktop-screen">
        {state?.running && status?.vm_running ? (
          <div className="account-desktop-panes">
            <DesktopPane
              account={{ id: state.id, label: app.title }}
              path={path}
              heading={app.title}
              showHeader={false}
              inputLabel={tr("{0} 输入", app.title)}
              closeLabel="关闭应用"
              visible={visible}
              windowVisible={windowVisible}
              focused
              onFocus={() => {}}
              keyboard={keyboard}
              systemClipboard={systemClipboard}
              onStatus={onStatus}
              onFallback={onFallback}
              onControl={() => {}}
              busy={sessionBusy}
              close={() => run("stop")}
            />
          </div>
        ) : (
          <div className="desktop-empty">
            <Monitor size={36} />
            <div className="desktop-empty-heading">
              <h2>{app.title}</h2>
              {state?.version && <span className="desktop-version">{`v${state.version}`}</span>}
            </div>
            <p>{tr("在环境中运行官方应用。应用从官方渠道安装，默认使用最新版本，登录与数据保留在环境里。")}</p>
            <div className="desktop-actions">
              {state ? (
                !state.installed ? (
                  <button className="primary" disabled={maintenanceBusy || !status?.vm_running} title={maintenanceHint} onClick={() => run("install")}>
                    <Download size={16} />{tr("下载并安装")}
                  </button>
                ) : (
                  <>
                    <button className="primary" disabled={sessionBusy || !status?.vm_running} onClick={() => run("start")}>
                      <Monitor size={16} />{tr("打开应用")}
                    </button>
                    <button className="quiet-button" disabled={maintenanceBusy} title={maintenanceHint} onClick={update}>
                      {tr("更新应用")}
                    </button>
                    <button className="quiet-button" disabled={maintenanceBusy} title={maintenanceHint} onClick={remove}>
                      <Trash2 size={15} />{tr("卸载")}
                    </button>
                  </>
                )
              ) : !status?.vm_running ? (
                <button className="primary" disabled>
                  <Download size={16} />{tr("下载并安装")}
                </button>
              ) : error ? (
                <button className="quiet-button" disabled={refreshDisabled} onClick={() => refresh()}>
                  {tr("重新读取状态")}
                </button>
              ) : null}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}
