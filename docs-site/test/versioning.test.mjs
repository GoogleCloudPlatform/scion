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
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { describe, it } from 'node:test';

import { scanSnapshotDistForUnprefixedLinks } from '../scripts/verify-snapshot-links.mjs';
import {
	loadVersionsManifest,
	navigateToVersionBase,
	resetManifestCacheForTests,
} from '../src/lib/manifest-client.mjs';
import {
	classifyTagChannel,
	compareReleaseVersions,
	ensureCurrentInManifest,
	extractRelativeDocPath,
	findNewerChannelRelease,
	getBuildVersionConfig,
	isSafeDocsBasePath,
	joinVersionUrl,
	normalizeBase,
	normalizeManifest,
	rewriteScionUrl,
} from '../src/lib/versioning.mjs';
import { markdownToHtml, mdxToJs } from 'satteri';
import {
	createSatteriHastRewritePlugin,
	createSatteriMdastRewritePlugin,
	rewriteFrontmatterLinks,
	rewriteHtmlSnippet,
	scionRewriteBaseIntegration,
} from '../src/plugins/rewrite-scion-base.mjs';

describe('normalizeBase & isSafeDocsBasePath', () => {
	it('defaults empty values to /scion and strips trailing slashes', () => {
		assert.equal(normalizeBase(undefined), '/scion');
		assert.equal(normalizeBase(''), '/scion');
		assert.equal(normalizeBase('/scion/'), '/scion');
		assert.equal(normalizeBase('scion/v0.3.0/'), '/scion/v0.3.0');
		assert.equal(normalizeBase('/scion/v0.4.0-preview.2/'), '/scion/v0.4.0-preview.2');
		assert.equal(normalizeBase('/scion/nightly-20261115'), '/scion/nightly-20261115');
	});

	it('rejects unsafe, traversal, external, or non-/scion paths', () => {
		for (const bad of [
			'//evil.example.com/scion',
			'https://evil.example.com/scion',
			'javascript:alert(1)',
			'/other-repo',
			'/scion-volumes/scratchpad',
			'/scion/../secret',
			'/scion/./v0.3.0',
		]) {
			assert.equal(isSafeDocsBasePath(bad), false, `expected ${bad} to be rejected`);
		}
		assert.throws(() => normalizeBase('/scion/../secret'), /Invalid DOCS_BASE/);
		assert.throws(() => normalizeBase('/other'), /Invalid DOCS_BASE/);
	});
});

describe('classifyTagChannel & getBuildVersionConfig', () => {
	it('classifies stable, preview/rc, nightly, and main refs accurately', () => {
		assert.equal(classifyTagChannel('main'), 'main');
		assert.equal(classifyTagChannel('v0.3.0'), 'stable');
		assert.equal(classifyTagChannel('refs/tags/v1.2.10'), 'stable');
		assert.equal(classifyTagChannel('v0.4.0-preview.2'), 'preview');
		assert.equal(classifyTagChannel('refs/tags/v0.4.0-rc.0'), 'preview');
		assert.equal(classifyTagChannel('nightly-20261115'), 'nightly');
		assert.equal(classifyTagChannel('v0.4.0-alpha.1'), null);
		assert.equal(classifyTagChannel('feature-branch'), null);
		assert.equal(classifyTagChannel(''), null);
	});

	it('derives build version config from env vars with safe defaults', () => {
		assert.deepEqual(getBuildVersionConfig({}), {
			base: '/scion',
			channel: 'main',
			version: 'main',
		});

		assert.deepEqual(
			getBuildVersionConfig({
				DOCS_BASE: '/scion/v0.4.0-preview.2/',
			}),
			{
				base: '/scion/v0.4.0-preview.2',
				channel: 'preview',
				version: 'v0.4.0-preview.2',
			},
		);

		assert.deepEqual(
			getBuildVersionConfig({
				DOCS_BASE: '/scion/v0.3.0',
				DOCS_CHANNEL: 'stable',
				DOCS_VERSION: 'v0.3.0',
			}),
			{
				base: '/scion/v0.3.0',
				channel: 'stable',
				version: 'v0.3.0',
			},
		);
	});

	it('rejects unclassifiable or contradictory snapshot build env configs', () => {
		assert.throws(
			() => getBuildVersionConfig({ DOCS_BASE: '/scion/foo' }),
			/Unclassifiable snapshot version/,
		);
		assert.throws(
			() => getBuildVersionConfig({ DOCS_BASE: '/scion/a/b' }),
			/Unclassifiable snapshot version/,
		);
		assert.throws(
			() =>
				getBuildVersionConfig({
					DOCS_BASE: '/scion/v0.3.0',
					DOCS_VERSION: 'nightly-20261115',
				}),
			/Mismatched DOCS_BASE/,
		);
		assert.throws(
			() =>
				getBuildVersionConfig({
					DOCS_BASE: '/scion/v0.3.0',
					DOCS_CHANNEL: 'nightly',
				}),
			/conflicts with tag/,
		);
		assert.throws(
			() => getBuildVersionConfig({ DOCS_BASE: '/scion', DOCS_VERSION: 'v0.3.0' }),
			/Invalid build version config for root base/,
		);
	});
});

describe('rewriteScionUrl', () => {
	it('leaves all URLs untouched when targetBase is /scion', () => {
		assert.equal(rewriteScionUrl('/scion/overview/', '/scion'), '/scion/overview/');
		assert.equal(rewriteScionUrl('/scion-volumes/work', '/scion'), '/scion-volumes/work');
	});

	it('rewrites internal /scion routes for snapshot bases while preserving boundaries', () => {
		const base = '/scion/v0.3.0';
		assert.equal(rewriteScionUrl('/scion', base), '/scion/v0.3.0');
		assert.equal(rewriteScionUrl('/scion/', base), '/scion/v0.3.0/');
		assert.equal(
			rewriteScionUrl('/scion/hosted/ha/kubernetes/#ingress', base),
			'/scion/v0.3.0/hosted/ha/kubernetes/#ingress',
		);
		assert.equal(
			rewriteScionUrl('/scion/overview/?tab=cli#install', base),
			'/scion/v0.3.0/overview/?tab=cli#install',
		);

		// Idempotent on already-prefixed paths (e.g. astro-d2 diagrams).
		assert.equal(
			rewriteScionUrl('/scion/v0.3.0/diagrams/arch.svg', base),
			'/scion/v0.3.0/diagrams/arch.svg',
		);

		// Must NOT rewrite /scion-volumes/... or /scion/versions.json or external URLs.
		assert.equal(
			rewriteScionUrl('/scion-volumes/scratchpad/notes.md', base),
			'/scion-volumes/scratchpad/notes.md',
		);
		assert.equal(rewriteScionUrl('/scion/versions.json', base), '/scion/versions.json');
		assert.equal(
			rewriteScionUrl('/scion/versions.json?ts=1', base),
			'/scion/versions.json?ts=1',
		);
		assert.equal(
			rewriteScionUrl('https://github.com/GoogleCloudPlatform/scion', base),
			'https://github.com/GoogleCloudPlatform/scion',
		);
		assert.equal(rewriteScionUrl('#section-anchor', base), '#section-anchor');
	});
});

describe('extractRelativeDocPath & joinVersionUrl', () => {
	it('extracts relative slugs across main and versioned snapshot bases', () => {
		assert.equal(
			extractRelativeDocPath('/scion/hosted/ha/kubernetes/', '/scion'),
			'hosted/ha/kubernetes/',
		);
		assert.equal(
			extractRelativeDocPath('/scion/v0.3.0/hosted/ha/kubernetes/', '/scion/v0.3.0'),
			'hosted/ha/kubernetes/',
		);
		assert.equal(extractRelativeDocPath('/scion/v0.3.0/', '/scion/v0.3.0'), 'overview/');
		assert.equal(extractRelativeDocPath('/scion', '/scion'), 'overview/');

		assert.equal(
			joinVersionUrl('/scion/v0.4.0-preview.2', 'hosted/ha/kubernetes/'),
			'/scion/v0.4.0-preview.2/hosted/ha/kubernetes/',
		);
		assert.equal(joinVersionUrl('/scion', '/overview/'), '/scion/overview/');
	});
});

describe('normalizeManifest & ensureCurrentInManifest', () => {
	it('sanitizes manifest entries, enforces canonical channel order, and drops unsafe paths', () => {
		const manifest = normalizeManifest({
			defaultChannel: 'main',
			channels: [
				{
					id: 'stable',
					latest: 'v0.3.0',
					versions: [
						{ version: 'v0.3.0', label: 'v0.3.0 (latest)', path: '/scion/v0.3.0/' },
						{ version: 'bad', label: 'bad', path: 'https://evil.example.com/scion' },
					],
				},
			],
		});

		assert.deepEqual(
			manifest.channels.map((c) => c.id),
			['main', 'stable', 'preview', 'nightly'],
		);
		const stable = manifest.channels.find((c) => c.id === 'stable');
		assert.equal(stable?.versions.length, 1);
		assert.equal(stable?.versions[0].path, '/scion/v0.3.0');
	});

	it('injects the current build into the manifest when not yet listed', () => {
		const empty = normalizeManifest(null);
		const merged = ensureCurrentInManifest(empty, {
			channel: 'preview',
			version: 'v0.4.0-preview.2',
			base: '/scion/v0.4.0-preview.2',
		});

		const mainCh = merged.channels.find((c) => c.id === 'main');
		assert.equal(mainCh?.versions[0]?.path, '/scion');

		const previewCh = merged.channels.find((c) => c.id === 'preview');
		assert.equal(previewCh?.latest, 'v0.4.0-preview.2');
		assert.equal(previewCh?.versions[0]?.path, '/scion/v0.4.0-preview.2');
	});
});

describe('Satteri Markdown & MDX base-path rewriter', () => {
	it('rewrites Markdown links, images, definitions, raw HTML, and frontmatter for snapshot builds', () => {
		const md = [
			'See [Install](/scion/getting-started/install/#docker) and [Ref][arch].',
			'',
			'![Diagram](/scion/diagrams/flow.svg)',
			'',
			'Keep `/scion-volumes/shared` and [vol](/scion-volumes/shared) untouched.',
			'',
			'<p><a href="/scion/overview/">Overview</a> <a href="/scion-volumes/x">Vol</a></p>',
			'',
			'[arch]: /scion/contributing/architecture/',
		].join('\n');

		const frontmatter = {
			hero: { actions: [{ link: '/scion/getting-started/install/' }] },
			prev: { link: '/scion/overview/' },
		};
		rewriteFrontmatterLinks(frontmatter, '/scion/v0.3.0');

		const { html } = markdownToHtml(md, {
			mdastPlugins: [() => createSatteriMdastRewritePlugin('/scion/v0.3.0')],
			hastPlugins: [() => createSatteriHastRewritePlugin('/scion/v0.3.0')],
		});

		assert.match(html, /href="\/scion\/v0\.3\.0\/getting-started\/install\/#docker"/);
		assert.match(html, /href="\/scion\/v0\.3\.0\/contributing\/architecture\/"/);
		assert.match(html, /src="\/scion\/v0\.3\.0\/diagrams\/flow\.svg"/);
		assert.match(html, /href="\/scion\/v0\.3\.0\/overview\/"/);
		assert.match(html, /href="\/scion-volumes\/shared"/);
		assert.match(html, /href="\/scion-volumes\/x"/);
		assert.equal(frontmatter.hero.actions[0].link, '/scion/v0.3.0/getting-started/install/');
		assert.equal(frontmatter.prev.link, '/scion/v0.3.0/overview/');
	});

	it('rewrites MDX JSX attributes and raw HTML snippets accurately', () => {
		const mdx = '<LinkCard href="/scion/local/templates/" title="Templates" />\n';
		const { code } = mdxToJs(mdx, {
			mdastPlugins: [() => createSatteriMdastRewritePlugin('/scion/nightly-20261115')],
			hastPlugins: [() => createSatteriHastRewritePlugin('/scion/nightly-20261115')],
		});

		assert.match(code, /\/scion\/nightly-20261115\/local\/templates\//);
		assert.equal(
			rewriteHtmlSnippet('<img src="/scion/hero.png" />', '/scion/v0.3.0'),
			'<img src="/scion/v0.3.0/hero.png" />',
		);
	});

	it('preserves renderer prototype chain when wrapping processor.createRenderer', async () => {
		class FakeRenderer {
			helperMethod() {
				return 'prototype-preserved';
			}
			/** @param {string} content @param {any} opts */
			async render(content, opts) {
				return { html: content, metadata: { frontmatter: opts?.frontmatter } };
			}
		}

		const fakeProcessor = {
			name: 'satteri',
			options: { mdastPlugins: [], hastPlugins: [] },
			async createRenderer() {
				return new FakeRenderer();
			},
		};

		const tmpCache = await fs.mkdtemp(path.join(os.tmpdir(), 'scion-astro-cache-'));
		try {
			const integration = scionRewriteBaseIntegration({ base: '/scion/v0.3.0' });
			const setupHook = integration.hooks['astro:config:setup'];
			assert.ok(setupHook);
			await setupHook(
				/** @type {any} */ ({
					command: 'build',
					config: {
						cacheDir: new URL(`file://${tmpCache}/`),
						markdown: { processor: fakeProcessor },
					},
				}),
			);

			const wrapped = await fakeProcessor.createRenderer();
			assert.ok(wrapped instanceof FakeRenderer);
			assert.equal(wrapped.helperMethod(), 'prototype-preserved');

			const fm = { prev: { link: '/scion/overview/' } };
			await wrapped.render('# Title', { frontmatter: fm });
			assert.equal(fm.prev.link, '/scion/v0.3.0/overview/');
		} finally {
			await fs.rm(tmpCache, { recursive: true, force: true });
		}
	});

	it('handles missing global process in getBuildVersionConfig default parameter', () => {
		const desc = Object.getOwnPropertyDescriptor(globalThis, 'process');
		assert.ok(desc);
		try {
			Object.defineProperty(globalThis, 'process', {
				value: undefined,
				configurable: true,
				writable: true,
			});
			assert.deepEqual(getBuildVersionConfig(), {
				base: '/scion',
				channel: 'main',
				version: 'main',
			});
		} finally {
			Object.defineProperty(globalThis, 'process', desc);
		}
	});
});

describe('manifest-client browser helpers', () => {
	it('caches /scion/versions.json and falls back gracefully on fetch error', async () => {
		resetManifestCacheForTests();
		const origFetch = globalThis.fetch;
		let calls = 0;
		globalThis.fetch = async () => {
			calls += 1;
			return /** @type {any} */ ({
				ok: true,
				json: async () => ({
					defaultChannel: 'main',
					channels: [
						{
							id: 'stable',
							latest: 'v0.3.0',
							versions: [{ version: 'v0.3.0', label: 'v0.3.0', path: '/scion/v0.3.0' }],
						},
					],
				}),
			});
		};

		try {
			const m1 = await loadVersionsManifest({
				channel: 'main',
				version: 'main',
				base: '/scion',
			});
			const m2 = await loadVersionsManifest({
				channel: 'main',
				version: 'main',
				base: '/scion',
			});
			assert.equal(calls, 1);
			assert.equal(m1.channels.find((c) => c.id === 'stable')?.latest, 'v0.3.0');
			assert.equal(m2.channels.find((c) => c.id === 'main')?.versions[0]?.path, '/scion');
		} finally {
			globalThis.fetch = origFetch;
			resetManifestCacheForTests();
		}
	});

	it('preserves slug on 200 HEAD and falls back to overview/ on 404', async () => {
		const origFetch = globalThis.fetch;
		const origWindow = globalThis.window;
		/** @type {string[]} */
		const assigned = [];

		globalThis.window = /** @type {any} */ ({
			location: {
				pathname: '/scion/hosted/ha/kubernetes/',
				hash: '#ingress',
				assign: (/** @type {string} */ url) => assigned.push(url),
			},
			setTimeout: globalThis.setTimeout.bind(globalThis),
			clearTimeout: globalThis.clearTimeout.bind(globalThis),
		});

		try {
			globalThis.fetch = async () => /** @type {any} */ ({ ok: true });
			await navigateToVersionBase('/scion/v0.3.0', '/scion');
			assert.equal(assigned.at(-1), '/scion/v0.3.0/hosted/ha/kubernetes/#ingress');

			globalThis.fetch = async () => /** @type {any} */ ({ ok: false, status: 404 });
			await navigateToVersionBase('/scion/v0.2.0', '/scion');
			assert.equal(assigned.at(-1), '/scion/v0.2.0/overview/');

			const countBeforeUnsafe = assigned.length;
			await navigateToVersionBase('https://evil.example.com/scion', '/scion');
			assert.equal(assigned.length, countBeforeUnsafe);
		} finally {
			globalThis.fetch = origFetch;
			globalThis.window = origWindow;
		}
	});
});

describe('compareReleaseVersions & findNewerChannelRelease', () => {
	it('orders semver releases, RCs, previews, and nightly dates accurately', () => {
		assert.ok((compareReleaseVersions('v0.4.0', 'v0.3.0') ?? 0) > 0);
		assert.ok((compareReleaseVersions('v0.4.0', 'v0.4.0-rc.1') ?? 0) > 0);
		assert.ok((compareReleaseVersions('v0.4.0-rc.0', 'v0.4.0-preview.9') ?? 0) > 0);
		assert.ok((compareReleaseVersions('v0.4.0-preview.10', 'v0.4.0-preview.2') ?? 0) > 0);
		assert.ok((compareReleaseVersions('nightly-20261115', 'nightly-20261114') ?? 0) > 0);
		assert.equal(compareReleaseVersions('v0.3.0', 'v0.3.0'), 0);
	});

	it('flags outdated snapshots without ever offering a downgrade when versions.json lags', () => {
		const staleManifest = normalizeManifest({
			channels: [
				{
					id: 'stable',
					latest: 'v0.3.0',
					versions: [
						{ version: 'v0.3.0', label: 'v0.3.0', path: '/scion/v0.3.0' },
						{ version: 'v0.2.0', label: 'v0.2.0', path: '/scion/v0.2.0' },
					],
				},
			],
		});

		// Viewing older v0.2.0 -> offers upgrade to v0.3.0.
		const upgrade = findNewerChannelRelease(staleManifest, {
			channel: 'stable',
			version: 'v0.2.0',
		});
		assert.equal(upgrade?.latestEntry.version, 'v0.3.0');

		// Viewing newer v0.4.0 before versions.json updates -> must NOT offer downgrade to v0.3.0!
		const mergedForNewer = ensureCurrentInManifest(staleManifest, {
			channel: 'stable',
			version: 'v0.4.0',
			base: '/scion/v0.4.0',
		});
		assert.equal(
			 mergedForNewer.channels.find((c) => c.id === 'stable')?.latest,
			'v0.4.0',
		);
		assert.equal(
			findNewerChannelRelease(staleManifest, { channel: 'stable', version: 'v0.4.0' }),
			null,
		);
		assert.equal(
			findNewerChannelRelease(mergedForNewer, { channel: 'stable', version: 'v0.4.0' }),
			null,
		);
	});
});

describe('scanSnapshotDistForUnprefixedLinks', () => {
	it('ignores banner back-to-main links and versions.json while catching unprefixed body links', async () => {
		const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), 'scion-docs-scan-'));
		try {
			await fs.writeFile(
				path.join(tmpDir, 'clean.html'),
				[
					'<scion-version-banner><a href="/scion/overview/">main</a></scion-version-banner>',
					'<a href="/scion/v0.3.0/overview/">ok</a>',
					'<a href="/scion-volumes/work">vol</a>',
				].join('\n'),
			);
			const cleanRes = await scanSnapshotDistForUnprefixedLinks(tmpDir, '/scion/v0.3.0');
			assert.equal(cleanRes.leaks.length, 0);

			await fs.writeFile(
				path.join(tmpDir, 'leak.html'),
				'<main><a href="/scion/local/tmux/">missed</a></main>',
			);
			const leakRes = await scanSnapshotDistForUnprefixedLinks(tmpDir, '/scion/v0.3.0');
			assert.equal(leakRes.leaks.length, 1);
			assert.equal(leakRes.leaks[0].url, '/scion/local/tmux/');
		} finally {
			await fs.rm(tmpDir, { recursive: true, force: true });
		}
	});
});
