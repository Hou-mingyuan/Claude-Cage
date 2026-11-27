export async function commitDesktopText(request, state, text, valid) {
  if (!state.available || !state.context || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
  const session = crypto.randomUUID().replaceAll("-", "");
  const begun = await request({ mode: "begin", context: state.context, session, sequence: 0 });
  if (!begun.available || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
  const result = await request({ mode: "commit", session, sequence: 1, text });
  if (!result.available) throw new Error("文字输入未完成，请重新点击输入框");
}

// Default Desktop uses the workbench input endpoint. Older guest sessions
// expose no editable context and rely on VNC paste for committed text.
export function desktopTextInput(client, request, valid, sharedClipboard) {
  let seenContext = false;
  const status = async (session) => {
    const state = await request({ mode: "status", session });
    if (state.available && state.context) seenContext = true;
    return state;
  };
  return {
    status,
    insert(text, { committed = false } = {}) {
      if (!committed && !sharedClipboard()) return client.cagePasteText(text);
      return client.cageRunInput(async (_sendKey, paste) => {
        const state = await status();
        if (committed && !state.available && !seenContext && !sharedClipboard() && valid(committed)) return paste(text);
        return commitDesktopText(request, state, text, () => valid(committed));
      });
    },
  };
}
