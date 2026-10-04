// Publishes the CLI downloads to R2 under cli/:
//   cli/v<version>/tjuclaw-<os>-<arch>[.exe], cli/v<version>/SHA256SUMS,
//   cli/install.sh, cli/install.ps1, cli/latest.json, cli/LATEST.
// Run after scripts/build-downloads.sh. A version already published is left
// alone, so a release push that does not bump package.json changes nothing.
// LATEST moves last: a failed run never points installs at a partial release.
import { readFileSync, readdirSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { publicObjectUrl, r2Exists, r2Put, readR2Config, sha256Hex } from './r2.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
export const TARGETS = ['linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64', 'windows-amd64', 'windows-arm64'];

export const binaryName = target => `tjuclaw-${target}${target.startsWith('windows-') ? '.exe' : ''}`;

// What must never ship: private code, internal hosts, local paths, secrets.
const FORBIDDEN = [
  ['private repository', /tjuclaw-server|tjuclaw-sandbox|tjuclaw-crawler|agentwego\/infra/],
  ['internal registry', /harbor\.agentwego\.com/],
  ['cluster address', /\b10\.244\.\d+\.\d+\b/],
  ['build machine path', /\/home\/[a-z_][a-z0-9_-]*\/|\/Users\/[A-Za-z0-9_-]+\/|C:\\Users\\/],
  ['private key', /-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/],
  ['API key', /\bsk-[A-Za-z0-9]{32,}\b|\bAKIA[0-9A-Z]{16}\b|\bghp_[A-Za-z0-9]{36}\b/],
];

/** Returns the labels of forbidden content found in a binary. */
export function scanBinary(bytes) {
  const text = Buffer.from(bytes).toString('latin1');
  return FORBIDDEN.filter(([, pattern]) => pattern.test(text)).map(([label]) => label);
}

export function checksums(files) {
  return files.map(({ name, bytes }) => `${sha256Hex(bytes)}  ${name}\n`).join('');
}

export function latestManifest(version, base, files, now = new Date()) {
  return {
    version,
    published_at: now.toISOString(),
    install: { unix: `curl -fsSL ${base}/install.sh | sh`, windows: `irm ${base}/install.ps1 | iex` },
    files: Object.fromEntries(files.map(({ target, name, bytes }) => [target, {
      url: `${base}/v${version}/${name}`, sha256: sha256Hex(bytes), size: bytes.length,
    }])),
  };
}

async function main() {
  const version = JSON.parse(readFileSync(resolve(root, 'package.json'), 'utf8')).version;
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error(`invalid version ${version}`);
  const r2 = readR2Config();
  const base = publicObjectUrl(r2, 'cli');
  const sumsKey = `cli/v${version}/SHA256SUMS`;
  if (await r2Exists(r2, sumsKey)) {
    console.log(`tjuclaw ${version} is already published; nothing to do.`);
    return;
  }
  const present = new Set(readdirSync(resolve(root, 'dist')));
  const files = TARGETS.map(target => {
    const name = binaryName(target);
    if (!present.has(name)) throw new Error(`missing dist/${name}; run scripts/build-downloads.sh`);
    return { target, name, bytes: readFileSync(resolve(root, 'dist', name)) };
  });
  for (const file of files) {
    const found = scanBinary(file.bytes);
    if (found.length) throw new Error(`${file.name} contains ${found.join(', ')}; not publishing`);
  }
  const immutable = 'public, max-age=31536000, immutable';
  const fresh = 'public, max-age=60';
  for (const file of files) {
    await r2Put(r2, `cli/v${version}/${file.name}`, file.bytes, {
      contentType: 'application/octet-stream', cacheControl: immutable,
      contentDisposition: `attachment; filename="${file.target.startsWith('windows-') ? 'tjuclaw.exe' : 'tjuclaw'}"`,
    });
    console.log(`uploaded ${file.name}`);
  }
  await r2Put(r2, sumsKey, Buffer.from(checksums(files)), { contentType: 'text/plain; charset=utf-8', cacheControl: immutable });
  for (const script of ['install.sh', 'install.ps1']) {
    await r2Put(r2, `cli/${script}`, readFileSync(resolve(root, 'install', script)), { contentType: 'text/plain; charset=utf-8', cacheControl: fresh });
  }
  await r2Put(r2, 'cli/latest.json', Buffer.from(JSON.stringify(latestManifest(version, base, files), null, 2) + '\n'), { contentType: 'application/json', cacheControl: fresh });
  await r2Put(r2, 'cli/LATEST', Buffer.from(`${version}\n`), { contentType: 'text/plain; charset=utf-8', cacheControl: fresh });
  console.log(`published tjuclaw ${version}: ${base}/install.sh`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch(error => { console.error(error.message); process.exit(1); });
}
