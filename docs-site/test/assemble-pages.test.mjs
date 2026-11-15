/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { describe, it } from 'node:test';

import {
	assemblePagesSite,
	buildVersionsManifest,
	extractVerifiedSnapshotTarball,
	renderRedirectHtml,
	selectRetainedReleases,
	snapshotAssetNames,
	verifyTarListingSafe,
} from '../scripts/assemble-pages.mjs';
import {
	patchLegacyAstroConfig,
	prepareLegacySnapshotDir,
} from '../scripts/prepare-legacy-snapshot.mjs';

function makeRelease(tagName, { hasDocs = true, isDraft = false, publishedAt = '2026-11-15T00:00:00Z' } = {}) {
	const { tarballName, checksumName } = snapshotAssetNames(tagName);
	return {
		tagName,
		isDraft,
		publishedAt,
		assets: hasDocs ? [{ name: tarballName }, { name: checksumName }] : [{ name: 'SHA256SUMS' }],
	};
}

async function createSnapshotTarball(dir, tag, files) {
	const stageDir = path.join(dir, `stage-${tag}`);
	await fs.mkdir(stageDir, { recursive: true });
	for (const [relPath, content] of Object.entries(files)) {
		const target = path.join(stageDir, relPath);
		await fs.mkdir(path.dirname(target), { recursive: true });
		await fs.writeFile(target, content, 'utf8');
	}
	const { tarballName, checksumName } = snapshotAssetNames(tag);
	const tarPath = path.join(dir, tarballName);
	const shaPath = path.join(dir, checksumName);
	execFileSync('tar', ['-czf', tarPath, '-C', stageDir, '.']);
	const hex = crypto.createHash('sha256').update(await fs.readFile(tarPath)).digest('hex');
	await fs.writeFile(shaPath, `${hex}  ${tarballName}\n`, 'utf8');
	return { tarPath, shaPath };
}

describe('selectRetainedReleases', () => {
	const now = new Date('2026-11-15T12:00:00Z');

	it('retains all stable releases, up to 5 active previews, and 14-day nightlies while pruning promoted/superseded lines', () => {
		const releases = [
			// Historical stable WITHOUT docs assets still prunes v0.2.0-preview.*!
			makeRelease('v0.2.0', { hasDocs: false }),
			makeRelease('v0.2.0-preview.3', { hasDocs: true }),
			// Stable releases with docs assets:
			makeRelease('v0.3.0', { publishedAt: '2026-11-01T00:00:00Z' }),
			makeRelease('v0.3.0-preview.7', { hasDocs: true }), // promoted -> pruned
			makeRelease('v0.4.0', { publishedAt: '2026-11-10T00:00:00Z' }),
			// Superseded minor line (0.3 < 0.4) -> pruned even if v0.3.0 hadn't existed:
			makeRelease('v0.1.0-preview.1', { hasDocs: true }),
			// Active v0.5.0 preview/rc line (7 candidates -> keep newest 5):
			makeRelease('v0.5.0-preview.1'),
			makeRelease('v0.5.0-preview.2'),
			makeRelease('v0.5.0-preview.8'),
			makeRelease('v0.5.0-preview.9'),
			makeRelease('v0.5.0-preview.10'),
			makeRelease('v0.5.0-rc.1'),
			makeRelease('v0.5.0-rc.2'),
			// Drafts & missing-asset releases are ignored:
			makeRelease('v0.5.0-rc.3', { isDraft: true }),
			makeRelease('v0.5.0-rc.4', { hasDocs: false }),
			// Nightlies (cutoff for 2026-11-15 minus 14 days = 20261101):
			makeRelease('nightly-20261115'),
			makeRelease('nightly-20261101'),
			makeRelease('nightly-20261031'), // older than 14 days -> pruned
		];

		const retained = selectRetainedReleases(releases, { now });
		assert.deepEqual(
			retained.stable.map((r) => r.tagName),
			['v0.4.0', 'v0.3.0'],
		);
		assert.deepEqual(
			retained.preview.map((r) => r.tagName),
			[
				'v0.5.0-rc.2',
				'v0.5.0-rc.1',
				'v0.5.0-preview.10',
				'v0.5.0-preview.9',
				'v0.5.0-preview.8',
			],
		);
		assert.deepEqual(
			retained.nightly.map((r) => r.tagName),
			['nightly-20261115', 'nightly-20261101'],
		);
	});

	it('retains patch previews on an already-released minor line until that patch is promoted', () => {
		const releases = [
			makeRelease('v0.3.0'),
			makeRelease('v0.3.1-preview.1'),
			makeRelease('v0.4.0-preview.2'),
		];
		const retained = selectRetainedReleases(releases, { now });
		assert.deepEqual(
			retained.preview.map((r) => r.tagName),
			['v0.4.0-preview.2', 'v0.3.1-preview.1'],
		);
	});
});

describe('buildVersionsManifest and renderRedirectHtml', () => {
	it('builds canonical manifest entries and escaped redirect HTML', () => {
		const retained = {
			stable: [makeRelease('v0.3.0', { publishedAt: '2026-11-02T14:20:00Z' })],
			preview: [
				makeRelease('v0.4.0-preview.2', { publishedAt: '2026-11-14T15:00:00Z' }),
				makeRelease('v0.4.0-preview.1', { publishedAt: '2026-11-07T15:00:00Z' }),
			],
			nightly: [],
		};
		const manifest = buildVersionsManifest(retained);
		assert.equal(manifest.defaultChannel, 'main');
		assert.deepEqual(
			manifest.channels.map((c) => c.id),
			['main', 'stable', 'preview', 'nightly'],
		);
		const stable = manifest.channels.find((c) => c.id === 'stable');
		assert.equal(stable?.latest, 'v0.3.0');
		assert.deepEqual(stable?.versions, [
			{
				version: 'v0.3.0',
				label: 'v0.3.0 (latest)',
				path: '/scion/v0.3.0',
				date: '2026-11-02',
			},
		]);
		const preview = manifest.channels.find((c) => c.id === 'preview');
		assert.equal(preview?.versions[0]?.label, 'v0.4.0-preview.2 (latest)');
		assert.equal(preview?.versions[1]?.label, 'v0.4.0-preview.1');

		const nightly = manifest.channels.find((c) => c.id === 'nightly');
		assert.equal(nightly?.latest, '');
		assert.deepEqual(nightly?.versions, []);

		const html = renderRedirectHtml('/scion/v0.3.0/overview/', 'Stable');
		assert.match(html, /data-pagefind-ignore/);
		assert.match(html, /http-equiv="refresh" content="0;url=\/scion\/v0\.3\.0\/overview\/"/);
		assert.match(
			html,
			/rel="canonical" href="https:\/\/googlecloudplatform\.github\.io\/scion\/v0\.3\.0\/overview\/"/,
		);
	});
});

describe('verifyTarListingSafe and extractVerifiedSnapshotTarball', () => {
	it('accepts regular files and directories while rejecting symlinks, absolute paths, and traversal', () => {
		verifyTarListingSafe(
			[
				'drwxr-xr-x root/root         0 2026-11-15 22:00 ./',
				'-rw-r--r-- root/root      1024 2026-11-15 22:00 ./overview/index.html',
				'-rw-r--r-- root/root       512 2026-11-15 22:00 ./_astro/chunk..123.js',
			].join('\n'),
		);

		assert.throws(() => verifyTarListingSafe(''), /empty/i);
		assert.throws(
			() =>
				verifyTarListingSafe(
					'lrwxrwxrwx root/root 0 2026-11-15 22:00 ./evil -> /etc/passwd',
				),
			/Unsafe tar entry type/,
		);
		assert.throws(
			() =>
				verifyTarListingSafe('-rw-r--r-- root/root 12 2026-11-15 22:00 /etc/passwd'),
			/Absolute tar entry path/,
		);
		assert.throws(
			() =>
				verifyTarListingSafe(
					'-rw-r--r-- root/root 12 2026-11-15 22:00 ./overview/../../escape.html',
				),
			/Path traversal/,
		);
	});

	it('rejects SHA-256 mismatches and assembles multi-version Pages output cleanly', async () => {
		const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), 'scion-assemble-test-'));
		try {
			const mainDistDir = path.join(tmpDir, 'main-dist');
			await fs.mkdir(path.join(mainDistDir, 'overview'), { recursive: true });
			await fs.writeFile(
				path.join(mainDistDir, 'overview/index.html'),
				'<html><body>main overview</body></html>',
				'utf8',
			);

			const assetsDir = path.join(tmpDir, 'assets');
			await fs.mkdir(assetsDir, { recursive: true });
			const v030 = await createSnapshotTarball(assetsDir, 'v0.3.0', {
				'overview/index.html': '<html><body>v0.3.0 overview</body></html>',
			});
			const v040p2 = await createSnapshotTarball(assetsDir, 'v0.4.0-preview.2', {
				'overview/index.html': '<html><body>v0.4.0-preview.2 overview</body></html>',
			});

			// Tampered checksum must fail closed.
			const badSha = path.join(assetsDir, 'bad.sha256');
			await fs.writeFile(badSha, `${'0'.repeat(64)}  scion-docs-v0.3.0.tar.gz\n`, 'utf8');
			await assert.rejects(
				() =>
					extractVerifiedSnapshotTarball({
						tarPath: v030.tarPath,
						shaPath: badSha,
						pagesRoot: path.join(tmpDir, 'tampered-out'),
						tag: 'v0.3.0',
					}),
				/SHA-256 mismatch/,
			);

			const outDir = path.join(tmpDir, 'assembled');
			const map = { 'v0.3.0': v030, 'v0.4.0-preview.2': v040p2 };
			const { manifest, retainedCount } = await assemblePagesSite({
				mainDistDir,
				outDir,
				releases: [
					makeRelease('v0.3.0', { publishedAt: '2026-11-01T00:00:00Z' }),
					makeRelease('v0.4.0-preview.2', { publishedAt: '2026-11-14T00:00:00Z' }),
				],
				resolveAssets: async (tag) => map[tag],
				now: new Date('2026-11-15T12:00:00Z'),
			});

			assert.equal(retainedCount, 2);
			assert.equal(
				await fs.readFile(path.join(outDir, 'overview/index.html'), 'utf8'),
				'<html><body>main overview</body></html>',
			);
			assert.equal(
				await fs.readFile(path.join(outDir, 'v0.3.0/overview/index.html'), 'utf8'),
				'<html><body>v0.3.0 overview</body></html>',
			);
			assert.equal(
				await fs.readFile(path.join(outDir, 'v0.4.0-preview.2/overview/index.html'), 'utf8'),
				'<html><body>v0.4.0-preview.2 overview</body></html>',
			);

			const writtenManifest = JSON.parse(
				await fs.readFile(path.join(outDir, 'versions.json'), 'utf8'),
			);
			assert.deepEqual(writtenManifest, manifest);

			assert.match(
				await fs.readFile(path.join(outDir, 'stable/index.html'), 'utf8'),
				/url=\/scion\/v0\.3\.0\/overview\//,
			);
			assert.match(
				await fs.readFile(path.join(outDir, 'preview/index.html'), 'utf8'),
				/url=\/scion\/v0\.4\.0-preview\.2\/overview\//,
			);
			assert.match(
				await fs.readFile(path.join(outDir, 'main/index.html'), 'utf8'),
				/url=\/scion\/overview\//,
			);
			// Empty channel (`nightly`) gracefully falls back to `/scion/overview/`.
			assert.match(
				await fs.readFile(path.join(outDir, 'nightly/index.html'), 'utf8'),
				/url=\/scion\/overview\//,
			);
		} finally {
			await fs.rm(tmpDir, { recursive: true, force: true });
		}
	});
});

describe('patchLegacyAstroConfig and prepareLegacySnapshotDir', () => {
	it('patches historical v0.3.0 and v0.4.0-preview.2 astro.config.mjs cleanly and idempotently', () => {
		const repoRoot = path.resolve(import.meta.dirname, '../..');
		for (const tag of ['v0.3.0', 'v0.4.0-preview.2']) {
			const raw = execFileSync('git', ['-C', repoRoot, 'show', `${tag}:docs-site/astro.config.mjs`], {
				encoding: 'utf8',
			});
			const patched = patchLegacyAstroConfig(raw);
			assert.match(patched, /base: buildVersion\.base,/);
			assert.match(patched, /scionRewriteBaseIntegration\(\{ base: buildVersion\.base \}\),/);
			assert.match(patched, /Banner: '\.\/src\/components\/Banner\.astro',/);
			assert.match(patched, /ThemeSelect: '\.\/src\/components\/ThemeSelect\.astro',/);
			assert.match(
				patched,
				/'\/advanced-local\/local-governance': `\$\{buildVersion\.base\}\/local\/local-governance\/`/,
			);
			// Historical sidebar entries remain intact:
			assert.match(patched, /slug: 'overview'/);
			// Idempotent on a second call:
			assert.equal(patchLegacyAstroConfig(patched), patched);
		}
	});

	it('is a no-op when targetDocsDir already contains src/lib/versioning.mjs', async () => {
		const mainDocsDir = path.resolve(import.meta.dirname, '..');
		const res = await prepareLegacySnapshotDir(mainDocsDir, mainDocsDir);
		assert.deepEqual(res, { backfilled: false });
	});
});
