#!/usr/bin/env node
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

// @ts-check
import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

import {
	CHANNEL_DEFINITIONS,
	classifyTagChannel,
	compareReleaseVersions,
	MAIN_VERSION_LABEL,
	ROOT_BASE,
} from '../src/lib/versioning.mjs';

const SITE_ORIGIN = 'https://googlecloudplatform.github.io';
const RESERVED_TOP_LEVEL_DIRS = new Set([
	'_astro',
	'contributing',
	'd2',
	'getting-started',
	'hosted',
	'landing',
	'local',
	'main',
	'nightly',
	'overview',
	'pagefind',
	'preview',
	'reference',
	'release-notes',
	'release-notes-archive',
	'stable',
	'workstation',
]);

/**
 * @typedef {Object} ReleaseMetadata
 * @property {string} tagName
 * @property {boolean} [isDraft]
 * @property {string} [publishedAt]
 * @property {Array<{ name: string }>} [assets]
 */

/**
 * Return the expected tarball and checksum asset names for a tag.
 *
 * @param {string} tag
 * @returns {{ tarballName: string, checksumName: string }}
 */
export function snapshotAssetNames(tag) {
	const tarballName = `scion-docs-${tag}.tar.gz`;
	return {
		tarballName,
		checksumName: `${tarballName}.sha256`,
	};
}

/**
 * Compute the `YYYYMMDD` UTC cutoff string `windowDays` prior to `now`.
 *
 * @param {Date} now
 * @param {number} windowDays
 * @returns {string}
 */
export function computeNightlyCutoffDate(now, windowDays = 14) {
	const cutoffMs = now.getTime() - windowDays * 24 * 60 * 60 * 1000;
	const d = new Date(cutoffMs);
	const yyyy = String(d.getUTCFullYear()).padStart(4, '0');
	const mm = String(d.getUTCMonth() + 1).padStart(2, '0');
	const dd = String(d.getUTCDate()).padStart(2, '0');
	return `${yyyy}${mm}${dd}`;
}

/**
 * Filter and sort GitHub Releases that carry verified docs snapshot tarballs according
 * to the approved retention policy:
 * - `stable`: all non-draft `vX.Y.Z` releases with snapshot assets (newest first)
 * - `preview`: up to `maxActivePreviews` (default 1, the single most recent) active
 *   `vX.Y.Z-(preview|rc).N` releases whose target `vX.Y.Z` has not yet been promoted to `stable`
 *   and (for `.0` previews) whose minor line has not been superseded by a higher `.0` stable
 *   release
 * - `nightly`: `nightly-YYYYMMDD` releases within `nightlyWindowDays` (default 14 days, newest first)
 *
 * Note: `allStableTags` is computed from ALL non-draft GitHub releases (even historical stable
 * releases without a docs tarball) so promoted preview lines are still pruned accurately.
 *
 * @param {ReleaseMetadata[]} releases
 * @param {{ now?: Date, maxActivePreviews?: number, nightlyWindowDays?: number }} [options]
 * @returns {{ stable: ReleaseMetadata[], preview: ReleaseMetadata[], nightly: ReleaseMetadata[] }}
 */
export function selectRetainedReleases(releases, options = {}) {
	const now = options.now ?? new Date();
	const maxActivePreviews = options.maxActivePreviews ?? 1;
	const nightlyWindowDays = options.nightlyWindowDays ?? 14;
	const nightlyCutoff = computeNightlyCutoffDate(now, nightlyWindowDays);

	const allStableTags = new Set();
	/** @type {Array<[number, number]>} */
	const allStableMinorLines = [];

	for (const rel of releases) {
		if (!rel || rel.isDraft || typeof rel.tagName !== 'string') continue;
		const tag = rel.tagName.trim();
		if (classifyTagChannel(tag) === 'stable') {
			allStableTags.add(tag);
			const m = /^v([0-9]+)\.([0-9]+)\.([0-9]+)$/.exec(tag);
			if (m) {
				allStableMinorLines.push([Number(m[1]), Number(m[2])]);
			}
		}
	}

	/** @type {ReleaseMetadata[]} */
	const stable = [];
	/** @type {ReleaseMetadata[]} */
	const previewCandidates = [];
	/** @type {ReleaseMetadata[]} */
	const nightly = [];

	for (const rel of releases) {
		if (!rel || rel.isDraft || typeof rel.tagName !== 'string') continue;
		const tag = rel.tagName.trim();
		const channel = classifyTagChannel(tag);
		if (!channel || channel === 'main') continue;

		const { tarballName, checksumName } = snapshotAssetNames(tag);
		const assetNames = new Set((rel.assets || []).map((a) => a.name));
		if (!assetNames.has(tarballName) || !assetNames.has(checksumName)) continue;

		if (channel === 'stable') {
			stable.push(rel);
		} else if (channel === 'preview') {
			const m = /^v([0-9]+)\.([0-9]+)\.([0-9]+)-(?:preview|rc)\.[0-9]+$/.exec(tag);
			if (!m) continue;
			const major = Number(m[1]);
			const minor = Number(m[2]);
			const patch = Number(m[3]);
			const baseStableTag = `v${major}.${minor}.${patch}`;
			if (allStableTags.has(baseStableTag)) {
				// Promoted to stable — prune pre-releases for that version.
				continue;
			}
			if (
				patch === 0 &&
				allStableMinorLines.some(
					([sMaj, sMin]) => sMaj > major || (sMaj === major && sMin >= minor),
				)
			) {
				// Superseded minor line — prune.
				continue;
			}
			previewCandidates.push(rel);
		} else if (channel === 'nightly') {
			const datePart = tag.slice('nightly-'.length);
			if (datePart.slice(0, 8) >= nightlyCutoff) {
				nightly.push(rel);
			}
		}
	}

	const sortNewestFirst = (/** @type {ReleaseMetadata} */ a, /** @type {ReleaseMetadata} */ b) =>
		(compareReleaseVersions(b.tagName, a.tagName) ?? 0);

	stable.sort(sortNewestFirst);
	previewCandidates.sort(sortNewestFirst);
	nightly.sort(sortNewestFirst);

	return {
		stable,
		preview: previewCandidates.slice(0, maxActivePreviews),
		nightly,
	};
}

/**
 * Build the `/scion/versions.json` manifest from retained releases per channel.
 *
 * @param {{ stable: ReleaseMetadata[], preview: ReleaseMetadata[], nightly: ReleaseMetadata[] }} retained
 * @returns {import('../src/lib/versioning.mjs').VersionsManifest}
 */
export function buildVersionsManifest(retained) {
	/**
	 * @param {ReleaseMetadata[]} list
	 * @returns {import('../src/lib/versioning.mjs').VersionEntry[]}
	 */
	const toEntries = (list) =>
		list.map((rel, idx) => {
			const tag = rel.tagName.trim();
			/** @type {import('../src/lib/versioning.mjs').VersionEntry} */
			const entry = {
				version: tag,
				label: idx === 0 ? `${tag} (latest)` : tag,
				path: `${ROOT_BASE}/${tag}`,
			};
			if (rel.publishedAt) {
				entry.date = rel.publishedAt.slice(0, 10);
			}
			return entry;
		});

	const byChannel = {
		main: [
			{
				version: 'main',
				label: MAIN_VERSION_LABEL,
				path: ROOT_BASE,
			},
		],
		stable: toEntries(retained.stable),
		preview: toEntries(retained.preview),
		nightly: toEntries(retained.nightly),
	};

	return {
		defaultChannel: 'main',
		channels: CHANNEL_DEFINITIONS.map((def) => {
			const versions = byChannel[def.id];
			return {
				id: def.id,
				label: def.label,
				latest: versions[0]?.version || '',
				versions,
			};
		}),
	};
}

/**
 * Render an HTML redirect page for a channel alias (`/scion/stable/`, `/scion/preview/`, etc.).
 *
 * @param {string} targetPath
 * @param {string} channelLabel
 * @returns {string}
 */
export function renderRedirectHtml(targetPath, channelLabel) {
	const safeTarget = targetPath.replace(/[&<>"']/g, (ch) => {
		const map = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
		return map[/** @type {keyof typeof map} */ (ch)];
	});
	const safeLabel = channelLabel.replace(/[&<>"']/g, '');
	const canonical = `${SITE_ORIGIN}${safeTarget}`;
	return [
		'<!doctype html>',
		'<html lang="en" data-pagefind-ignore>',
		'<head>',
		'  <meta charset="utf-8">',
		`  <meta http-equiv="refresh" content="0;url=${safeTarget}">`,
		'  <meta name="robots" content="noindex">',
		`  <link rel="canonical" href="${canonical}">`,
		`  <title>Redirecting to ${safeLabel} documentation...</title>`,
		'</head>',
		'<body>',
		`  <p>Redirecting to <a href="${safeTarget}">${safeLabel} documentation</a>...</p>`,
		'</body>',
		'</html>',
		'',
	].join('\n');
}

const TAR_VERBOSE_PATH_RE =
	/^[d-].*?(?:\s[0-9]{4}-[0-9]{2}-[0-9]{2}\s+[0-9]{2}:[0-9]{2}(?::[0-9]{2})?|\s[A-Za-z]{3}\s+[0-9]{1,2}\s+(?:[0-9]{2}:[0-9]{2}|[0-9]{4}))\s(.+)$/;

/**
 * Validate a single tar entry path, rejecting empty paths, absolute paths, Windows backslashes,
 * and `..` traversal segments.
 *
 * @param {string} rawEntryPath
 */
function verifyTarEntryPathSafe(rawEntryPath) {
	const entryPath = rawEntryPath.replace(/\r$/, '');
	if (!entryPath.trim()) {
		throw new Error('Empty tar entry path rejected.');
	}
	if (entryPath.startsWith('/') || entryPath.includes('\\')) {
		throw new Error(`Absolute or backslash tar entry path rejected: ${entryPath}`);
	}
	const cleaned = entryPath.replace(/^\.\//, '');
	const segments = cleaned.split('/').filter(Boolean);
	if (segments.some((seg) => seg === '..')) {
		throw new Error(`Path traversal tar entry rejected: ${entryPath}`);
	}
}

/**
 * Validate `tar -tvzf` listing lines (and optional `tar -tzf` plain path lines) to block path
 * traversal (`..`), absolute paths, backslashes, and symbolic/hard links (`l`, `h`, block/char
 * devices) before extracting an archive. Supports both GNU tar (`YYYY-MM-DD HH:MM`) and BSD/macOS
 * tar (`Mon DD HH:MM` / `Mon DD  YYYY`) verbose listings and fails closed on unparseable lines.
 *
 * @param {string} verboseListing
 * @param {string} [plainListing]
 */
export function verifyTarListingSafe(verboseListing, plainListing) {
	const lines = verboseListing.split('\n').filter((line) => line.trim().length > 0);
	if (lines.length === 0) {
		throw new Error('Archive is empty.');
	}
	for (const line of lines) {
		const typeChar = line[0];
		if (typeChar !== '-' && typeChar !== 'd') {
			throw new Error(`Unsafe tar entry type "${typeChar}" in archive line: ${line}`);
		}
		const match = TAR_VERBOSE_PATH_RE.exec(line);
		if (!match) {
			throw new Error(`Could not parse tar entry path from line: ${line}`);
		}
		verifyTarEntryPathSafe(match[1]);
	}

	if (plainListing !== undefined) {
		const plainLines = plainListing
			.split('\n')
			.map((l) => l.replace(/\r$/, ''))
			.filter((l) => l.length > 0);
		if (plainLines.length !== lines.length) {
			throw new Error(
				`Tar listing mismatch: verbose had ${lines.length} entries, plain had ${plainLines.length}.`,
			);
		}
		for (const rawPath of plainLines) {
			verifyTarEntryPathSafe(rawPath);
		}
	}
}

/**
 * Verify a snapshot tarball's SHA-256 checksum and extract it safely into `<pagesRoot>/<tag>/`.
 *
 * @param {{ tarPath: string, shaPath: string, pagesRoot: string, tag: string }} params
 */
export async function extractVerifiedSnapshotTarball({ tarPath, shaPath, pagesRoot, tag }) {
	if (!classifyTagChannel(tag) || classifyTagChannel(tag) === 'main') {
		throw new Error(`Invalid snapshot tag "${tag}".`);
	}
	if (RESERVED_TOP_LEVEL_DIRS.has(tag)) {
		throw new Error(`Tag "${tag}" collides with a reserved top-level docs directory.`);
	}

	const expectedRaw = (await fs.readFile(shaPath, 'utf8')).trim();
	const expectedHex = expectedRaw.split(/\s+/)[0]?.toLowerCase() || '';
	if (!/^[a-f0-9]{64}$/.test(expectedHex)) {
		throw new Error(`Malformed SHA-256 checksum file ${shaPath}: "${expectedRaw}"`);
	}

	const buf = await fs.readFile(tarPath);
	const actualHex = crypto.createHash('sha256').update(buf).digest('hex');
	if (actualHex !== expectedHex) {
		throw new Error(
			`SHA-256 mismatch for ${path.basename(tarPath)}: expected ${expectedHex}, got ${actualHex}`,
		);
	}

	const listing = execFileSync('tar', ['-tvzf', tarPath], {
		encoding: 'utf8',
		maxBuffer: 32 * 1024 * 1024,
	});
	const plainListing = execFileSync('tar', ['-tzf', tarPath], {
		encoding: 'utf8',
		maxBuffer: 32 * 1024 * 1024,
	});
	verifyTarListingSafe(listing, plainListing);

	const destDir = path.join(pagesRoot, tag);
	await fs.rm(destDir, { recursive: true, force: true });
	await fs.mkdir(destDir, { recursive: true });
	execFileSync('tar', ['--no-same-owner', '--no-same-permissions', '-xzf', tarPath, '-C', destDir]);
}

/**
 * Assemble the complete multi-version GitHub Pages directory from:
 * - `mainDistDir` (`main` build output for `/scion/`)
 * - `releases` metadata + `resolveAssets(tag)` callback providing `{ tarPath, shaPath }`
 *
 * @param {{
 *   mainDistDir: string,
 *   outDir: string,
 *   releases: ReleaseMetadata[],
 *   resolveAssets: (tag: string) => Promise<{ tarPath: string, shaPath: string }>,
 *   now?: Date,
 * }} params
 */
export async function assemblePagesSite({ mainDistDir, outDir, releases, resolveAssets, now }) {
	await fs.rm(outDir, { recursive: true, force: true });
	await fs.cp(mainDistDir, outDir, { recursive: true });

	const retained = selectRetainedReleases(releases, { now });
	const allRetained = [...retained.stable, ...retained.preview, ...retained.nightly];

	for (const rel of allRetained) {
		const tag = rel.tagName.trim();
		const { tarPath, shaPath } = await resolveAssets(tag);
		await extractVerifiedSnapshotTarball({ tarPath, shaPath, pagesRoot: outDir, tag });
	}

	const manifest = buildVersionsManifest(retained);
	await fs.writeFile(
		path.join(outDir, 'versions.json'),
		`${JSON.stringify(manifest, null, 2)}\n`,
		'utf8',
	);

	// Write `/scion/{main,stable,preview,nightly}/index.html` channel alias redirects.
	for (const ch of manifest.channels) {
		const aliasDir = path.join(outDir, ch.id);
		await fs.mkdir(aliasDir, { recursive: true });
		const latestEntry = ch.versions[0];
		const targetBase = latestEntry ? latestEntry.path : ROOT_BASE;
		const targetUrl = `${targetBase}/overview/`;
		await fs.writeFile(
			path.join(aliasDir, 'index.html'),
			renderRedirectHtml(targetUrl, ch.label),
			'utf8',
		);
	}

	return { manifest, retainedCount: allRetained.length };
}

/**
 * Fetch release metadata from GitHub via `gh api`.
 *
 * @param {string} repo
 * @returns {ReleaseMetadata[]}
 */
function fetchGitHubReleases(repo) {
	const raw = execFileSync(
		'gh',
		['api', `repos/${repo}/releases?per_page=100`, '--paginate', '--slurp'],
		{ encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 },
	);
	const pages = JSON.parse(raw);
	const flat = Array.isArray(pages) ? pages.flat() : [];
	return flat.map((/** @type {{ tag_name: string, draft?: boolean, published_at?: string, created_at?: string, assets?: Array<{ name: string }> }} */ r) => ({
		tagName: r.tag_name,
		isDraft: Boolean(r.draft),
		publishedAt: r.published_at || r.created_at || '',
		assets: Array.isArray(r.assets) ? r.assets.map((/** @type {{ name: string }} */ a) => ({ name: a.name })) : [],
	}));
}

/**
 * @param {string[]} argv
 */
function parseCliArgs(argv) {
	/** @type {Record<string, string>} */
	const args = {};
	for (let i = 0; i < argv.length; i += 1) {
		const flag = argv[i];
		const val = argv[i + 1];
		if (
			(flag === '--main-dist' ||
				flag === '--out-dir' ||
				flag === '--repo' ||
				flag === '--releases-json' ||
				flag === '--assets-dir') &&
			val
		) {
			args[flag.slice(2)] = val;
			i += 1;
		} else {
			throw new Error(`Unknown or incomplete argument: ${flag}`);
		}
	}
	if (!args['main-dist'] || !args['out-dir']) {
		throw new Error(
			'Usage: assemble-pages.mjs --main-dist <dir> --out-dir <dir> [--repo <owner/repo> | --releases-json <file> --assets-dir <dir>]',
		);
	}
	return args;
}

async function main() {
	const args = parseCliArgs(process.argv.slice(2));
	const mainDistDir = path.resolve(args['main-dist']);
	const outDir = path.resolve(args['out-dir']);

	/** @type {ReleaseMetadata[]} */
	let releases = [];
	/** @type {string | null} */
	let downloadTmpDir = null;

	try {
		/** @type {(tag: string) => Promise<{ tarPath: string, shaPath: string }>} */
		let resolveAssets;

		if (args['releases-json']) {
			releases = JSON.parse(await fs.readFile(path.resolve(args['releases-json']), 'utf8'));
			const assetsDir = path.resolve(args['assets-dir'] || '.');
			resolveAssets = async (tag) => {
				const { tarballName, checksumName } = snapshotAssetNames(tag);
				return {
					tarPath: path.join(assetsDir, tarballName),
					shaPath: path.join(assetsDir, checksumName),
				};
			};
		} else if (args.repo) {
			releases = fetchGitHubReleases(args.repo);
			downloadTmpDir = await fs.mkdtemp(path.join(os.tmpdir(), 'scion-docs-assets-'));
			const dlRoot = downloadTmpDir;
			resolveAssets = async (tag) => {
				const { tarballName, checksumName } = snapshotAssetNames(tag);
				const tagDir = path.join(dlRoot, tag);
				await fs.mkdir(tagDir, { recursive: true });
				execFileSync(
					'gh',
					[
						'release',
						'download',
						tag,
						'--repo',
						args.repo,
						'--dir',
						tagDir,
						'--pattern',
						tarballName,
						'--pattern',
						checksumName,
					],
					{ stdio: 'inherit' },
				);
				return {
					tarPath: path.join(tagDir, tarballName),
					shaPath: path.join(tagDir, checksumName),
				};
			};
		} else {
			throw new Error('Provide either --repo <owner/repo> or --releases-json <file>.');
		}

		const { manifest, retainedCount } = await assemblePagesSite({
			mainDistDir,
			outDir,
			releases,
			resolveAssets,
		});

		const summary = manifest.channels
			.map((ch) => `${ch.id}:${ch.versions.length}`)
			.join(', ');
		console.log(
			`assemble-pages: assembled main + ${retainedCount} tag snapshot(s) (${summary}) into ${outDir}`,
		);
	} finally {
		if (downloadTmpDir) {
			await fs.rm(downloadTmpDir, { recursive: true, force: true });
		}
	}
}

if (import.meta.url === `file://${process.argv[1]}`) {
	main().catch((err) => {
		console.error(err);
		process.exit(1);
	});
}
