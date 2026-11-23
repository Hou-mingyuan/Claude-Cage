export const desktopTitle = (a) =>
  a.number ? `#${String(a.number).padStart(2, "0")} ${a.label}` : a.label;

export function mergedDefaultInstance(catalog) {
  const target = catalog?.redirects?.default;
  if (!target || !Array.isArray(catalog?.instances)) return null;
  return catalog.instances.find((item) => item.id === target) || null;
}

export function reconcileAccountDesktopPanes(oldPanes, catalog, viewingDefault = false) {
  if (!catalog?.instances) return oldPanes;
  if (!oldPanes.length) {
    if (!viewingDefault) return oldPanes;
    const merged = mergedDefaultInstance(catalog);
    return merged && merged.id !== "default" ? [merged] : oldPanes;
  }
  if (oldPanes.some((p) => catalog.redirects?.[p.id] === "default")) return [];
  const seen = new Set();
  const updated = [];
  for (const pane of oldPanes) {
    const id = catalog.redirects?.[pane.id] || pane.id;
    const item = catalog.instances.find((a) => a.id === id);
    if (!item || seen.has(id) || id === "default") continue;
    seen.add(id);
    updated.push(item);
  }
  return updated;
}

export function desktopClipboardState({ ready, focused, visible, windowVisible, native }) {
  const connected = Boolean(ready && focused && native);
  return {
    connected,
    pollingVisible: Boolean(connected && visible && windowVisible),
  };
}

export function desktopMergeNoticeTarget(previousRedirect, catalog) {
  const nextRedirect = catalog?.redirects?.default || "";
  if (!nextRedirect || nextRedirect === previousRedirect) return null;
  return mergedDefaultInstance(catalog);
}
