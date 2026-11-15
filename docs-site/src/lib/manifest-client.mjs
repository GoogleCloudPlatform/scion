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
import {
	ensureCurrentInManifest,
	extractRelativeDocPath,
	isSafeDocsBasePath,
	joinVersionUrl,
	MANIFEST_PATH,
	normalizeManifest,
} from './versioning.mjs';

/** @type {Promise<import('./versioning.mjs').VersionsManifest> | null} */
let rawManifestPromise = null;

/** Reset the cached manifest promise (used by unit tests). */
export function resetManifestCacheForTests() {
	rawManifestPromise = null;
}

/**
 * Fetch `/scion/versions.json` at most once per page load and merge with the current build's metadata.
 *
 * @param {{ channel: import('./versioning.mjs').ChannelId, version: string, base: string }} current
 * @param {string} [manifestUrl]
 * @returns {Promise<import('./versioning.mjs').VersionsManifest>}
 */
export async function loadVersionsManifest(current, manifestUrl = MANIFEST_PATH) {
	if (!rawManifestPromise) {
		rawManifestPromise = (async () => {
			try {
				const res = await fetch(manifestUrl, { cache: 'no-cache' });
				if (!res.ok) return normalizeManifest(null);
				const data = await res.json();
				return normalizeManifest(data);
			} catch {
				return normalizeManifest(null);
			}
		})();
	}
	const baseManifest = await rawManifestPromise;
	return ensureCurrentInManifest(baseManifest, current);
}

/**
 * Navigate to `targetBasePath`, preserving the current relative page slug when that page exists
 * in the target snapshot and falling back to `<targetBasePath>/overview/` otherwise.
 *
 * @param {string} targetBasePath
 * @param {string} currentBase
 */
export async function navigateToVersionBase(targetBasePath, currentBase) {
	if (!isSafeDocsBasePath(targetBasePath)) return;

	const relSlug = extractRelativeDocPath(window.location.pathname, currentBase);
	const fallbackUrl = joinVersionUrl(targetBasePath, 'overview/');

	if (!relSlug || relSlug === 'overview/' || relSlug === 'overview') {
		window.location.assign(fallbackUrl);
		return;
	}

	const candidateUrl = joinVersionUrl(targetBasePath, relSlug);
	const hash = window.location.hash || '';

	const controller = new AbortController();
	const timeoutId = window.setTimeout(() => controller.abort(), 2500);
	try {
		const res = await fetch(candidateUrl, {
			method: 'HEAD',
			signal: controller.signal,
		});
		if (res.ok) {
			window.location.assign(`${candidateUrl}${hash}`);
			return;
		}
	} catch {
		// Network error or timeout — fall back to overview cleanly.
	} finally {
		window.clearTimeout(timeoutId);
	}

	window.location.assign(fallbackUrl);
}
