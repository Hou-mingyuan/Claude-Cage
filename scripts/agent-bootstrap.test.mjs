import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { parseArgs, selectAsset, selectStaticAsset, checksumFor, makePlan, downloadPlan, httpsResponse, envProxyHint } from '../www/public/agent-bootstrap.mjs';

const tag = 'v0.1.0-test';
const prefix = `https://github.com/Lee-xeo/Claude-Cage/releases/download/${tag}/`;
const data = Buffer.from('fake package fixture');
const hash = createHash('sha256').update(data).digest('hex');
const proxyNames = ['HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy', 'NODE_USE_ENV_PROXY', 'NODE_OPTIONS'];
// The host running the tests may itself use a proxy; pin the variables per case.
async function withProxyEnv(vars, fn) {
  const saved = proxyNames.map(name => [name, process.env[name]]);
  for (const name of proxyNames) delete process.env[name];
  Object.assign(process.env, vars);
  try { return await fn(); } finally {
    for (const name of proxyNames) delete process.env[name];
    for (const [name, value] of saved) if (value !== undefined) process.env[name] = value;
  }
}
const credentialedProxy = { HTTPS_PROXY: 'http://user:proxy-secret@127.0.0.1:61214' };
function release(platform) {
  const suffix = { 'darwin-arm64': 'macos-arm64.dmg', 'darwin-amd64': 'macos-amd64.dmg', 'linux-amd64': 'linux-amd64.deb', 'win32-amd64': 'windows-amd64-setup-unsigned.exe' }[platform];
  const name = `Cage-${tag.slice(1)}-${suffix}`;
  return { tag_name: tag, assets: [{ name, size: data.length, browser_download_url: prefix + name }, { name: 'SHA256SUMS', browser_download_url: prefix + 'SHA256SUMS' }] };
}

test('select only exact platform artifacts and explicit unsigned consent', () => {
  for (const platform of ['darwin-arm64', 'darwin-amd64', 'linux-amd64', 'win32-amd64']) {
    const options = parseArgs(['plan', '--platform', platform, '--version', tag, '--allow-unsigned']);
    const selected = selectAsset(release(platform), options);
    assert.equal(selected.unsigned, platform === 'win32-amd64');
    assert.equal(selected.tag, tag);
  }
  assert.throws(() => selectAsset(release('win32-amd64'), { platform: 'win32-amd64' }), { code: 'unsigned_package_requires_consent' });
  // Name a supported platform so the result does not depend on the test host.
  assert.throws(() => parseArgs(['download', '--platform', 'linux-amd64', '--destination', 'relative']), { code: 'invalid_input' });
  assert.throws(() => parseArgs(['--platform', 'linux-amd64', '--version', '../../bad']), { code: 'invalid_input' });
  const wrong = release('linux-amd64'); wrong.assets[0].browser_download_url = 'https://example.org/package';
  assert.throws(() => selectAsset(wrong, { platform: 'linux-amd64' }), { code: 'invalid_release' });
});

test('checksum manifest must identify exactly one package', () => {
  assert.equal(checksumFor(`${hash}  Cage.deb\r\n`, 'Cage.deb'), hash);
  assert.throws(() => checksumFor(`${hash}  other.deb`, 'Cage.deb'), { code: 'invalid_checksum_manifest' });
  assert.throws(() => checksumFor(`${hash}  Cage.deb\n${hash} *Cage.deb`, 'Cage.deb'), { code: 'invalid_checksum_manifest' });
});

test('plan pins metadata and sums to one release; downloads are verified and exclusive', async t => {
  const options = parseArgs(['plan', '--platform', 'linux-amd64', '--version', tag]);
  const metadata = release(options.platform);
  const fetchFixture = async (url, init = {}) => {
    if (String(url).startsWith('https://api.github.com/')) assert.fail('an explicit version must not use the REST API');
    if (String(url).endsWith('SHA256SUMS')) return new Response(`${hash}  ${metadata.assets[0].name}\n`);
    if (init.method === 'HEAD') return new Response(null, { headers: { 'content-length': String(data.length) } });
    return new Response(data);
  };
  const plan = await makePlan(options, fetchFixture);
  assert.equal(plan.sha256, hash);
  assert.equal(plan.cli, '/usr/bin/claude-cage');
  const directory = await mkdtemp(join(tmpdir(), 'cage-bootstrap-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const result = await downloadPlan(plan, directory, fetchFixture);
  assert.deepEqual(await readFile(result.path), data);
  assert.ok(result.installation.argv.includes(result.path));
  await writeFile(result.path, 'existing user file');
  await assert.rejects(downloadPlan(plan, directory, fetchFixture), { code: 'EEXIST' });
  assert.equal(await readFile(result.path, 'utf8'), 'existing user file');
  await assert.rejects(downloadPlan({ ...plan, asset: 'bad.deb', sha256: '0'.repeat(64) }, directory, fetchFixture), { code: 'checksum_mismatch' });
  assert.deepEqual(await readdir(directory), [plan.asset]);
});

test('refuse HTTP redirects and redact transport failures at the CLI boundary', async () => {
  await assert.rejects(httpsResponse('https://example.org/', async () => new Response(null, { status: 302, headers: { location: 'http://example.org/' } })), { code: 'unsafe_download' });
  const refused = async () => { throw new Error('https://secret.example/private?token=do-not-echo'); };
  await withProxyEnv({}, () => assert.rejects(httpsResponse('https://example.org/', refused), error => error.code === 'download_connection_failed' && error.next_action === 'check_host_network' && !error.message.includes('do-not-echo') && !error.message.includes('PROXY')));
  await withProxyEnv(credentialedProxy, () => assert.rejects(httpsResponse('https://example.org/', refused), error => error.code === 'download_connection_failed' && error.message.includes('HTTPS_PROXY/HTTP_PROXY is set') && !error.message.includes('proxy-secret') && !error.message.includes('61214')));
});

test('env proxy hints depend on Node support and never echo proxy values', () => {
  const old = { version: 'v22.12.0', execArgv: [], flagSupported: false };
  const current = { version: 'v24.15.0', execArgv: [], flagSupported: true };
  assert.equal(envProxyHint({}, old), '');
  assert.equal(envProxyHint({ NODE_USE_ENV_PROXY: '1' }, current), '');
  const legacy = envProxyHint(credentialedProxy, old);
  assert.ok(legacy.includes('v22.12.0') && legacy.includes('node --use-env-proxy') && legacy.includes('curl') && legacy.includes('sha256'));
  assert.match(envProxyHint({ http_proxy: 'http://127.0.0.1:1' }, current), /rerun as node --use-env-proxy/);
  for (const [env, runtime] of [[{ ...credentialedProxy, NODE_USE_ENV_PROXY: '1' }, current], [{ ...credentialedProxy, NODE_OPTIONS: '--use-env-proxy' }, current], [credentialedProxy, { ...current, execArgv: ['--use-env-proxy'] }]]) {
    assert.match(envProxyHint(env, runtime), /Node uses it/);
  }
  for (const runtime of [old, current]) assert.ok(!envProxyHint(credentialedProxy, runtime).includes('proxy-secret') && !envProxyHint(credentialedProxy, runtime).includes('127.0.0.1'));
});

test('stalled and interrupted package downloads keep specific network codes', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'cage-bootstrap-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const plan = { asset: 'Cage-0.1.0-test-linux-amd64.deb', url: prefix + 'Cage-0.1.0-test-linux-amd64.deb', size: data.length, sha256: hash, installation: {} };
  const stalled = async (url, init) => new Response(new ReadableStream({ start(controller) {
    controller.enqueue(data.subarray(0, 4));
    init.signal.addEventListener('abort', () => controller.error(init.signal.reason));
  } }));
  await withProxyEnv({}, () => assert.rejects(downloadPlan(plan, directory, stalled, 50), error => error.code === 'download_timeout' && error.next_action === 'check_host_network' && !error.message.includes('PROXY')));
  const broken = async () => new Response(new ReadableStream({ pull(controller) { controller.error(new TypeError('terminated https://secret.example/?token=do-not-echo')); } }));
  await withProxyEnv(credentialedProxy, () => assert.rejects(downloadPlan(plan, directory, broken), error => error.code === 'download_interrupted' && error.next_action === 'check_host_network' && error.message.includes('HTTPS_PROXY/HTTP_PROXY is set') && !error.message.includes('proxy-secret') && !error.message.includes('do-not-echo')));
  assert.deepEqual(await readdir(directory), []);
});

test('static release files select the package, its checksum and unsigned consent', () => {
  const manifest = `${hash}  Cage-0.1.0-test-windows-amd64-setup-unsigned.exe\n${hash} *Cage-0.1.0-test-linux-amd64.deb\n`;
  assert.equal(selectStaticAsset(tag, manifest, { platform: 'linux-amd64' }).name, 'Cage-0.1.0-test-linux-amd64.deb');
  assert.throws(() => selectStaticAsset(tag, manifest, { platform: 'win32-amd64' }), { code: 'unsigned_package_requires_consent' });
  assert.equal(selectStaticAsset(tag, manifest, { platform: 'win32-amd64', allowUnsigned: true }).unsigned, true);
  assert.throws(() => selectStaticAsset(tag, manifest, { platform: 'darwin-arm64' }), { code: 'asset_unavailable' });
  assert.throws(() => selectStaticAsset('../x', manifest, { platform: 'linux-amd64' }), { code: 'invalid_release' });
});

test('a rate-limited latest lookup falls back to the release redirect and static files', async () => {
  const options = parseArgs(['plan', '--platform', 'linux-amd64']);
  const name = 'Cage-0.1.0-test-linux-amd64.deb';
  const seen = [];
  const fetchFixture = async (url, init = {}) => {
    seen.push(`${init.method ?? 'GET'} ${url}`);
    if (String(url).startsWith('https://api.github.com/')) return new Response('limit', { status: 403 });
    if (String(url).endsWith('/releases/latest')) return new Response(null, { status: 302, headers: { location: `https://github.com/Lee-xeo/Claude-Cage/releases/tag/${tag}` } });
    if (String(url).endsWith('SHA256SUMS')) return new Response(`${hash}  ${name}\n`);
    if (init.method === 'HEAD') return new Response(null, { headers: { 'content-length': String(data.length) } });
    return assert.fail(`unexpected ${url}`);
  };
  const plan = await makePlan(options, fetchFixture);
  assert.equal(plan.release, tag);
  assert.equal(plan.url, prefix + name);
  assert.equal(plan.size, data.length);
  assert.equal(plan.sha256, hash);
  assert.ok(seen.some(line => line.startsWith('HEAD https://github.com/Lee-xeo/Claude-Cage/releases/latest')));
  const unresolved = async url => String(url).startsWith('https://api.github.com/') ? new Response('limit', { status: 429 }) : new Response('page', { status: 200 });
  await assert.rejects(makePlan(options, unresolved), error => error.code === 'rate_limited' && error.next_action === 'choose_version' && error.message.includes('--version'));
});
