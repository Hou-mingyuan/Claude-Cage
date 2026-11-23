import React, { useRef, useState } from "react";
import { Globe2 } from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { InlineSelect } from "./inline-select";
import {
  applyInterfaceLanguage,
  languages,
  prepareInterfaceLanguage,
  systemLanguage,
  useInterfaceLanguage,
  useTranslation,
} from "./interface-language.mjs";
import "./language-settings.css";

export function LanguageSettings({ compact = false, row = false }) {
  const t = useTranslation();
  const { language, active } = useInterfaceLanguage();
  const systemName = compact && language === systemLanguage
    ? languages.find(({ id }) => id === active)?.name
    : undefined;
  const [saving, setSaving] = useState(false);
  const pending = useRef(false);
  const [error, setError] = useState("");
  async function choose(next) {
    if (pending.current || next === language) return;
    pending.current = true;
    setSaving(true);
    setError("");
    try {
      // Load the selected catalog before persisting the choice. In system mode
      // the host resolves the active language.
      const system = next === systemLanguage;
      const active = system ? (await api("/client/language")).system : next;
      const catalog = await prepareInterfaceLanguage(active);
      const saved = await api("/client/language", { language: next });
      await applyInterfaceLanguage(saved, active === saved.active ? catalog : undefined);
      if (nativeAvailable()) await native("refreshLanguage").catch(() => {});
    } catch {
      setError("无法保存界面语言，请重试。");
    } finally {
      pending.current = false;
      setSaving(false);
    }
  }
  if (row) {
    return (
      <>
        <div className="settings-row">
          <div className="settings-row-copy">
            <strong>{t("界面语言")}</strong>
          </div>
          <div className="settings-row-control">
            <InlineSelect aria-label={t("界面语言")} value={language} displayLabel={systemName} onValueChange={choose} aria-busy={saving}>
              <option value={systemLanguage}>{t("跟随系统")}</option>
              {languages.map(({ id, name }) => <option key={id} value={id} lang={id}>{name}</option>)}
            </InlineSelect>
          </div>
        </div>
        {error && <p className="error-message" role="alert">{t(error)}</p>}
      </>
    );
  }
  return (
    <>
      <label className={compact ? "startup-language" : undefined}>
        <span>{compact && <Globe2 size={15} aria-hidden="true" />} {t("界面语言")}</span>
        <InlineSelect inline={!compact} aria-label={t("界面语言")} value={language} displayLabel={systemName} onValueChange={choose} aria-busy={saving}>
          <option value={systemLanguage}>{t("跟随系统")}</option>
          {languages.map(({ id, name }) => <option key={id} value={id} lang={id}>{name}</option>)}
        </InlineSelect>
      </label>
      {!compact && <p className="field-help">{t("默认跟随主机的显示语言。切换后立即更新界面，无需重启。")}</p>}
      {error && <p className="error-message" role="alert">{t(error)}</p>}
    </>
  );
}
