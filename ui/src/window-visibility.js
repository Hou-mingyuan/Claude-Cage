import { useSyncExternalStore } from "react";

const visible = () =>
  document.visibilityState !== "hidden" && window.cageWindowVisible !== false;

export const isWindowFocused = () => {
  if (!visible()) return false;
  if (typeof window.cageWindowFocused === "boolean") return window.cageWindowFocused;
  return document.hasFocus();
};

function subscribe(notify) {
  document.addEventListener("visibilitychange", notify);
  window.addEventListener("cage-window-visibility", notify);
  return () => {
    document.removeEventListener("visibilitychange", notify);
    window.removeEventListener("cage-window-visibility", notify);
  };
}

function subscribeFocus(notify) {
  document.addEventListener("visibilitychange", notify);
  window.addEventListener("cage-window-visibility", notify);
  window.addEventListener("cage-window-focus", notify);
  window.addEventListener("focus", notify);
  window.addEventListener("blur", notify);
  return () => {
    document.removeEventListener("visibilitychange", notify);
    window.removeEventListener("cage-window-visibility", notify);
    window.removeEventListener("cage-window-focus", notify);
    window.removeEventListener("focus", notify);
    window.removeEventListener("blur", notify);
  };
}

export function useWindowVisibility() {
  return useSyncExternalStore(subscribe, visible);
}

export function useWindowFocus() {
  return useSyncExternalStore(subscribeFocus, isWindowFocused);
}
