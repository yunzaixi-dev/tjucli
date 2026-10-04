import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { TARGETS, binaryName, checksums, latestManifest, scanBinary } from './publish-downloads.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

test('the leak scan stops private code, internal hosts, local paths and keys', () => {
  assert.deepEqual(scanBinary(Buffer.from('plain public cli code https://app.tjuclaw.cloud/api')), []);
  for (const [sample, label] of [
    ['github.com/yunzaixi-dev/tjuclaw-server/internal/auth', 'private repository'],
    ['harbor.agentwego.com/agentwego/x', 'internal registry'],
    ['dial 10.244.3.17:8090', 'cluster address'],
    ['/home/yun/Desktop/tjuclaw/cli/main.go', 'build machine path'],
    ['-----BEGIN OPENSSH PRIVATE KEY-----', 'private key'],
    [`token sk-${'a'.repeat(40)}`, 'API key'],
  ]) assert.deepEqual(scanBinary(Buffer.from(`xx\0${sample}\0yy`)), [label], sample);
});

test('checksums and the manifest name every target', () => {
  const files = TARGETS.map(target => ({ target, name: binaryName(target), bytes: Buffer.from(target) }));
  const sums = checksums(files).trim().split('\n');
  assert.equal(sums.length, 6);
  assert.match(sums[4], /^[0-9a-f]{64}  tjuclaw-windows-amd64\.exe$/);
  const manifest = latestManifest('0.0.31', 'https://dl.example/cli', files, new Date('2026-10-04T00:00:00Z'));
  assert.equal(manifest.files['darwin-arm64'].url, 'https://dl.example/cli/v0.0.31/tjuclaw-darwin-arm64');
  assert.equal(manifest.install.unix, 'curl -fsSL https://dl.example/cli/install.sh | sh');
  assert.equal(Object.keys(manifest.files).length, 6);
});

test('the built binaries pass the leak scan', { skip: !existsSync(resolve(root, 'dist')) && 'run scripts/build-downloads.sh first' }, () => {
  for (const target of TARGETS) {
    assert.deepEqual(scanBinary(readFileSync(resolve(root, 'dist', binaryName(target)))), [], target);
  }
});

test('publish uploads binaries, then checksums and scripts, and moves LATEST last; a published version is skipped', { skip: !existsSync(resolve(root, 'dist')) && 'run scripts/build-downloads.sh first' }, async () => {
  const { publish } = await import('./publish-downloads.mjs');
  const env = {
    R2_ENDPOINT: 'https://account.r2.cloudflarestorage.com', R2_BUCKET: 'tjuclaw-release',
    R2_PUBLIC_URL: 'https://dl.example', R2_ACCESS_KEY_ID: 'id', R2_SECRET_ACCESS_KEY: 'secret',
  };
  const stored = new Map();
  const puts = [];
  const fetchFn = async (url, options) => {
    const key = decodeURIComponent(new URL(url).pathname.replace('/tjuclaw-release/', ''));
    if (options.method === 'HEAD') return { ok: stored.has(key), status: stored.has(key) ? 200 : 404 };
    if (options.method === 'PUT') {
      stored.set(key, { body: options.body, type: options.headers['content-type'] });
      puts.push(key);
      return { ok: true, status: 200 };
    }
    throw new Error(`unexpected ${options.method}`);
  };
  await publish({ env, fetchFn, log: () => {} });
  const version = JSON.parse(readFileSync(resolve(root, 'package.json'), 'utf8')).version;
  assert.equal(puts.length, 6 + 1 + 2 + 2);
  assert.deepEqual(puts.slice(0, 6), TARGETS.map(target => `cli/v${version}/${binaryName(target)}`));
  assert.equal(puts.at(-1), 'cli/LATEST');
  assert.equal(String(stored.get('cli/LATEST').body), `${version}\n`);
  assert.equal(stored.get('cli/install.ps1').type, 'text/plain; charset=utf-8');
  const manifest = JSON.parse(String(stored.get('cli/latest.json').body));
  assert.equal(manifest.files['linux-amd64'].url, `https://dl.example/cli/v${version}/tjuclaw-linux-amd64`);
  puts.length = 0;
  await publish({ env, fetchFn, log: () => {} });
  assert.equal(puts.length, 0, 'a published version is not uploaded again');
});
