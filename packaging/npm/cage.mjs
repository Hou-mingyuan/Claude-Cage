#!/usr/bin/env node
import { readFile } from 'node:fs/promises';
import { nativeCommand, runNative } from './launcher.mjs';

try {
  const { version } = JSON.parse(await readFile(new URL('../../package.json', import.meta.url), 'utf8'));
  const platform = ({ darwin: './macos.mjs', linux: './linux.mjs', win32: './windows.mjs' })[process.platform];
  if (!platform) throw new Error('This platform has no Cage desktop installer.');
  const adapter = await import(platform);
  const args = process.argv.slice(2);
  const executable = await nativeCommand(version, adapter, args);
  if (executable) process.exitCode = await runNative(executable, args);
} catch (error) {
  console.error(`Cage: ${error.message}`);
  process.exitCode = error.exitCode === 2 ? 2 : 1;
}
