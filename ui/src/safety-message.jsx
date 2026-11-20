import React, { createContext, useContext } from "react";
import { isSafetyCheckMessage } from "./safety-state.mjs";
import { useTranslation } from "./interface-language.mjs";

export const SafetyChecklistContext = createContext(null);

export function SafetyMessage({ message }) {
  const tr = useTranslation();
  const openChecklist = useContext(SafetyChecklistContext);
  if (!openChecklist || !isSafetyCheckMessage(message)) return tr(message);
  return <>
    <span className="safety-message-text">{tr(message)}</span>
    <button type="button" className="text-button safety-checklist-link" onClick={openChecklist}>
      {tr("查看检查清单")}
    </button>
  </>;
}
