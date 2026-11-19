import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { runContributionInvite } from './contribution-invite.mjs';

const repo = 'owner/project';
const prefix = `/repos/${repo}`;
const issue = () => ({ number: 42, state: 'open', user: { login: 'author', id: 7, type: 'User' }, labels: [{ name: 'contribution' }, { name: 'accepted' }] });
function setup(overrides = {}) {
  const calls = [];
  const current = issue();
  const responses = {
    [`GET ${prefix}/collaborators/reviewer/permission`]: { status: 200, data: { permission: 'admin' } },
    [`GET ${prefix}/issues/42`]: { status: 200, data: current },
    [`GET ${prefix}/collaborators/author/permission`]: { status: 404 },
    [`GET ${prefix}/invitations?per_page=100&page=1`]: { status: 200, data: [] },
    [`PUT ${prefix}/collaborators/author`]: { status: 201, data: {} },
    [`GET ${prefix}/issues/42/comments?per_page=100&page=1`]: { status: 200, data: [] },
    [`POST ${prefix}/issues/42/comments`]: { status: 201, data: {} },
    ...overrides,
  };
  const options = {
    repository: repo, actor: 'reviewer', eventName: 'issues',
    event: { action: 'labeled', label: { name: 'accepted' }, issue: issue() },
    token: 'issue-token', inviteToken: 'invite-token',
    request: async (method, path, token, body) => {
      calls.push({ method, path, token, body });
      const response = responses[`${method} ${path}`];
      assert.ok(response, `Unexpected request: ${method} ${path}`);
      return response;
    },
  };
  return { options, calls, current };
}
const writes = calls => calls.filter(call => call.method !== 'GET');

test('only admin or maintain permission can approve an invitation', async () => {
  for (const permission of ['write', 'triage', 'read']) {
    const { options, calls } = setup({ [`GET ${prefix}/collaborators/reviewer/permission`]: { status: 200, data: { permission } } });
    await assert.rejects(runContributionInvite(options), /审核.*权限/);
    assert.deepEqual(writes(calls), []);
  }
});

test('requires a contribution issue that is still open and accepted at execution time', async () => {
  for (const change of [{ labels: [{ name: 'contribution' }] }, { labels: [{ name: 'accepted' }] }, { state: 'closed' }, { pull_request: {} }, { user: { login: 'robot', type: 'Bot' } }]) {
    const { options, calls, current } = setup();
    Object.assign(current, change);
    assert.equal((await runContributionInvite(options)).status, 'skipped');
    assert.deepEqual(writes(calls), []);
  }
});

test('invites the current issue author with push access, using only the dedicated token', async () => {
  const { options, calls } = setup();
  options.event.issue.user.login = 'stale-author';
  const result = await runContributionInvite(options);
  assert.equal(result.status, 'invited');
  assert.deepEqual(writes(calls)[0], { method: 'PUT', path: `${prefix}/collaborators/author`, token: 'invite-token', body: { permission: 'push' } });
  const comment = writes(calls)[1];
  assert.equal(comment.token, 'issue-token');
  assert.match(comment.body.body, /@author/);
  assert.match(comment.body.body, /写权限/);
  assert.match(comment.body.body, /invitations/);
  assert.match(comment.body.body, /PR/);
  assert.ok(!JSON.stringify(comment.body).includes('invite-token'));
});

test('existing collaborators keep their access without an invitation token', async () => {
  const { options, calls } = setup({ [`GET ${prefix}/collaborators/author/permission`]: { status: 200, data: { permission: 'write' } } });
  options.inviteToken = '';
  assert.equal((await runContributionInvite(options)).status, 'already-collaborator');
  assert.ok(writes(calls).every(call => !call.path.includes('/collaborators/')));
});

test('finds a pending invitation across pages and does not send another', async () => {
  const pending = Array.from({ length: 100 }, (_, i) => ({ invitee: { id: i + 100 } }));
  const { options, calls } = setup({
    [`GET ${prefix}/invitations?per_page=100&page=1`]: { status: 200, data: pending },
    [`GET ${prefix}/invitations?per_page=100&page=2`]: { status: 200, data: [{ invitee: { id: 7 } }] },
  });
  assert.equal((await runContributionInvite(options)).status, 'invitation-pending');
  assert.ok(writes(calls).every(call => !call.path.includes('/collaborators/')));
});

test('updates the bot reminder rather than creating duplicate comments', async () => {
  const { options, calls } = setup({
    [`GET ${prefix}/issues/42/comments?per_page=100&page=1`]: { status: 200, data: [
      { id: 8, user: { login: 'author' }, body: '<!-- contribution-invite --> forged' },
      { id: 9, user: { login: 'github-actions[bot]' }, body: '<!-- contribution-invite --> old' },
    ] },
    [`PATCH ${prefix}/issues/comments/9`]: { status: 200, data: {} },
  });
  await runContributionInvite(options);
  assert.equal(writes(calls).at(-1).path, `${prefix}/issues/comments/9`);
});

test('missing dedicated token prevents any invitation or success reminder', async () => {
  const { options, calls } = setup();
  options.inviteToken = '';
  await assert.rejects(runContributionInvite(options), /CONTRIBUTION_INVITE_TOKEN/);
  assert.deepEqual(writes(calls), []);
});

test('an invitation API failure does not publish a success reminder', async () => {
  const { options, calls } = setup({ [`PUT ${prefix}/collaborators/author`]: { status: 403, data: { message: 'Forbidden' } } });
  await assert.rejects(runContributionInvite(options), /403/);
  assert.equal(writes(calls).length, 1);
});

test('manual retry uses its issue number and the same permission checks', async () => {
  const { options } = setup({ [`GET ${prefix}/collaborators/reviewer/permission`]: { status: 200, data: { permission: 'write', role_name: 'maintain' } } });
  options.eventName = 'workflow_dispatch';
  options.event = { inputs: { issue_number: '42' } };
  assert.equal((await runContributionInvite(options)).status, 'invited');
});

test('other label events and invalid manual issue numbers cannot invite anyone', async () => {
  const { options, calls } = setup();
  options.event.label.name = 'bug';
  assert.equal((await runContributionInvite(options)).status, 'skipped');
  options.eventName = 'workflow_dispatch';
  options.event = { inputs: { issue_number: '42/../43' } };
  await assert.rejects(runContributionInvite(options), /Issue 编号/);
  assert.deepEqual(writes(calls), []);
});

test('the shipped CLI reads the Actions event and sends authenticated API requests', async t => {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'contribution-invite-'));
  t.after(() => fs.rm(directory, { recursive: true, force: true }));
  const { options } = setup();
  const eventPath = path.join(directory, 'event.json');
  await fs.writeFile(eventPath, JSON.stringify(options.event));
  const requests = [];
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
    requests.push({ method: req.method, url: req.url, authorization: req.headers.authorization });
    try {
      const response = await options.request(req.method, req.url, req.headers.authorization?.replace('Bearer ', ''), body);
      res.writeHead(response.status, { 'Content-Type': 'application/json' });
      res.end(response.data ? JSON.stringify(response.data) : '');
    } catch (error) {
      res.writeHead(500);
      res.end(JSON.stringify({ message: error.message }));
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const child = spawn(process.execPath, [fileURLToPath(new URL('./contribution-invite.mjs', import.meta.url))], {
    env: { ...process.env, GITHUB_EVENT_PATH: eventPath, GITHUB_EVENT_NAME: 'issues', GITHUB_ACTOR: 'reviewer', GITHUB_REPOSITORY: repo,
      GITHUB_TOKEN: 'issue-token', CONTRIBUTION_INVITE_TOKEN: 'invite-token', GITHUB_API_URL: `http://127.0.0.1:${server.address().port}` },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  t.after(() => child.kill());
  let stdout = '', stderr = '';
  child.stdout.on('data', data => { stdout += data; });
  child.stderr.on('data', data => { stderr += data; });
  const code = await new Promise((resolve, reject) => { child.once('error', reject); child.once('close', resolve); });
  assert.equal(code, 0, stderr);
  assert.equal(JSON.parse(stdout).status, 'invited');
  assert.ok(requests.some(req => req.method === 'PUT' && req.url.endsWith('/collaborators/author') && req.authorization === 'Bearer invite-token'));
  assert.ok(requests.some(req => req.method === 'POST' && req.authorization === 'Bearer issue-token'));
  assert.ok(!stdout.includes('invite-token') && !stderr.includes('invite-token'));
});
