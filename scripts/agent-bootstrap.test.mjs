import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { parseArgs, selectAsset, checksumFor, makePlan, downloadPlan, httpsResponse } from '../www/public/agent-bootstrap.mjs';

const tag = 'v0.1.0-test';
const prefix = `https://github.com/Lee-xeo/Claude-Cage/releases/download/${tag}/`;
const data = Buffer.from('fake package fixture');
const hash = createHash('sha256').update(data).digest('hex');
function release(platform) {
  const suffix = { 'darwin-arm64': 'macos-arm64.dmg', 'linux-amd64': 'linux-amd64.deb', 'win32-amd64': 'windows-amd64-setup-unsigned.exe' }[platform];
  const name = `Cage-${tag.slice(1)}-${suffix}`;
  return { tag_name: tag, assets: [{ name, size: data.length, browser_download_url: prefix + name }, { name: 'SHA256SUMS', browser_download_url: prefix + 'SHA256SUMS' }] };
}

test('select only exact platform artifacts and explicit unsigned consent', () => {
  for (const platform of ['darwin-arm64', 'linux-amd64', 'win32-amd64']) {
    const options = parseArgs(['plan', '--platform', platform, '--version', tag, '--allow-unsigned']);
    const selected = selectAsset(release(platform), options);
    assert.equal(selected.unsigned, platform === 'win32-amd64');
    assert.equal(selected.tag, tag);
  }
  assert.throws(() => selectAsset(release('win32-amd64'), { platform: 'win32-amd64' }), { code: 'unsigned_package_requires_consent' });
  assert.throws(() => parseArgs(['download', '--destination', 'relative']), { code: 'invalid_input' });
  assert.throws(() => parseArgs(['--version', '../../bad']), { code: 'invalid_input' });
  const wrong = release('linux-amd64'); wrong.assets[0].browser_download_url = 'https://example.org/package';
  assert.throws(() => selectAsset(wrong, { platform: 'linux-amd64' }), { code: 'invalid_release' });
  const oversized = release('linux-amd64'); oversized.assets[0].size = 2 * 1024 ** 3 + 1;
  assert.throws(() => selectAsset(oversized, { platform: 'linux-amd64' }), { code: 'invalid_release' });
});

test('checksum manifest must identify exactly one package', () => {
  assert.equal(checksumFor(`${hash}  Cage.deb\r\n`, 'Cage.deb'), hash);
  assert.throws(() => checksumFor(`${hash}  other.deb`, 'Cage.deb'), { code: 'invalid_checksum_manifest' });
  assert.throws(() => checksumFor(`${hash}  Cage.deb\n${hash} *Cage.deb`, 'Cage.deb'), { code: 'invalid_checksum_manifest' });
});

test('plan pins metadata and sums to one release; downloads are verified and exclusive', async t => {
  const options = parseArgs(['plan', '--platform', 'linux-amd64', '--version', tag]);
  const metadata = release(options.platform);
  const manifest = `\uFEFF${hash}  ${metadata.assets[0].name}\r\n`;
  metadata.assets[0].digest = `sha256:${hash}`;
  metadata.assets[1].digest = 'sha256:' + createHash('sha256').update(manifest).digest('hex');
  const fetchFixture = async url => {
    if (String(url).startsWith('https://api.github.com/')) return new Response(JSON.stringify(metadata));
    if (String(url).endsWith('SHA256SUMS')) return new Response(manifest);
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
  for (const item of metadata.assets) {
    const original = item.digest;
    item.digest = 'sha256:' + '0'.repeat(64);
    await assert.rejects(makePlan(options, fetchFixture), { code: 'checksum_mismatch' });
    item.digest = original;
  }
});

test('only release asset requests follow GitHub CDN redirects', async () => {
  const asset = prefix + 'SHA256SUMS';
  const cdn = 'https://release-assets.githubusercontent.com/fixture?signature=test';
  for (const [source, location] of [[asset, 'http://release-assets.githubusercontent.com/'], [asset, 'https://example.org/'], [asset, cdn.replace('.com/', '.com:444/')], ['https://api.github.com/repos/Lee-xeo/Claude-Cage/releases/latest', cdn]]) {
    const calls = [];
    await assert.rejects(httpsResponse(source, async url => {
      calls.push(url.href);
      return new Response(null, { status: 302, headers: { location } });
    }), { code: 'unsafe_download' });
    assert.deepEqual(calls, [source]);
  }
  const response = await httpsResponse(asset, async url => url.href === cdn ? new Response('ok') : new Response(null, { status: 302, headers: { location: cdn } }));
  assert.equal(await response.text(), 'ok');
  await assert.rejects(httpsResponse(asset, async () => { throw new Error('https://secret.example/private?token=do-not-echo'); }), error => error.code === 'download_connection_failed' && !error.message.includes('do-not-echo'));
});
