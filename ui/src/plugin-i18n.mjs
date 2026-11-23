// Helpers for localized plugin displayName/description and market author display.

export function pickPluginLocale(i18n, language) {
  if (!i18n || typeof i18n !== "object") return null;
  const target = typeof language === "string" ? language.trim() : "";
  if (!target) return null;
  if (Object.hasOwn(i18n, target) && i18n[target]) return i18n[target];
  const lower = target.toLowerCase();
  const keys = Object.keys(i18n);
  const exact = keys.find(key => key.toLowerCase() === lower);
  if (exact && i18n[exact]) return i18n[exact];
  const primary = lower.split("-")[0];
  if (primary && primary !== lower) {
    const base = keys.find(key => key.toLowerCase() === primary);
    if (base && i18n[base]) return i18n[base];
  }
  return null;
}

export function localizedPluginText(item, field, language, fallbackSource = null) {
  const own = pickPluginLocale(item?.i18n, language)?.[field]?.trim();
  if (own) return own;
  const fallback = fallbackSource ? pickPluginLocale(fallbackSource?.i18n, language)?.[field]?.trim() : "";
  if (fallback) return fallback;
  return "";
}

export function formatMarketAuthor(author, translateFn) {
  if (author?.official) return translateFn("Cage 官方");
  const name = author?.name?.trim();
  if (!name) return translateFn("作者未注明");
  return name;
}
