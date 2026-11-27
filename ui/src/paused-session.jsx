import React, { useRef, useState } from "react";
import { useTranslation } from "./interface-language.mjs";
import "./paused-session.css";

// Both clients resume through the authenticated session API, never through xterm.
export function PausedSession({ id, request, onContinued, disabled }) {
  const tr = useTranslation();
  const pending = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function resume() {
    if (pending.current || disabled) return;
    pending.current = true;
    setBusy(true);
    setError("");
    try {
      const session = await request("/sessions/" + encodeURIComponent(id) + "/continue", {body:{}});
      onContinued(session);
    } catch (e) {
      setError(e.message);
    } finally {
      pending.current = false;
      setBusy(false);
    }
  }
  return <div className="paused-session">
    <strong role="status">{tr("Claude 已暂停")}</strong>
    <button type="button" disabled={disabled || busy} onClick={resume}>{busy ? tr("正在恢复…") : tr("继续对话")}</button>
    {error && <span role="alert">{tr(error)}</span>}
  </div>;
}
