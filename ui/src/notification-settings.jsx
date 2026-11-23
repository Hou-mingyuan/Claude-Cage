import React, { useEffect, useState } from "react";
import { Bell, BellOff, CheckCircle2, ExternalLink, LoaderCircle, Send } from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { InlineSelect } from "./inline-select";
import { useTranslation } from "./interface-language.mjs";
import {
  defaultNotificationPreferences,
  normalizeNotificationPreferences,
} from "./notifications.mjs";
import "./notification-settings.css";

function browserPermissionStatus() {
  if (typeof window === "undefined" || typeof window.Notification !== "function") {
    return { supported: false, permission: "unsupported", canOpenSettings: false };
  }
  const value = window.Notification.permission;
  return {
    supported: true,
    permission: value === "granted" ? "authorized" : value === "denied" ? "denied" : "not_determined",
    canOpenSettings: false,
    sound: true,
  };
}

export function NotificationSettings({ onUpdated }) {
  const tr = useTranslation();
  const [prefs, setPrefs] = useState(defaultNotificationPreferences);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [systemStatus, setSystemStatus] = useState(null);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");

  const refreshSystemStatus = async () => {
    if (nativeAvailable()) {
      try {
        const result = await native("notificationStatus");
        setSystemStatus(result);
        return result;
      } catch {
        const fallback = { supported: false, permission: "unsupported", canOpenSettings: false };
        setSystemStatus(fallback);
        return fallback;
      }
    }
    const browser = browserPermissionStatus();
    setSystemStatus(browser);
    return browser;
  };

  useEffect(() => {
    const controller = new AbortController();
    Promise.allSettled([
      api("/client/notifications", undefined, undefined, {}, { signal: controller.signal }),
      refreshSystemStatus(),
    ]).then(([savedResult]) => {
      if (controller.signal.aborted) return;
      if (savedResult.status === "fulfilled") {
        setPrefs(normalizeNotificationPreferences(savedResult.value));
      } else {
        setError(savedResult.reason?.message || tr("无法读取通知设置，请重试。"));
      }
      setLoading(false);
    });
    const onFocus = () => {
      void refreshSystemStatus();
    };
    window.addEventListener("focus", onFocus);
    window.addEventListener("cage-window-focus", onFocus);
    return () => {
      controller.abort();
      window.removeEventListener("focus", onFocus);
      window.removeEventListener("cage-window-focus", onFocus);
    };
  }, []);

  const persist = async (patch) => {
    const next = normalizeNotificationPreferences({ ...prefs, ...patch });
    setPrefs(next);
    setSaving(true);
    setError("");
    setFeedback("");
    try {
      const saved = normalizeNotificationPreferences(await api("/client/notifications", next));
      setPrefs(saved);
      onUpdated?.(saved);
      window.dispatchEvent(new CustomEvent("cage-notification-preferences", { detail: saved }));
      const enablesAny = saved.turn_completion !== "off" || saved.approval !== "off";
      if (enablesAny && systemStatus?.supported && systemStatus.permission === "not_determined") {
        await requestPermission();
      }
    } catch (e) {
      setError(e.message || tr("无法保存通知设置，请重试。"));
    } finally {
      setSaving(false);
    }
  };

  const requestPermission = async () => {
    setError("");
    setFeedback("");
    try {
      if (nativeAvailable()) {
        const updated = await native("requestNotificationPermission");
        setSystemStatus(updated);
        return updated;
      }
      if (typeof window.Notification === "function") {
        await window.Notification.requestPermission();
        return refreshSystemStatus();
      }
    } catch (e) {
      setError(e.message || tr("无法请求系统通知权限。"));
    }
    return systemStatus;
  };

  const openSystemSettings = async () => {
    if (!nativeAvailable()) return;
    try {
      await native("openNotificationSettings");
    } catch (e) {
      setError(e.message || tr("无法打开系统通知设置。"));
    }
  };

  const sendTestNotification = async () => {
    setTesting(true);
    setError("");
    setFeedback("");
    try {
      if (nativeAvailable()) {
        // The system permission prompt appears only from this explicit action
        // or from turning notifications on, never from a background event.
        if (systemStatus?.permission === "not_determined") await requestPermission();
        const result = await native("showNotification", {
          id: "cage-test-" + Date.now(),
          title: tr("Cage 通知测试"),
          body: tr("Claude Code 完成回复时会像这样提醒你，点击可回到 Cage。"),
          terminal_id: "",
          connection_id: "",
          sound: prefs.sound,
        });
        await refreshSystemStatus();
        if (result?.delivered) {
          setFeedback(tr("已发送测试通知。"));
        } else if (result?.permission === "denied" || result?.permission === "not_determined") {
          setError(tr("系统通知权限已关闭，请在系统设置中允许 Cage 发送通知。"));
        } else if (result?.permission === "unavailable") {
          setError(tr("未检测到系统通知服务，请确认桌面环境已开启通知后重试。"));
        } else {
          setError(tr("当前环境暂不支持系统原生通知。"));
        }
      } else if (typeof window.Notification === "function") {
        if (window.Notification.permission === "default") {
          await window.Notification.requestPermission();
          await refreshSystemStatus();
        }
        if (window.Notification.permission === "granted") {
          new window.Notification(tr("Cage 通知测试"), {
            body: tr("Claude Code 完成回复时会像这样提醒你。"),
            silent: !prefs.sound,
          });
          setFeedback(tr("已发送测试通知。"));
        } else {
          setError(tr("浏览器未授予通知权限，请先允许此页面发送通知。"));
        }
      } else {
        setError(tr("当前环境不支持系统通知。"));
      }
    } catch (e) {
      setError(e.message || tr("发送测试通知失败。"));
    } finally {
      setTesting(false);
    }
  };

  const permission = systemStatus?.permission || "unknown";
  const allOff = prefs.turn_completion === "off" && prefs.approval === "off";
  // Windows follows its own notification sound setting; show the switch only
  // where Cage can actually choose a sound.
  const soundSupported = systemStatus?.sound !== false;

  const modeOptions = [
    ["unfocused", tr("仅未聚焦时")],
    ["always", tr("始终")],
    ["off", tr("关闭")],
  ];

  return (
    <section className="notification-settings" aria-label={tr("通知")}>
      <div className="section-title">
        <h3>{tr("通知")}</h3>
        <p>{tr("在终端中的 Claude Code 完成回复或等待确认时发送系统通知，点击通知可回到对应终端。修改后立即生效，无需重启环境。")}</p>
      </div>

      {systemStatus && (
        <div className="notification-permission-card" data-state={permission}>
          <div className="notification-permission-copy">
            {permission === "denied" || permission === "unsupported" || permission === "unavailable" ? (
              <BellOff size={18} aria-hidden="true" />
            ) : permission === "authorized" || permission === "provisional" ? (
              <CheckCircle2 size={18} aria-hidden="true" />
            ) : (
              <Bell size={18} aria-hidden="true" />
            )}
            <div>
              <strong>
                {permission === "authorized" || permission === "provisional"
                  ? tr("系统通知已就绪")
                  : permission === "not_determined"
                    ? tr("尚未开启系统通知权限")
                    : permission === "denied"
                      ? tr("系统通知权限已关闭")
                      : permission === "unavailable"
                        ? tr("系统通知服务不可用")
                        : tr("当前运行方式暂不支持系统通知")}
              </strong>
              <p>
                {permission === "authorized" || permission === "provisional"
                  ? tr("Cage 可以直接通过系统通知中心提醒你。")
                  : permission === "not_determined"
                    ? tr("授权后，切到其他应用时也能及时收到 Claude Code 完成提醒。")
                    : permission === "denied"
                      ? tr("请在系统设置中为 Cage 打开通知权限，返回后状态会自动刷新。")
                      : permission === "unavailable"
                        ? tr("未检测到系统通知服务，请确认桌面环境已开启通知后重试。")
                        : tr("请使用 Cage 桌面应用以获得完整的系统通知与点击回跳体验。")}
              </p>
            </div>
          </div>
          <div className="notification-permission-actions">
            {permission === "not_determined" && (
              <button type="button" className="secondary-button" onClick={requestPermission} disabled={saving}>
                {tr("允许通知")}
              </button>
            )}
            {permission === "denied" && systemStatus.canOpenSettings && (
              <button type="button" className="secondary-button" onClick={openSystemSettings}>
                <ExternalLink size={14} aria-hidden="true" />
                {tr("打开系统设置")}
              </button>
            )}
          </div>
        </div>
      )}

      <div className="notification-row">
        <div className="notification-row-copy">
          <strong>{tr("回合完成通知")}</strong>
          <small>
            {tr("Claude Code 结束本轮回复时提醒。选择「仅未聚焦时」会在切换到其他应用或查看其他标签时通知，正在看该终端时不打扰；取消或 API 出错结束时不发送。")}
          </small>
        </div>
        <div className="notification-row-control">
          <InlineSelect
            aria-label={tr("回合完成通知")}
            value={prefs.turn_completion}
            disabled={loading || saving}
            onValueChange={(value) => persist({ turn_completion: value })}
          >
            {modeOptions.map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </InlineSelect>
        </div>
      </div>

      <div className="notification-row">
        <div className="notification-row-copy">
          <strong>{tr("等待确认通知")}</strong>
          <small>{tr("终端中的 Claude Code 等待你批准权限或操作时发送系统通知。")}</small>
        </div>
        <div className="notification-row-control">
          <InlineSelect
            aria-label={tr("等待确认通知")}
            value={prefs.approval}
            disabled={loading || saving}
            onValueChange={(value) => persist({ approval: value })}
          >
            {modeOptions.map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </InlineSelect>
        </div>
      </div>

      {soundSupported && <label className="toggle-row">
        <span>
          <strong>{tr("通知提示音")}</strong>
          <small>{tr("发出系统通知时播放系统默认提示音。关闭后仅显示静默横幅。")}</small>
        </span>
        <input
          role="switch"
          type="checkbox"
          checked={prefs.sound}
          disabled={loading || saving || allOff}
          onChange={(e) => persist({ sound: e.target.checked })}
        />
      </label>}

      <div className="notification-test-row">
        <button
          type="button"
          className="secondary-button"
          onClick={sendTestNotification}
          disabled={loading || testing || permission === "unsupported" || permission === "unavailable"}
        >
          {testing ? <LoaderCircle size={15} className="spin" /> : <Send size={15} />}
          {tr("发送测试通知")}
        </button>
        {feedback && <span className="footnote" role="status">{feedback}</span>}
      </div>

      {error && (
        <p className="error-message" role="alert">
          {tr(error)}
        </p>
      )}
    </section>
  );
}
