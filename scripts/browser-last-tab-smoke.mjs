import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import { createPrivacyController } from './browser-privacy-fixture.mjs';

// Headless Chromium never exits with its last window. With a display (as in
// the guest), this also covers the browser staying up after the last tab.
const fixture = await createPrivacyController({ headless: !process.env.DISPLAY });
const { api, cdp, evaluate, waitFor, origin } = fixture;
const events = [];
const stream = new AbortController();
let eventLoop;
try {
  const response = await fixture.states(stream.signal);
  assert.equal(response.status, 200);
  eventLoop = (async () => {
    const decoder = new TextDecoder();
    let buffer = '';
    for await (const chunk of response.body) {
      buffer += decoder.decode(chunk, { stream: true });
      let end;
      while ((end = buffer.indexOf('\n\n')) !== -1) {
        const frame = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (frame.startsWith('data: ')) events.push(JSON.parse(frame.slice(6)));
      }
    }
  })().catch(error => { if (!stream.signal.aborted) throw error; });
  const assertEmpty = async () => {
    // Chromium shuts down shortly after its last target is gone.
    await delay(1250);
    assert.equal((await api('/health')).ready, true);
    const state = await api('/tabs');
    assert.deepEqual(state.tabs, []);
    assert.equal(state.active, '');
    await waitFor(() => events.at(-1)?.tabs.length === 0, 'empty browser event');
    assert.equal(events.at(-1).active, '', 'empty snapshot must not reference a destroyed tab');
    await delay(1500);
    assert.deepEqual(await api('/health'), { ready: true }, 'the browser must outlive its last window');
  };
  await api('/close', { id: (await api('/tabs')).active });
  await assertEmpty();
  const reopened = await api('/new', { url: origin + '/reopened' });
  assert.equal((await api('/tabs')).tabs.length, 1);
  assert.equal((await api('/tabs')).active, reopened.id);

  await waitFor(() => evaluate(reopened.id, "location.pathname === '/reopened' && document.readyState === 'complete'"), 'reopened page');
  // Native Chromium closes must publish the same empty state as the HTTP action.
  // Chromium ignores a close during a new window's first navigation.
  await waitFor(async () => (await api('/tabs')).tabs[0]?.url.endsWith('/reopened'), 'reopened page');
  await cdp(reopened.id, 'Target.closeTarget', { targetId: reopened.id });
  await waitFor(async () => (await api('/tabs')).tabs.length === 0, 'external close');
  await assertEmpty();
  const privateTab = await api('/new', { url: origin + '/private', incognito: true });
  const context = (await cdp(privateTab.id, 'Target.getTargetInfo')).targetInfo.browserContextId;
  await waitFor(() => evaluate(privateTab.id, "location.pathname === '/private' && document.readyState === 'complete'"), 'private page');
  await api('/close', { id: privateTab.id });
  await assertEmpty();
  const contexts = await cdp('', 'Target.getBrowserContexts');
  assert.ok(!contexts.browserContextIds.includes(context), 'last private tab must dispose its context');
  const final = await api('/new', { url: origin + '/final' });
  assert.equal((await api('/tabs')).active, final.id);
  if (process.env.DISPLAY) {
    await waitFor(async () => (await cdp(final.id, 'Browser.getWindowForTarget', { targetId: final.id })).bounds.windowState === 'fullscreen',
      'a window opened after the last one closed is fullscreen');
  }
  console.log(JSON.stringify({ passed: true, lastTabClosed: true, emptyEvents: true,
    externalClose: true, privateContextDisposed: true, reopened: true }));
} finally {
  stream.abort();
  await eventLoop;
  await fixture.close();
}
