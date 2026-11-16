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

const OVERLAY_FILES = [
	'package.json',
	'package-lock.json',
	'public/versions.json',
	'scripts/verify-snapshot-links.mjs',
	'src/components/Banner.astro',
	'src/components/ThemeSelect.astro',
	'src/components/VersionSelect.astro',
	'src/lib/manifest-client.mjs',
	'src/lib/versioning.mjs',
	'src/plugins/rewrite-scion-base.mjs',
];

/**
 * Upgrade a pre-versioning `astro.config.mjs` source string so a historical tag checkout
 * (e.g. `v0.3.0` or `v0.4.0-preview.2`) keeps its own `sidebar` and `redirects` entries
 * while gaining `buildVersion.base`, `scionRewriteBaseIntegration`, and the version selector/banner.
 *
 * Fails closed with a descriptive error if expected config anchors are missing.
 *
 * @param {string} source
 * @returns {string}
 */
export function patchLegacyAstroConfig(source) {
	if (source.includes('scionRewriteBaseIntegration')) {
		return source;
	}

	const importAnchor = "import starlightLinksValidator from 'starlight-links-validator';";
	if (!source.includes(importAnchor)) {
		throw new Error('patchLegacyAstroConfig: missing starlightLinksValidator import anchor.');
	}

	let out = source.replace(
		importAnchor,
		[
			importAnchor,
			"import { getBuildVersionConfig } from './src/lib/versioning.mjs';",
			"import { scionRewriteBaseIntegration } from './src/plugins/rewrite-scion-base.mjs';",
			'',
			'const buildVersion = getBuildVersionConfig();',
		].join('\n'),
	);

	const baseRe = /\bbase:\s*['"]\/scion['"]\s*,/;
	if (!baseRe.test(out)) {
		throw new Error("patchLegacyAstroConfig: missing `base: '/scion',` anchor.");
	}
	out = out.replace(baseRe, 'base: buildVersion.base,');

	// Rewrite `redirects: { '/from': '/to', ... },` so targets carry `${buildVersion.base}` and trailing `/`.
	const redirectsRe = /\bredirects:\s*\{([\s\S]*?)\n([ \t]*)\},/;
	const redirectsMatch = redirectsRe.exec(out);
	if (redirectsMatch) {
		const indent = redirectsMatch[2];
		const rawEntries = redirectsMatch[1].replace(
			/(:\s*)'(\/[^']+)'/g,
			(_m, colon, target) => {
				const withSlash = target.endsWith('/') ? target : `${target}/`;
				return `${colon}\`\${buildVersion.base}${withSlash}\``;
			},
		);
		out = out.replace(redirectsRe, `redirects: {${rawEntries}\n${indent}},`);
	}

	const integrationsRe = /integrations:\s*\[\s*\n(\s*)d2\(\),/;
	if (!integrationsRe.test(out)) {
		throw new Error('patchLegacyAstroConfig: missing `integrations: [ d2(),` anchor.');
	}
	out = out.replace(
		integrationsRe,
		(_m, indent) =>
			`integrations: [\n${indent}scionRewriteBaseIntegration({ base: buildVersion.base }),\n${indent}d2(),`,
	);

	const starlightTitleRe = /(starlight\(\{[\s\S]*?\btitle:\s*['"][^'"]+['"]\s*,)/;
	if (!starlightTitleRe.test(out)) {
		throw new Error('patchLegacyAstroConfig: missing `starlight({ ... title: ...` anchor.');
	}
	out = out.replace(
		starlightTitleRe,
		`$1\n\t\t\tcomponents: {\n\t\t\t\tBanner: './src/components/Banner.astro',\n\t\t\t\tThemeSelect: './src/components/ThemeSelect.astro',\n\t\t\t},`,
	);

	return out;
}

/**
 * Overlay the versioning UI/build shell from `mainDocsDir` onto `targetDocsDir` when `targetDocsDir`
 * comes from a historical tag prior to PR 1.
 *
 * @param {string} mainDocsDir
 * @param {string} targetDocsDir
 * @returns {Promise<{ backfilled: boolean }>}
 */
export async function prepareLegacySnapshotDir(mainDocsDir, targetDocsDir) {
	const versioningPath = path.join(targetDocsDir, 'src/lib/versioning.mjs');
	try {
		await fs.access(versioningPath);
		return { backfilled: false };
	} catch {
		// Legacy tag — proceed with backfill overlay.
	}

	for (const rel of OVERLAY_FILES) {
		const src = path.join(mainDocsDir, rel);
		const dst = path.join(targetDocsDir, rel);
		await fs.mkdir(path.dirname(dst), { recursive: true });
		await fs.copyFile(src, dst);
	}

	const astroConfigPath = path.join(targetDocsDir, 'astro.config.mjs');
	const rawConfig = await fs.readFile(astroConfigPath, 'utf8');
	const patchedConfig = patchLegacyAstroConfig(rawConfig);
	await fs.writeFile(astroConfigPath, patchedConfig, 'utf8');

	return { backfilled: true };
}

/**
 * @param {string[]} argv
 */
function parseArgs(argv) {
	/** @type {Record<string, string>} */
	const out = {};
	for (let i = 0; i < argv.length; i += 1) {
		const arg = argv[i];
		if (arg === '--main-docs' && argv[i + 1]) {
			out.mainDocs = argv[++i];
		} else if (arg === '--target-docs' && argv[i + 1]) {
			out.targetDocs = argv[++i];
		} else {
			throw new Error(`Unknown or incomplete argument: ${arg}`);
		}
	}
	if (!out.mainDocs || !out.targetDocs) {
		throw new Error(
			'Usage: prepare-legacy-snapshot.mjs --main-docs <path> --target-docs <path>',
		);
	}
	return out;
}

async function main() {
	const { mainDocs, targetDocs } = parseArgs(process.argv.slice(2));
	const { backfilled } = await prepareLegacySnapshotDir(
		path.resolve(mainDocs),
		path.resolve(targetDocs),
	);
	if (backfilled) {
		console.log(`prepare-legacy-snapshot: overlaid versioning shell into ${targetDocs}`);
	} else {
		console.log(`prepare-legacy-snapshot: ${targetDocs} already has native versioning support`);
	}
}

if (import.meta.url === `file://${process.argv[1]}`) {
	main().catch((err) => {
		console.error(err);
		process.exit(1);
	});
}
