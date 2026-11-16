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
import fs from 'node:fs/promises';
import path from 'node:path';
import { getBuildVersionConfig, MANIFEST_PATH, ROOT_BASE } from '../src/lib/versioning.mjs';

const BANNER_RE = /<scion-version-banner\b[\s\S]*?<\/scion-version-banner>/g;
const ATTR_RE = /\b(href|src)=(["'])(\/scion(?:[/?#][^"']*)?)\2/g;

/**
 * Recursively list all `.html` files under `dir`.
 *
 * @param {string} dir
 * @returns {Promise<string[]>}
 */
async function collectHtmlFiles(dir) {
	/** @type {string[]} */
	const results = [];
	const entries = await fs.readdir(dir, { withFileTypes: true });
	for (const entry of entries) {
		const full = path.join(dir, entry.name);
		if (entry.isDirectory()) {
			results.push(...(await collectHtmlFiles(full)));
		} else if (entry.isFile() && entry.name.endsWith('.html')) {
			results.push(full);
		}
	}
	return results;
}

/**
 * Scan rendered HTML in a snapshot `dist/` directory to ensure no internal `/scion/...`
 * `href` or `src` attributes escaped the `${expectedBase}` prefix (outside `<scion-version-banner>`,
 * which intentionally links back to `/scion/...` on `main`, and `/scion/versions.json`).
 *
 * @param {string} distDir
 * @param {string} expectedBase
 * @returns {Promise<{ htmlFiles: number, leaks: Array<{ file: string, attr: string, url: string }> }>}
 */
export async function scanSnapshotDistForUnprefixedLinks(distDir, expectedBase) {
	const files = await collectHtmlFiles(distDir);
	/** @type {Array<{ file: string, attr: string, url: string }>} */
	const leaks = [];

	for (const file of files) {
		const raw = await fs.readFile(file, 'utf8');
		const withoutBanner = raw.replace(BANNER_RE, '');
		for (const match of withoutBanner.matchAll(ATTR_RE)) {
			const attr = match[1];
			const url = match[3];
			const suffixIdx = url.search(/[?#]/);
			const pathname = suffixIdx === -1 ? url : url.slice(0, suffixIdx);
			if (pathname === MANIFEST_PATH) continue;
			if (pathname === expectedBase || pathname.startsWith(`${expectedBase}/`)) continue;
			leaks.push({ file: path.relative(distDir, file), attr, url });
		}
	}

	return { htmlFiles: files.length, leaks };
}

async function main() {
	const distDir = path.resolve(process.argv[2] || 'dist');
	const { base } = getBuildVersionConfig(process.env);
	if (base === ROOT_BASE) {
		console.error(
			`verify-snapshot-links: DOCS_BASE must be a snapshot subpath (got "${base}").`,
		);
		process.exit(1);
	}

	const { htmlFiles, leaks } = await scanSnapshotDistForUnprefixedLinks(distDir, base);
	if (leaks.length > 0) {
		console.error(
			`verify-snapshot-links: found ${leaks.length} unprefixed /scion link(s) across ${htmlFiles} HTML file(s):`,
		);
		for (const leak of leaks.slice(0, 25)) {
			console.error(`  ${leak.file}: ${leak.attr}="${leak.url}"`);
		}
		process.exit(1);
	}

	console.log(
		`verify-snapshot-links: ok (${htmlFiles} HTML file(s) scanned under ${base}, 0 unprefixed /scion links)`,
	);
}

if (import.meta.url === `file://${process.argv[1]}`) {
	main().catch((err) => {
		console.error(err);
		process.exit(1);
	});
}
