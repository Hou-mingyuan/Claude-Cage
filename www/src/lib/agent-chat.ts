import { pageProof } from "./page-proof.js";
import { marked } from "marked";
import DOMPurify from "dompurify";

const $ = <T extends HTMLElement = HTMLElement>(id: string) =>
  document.getElementById(id) as T;
const endpoint = "https://token-link.shop/cage-support/ask";
const form = $<HTMLFormElement>("ask"),
  input = $<HTMLTextAreaElement>("question"),
  file = $<HTMLInputElement>("file");
const thread = $("thread"),
  dialog = $<HTMLDialogElement>("document");
const guides: { path: string; title: string; sections: [string, string][] }[] =
  JSON.parse($("doc-map").textContent!);
import {
  openHistory,
  listChats,
  readChat,
  readTurn,
  readTurns,
  saveChange,
  eraseChat,
  type SavedChat,
  type SavedTurn,
  type Source,
  type Draft,
} from "./agent-history";
type Turn = SavedTurn & { nodes: HTMLElement[] };
type Chat = SavedChat & {
  turns: Turn[];
  loaded: boolean;
  saved: boolean;
  loadingPhoto: number;
  logReader?: Worker;
  busy: boolean;
  notice: string;
};
let persistent = false;

let channel: BroadcastChannel | undefined;
try {
  channel = new BroadcastChannel("cage-assistant-history");
} catch {
  /* Storage still works without live tab notifications. */
}
const selectionKey = "cage-assistant-selected";
const chats: Chat[] = [];
let active: Chat;
function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  cls = "",
  text = "",
) {
  const node = document.createElement(tag);
  node.className = cls;
  node.textContent = text;
  return node;
}
function btn(label: string, fn: () => void, cls = "") {
  const b = el("button", cls, label);
  b.type = "button";
  b.onclick = fn;
  return b;
}
function template(id: string) {
  return $<HTMLTemplateElement>(id).content.cloneNode(true);
}
function notice(text: string, chat = active) {
  chat.notice = text;
  if (chat === active) $("status").textContent = text;
}
function chatName(chat: Chat) {
  return (
    chat.customTitle ||
    chat.summaryTitle ||
    (chat.title !== "新对话"
      ? chat.title
      : chat.draft.trim().slice(0, 26) ||
        chat.photo?.name ||
        chat.log?.name ||
        chat.title)
  );
}
function sync() {
  $("welcome").hidden = active.turns.length > 0;
  $("shortcuts").hidden = $("welcome").hidden;
  document
    .querySelector(".chat")!
    .classList.toggle("has-turns", active.turns.length > 0);
  $("chat-title").textContent = chatName(active);
  $("status").textContent = active.notice;
  $("attachment").hidden = !active.photo;
  if (active.photo) {
    $<HTMLImageElement>("photo-preview").src = active.photo.url;
    $("photo-name").textContent = active.photo.name;
  } else {
    $<HTMLImageElement>("photo-preview").removeAttribute("src");
  }
  $("log-attachment").hidden = !active.log;
  if (active.log) {
    $("log-name").textContent = active.log.name;
    $("log-report").textContent = active.log.report;
    const field = $<HTMLTextAreaElement>("log-excerpt");
    if (field.value !== active.log.excerpt) field.value = active.log.excerpt;
  } else {
    $<HTMLDetailsElement>("log-details").open = false;
  }
  $("web").setAttribute("aria-pressed", String(active.web));
  $<HTMLButtonElement>("send").disabled =
    active.busy ||
    !!active.loadingPhoto ||
    !!active.logReader ||
    (!!active.log && !active.log.excerpt.trim()) ||
    !(input.value.trim() || active.photo || active.log);
  input.style.height = "auto";
  input.style.height = Math.min(140, input.scrollHeight) + "px";
}
function memoryChat(meta: SavedChat, saved: boolean): Chat {
  return {
    ...meta,
    turns: [],
    loaded: !saved,
    saved,
    loadingPhoto: 0,

    busy: false,
    notice: "",
  };
}
function storageError() {
  $("storage-warning").textContent =
    "浏览器未能保存最近的更改。请检查站点存储权限或可用空间。";
}
function metadata(c: Chat): SavedChat {
  return {
    id: c.id,
    updated: c.updated,
    title: c.title,
    summaryTitle: c.summaryTitle,
    customTitle: c.customTitle,
    draft: c.draft,
    web: c.web,
    photo: c.photo,
    log: c.log,
  };
}
function persist(chat: Chat, patch: Partial<SavedChat>, turn?: Turn) {
  if (!persistent || !chats.includes(chat)) return;
  const create = !chat.saved;
  chat.saved = true;
  chat.updated = Date.now();
  const data = turn ? (({ nodes, ...record }) => record)(turn) : undefined;
  void saveChange(metadata(chat), create, patch, data)
    .then((saved) => {
      if (saved) channel?.postMessage({ id: chat.id, turnId: turn?.id });
    })
    .catch(() => {
      if (create) chat.saved = false;
      storageError();
    });
}
function historyNav() {
  const rows = [...chats].sort((a, b) => b.updated - a.updated);
  $("history").replaceChildren(
    ...rows.map((chat) => {
      const name = chatName(chat);
      const row = el("div", "history-row");
      const button = btn("", () => void select(chat), "history-item");
      button.setAttribute("aria-current", String(chat === active));
      button.append(template("history-icon"), el("span", "", name));
      row.append(button);
      if (
        chat.saved ||
        chat.turns.length ||
        chat.draft ||
        chat.photo ||
        chat.log
      ) {
        const rename = btn(
          "",
          () => openRename(chat),
          "history-action history-rename",
        );
        rename.dataset.chatId = chat.id;
        rename.dataset.tip = "重命名";
        rename.setAttribute("aria-label", "重命名对话：" + name);
        rename.append(template("rename-icon"));
        row.append(rename);
        const remove = btn(
          "",
          () => confirmRemoval(chat, name),
          "history-action history-delete",
        );
        remove.setAttribute("aria-label", "删除对话：" + name);
        remove.dataset.tip = "删除对话";
        remove.append(template("delete-icon"));
        row.append(remove);
      }
      return row;
    }),
  );
}
function dropChat(chat: Chat) {
  const at = chats.indexOf(chat);
  if (at < 0) return;
  if (renameTarget === chat && renameDialog.open) renameDialog.close();
  chats.splice(at, 1);
  chat.loadingPhoto = 0;
  chat.logReader?.terminate();
  chat.logReader = undefined;
  if (active === chat) {
    if (chats.length)
      void select([...chats].sort((a, b) => b.updated - a.updated)[0]);
    else createBlank();
  } else historyNav();
}
const renameDialog = $<HTMLDialogElement>("rename-chat");
const renameInput = $<HTMLInputElement>("new-title");
let renameTarget: Chat | undefined;
function openRename(chat: Chat) {
  renameTarget = chat;
  renameInput.value = chatName(chat);
  renameInput.setCustomValidity("");
  renameDialog.showModal();
  renameInput.focus();
  renameInput.select();
}
$("cancel-rename").onclick = () => renameDialog.close();
renameInput.oninput = () => renameInput.setCustomValidity("");
$<HTMLFormElement>("rename-form").onsubmit = (e) => {
  e.preventDefault();
  const chat = renameTarget;
  if (!chat || !chats.includes(chat)) {
    renameDialog.close();
    return;
  }
  const title = renameInput.value.trim();
  if (!title) {
    renameInput.setCustomValidity("请输入对话标题。");
    renameInput.reportValidity();
    return;
  }
  chat.customTitle = title;
  persist(chat, { customTitle: title });
  renameDialog.close();
  historyNav();
  if (active === chat) sync();
  const row = Array.from(
    $("history").querySelectorAll<HTMLButtonElement>(".history-rename"),
  ).find((button) => button.dataset.chatId === chat.id);
  row?.focus();
};
const deleteDialog = $<HTMLDialogElement>("delete-chat");
let deleteTarget: Chat | undefined;
function confirmRemoval(chat: Chat, name: string) {
  deleteTarget = chat;
  $("delete-name").textContent =
    "「" + name + "」的消息、截图和草稿将从此浏览器删除。";
  deleteDialog.showModal();
  $("cancel-delete").focus();
}
$("cancel-delete").onclick = () => deleteDialog.close();
$("confirm-delete").onclick = async () => {
  const target = deleteTarget;
  if (!target) return;
  try {
    if (persistent) await eraseChat(target.id);
    channel?.postMessage({ id: target.id, deleted: true });
    deleteDialog.close();
    dropChat(target);
  } catch {
    deleteDialog.close();
    storageError();
  }
};
function rail(open: boolean) {
  $("workspace").classList.toggle("rail-open", open);
  $("backdrop").hidden = !open;
  $("menu").setAttribute("aria-expanded", String(open));
  if (open) $("new-chat").focus();
}
async function select(chat: Chat) {
  active = chat;
  try {
    localStorage.setItem(selectionKey, chat.id);
    sessionStorage.setItem(selectionKey, chat.id);
  } catch {}
  if (!chat.loaded) {
    input.disabled = true;
    thread.replaceChildren();
    try {
      await restoreTurns(chat);
    } catch {
      storageError();
    }
    if (active !== chat) return;
  }
  input.disabled = false;
  input.value = chat.draft;
  thread.replaceChildren(...chat.turns.flatMap((t) => t.nodes));
  rail(false);
  sync();
  historyNav();
  thread.scrollTop = thread.scrollHeight;
}
function newChat() {
  if (
    active &&
    !active.turns.length &&
    !active.photo &&
    !active.log &&
    !input.value.trim()
  ) {
    rail(false);
    input.focus();
    return;
  }
  const blank = chats.find(
    (c) =>
      !c.saved &&
      !c.turns.length &&
      !c.draft &&
      !c.photo &&
      !c.log &&
      !c.loadingPhoto &&
      !c.logReader,
  );
  if (blank) void select(blank);
  else createBlank();
}
function createBlank() {
  const chat = memoryChat(
    {
      id: crypto.randomUUID(),
      updated: Date.now(),
      title: "新对话",
      draft: "",
      web: false,
    },
    false,
  );
  chats.unshift(chat);
  select(chat);
}
$("new-chat").onclick = () => {
  newChat();
  input.focus();
};
$("menu").onclick = () => rail(!$("workspace").classList.contains("rail-open"));
$("backdrop").onclick = () => {
  rail(false);
  $("menu").focus();
};
document.addEventListener("keydown", (e) => {
  if (
    e.key === "Escape" &&
    !deleteDialog.open &&
    !renameDialog.open &&
    !dialog.open &&
    $("workspace").classList.contains("rail-open")
  ) {
    rail(false);
    $("menu").focus();
  }
});
input.oninput = () => {
  active.draft = input.value;
  persist(active, { draft: active.draft });
  historyNav();
  sync();
};
input.onkeydown = (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    if (!$<HTMLButtonElement>("send").disabled) form.requestSubmit();
  }
};
$("web").onclick = () => {
  active.web = !active.web;
  if (active.saved) persist(active, { web: active.web });
  sync();
};
$("attach").onclick = $("diagnose").onclick = () => file.click();
$("remove-photo").onclick = () => {
  active.loadingPhoto = 0;
  active.photo = undefined;
  persist(active, { photo: undefined });
  historyNav();
  sync();
  input.focus();
};
const zipInput = $<HTMLInputElement>("log-file");
$("attach-log").onclick = () => zipInput.click();
zipInput.onchange = () => {
  const f = zipInput.files?.[0];
  if (f) void attachLog(f);
  zipInput.value = "";
};
$("remove-log").onclick = () => {
  active.logReader?.terminate();
  active.logReader = undefined;
  active.log = undefined;
  persist(active, { log: undefined });
  notice("");
  sync();
  historyNav();
  input.focus();
};
$<HTMLTextAreaElement>("log-excerpt").oninput = (e) => {
  if (!active.log) return;
  active.log = {
    ...active.log,
    excerpt: (e.currentTarget as HTMLTextAreaElement).value,
  };
  persist(active, { log: active.log });
  sync();
};
function attachLog(f: File) {
  const chat = active;
  chat.logReader?.terminate();
  let worker: Worker;
  try {
    worker = new Worker(new URL("./log-archive.ts", import.meta.url), {
      type: "module",
    });
  } catch {
    chat.logReader = undefined;
    notice("当前浏览器无法读取 ZIP 日志。");
    sync();
    return;
  }
  chat.logReader = worker;
  notice("正在本机读取 ZIP 日志…", chat);
  sync();
  function done(error?: string, log?: import("./log-archive").LogAttachment) {
    worker.terminate();
    if (chat.logReader !== worker) return;
    chat.logReader = undefined;
    if (log) {
      chat.log = log;
      persist(chat, { log });
      historyNav();
      notice("只发送选取的日志文字，可展开检查后再发送。", chat);
    } else notice(error || "ZIP 无法读取，请重新导出。", chat);
    if (active === chat) {
      sync();
      input.focus();
    }
  }
  worker.onmessage = (e) => done(e.data.error, e.data.log);
  worker.onerror = () => done("ZIP 无法读取，请重新导出或缩小文件。");
  worker.postMessage(f);
}
let photoJob = 0;
async function attach(f: File) {
  if (
    /\.zip$/i.test(f.name) ||
    ["application/zip", "application/x-zip-compressed"].includes(f.type)
  )
    return attachLog(f);
  const chat = active,
    job = ++photoJob;
  chat.loadingPhoto = job;
  notice("正在准备图片…");
  sync();
  const url = URL.createObjectURL(f);
  try {
    if (
      !["image/jpeg", "image/png", "image/webp"].includes(f.type) ||
      f.size > 12 * 1024 * 1024
    )
      throw Error("请选择 12 MB 以内的 PNG、JPEG 或 WebP 图片。");
    const im = new Image();
    im.src = url;
    await im.decode();
    if (im.naturalWidth * im.naturalHeight > 40000000)
      throw Error("图片像素过大，请裁剪后添加。");
    const scale = Math.min(
      1,
      1600 / Math.max(im.naturalWidth, im.naturalHeight),
    );
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(im.naturalWidth * scale));
    canvas.height = Math.max(1, Math.round(im.naturalHeight * scale));
    const ctx = canvas.getContext("2d")!;
    ctx.fillStyle = "#fff";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(im, 0, 0, canvas.width, canvas.height);
    let data = canvas.toDataURL("image/jpeg", 0.88);
    if (data.length > 1950000) data = canvas.toDataURL("image/jpeg", 0.68);
    if (data.length > 1950000)
      throw Error("图片细节过多，请裁剪到需要查看的区域。");
    if (chat.loadingPhoto === job) {
      chat.photo = { name: f.name || "粘贴的截图", url: data };
      persist(chat, { photo: chat.photo });
      historyNav();
      notice("", chat);
    }
  } catch (e) {
    if (chat.loadingPhoto === job)
      notice(
        e instanceof Error && e.name === "Error"
          ? e.message
          : "图片无法打开，请换一张再试。",
        chat,
      );
  } finally {
    URL.revokeObjectURL(url);
    if (chat.loadingPhoto === job) chat.loadingPhoto = 0;
    if (chat === active) {
      sync();
      input.focus();
    }
  }
}
file.onchange = () => {
  const f = file.files?.[0];
  if (f) void attach(f);
  file.value = "";
};
form.addEventListener("paste", (e) => {
  const f = Array.from(e.clipboardData?.files ?? [])[0];
  if (f) {
    e.preventDefault();
    void attach(f);
  }
});
for (const name of ["dragover", "drop"])
  form.addEventListener(name, (e) => {
    const d = (e as DragEvent).dataTransfer;
    if (!d?.types.includes("Files")) return;
    e.preventDefault();
    form.classList.toggle("dragging", name === "dragover");
    if (name === "drop" && d.files[0]) void attach(d.files[0]);
  });
form.ondragleave = () => form.classList.remove("dragging");
$("find-doc").onclick = () => {
  input.value = "本机的项目文件夹要怎么添加？带我找到手册里的对应章节。";
  sync();
  form.requestSubmit();
};
$("report").onclick = () => {
  input.value = "帮我整理一个 GitHub Issue。";
  sync();
  form.requestSubmit();
};

let previewId = 0;
$("close-doc").onclick = () => dialog.close();
dialog.onclick = (e) => {
  if (e.target === dialog && e.clientX < dialog.getBoundingClientRect().left)
    dialog.close();
};
async function openDoc(url: URL, title: string, chapter?: string) {
  const id = ++previewId;
  const body = $("doc-text");
  body.replaceChildren(
    el("h2", "", chapter || title),
    el("p", "", "正在读取文档…"),
  );
  $<HTMLAnchorElement>("doc-link").href = url.href;
  if (!dialog.open) dialog.showModal();
  try {
    const res = await fetch(url.pathname);
    if (!res.ok) throw Error();
    const page = new DOMParser().parseFromString(await res.text(), "text/html");
    const content = page.querySelector(".guide-prose");
    if (!content) throw Error();
    const heading = url.hash
      ? page.getElementById(decodeURIComponent(url.hash.slice(1)))
      : null;
    let section = heading?.nextElementSibling ?? content.firstElementChild;
    let excerpts: string[] = [];
    while (section && excerpts.length < 5) {
      if (section.matches("h2") && excerpts.length) break;
      if (!section.matches("figure,img,script") && section.textContent?.trim())
        excerpts.push(section.textContent.trim().slice(0, 650));
      section = section.nextElementSibling;
    }
    if (!excerpts.length) throw Error();
    if (id === previewId)
      body.replaceChildren(
        el("h2", "", chapter || title),
        ...excerpts.map((t) => el("p", "", t)),
      );
  } catch {
    if (id === previewId)
      body.replaceChildren(
        el("h2", "", chapter || title),
        el("p", "", "预览暂时无法加载，可直接打开完整文档。"),
      );
  }
}
function citation(src: Source, id: number, inline: boolean): HTMLElement {
  const prefix = "[" + id + "]",
    cls = inline ? "cite" : "source";
  if (!src.url)
    return el("span", cls, inline ? prefix : prefix + " " + src.title);
  let url: URL;
  try {
    url = new URL(src.url);
  } catch {
    return el("span", cls, prefix);
  }
  if (url.protocol !== "https:") return el("span", cls, prefix);
  const guide =
    url.hostname === "claude-cage.com" &&
    guides.find((g) => g.path === url.pathname);
  if (guide) {
    const section = guide.sections.find((h) => h[0] === src.title);
    if (section) url.hash = section[1];
    const label = section ? guide.title + " · " + section[0] : guide.title;
    const b = btn(
      inline ? prefix : prefix + " " + label,
      () => void openDoc(url, guide.title, section?.[0]),
      cls,
    );
    b.setAttribute("aria-label", "预览文档：" + label);
    return b;
  }
  const link = el("a", cls, inline ? prefix : prefix + " " + src.title);
  link.href = url.href;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  return link;
}
function richAnswer(text: string, refs: Source[]) {
  const block = el("div", "answer");
  block.append(
    DOMPurify.sanitize(marked.parse(text, { async: false }), {
      ALLOWED_TAGS: [
        "p",
        "br",
        "strong",
        "em",
        "code",
        "pre",
        "ul",
        "ol",
        "li",
        "h1",
        "h2",
        "h3",
        "h4",
        "blockquote",
        "table",
        "thead",
        "tbody",
        "th",
        "tr",
        "td",
      ],
      ALLOWED_ATTR: [],
      RETURN_DOM_FRAGMENT: true,
    }),
  );
  const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
  const texts: Text[] = [];
  while (walker.nextNode()) texts.push(walker.currentNode as Text);
  const used = new Set<number>();
  for (const node of texts) {
    if (node.parentElement?.closest("pre,code")) continue;
    const value = node.data,
      frag = document.createDocumentFragment();
    let pos = 0;
    for (const m of value.matchAll(/\[(\d+)\]/g)) {
      const id = Number(m[1]);
      if (!refs[id - 1]) continue;
      frag.append(value.slice(pos, m.index), citation(refs[id - 1], id, true));
      pos = m.index! + m[0].length;
      used.add(id);
    }
    if (pos) {
      frag.append(value.slice(pos));
      node.replaceWith(frag);
    }
  }
  const sources = el("div", "sources");
  for (const id of used) sources.append(citation(refs[id - 1], id, false));
  return [block, sources];
}
function draftCard(draft: Draft, changed: () => void) {
  const card = el("section", "issue");
  function receipt() {
    const link = el("a", "", "查看 Issue #" + draft.issueNumber + " ↗");
    link.href =
      "https://github.com/Lee-xeo/Claude-Cage/issues/" + draft.issueNumber;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    card.replaceChildren(
      el("h3", "", "Issue 已提交"),
      el("p", "", draft.title),
      link,
    );
  }
  if (
    draft.issueNumber &&
    Number.isSafeInteger(draft.issueNumber) &&
    draft.issueNumber > 0
  ) {
    receipt();
    return card;
  }
  card.append(el("h3", "", "GitHub Issue 草稿"));
  const title = el("input");
  title.value = draft.title;
  title.maxLength = 160;
  const body = el("textarea");
  body.value = draft.body;
  body.maxLength = 6500;
  title.oninput = body.oninput = () => {
    draft.title = title.value;
    draft.body = body.value;
    changed();
  };
  for (const [name, field] of [
    ["标题", title],
    ["正文", body],
  ] as const) {
    const label = el("label", "", name);
    label.append(field);
    card.append(label);
  }
  const msg = el(
    "p",
    "",
    "确认后，项目机器人会将以上文字公开发布到 Lee-xeo/Claude-Cage。截图不会随草稿上传。",
  );
  const publish = btn("确认并公开提交", async () => {
    if (!title.value.trim() || !body.value.trim()) {
      msg.textContent = "请填写标题和正文。";
      return;
    }
    publish.disabled = true;
    title.readOnly = body.readOnly = true;
    msg.textContent = "正在提交到 GitHub…";
    try {
      const res = await fetch(endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          action: "publish_issue",
          proof: await pageProof(endpoint, "cage_issue"),
          ticket: draft.ticket,
          title: title.value,
          body: body.value,
        }),
        signal: AbortSignal.timeout(40000),
      });
      const data = await res.json();
      if (!res.ok) throw Error(data.error || "提交失败，请重试。");
      if (
        !/^https:\/\/github\.com\/Lee-xeo\/Claude-Cage\/issues\/\d+$/.test(
          data.url,
        )
      )
        throw Error("未取得 Issue 链接，请重试。");
      draft.issueNumber = Number(data.number);
      changed();
      receipt();
    } catch (e) {
      publish.disabled = false;
      title.readOnly = body.readOnly = false;
      msg.textContent = e instanceof Error ? e.message : "提交失败，请重试。";
    }
  });
  card.append(msg, publish);
  return card;
}
function speaker() {
  const s = el("div", "speaker");
  s.append(template("bot-avatar"), el("span", "", "Cage 助手"));
  return s;
}
function sendMessage(question: string) {
  if (
    active.busy ||
    active.loadingPhoto ||
    active.logReader ||
    (active.log && !active.log.excerpt.trim())
  )
    return;
  const chat = active,
    photo = chat.photo,
    log = chat.log;
  question = question.trim();
  if (!question && !photo && !log) return;
  const turn = makeTurn({
    id: crypto.randomUUID(),
    chatId: chat.id,
    created: Date.now(),
    question:
      question ||
      (log
        ? "请分析这份诊断日志，告诉我发生了什么以及下一步怎么做。"
        : "请帮我看看这张图片。"),
    log,
    photo,
    web: chat.web,
  });
  chat.turns.push(turn);
  if (chat.turns.length === 1) chat.title = turn.question.slice(0, 26);
  chat.photo = undefined;
  chat.log = undefined;
  chat.draft = input.value = "";
  persist(
    chat,
    {
      title: chat.title,
      draft: "",
      photo: undefined,
      log: undefined,
      web: chat.web,
    },
    turn,
  );
  thread.append(...turn.nodes);
  historyNav();
  void run(chat, turn);
}
function makeTurn(data: SavedTurn): Turn {
  const u = el("div", "user-turn");
  if (data.photo) {
    const im = el("img", "user-photo");
    im.alt = "你发送的图片";
    im.src = data.photo.url;
    u.append(im);
  }
  if (data.log) {
    const details = el("details", "sent-log");
    details.append(
      el("summary", "", "日志 ZIP · " + data.log.name),
      el("p", "", data.log.report),
      el("pre", "", data.log.excerpt),
    );
    u.append(details);
  }
  u.append(el("div", "bubble", data.question));
  return { ...data, nodes: [u, el("div", "bot-turn")] };
}
function showAnswer(chat: Chat, turn: Turn) {
  const a = turn.nodes[1];
  a.replaceChildren(speaker(), ...richAnswer(turn.answer!, turn.sources || []));
  if (turn.issue?.ticket)
    a.append(draftCard(turn.issue, () => persist(chat, {}, turn)));
  if (turn.web && !turn.webCount)
    a.append(el("p", "progress-note", "本次联网查询未取得资料。"));
  const actions = el("div", "turn-tools");
  const copy = btn("复制回答", async () => {
    try {
      await navigator.clipboard.writeText(turn.answer!);
      copy.textContent = "已复制";
    } catch {
      notice("请选中回答文字后复制。", chat);
    }
  });
  actions.append(copy);
  if (!turn.issue)
    actions.append(
      btn("整理为 Issue", () => {
        if (!chat.busy)
          sendMessage(
            "请把刚才关于“" +
              turn.question.slice(0, 150) +
              "”的问题和已经确认的信息整理成 GitHub Issue 草稿。",
          );
      }),
    );
  a.append(actions);
}
function showRetry(chat: Chat, turn: Turn) {
  const a = turn.nodes[1];
  const retry = btn(
    "重试",
    () => {
      if (!chat.busy) void run(chat, turn);
    },
    "retry",
  );
  a.replaceChildren(
    speaker(),
    el("p", "", turn.error || "上次回答尚未完成，可点重试继续。"),
    retry,
  );
}
async function restoreTurns(chat: Chat) {
  const records = await readTurns(chat.id);
  chat.turns = records
    .sort((a, b) => a.created - b.created || a.id.localeCompare(b.id))
    .map((record) => {
      const turn = makeTurn(record);
      if (turn.answer) showAnswer(chat, turn);
      else showRetry(chat, turn);
      return turn;
    });
  chat.loaded = true;
}
async function run(chat: Chat, turn: Turn) {
  chat.busy = true;
  const a = turn.nodes[1];
  a.replaceChildren(
    speaker(),
    el(
      "p",
      "progress",
      turn.log
        ? "正在分析日志并查阅资料…"
        : turn.photo
          ? "正在看图并查阅资料…"
          : "正在查阅资料…",
    ),
  );
  notice("", chat);
  if (active === chat) {
    sync();
    thread.scrollTop = thread.scrollHeight;
  }
  const history = chat.turns
    .slice(0, chat.turns.indexOf(turn))
    .filter((t) => t.answer)
    .slice(-6)
    .flatMap((t) => [
      {
        role: "user",
        content: Array.from(
          t.question +
            (t.log ? "\n此前日志节选：" + t.log.excerpt.slice(0, 1200) : "") +
            (t.observation ? "\n该轮截图：" + t.observation : ""),
        )
          .slice(0, 1800)
          .join(""),
      },
      {
        role: "assistant",
        content: Array.from(t.answer!).slice(0, 1800).join(""),
      },
    ]);
  try {
    const suggestTitle = !chat.customTitle && !chat.summaryTitle;
    const res = await fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        proof: await pageProof(endpoint, "cage_ask"),
        suggestTitle,
        question: turn.question,
        history,
        collect: true,
        web: turn.web,
        photo: turn.photo?.url,
        logExcerpt: turn.log?.excerpt,
      }),
      signal: AbortSignal.timeout(85000),
    });
    const data = await res.json();
    if (!res.ok || typeof data.answer !== "string")
      throw Error(data.error || "暂时没有收到回答。");
    turn.answer = data.answer;
    turn.observation = data.observation;
    turn.sources = data.sources || [];
    turn.issue = data.issue;
    turn.webCount = data.webCount;
    turn.error = undefined;
    showAnswer(chat, turn);
    const suggestion =
      suggestTitle &&
      !chat.customTitle &&
      !chat.summaryTitle &&
      typeof data.chatTitle === "string"
        ? data.chatTitle.trim()
        : "";
    if (suggestion) {
      chat.summaryTitle = suggestion;
      historyNav();
    }
    persist(chat, suggestion ? { summaryTitle: suggestion } : {}, turn);
  } catch (e) {
    turn.error =
      e instanceof Error && !/fetch|timeout|JSON|load failed/i.test(e.message)
        ? e.message
        : "连接暂时失败，消息仍在这里。";
    showRetry(chat, turn);
    persist(chat, {}, turn);
  } finally {
    chat.busy = false;
    if (active === chat) {
      sync();
      if (a.offsetHeight < thread.clientHeight * 0.7)
        thread.scrollTop = thread.scrollHeight;
    }
  }
}
form.onsubmit = (e) => {
  e.preventDefault();
  sendMessage(input.value);
};

async function initialize() {
  input.disabled = true;
  $<HTMLButtonElement>("new-chat").disabled = true;
  form.inert = true;
  $("shortcuts").inert = true;
  let selected = "";
  try {
    await openHistory();
    const records = await listChats();
    records.sort((a, b) => b.updated - a.updated);
    chats.push(...records.map((r) => memoryChat(r, true)));
    persistent = true;
    try {
      selected =
        sessionStorage.getItem(selectionKey) ||
        localStorage.getItem(selectionKey) ||
        "";
    } catch {}
  } catch {
    storageError();
  }
  if (chats.length)
    await select(chats.find((c) => c.id === selected) ?? chats[0]);
  else createBlank();
  input.disabled = false;
  $<HTMLButtonElement>("new-chat").disabled = false;
  form.inert = false;
  $("shortcuts").inert = false;
}
if (channel)
  channel.onmessage = async ({ data }) => {
    if (!persistent || typeof data?.id !== "string") return;
    try {
      const record = await readChat(data.id);
      let chat = chats.find((c) => c.id === data.id);
      if (!record) {
        if (chat) dropChat(chat);
        return;
      }
      if (!chat) {
        chat = memoryChat(record, true);
        chats.push(chat);
      } else {
        Object.assign(chat, record);
        if (data.turnId && chat.loaded) {
          const saved = await readTurn(data.turnId);
          if (saved && chats.includes(chat)) {
            const existing = chat.turns.find((t) => t.id === saved.id);
            if (existing) {
              Object.assign(existing, saved);
              if (existing.answer) showAnswer(chat, existing);
              else showRetry(chat, existing);
            } else {
              const turn = makeTurn(saved);
              if (turn.answer) showAnswer(chat, turn);
              else showRetry(chat, turn);
              chat.turns.push(turn);
              chat.turns.sort(
                (a, b) => a.created - b.created || a.id.localeCompare(b.id),
              );
            }
            if (chat === active)
              thread.replaceChildren(...chat.turns.flatMap((t) => t.nodes));
          }
        }
        if (chat === active) {
          if (input.value !== chat.draft) input.value = chat.draft;
          sync();
        }
      }
      historyNav();
    } catch {
      storageError();
    }
  };
void initialize();
