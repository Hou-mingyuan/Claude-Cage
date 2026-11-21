import assert from 'node:assert/strict';
import test from 'node:test';
import { browserCookies } from './browser-cookies.mjs';
import { BrowserVault } from './browser-vault.mjs';

const tab = { targetId: 'tab', url: 'https://sub.example.test/page', browserContextId: 'private-context', incognito: true };
const cookie = (domain, extra = {}) => ({ name: 'session', value: 'fixture-secret', domain, path: '/', expires: -1, session: true, ...extra });

function storage(initial) {
  let cookies = initial;
  const deletions = [];
  const cdp = async (method, params, session) => {
    if (method === 'Storage.getCookies') {
      assert.deepEqual(params, { browserContextId: 'private-context' });
      return { cookies };
    }
    assert.equal(method, 'Network.deleteCookies');
    assert.equal(session, 'sid');
    deletions.push(params);
    cookies = cookies.filter(item => !(item.name === params.name && item.domain === params.domain && item.path === params.path &&
      JSON.stringify(item.partitionKey) === JSON.stringify(params.partitionKey)));
  };
  return { cdp, deletions, run: input => browserCookies(cdp, async () => 'sid', tab, { scope: 'all', ...input }) };
}

test('all search results expose selectable keys across pages and batch deletion preserves unmatched cookies', async () => {
  const f = storage([
    ...Array.from({ length: 57 }, (_, i) => cookie('sub.example.test', { name: `batch-${i}` })),
    cookie('sub.example.test', { name: 'unmatched' }),
    cookie('other.test', { name: 'batch-other-site' }),
    cookie('sub.example.test', { name: 'batch-opaque', partitionKeyOpaque: true }),
  ]);
  const result = await f.run({ mode: 'list', scope: 'site', query: 'BATCH-', page: 1 });
  assert.equal(result.total, 58);
  assert.equal(result.cookies.length, 8);
  assert.equal(result.selectableKeys.length, 57);
  assert.equal(new Set(result.selectableKeys).size, 57);
  const after = await f.run({ mode: 'delete-selected', scope: 'site', query: 'BATCH-', page: 1, cookieKeys: result.selectableKeys });
  assert.equal(f.deletions.length, 57);
  assert.equal(after.total, 1);
  assert.equal(after.page, 0);
  assert.deepEqual(after.selectableKeys, []);
  assert.deepEqual((await f.run({ mode: 'list' })).cookies.map(item => item.name).sort(), ['batch-opaque', 'batch-other-site', 'unmatched']);
});

test('batch selection keeps exact paths and partition keys and deduplicates requested keys', async () => {
  const partitionKey = { topLevelSite: 'https://example.test', hasCrossSiteAncestor: true };
  const f = storage([cookie('sub.example.test'), cookie('sub.example.test', { path: '/nested' }), cookie('sub.example.test', { partitionKey })]);
  const { cookies } = await f.run({ mode: 'list' });
  const selected = cookies.filter(item => item.path === '/nested' || item.partitionKey).map(item => item.key);
  const after = await f.run({ mode: 'delete-selected', cookieKeys: [...selected, selected[0]] });
  assert.equal(after.total, 1);
  assert.equal(after.cookies[0].path, '/');
  assert.equal(after.cookies[0].partitionKey, undefined);
  assert.equal(f.deletions.length, 2);
  assert.ok(f.deletions.some(item => item.path === '/nested'));
  assert.ok(f.deletions.some(item => item.partitionKey === partitionKey));
});

test('invalid, stale, out-of-scope or opaque batches fail before deleting any selected cookies', async () => {
  const f = storage([cookie('sub.example.test'), cookie('other.test'), cookie('sub.example.test', { name: 'opaque', partitionKeyOpaque: true })]);
  const { cookies } = await f.run({ mode: 'list' });
  const valid = cookies.find(item => item.name === 'session' && item.domain === 'sub.example.test').key;
  const other = cookies.find(item => item.domain === 'other.test').key;
  const opaque = cookies.find(item => item.partitionKeyOpaque).key;
  for (const cookieKeys of [undefined, [], 'not-an-array', [null], ['bad'], [valid, '0'.repeat(64)], [valid, opaque]]) {
    await assert.rejects(f.run({ mode: 'delete-selected', cookieKeys }));
  }
  await assert.rejects(f.run({ mode: 'delete-selected', scope: 'site', cookieKeys: [valid, other] }), /已变化/);
  await assert.rejects(f.run({ mode: 'delete-selected', query: 'opaque', cookieKeys: [opaque, valid] }), /已变化/);
  await assert.rejects(f.run({ mode: 'delete-selected', url: 'https://previous.test/', cookieKeys: [valid] }), /网页已切换/);
  assert.deepEqual(f.deletions, []);
});

test('a transport failure during a batch reports partial deletion and requests a refresh', async () => {
  const f = storage([cookie('sub.example.test', { name: 'first' }), cookie('sub.example.test', { name: 'second' })]);
  const result = await f.run({ mode: 'list' });
  let attempts = 0;
  const cdp = async (...args) => {
    if (args[0] === 'Network.deleteCookies' && ++attempts === 2) throw new Error('connection closed');
    return f.cdp(...args);
  };
  await assert.rejects(browserCookies(cdp, async () => 'sid', tab, {
    mode: 'delete-selected', scope: 'all', cookieKeys: result.selectableKeys,
  }), /部分 Cookie 可能已删除.*刷新列表/);
  assert.equal(f.deletions.length, 1);
  assert.equal((await f.run({ mode: 'list' })).total, 1);
});

test('site scope includes parent domain cookies and excludes host-only parent and lookalikes', async () => {
  const calls = [];
  const cdp = async (method, params) => {
    calls.push({ method, params });
    return { cookies: [cookie('.example.test'), cookie('example.test'), cookie('sub.example.test'), cookie('.notexample.test'), cookie('other.example.test')] };
  };
  const result = await browserCookies(cdp, async () => 'sid', tab, { mode: 'list', scope: 'site' });
  assert.deepEqual(result.cookies.map(cookie => cookie.domain), ['.example.test', 'sub.example.test']);
  assert.equal(result.incognito, true);
  assert.ok(result.cookies.every(cookie => !('value' in cookie)));
  assert.deepEqual(calls, [{ method: 'Storage.getCookies', params: { browserContextId: 'private-context' } }]);
});

test('opaque partitions refuse site-wide and individual deletion before any mutation', async () => {
  const calls = [];
  const cdp = async method => { calls.push(method); return { cookies: [cookie('sub.example.test'), cookie('sub.example.test', { partitionKeyOpaque: true })] }; };
  const data = await browserCookies(cdp, async () => 'sid', tab, { mode: 'list', scope: 'site' });
  await assert.rejects(browserCookies(cdp, async () => 'sid', tab, { mode: 'delete', scope: 'site', cookieKey: data.cookies.find(cookie => cookie.partitionKeyOpaque).key }), /分区/);
  await assert.rejects(browserCookies(cdp, async () => 'sid', tab, { mode: 'clear', scope: 'site' }), /分区/);
  assert.ok(calls.every(method => method === 'Storage.getCookies'));
});

test('stale page or authorization tabs cannot reach cookie storage', async () => {
  const cdp = () => { throw new Error('must not contact cookie storage'); };
  await assert.rejects(browserCookies(cdp, null, tab, { mode: 'clear', scope: 'all', url: 'https://previous.test/' }), /网页已切换/);
  await assert.rejects(browserCookies(cdp, null, { ...tab, login: true }, { mode: 'list', scope: 'all' }), /不可管理/);
});

test('an omitted page address forwarded as empty still lists cookies', async () => {
  const cdp = async () => ({ cookies: [cookie('sub.example.test')] });
  const data = await browserCookies(cdp, null, tab, { url: '', mode: 'list', scope: 'all', query: '', page: 0 });
  assert.equal(data.cookies.length, 1);
});

test('vault never attaches credential observers to private or authorization tabs', async () => {
  for (const mode of ['incognito', 'login']) {
    const vault = new BrowserVault({ cdp: () => { throw new Error('must not install credential observer'); }, active: () => 'tab', tab: () => ({ [mode]: true }) });
    await vault.attach('tab', 'sid');
    assert.equal(vault.sessions.size, 0);
    await vault.request({ action: 'status', lease: 'f'.repeat(64), tab: 'tab' });
    await assert.rejects(vault.request({ action: 'fill', lease: 'f'.repeat(64), tab: 'tab' }), /网页已切换/);
  }
});

// A cookie store that follows Chromium's CDP conventions: a bare domain is
// host-only, a leading dot is a domain cookie, and a domain on the public
// suffix list is silently stored host-only on that suffix.
function writableStorage(initial, { suffixes = ['co.uk'], reject = () => false, drop = () => false } = {}) {
  let cookies = initial.map(item => ({ ...item }));
  const calls = [];
  const identity = item => JSON.stringify([item.name, item.domain, item.path, item.partitionKey || null]);
  const cdp = async (method, params, session) => {
    calls.push(method);
    if (method === 'Storage.getCookies') {
      assert.deepEqual(params, { browserContextId: 'private-context' });
      return { cookies: cookies.map(item => ({ ...item })) };
    }
    if (method === 'Storage.setCookies') {
      assert.equal(params.browserContextId, 'private-context');
      for (const param of params.cookies) {
        if (reject(param)) throw new Error('Invalid cookie fields');
        const stored = { ...param, session: !param.expires, expires: param.expires || -1, size: param.name.length + param.value.length };
        if (suffixes.includes(param.domain.slice(1))) stored.domain = param.domain.slice(1);
        if (drop(param)) continue;
        cookies = cookies.filter(item => identity(item) !== identity(stored)).concat(stored);
      }
      return {};
    }
    assert.equal(method, 'Network.deleteCookies');
    assert.equal(session, 'sid');
    cookies = cookies.filter(item => !(item.name === params.name && item.domain === params.domain && item.path === params.path &&
      JSON.stringify(item.partitionKey) === JSON.stringify(params.partitionKey)));
  };
  const run = (input, target = tab) => browserCookies(cdp, async () => 'sid', target, { scope: 'site', ...input });
  return { cdp, calls, run, cookies: () => cookies };
}
const fresh = (extra = {}) => ({ name: 'added', value: 'fixture-secret', domain: 'sub.example.test', path: '/', secure: true, httpOnly: false, ...extra });

test('set adds host-only and parent-domain cookies in the tab context and never returns values', async () => {
  const f = writableStorage([]);
  const result = await f.run({ mode: 'set', cookie: fresh({ httpOnly: true, sameSite: 'Lax', expires: Date.now() / 1000 + 3600 }) });
  assert.equal(result.total, 1);
  assert.ok(result.cookies.every(item => !('value' in item)));
  await f.run({ mode: 'set', cookie: fresh({ name: 'parent', domain: '.EXAMPLE.test ', secure: false }) });
  const stored = f.cookies();
  assert.deepEqual(stored.map(item => [item.name, item.domain, item.secure, item.sourceScheme]),
    [['added', 'sub.example.test', true, 'Secure'], ['parent', '.example.test', false, 'Secure']]);
  assert.equal(stored[0].httpOnly, true);
  assert.equal(stored[0].sameSite, 'Lax');
  assert.equal(stored[0].session, false);
  assert.equal(stored[1].session, true);
  const insecure = writableStorage([]);
  await insecure.run({ mode: 'set', cookie: fresh({ secure: false }) }, { ...tab, url: 'http://sub.example.test/' });
  assert.equal(insecure.cookies()[0].sourceScheme, 'NonSecure');
});

test('set refuses other sites, public suffix rewrites, prefixes and invalid fields without leaving changes', async () => {
  const f = writableStorage([cookie('other.test')]);
  const refused = [
    [{ domain: 'other.test' }, /当前网站/], [{ domain: '.notexample.test' }, /当前网站/], [{ domain: 'example.test' }, /当前网站/],
    [{ domain: '.test' }, /当前网站/], [{ domain: '.deeper.sub.example.test' }, /当前网站/],
    [{ name: '' }, /名称/], [{ name: 'a=b' }, /名称/], [{ name: ' padded' }, /名称/], [{ name: 'line\nbreak' }, /名称/],
    [{ value: 'a;b' }, /值/], [{ value: 'tab\there' }, /值/], [{ value: 'x'.repeat(4096) }, /4096/],
    [{ path: 'relative' }, /路径/], [{ path: '/' + 'p'.repeat(1024) }, /路径/],
    [{ sameSite: 'Loose' }, /SameSite/], [{ sameSite: 'None', secure: false }, /SameSite=None/],
    [{ expires: Date.now() / 1000 - 1 }, /过期时间/], [{ expires: 'tomorrow' }, /过期时间/],
    [{ name: '__Secure-a', secure: false }, /__Secure-/], [{ name: '__host-a', domain: '.example.test' }, /__Host-/],
    [{ name: '__Host-a', path: '/nested' }, /__Host-/], [{ name: '__Http-a' }, /HttpOnly/],
    [{ partitioned: true }, /分区/], [{ secure: 'yes' }, /无效/],
  ];
  for (const [extra, pattern] of refused) await assert.rejects(f.run({ mode: 'set', cookie: fresh(extra) }), pattern, JSON.stringify(extra));
  await assert.rejects(f.run({ mode: 'set' }), /无效/);
  await assert.rejects(f.run({ mode: 'set', scope: 'all', cookie: fresh() }, { ...tab, url: 'cage://settings#privacy' }), /网站页面/);
  await assert.rejects(f.run({ mode: 'set', cookie: fresh() }, { ...tab, login: true }), /不可管理/);
  await assert.rejects(f.run({ mode: 'set', url: 'https://previous.test/', cookie: fresh() }), /网页已切换/);
  assert.ok(f.calls.every(method => method === 'Storage.getCookies'));
  const ip = writableStorage([]);
  await assert.rejects(ip.run({ mode: 'set', cookie: fresh({ domain: '.0.0.1' }) }, { ...tab, url: 'http://127.0.0.1/' }), /当前网站/);
  const suffix = writableStorage([], { suffixes: ['example.test'] });
  await assert.rejects(suffix.run({ mode: 'set', cookie: fresh({ domain: '.example.test' }) }), /公共后缀/);
  assert.deepEqual(suffix.cookies(), []);
  const dropped = writableStorage([cookie('sub.example.test', { name: 'added' })], { drop: param => param.value === 'dropped' });
  const before = dropped.cookies();
  await assert.rejects(dropped.run({ mode: 'set', cookieKey: (await dropped.run({ mode: 'list' })).cookies[0].key,
    cookie: fresh({ secure: false, value: 'dropped' }) }), /未接受/);
  assert.deepEqual(dropped.cookies(), before);
  const failing = writableStorage([], { reject: () => true });
  await assert.rejects(failing.run({ mode: 'set', cookie: fresh() }), /浏览器拒绝/);
});

test('set edits a listed cookie in place or moves it, and refuses stale, duplicate or partitioned targets', async () => {
  const partitionKey = { topLevelSite: 'https://example.test', hasCrossSiteAncestor: true };
  const f = writableStorage([cookie('sub.example.test', { priority: 'High' }), cookie('sub.example.test', { name: 'taken' }),
    cookie('sub.example.test', { name: 'shared', partitionKey }), cookie('other.test', { name: 'elsewhere' })]);
  const listed = (await f.run({ mode: 'list', scope: 'all' })).cookies;
  const keyOf = name => listed.find(item => item.name === name).key;
  await f.run({ mode: 'set', cookieKey: keyOf('session'), cookie: fresh({ name: 'session', value: 'changed' }) });
  let stored = f.cookies().find(item => item.name === 'session');
  assert.equal(stored.value, 'changed');
  assert.equal(stored.priority, 'High');
  const renamed = await f.run({ mode: 'set', scope: 'all', cookieKey: keyOf('session'), cookie: fresh({ name: 'renamed', path: '/app' }) });
  assert.deepEqual(f.cookies().filter(item => item.domain === 'sub.example.test' && !item.partitionKey).map(item => item.name).sort(), ['renamed', 'taken']);
  assert.equal(renamed.total, 4);
  await assert.rejects(f.run({ mode: 'set', cookieKey: keyOf('session'), cookie: fresh() }), /已变化/);
  await assert.rejects(f.run({ mode: 'set', cookie: fresh({ name: 'taken' }) }), /已存在/);
  await assert.rejects(f.run({ mode: 'set', cookieKey: keyOf('shared'), cookie: fresh({ name: 'shared' }) }), /分区/);
  await assert.rejects(f.run({ mode: 'set', scope: 'all', cookieKey: keyOf('elsewhere'), cookie: fresh({ name: 'elsewhere' }) }), /已变化/);
  await assert.rejects(f.run({ mode: 'set', cookieKey: 'bad', cookie: fresh() }), /已变化/);
  stored = f.cookies().find(item => item.name === 'elsewhere');
  assert.equal(stored.domain, 'other.test');
});
