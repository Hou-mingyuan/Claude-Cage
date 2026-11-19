// Store each turn separately so typing a draft doesn't rewrite earlier screenshots.
import type { LogAttachment } from "./log-archive";
export type Photo = { name: string; url: string };
export type Source = { title: string; url: string };
export type Draft = {
  title: string;
  body: string;
  ticket: string;
  issueNumber?: number;
};
export type SavedChat = {
  id: string;
  updated: number;
  title: string;
  summaryTitle?: string;
  customTitle?: string;
  draft: string;
  web: boolean;
  photo?: Photo;
  log?: LogAttachment;
};
export type SavedTurn = {
  id: string;
  chatId: string;
  created: number;
  question: string;
  photo?: Photo;
  log?: LogAttachment;
  web: boolean;
  answer?: string;
  observation?: string;
  sources?: Source[];
  issue?: Draft;
  webCount?: number;
  error?: string;
};

let db: IDBDatabase;
export function openHistory(): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open("cage-assistant-history", 1);
    request.onupgradeneeded = () => {
      const database = request.result;
      database.createObjectStore("chats", { keyPath: "id" });
      database
        .createObjectStore("turns", { keyPath: "id" })
        .createIndex("chat", "chatId");
    };
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      db = request.result;
      db.onversionchange = () => db.close();
      resolve();
    };
  });
}
function read<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}
export function listChats(): Promise<SavedChat[]> {
  return read(db.transaction("chats").objectStore("chats").getAll());
}
export function readChat(id: string): Promise<SavedChat | undefined> {
  return read(db.transaction("chats").objectStore("chats").get(id));
}
export function readTurn(id: string): Promise<SavedTurn | undefined> {
  return read(db.transaction("turns").objectStore("turns").get(id));
}
export function readTurns(id: string): Promise<SavedTurn[]> {
  return read(
    db.transaction("turns").objectStore("turns").index("chat").getAll(id),
  );
}
function committed(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onabort = () => reject(tx.error ?? Error("未能写入浏览器"));
  });
}

export async function saveChange(
  initial: SavedChat,
  create: boolean,
  patch: Partial<SavedChat>,
  turn?: SavedTurn,
) {
  const tx = db.transaction(
      turn ? ["chats", "turns"] : ["chats"],
      "readwrite",
      { durability: "strict" },
    ),
    store = tx.objectStore("chats");
  let saved = false;
  function finish() {
    if (turn) tx.objectStore("turns").put(turn);
    saved = true;
    tx.commit();
  }
  if (create) {
    // The new UUID is unique; enqueue all first writes before the page can close.
    store.add({ ...initial, ...patch });
    finish();
  } else {
    const current = store.get(initial.id);
    current.onsuccess = () => {
      // A late model response or another tab's old draft cannot revive a deleted chat.
      if (!current.result) return;
      store.put({ ...current.result, ...patch, updated: initial.updated });
      finish();
    };
  }
  await committed(tx);
  return saved;
}
export async function eraseChat(id: string) {
  const tx = db.transaction(["chats", "turns"], "readwrite");
  tx.objectStore("chats").delete(id);
  const cursor = tx.objectStore("turns").index("chat").openKeyCursor(id);
  cursor.onsuccess = () => {
    const row = cursor.result;
    if (row) {
      tx.objectStore("turns").delete(row.primaryKey);
      row.continue();
    }
  };
  await committed(tx);
}
