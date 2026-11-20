import React from "react";
import { useTranslation } from "./interface-language.mjs";

export function ProjectRecovery({ recovery, disabled, onRetry, onSettings }) {
  const tr = useTranslation();
  return <div className="project-recovery-actions">
    {["environment-update", "environment-restart"].includes(recovery) && onSettings &&
      <button type="button" disabled={disabled} onClick={onSettings}>{recovery === "environment-update" ? tr("查看环境组件") : tr("打开环境设置")}</button>}
    {recovery === "project-directory" && onSettings &&
      <button type="button" disabled={disabled} onClick={onSettings}>{tr("查看诊断记录")}</button>}
    <button type="button" disabled={disabled} onClick={onRetry}>{recovery === "project-approval" ? tr("完成项目授权") : tr("重新连接项目")}</button>
  </div>;
}
