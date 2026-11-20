import { useSyncExternalStore } from "react";
import languages from "../../internal/uilocale/languages.json";

export { languages };
export const defaultLanguage = "zh-CN";
export const systemLanguage = "system";

const catalogs = import.meta.glob("./locales/*.json", { import: "default" });
const listeners = new Set();
let state = { language: systemLanguage, active: defaultLanguage };
let messages = {};

function subscribe(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useInterfaceLanguage() {
  return useSyncExternalStore(subscribe, () => state);
}

export async function prepareInterfaceLanguage(active) {
  if (active === defaultLanguage) return {};
  const load = catalogs[`./locales/${active}.json`];
  if (!load) throw new Error("请选择支持的界面语言。");
  return load();
}

export async function applyInterfaceLanguage(preference, catalog) {
  const language = languages.find(item => item.id === preference.active);
  if (!language) throw new Error("请选择支持的界面语言。");
  messages = catalog ?? await prepareInterfaceLanguage(language.id);
  document.documentElement.lang = language.id;
  document.documentElement.dir = language.rtl ? "rtl" : "ltr";
  state = { language: preference.language, active: language.id };
  for (const listener of listeners) listener();
}

export function translate(source, ...values) {
  const message = Object.hasOwn(messages, source) ? messages[source] : source;
  return values.length ? message.replace(/\{(\d+)\}/g, (match, index) =>
    Number(index) < values.length ? String(values[index]) : match,
  ) : message;
}

export function useTranslation() {
  useInterfaceLanguage();
  return translate;
}
