import test from 'node:test';
import assert from 'node:assert/strict';
import { runIssueSystemLabels, systemLabelsFromBody } from './issue-system-labels.mjs';

test('reads the host and guest dropdowns of the issue forms', () => {
  const body = '### 问题描述与实际表现\n\n在 Windows 上也见过，Linux 没试\n\n### 主机系统\n\nmacOS\n\n### 主机版本与架构\n\nmacOS 26.4 arm64\n\n### 客机系统\n\nLinux\n\n### 客机版本与架构\n\nUbuntu 24.04 arm64\n\n### 复现步骤\n\n打开 Windows 客机';
  assert.deepEqual(systemLabelsFromBody(body), ['macOS 主机', 'Linux 客机']);
});

test('reads multi-select answers and ignores sides that do not apply', () => {
  const body = '### 涉及的主机系统\n\nmacOS, Windows, Linux\n\n### 涉及的客机系统\n\n不涉及或不确定\n';
  assert.deepEqual(systemLabelsFromBody(body), ['macOS 主机', 'Windows 主机', 'Linux 主机']);
});

test('reads free-text fields, including a field shared by both sides', () => {
  assert.deepEqual(
    systemLabelsFromBody('### 主机与客机系统\r\n\r\n主机：Windows 11 24H2 x64\r\n客机：Debian 13\r\n两边都装了 macOS 风格主题\r\n'),
    ['Windows 主机', 'Linux 客机'],
  );
  assert.deepEqual(systemLabelsFromBody('### 主机系统与架构\n\n全部平台\n\n### 客机系统与架构\n\n_No response_'), ['macOS 主机', 'Windows 主机', 'Linux 主机']);
  assert.deepEqual(systemLabelsFromBody('没有表单标题的正文，提到 macOS 主机'), []);
  assert.deepEqual(systemLabelsFromBody(null), []);
});

function setup(issue) {
  const calls = [];
  const request = async (method, path, token, body) => {
    calls.push({ method, path, token, body });
    return method === 'GET' ? { status: 200, data: issue } : { status: 200, data: [] };
  };
  return { calls, options: { repository: 'owner/project', number: 42, token: 'issue-token', request } };
}

test('adds only the system labels the issue does not have yet', async () => {
  const { calls, options } = setup({ body: '### 主机系统\n\nmacOS\n\n### 客机系统\n\nLinux', labels: [{ name: 'bug' }, { name: 'macOS 主机' }] });
  assert.deepEqual(await runIssueSystemLabels(options), { status: 'labeled', labels: ['Linux 客机'] });
  assert.deepEqual(calls[1], { method: 'POST', path: '/repos/owner/project/issues/42/labels', token: 'issue-token', body: { labels: ['Linux 客机'] } });
});

test('leaves issues without a recognised system and pull requests untouched', async () => {
  const plain = setup({ body: '### 文档位置\n\nREADME.md', labels: [] });
  assert.deepEqual(await runIssueSystemLabels(plain.options), { status: 'unchanged', labels: [] });
  const pull = setup({ body: '### 主机系统\n\nmacOS', labels: [], pull_request: {} });
  assert.deepEqual(await runIssueSystemLabels(pull.options), { status: 'skipped', labels: [] });
  assert.equal(plain.calls.length + pull.calls.length, 2);
});

test('rejects invalid input and failed requests', async () => {
  const { options } = setup({ body: '', labels: [] });
  await assert.rejects(runIssueSystemLabels({ ...options, number: '1/../2' }), /正整数/);
  await assert.rejects(runIssueSystemLabels({ ...options, token: '' }), /GITHUB_TOKEN/);
  await assert.rejects(runIssueSystemLabels({ ...options, request: async () => ({ status: 404 }) }), /HTTP 404/);
});
