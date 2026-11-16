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

export const ROOT_BASE = '/scion';
export const MANIFEST_PATH = '/scion/versions.json';
export const MAIN_VERSION_LABEL = 'HEAD (latest)';

/**
 * Canonical ordered release channels for the documentation selector.
 * Matches `@ptone@google.com`'s approved channel taxonomy:
 * - `main`: bleeding-edge HEAD on `main` (served at `/scion/`)
 * - `stable`: `vX.Y.Z` production releases
 * - `preview`: `vX.Y.Z-preview.N` and `vX.Y.Z-rc.N` pre-releases
 * - `nightly`: `nightly-YYYYMMDD` daily snapshots (14-day window)
 */
export const CHANNEL_DEFINITIONS = /** @type {const} */ ([
	{ id: 'main', label: 'main' },
	{ id: 'stable', label: 'Stable' },
	{ id: 'preview', label: 'Preview' },
	{ id: 'nightly', label: 'Nightly' },
]);

/** @typedef {'main' | 'stable' | 'preview' | 'nightly'} ChannelId */

/**
 * @typedef {Object} VersionEntry
 * @property {string} version
 * @property {string} label
 * @property {string} path
 * @property {string} [date]
 */

/**
 * @typedef {Object} ChannelEntry
 * @property {ChannelId} id
 * @property {string} label
 * @property {string} latest
 * @property {VersionEntry[]} versions
 */

/**
 * @typedef {Object} VersionsManifest
 * @property {ChannelId} defaultChannel
 * @property {ChannelEntry[]} channels
 */

const VALID_CHANNELS = new Set(CHANNEL_DEFINITIONS.map((c) => c.id));

/**
 * Safe docs base path under `/scion`: `/scion` or `/scion/<safe-segment>[/...]`.
 * Rejects protocol-relative (`//`), external schemes, `..` traversal, and paths outside `/scion`.
 */
const SAFE_DOCS_BASE_RE = /^\/scion(?:\/[A-Za-z0-9][A-Za-z0-9._+-]*)*\/?$/;

const STABLE_TAG_RE = /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const PREVIEW_TAG_RE =
	/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(preview|rc)\.(0|[1-9][0-9]*)$/;
const NIGHTLY_TAG_RE = /^nightly-[0-9]{8,}$/;

/**
 * Normalize a configured base path (e.g. `'/scion/v0.3.0/'` -> `'/scion/v0.3.0'`).
 * Falls back to `'/scion'` when empty.
 *
 * @param {string | undefined | null} rawBase
 * @returns {string}
 */
export function normalizeBase(rawBase) {
	if (!rawBase || typeof rawBase !== 'string') return ROOT_BASE;
	const trimmed = rawBase.trim();
	if (!trimmed) return ROOT_BASE;
	const withLeading = trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
	const stripped = withLeading.length > 1 ? withLeading.replace(/\/+$/, '') : withLeading;
	if (!isSafeDocsBasePath(stripped)) {
		throw new Error(`Invalid DOCS_BASE "${rawBase}": must be "${ROOT_BASE}" or a safe subpath under "${ROOT_BASE}/".`);
	}
	return stripped;
}

/**
 * Check that a candidate base path from config or `versions.json` is a safe same-origin `/scion` path.
 *
 * @param {unknown} candidate
 * @returns {candidate is string}
 */
export function isSafeDocsBasePath(candidate) {
	if (typeof candidate !== 'string' || !SAFE_DOCS_BASE_RE.test(candidate)) {
		return false;
	}
	const segments = candidate.split('/').filter(Boolean);
	return segments[0] === 'scion' && segments.every((seg) => seg !== '.' && seg !== '..');
}

/**
 * Classify a git tag (or `'main'`) into its release channel.
 *
 * @param {string | undefined | null} refName
 * @returns {ChannelId | null}
 */
export function classifyTagChannel(refName) {
	if (!refName || typeof refName !== 'string') return null;
	const tag = refName.trim().replace(/^refs\/tags\//, '');
	if (tag === 'main') return 'main';
	if (NIGHTLY_TAG_RE.test(tag)) return 'nightly';
	if (PREVIEW_TAG_RE.test(tag)) return 'preview';
	if (STABLE_TAG_RE.test(tag)) return 'stable';
	return null;
}

/**
 * Read and validate build-time versioning configuration from environment variables.
 * Rejects contradictory or unclassifiable snapshot configurations so misconfigured builds
 * fail fast instead of publishing broken metadata.
 *
 * @param {Record<string, string | undefined>} [env]
 * @returns {{ base: string, channel: ChannelId, version: string }}
 */
export function getBuildVersionConfig(env = typeof process !== 'undefined' ? process.env : {}) {
	const base = normalizeBase(env.DOCS_BASE);
	const rawChannel = (env.DOCS_CHANNEL || '').trim().toLowerCase();
	if (rawChannel && !VALID_CHANNELS.has(/** @type {ChannelId} */ (rawChannel))) {
		throw new Error(`Invalid DOCS_CHANNEL "${env.DOCS_CHANNEL}": must be one of main, stable, preview, nightly.`);
	}

	if (base === ROOT_BASE) {
		const rawVersion = ((env.DOCS_VERSION || '').trim() || 'main').replace(/^refs\/tags\//, '');
		if (rawVersion !== 'main' || (rawChannel && rawChannel !== 'main')) {
			throw new Error(
				`Invalid build version config for root base "${ROOT_BASE}": expected channel/version "main", got channel="${rawChannel || 'main'}", version="${rawVersion}".`,
			);
		}
		return { base, channel: 'main', version: 'main' };
	}

	const derivedTag = base.slice(ROOT_BASE.length + 1);
	const version = ((env.DOCS_VERSION || '').trim() || derivedTag).replace(/^refs\/tags\//, '');
	if (base !== `${ROOT_BASE}/${version}`) {
		throw new Error(
			`Mismatched DOCS_BASE "${base}" and DOCS_VERSION "${version}": expected DOCS_BASE="${ROOT_BASE}/${version}".`,
		);
	}

	const inferredChannel = classifyTagChannel(version);
	if (!inferredChannel || inferredChannel === 'main') {
		throw new Error(
			`Unclassifiable snapshot version "${version}": must match vX.Y.Z, vX.Y.Z-(preview|rc).N, or nightly-YYYYMMDD.`,
		);
	}
	if (rawChannel && rawChannel !== inferredChannel) {
		throw new Error(
			`DOCS_CHANNEL "${rawChannel}" conflicts with tag "${version}" (classified as "${inferredChannel}").`,
		);
	}

	return { base, channel: inferredChannel, version };
}

/**
 * Compare two release version identifiers within the same family (`vX.Y.Z[-(preview|rc).N]` or `nightly-YYYYMMDD`).
 * Returns `> 0` when `a` is newer than `b`, `< 0` when `a` is older than `b`, `0` when equal,
 * or `null` when the two identifiers are not comparable.
 *
 * @param {string} a
 * @param {string} b
 * @returns {number | null}
 */
export function compareReleaseVersions(a, b) {
	if (a === b) return 0;

	const nightlyA = /^nightly-([0-9]{8,})$/.exec(a);
	const nightlyB = /^nightly-([0-9]{8,})$/.exec(b);
	if (nightlyA && nightlyB) {
		if (nightlyA[1].length !== nightlyB[1].length) {
			return nightlyA[1].length - nightlyB[1].length;
		}
		return nightlyA[1].localeCompare(nightlyB[1]);
	}

	const semverRe =
		/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(preview|rc)\.(0|[1-9][0-9]*))?$/;
	const sa = semverRe.exec(a);
	const sb = semverRe.exec(b);
	if (!sa || !sb) return null;

	for (let i = 1; i <= 3; i += 1) {
		const diff = Number(sa[i]) - Number(sb[i]);
		if (diff !== 0) return diff;
	}

	const stageRank = (/** @type {string | undefined} */ stage) => {
		if (!stage) return 3; // stable > rc > preview
		if (stage === 'rc') return 2;
		return 1;
	};
	const rankDiff = stageRank(sa[4]) - stageRank(sb[4]);
	if (rankDiff !== 0) return rankDiff;

	if (sa[4] && sb[4]) {
		return Number(sa[5]) - Number(sb[5]);
	}
	return 0;
}

/**
 * Return the channel's newer `latest` entry only when `manifest` actually lists a release
 * strictly newer than `current.version` (never offering a downgrade if `versions.json` is stale).
 *
 * @param {VersionsManifest} manifest
 * @param {{ channel: ChannelId, version: string }} current
 * @returns {{ channelLabel: string, latestEntry: VersionEntry } | null}
 */
export function findNewerChannelRelease(manifest, current) {
	if (current.channel === 'main') return null;
	const ch = manifest.channels.find((c) => c.id === current.channel);
	if (!ch || !ch.latest || ch.latest === current.version) return null;

	const latestIdx = ch.versions.findIndex((v) => v.version === ch.latest);
	if (latestIdx === -1) return null;
	const latestEntry = ch.versions[latestIdx];

	const cmp = compareReleaseVersions(latestEntry.version, current.version);
	if (cmp !== null) {
		return cmp > 0 ? { channelLabel: ch.label, latestEntry } : null;
	}

	const currentIdx = ch.versions.findIndex((v) => v.version === current.version);
	if (currentIdx !== -1 && currentIdx > latestIdx) {
		return { channelLabel: ch.label, latestEntry };
	}
	return null;
}

/**
 * Rewrite an authored `/scion/...` internal URL to `${targetBase}/...` when building
 * a versioned snapshot (`targetBase !== '/scion'`).
 *
 * Leaves external URLs, anchors, non-`/scion` paths (such as `/scion-volumes/...`),
 * `/scion/versions.json`, and already-prefixed `${targetBase}/...` URLs untouched.
 *
 * @param {string} url
 * @param {string} targetBase
 * @returns {string}
 */
export function rewriteScionUrl(url, targetBase) {
	if (!url || typeof url !== 'string') return url;
	const normalizedTarget = normalizeBase(targetBase);
	if (normalizedTarget === ROOT_BASE) return url;

	if (!url.startsWith(ROOT_BASE)) return url;
	const nextChar = url[ROOT_BASE.length];
	if (nextChar !== undefined && nextChar !== '/' && nextChar !== '?' && nextChar !== '#') {
		// e.g. `/scion-volumes/...` — not a `/scion` site route.
		return url;
	}

	// Split off query/hash to check exact pathname boundaries cleanly.
	const suffixIdx = url.search(/[?#]/);
	const pathname = suffixIdx === -1 ? url : url.slice(0, suffixIdx);
	const suffix = suffixIdx === -1 ? '' : url.slice(suffixIdx);

	if (pathname === MANIFEST_PATH) {
		return url;
	}
	if (pathname === normalizedTarget || pathname.startsWith(`${normalizedTarget}/`)) {
		return url;
	}

	const rest = pathname.slice(ROOT_BASE.length);
	return `${normalizedTarget}${rest}${suffix}`;
}

/**
 * Extract the version-independent relative documentation slug from a browser `pathname`.
 * For example:
 * - `('/scion/hosted/ha/kubernetes/', '/scion')` -> `'hosted/ha/kubernetes/'`
 * - `('/scion/v0.3.0/hosted/ha/kubernetes/', '/scion/v0.3.0')` -> `'hosted/ha/kubernetes/'`
 * - `('/scion/v0.3.0/', '/scion/v0.3.0')` -> `'overview/'`
 *
 * @param {string} pathname
 * @param {string} currentBase
 * @returns {string}
 */
export function extractRelativeDocPath(pathname, currentBase) {
	const base = normalizeBase(currentBase);
	let rel = '';
	if (pathname === base || pathname === `${base}/`) {
		rel = '';
	} else if (pathname.startsWith(`${base}/`)) {
		rel = pathname.slice(base.length + 1);
	} else if (pathname === ROOT_BASE || pathname === `${ROOT_BASE}/`) {
		rel = '';
	} else if (pathname.startsWith(`${ROOT_BASE}/`)) {
		rel = pathname.slice(ROOT_BASE.length + 1);
	}
	const cleaned = rel.replace(/^\/+/, '');
	if (!cleaned) return 'overview/';
	return cleaned;
}

/**
 * Join a validated version base path (e.g. `'/scion'` or `'/scion/v0.3.0'`) with a relative doc path.
 *
 * @param {string} versionBasePath
 * @param {string} relativeDocPath
 * @returns {string}
 */
export function joinVersionUrl(versionBasePath, relativeDocPath) {
	const base = normalizeBase(versionBasePath);
	const rel = (relativeDocPath || 'overview/').replace(/^\/+/, '');
	return `${base}/${rel}`;
}

/**
 * Normalize and sanitize an arbitrary JSON payload into a valid `VersionsManifest`,
 * guaranteeing canonical channel ordering and safe `/scion` base paths.
 *
 * @param {unknown} raw
 * @returns {VersionsManifest}
 */
export function normalizeManifest(raw) {
	const obj = raw && typeof raw === 'object' ? /** @type {Record<string, unknown>} */ (raw) : {};
	const rawDefault = typeof obj.defaultChannel === 'string' ? obj.defaultChannel : 'main';
	const defaultChannel = /** @type {ChannelId} */ (
		VALID_CHANNELS.has(/** @type {ChannelId} */ (rawDefault)) ? rawDefault : 'main'
	);

	const byId = new Map();
	if (Array.isArray(obj.channels)) {
		for (const item of obj.channels) {
			if (!item || typeof item !== 'object') continue;
			const ch = /** @type {Record<string, unknown>} */ (item);
			const id = typeof ch.id === 'string' ? ch.id : '';
			if (!VALID_CHANNELS.has(/** @type {ChannelId} */ (id))) continue;

			/** @type {VersionEntry[]} */
			const versions = [];
			if (Array.isArray(ch.versions)) {
				for (const v of ch.versions) {
					if (!v || typeof v !== 'object') continue;
					const ve = /** @type {Record<string, unknown>} */ (v);
					const version = typeof ve.version === 'string' ? ve.version.trim() : '';
					const rawPath = typeof ve.path === 'string' ? ve.path.trim() : '';
					if (!version || !isSafeDocsBasePath(rawPath)) continue;
					const normalizedPath = normalizeBase(rawPath);
					const label =
						typeof ve.label === 'string' && ve.label.trim() ? ve.label.trim() : version;
					/** @type {VersionEntry} */
					const entry = { version, label, path: normalizedPath };
					if (typeof ve.date === 'string' && ve.date.trim()) {
						entry.date = ve.date.trim();
					}
					versions.push(entry);
				}
			}

			const latest =
				typeof ch.latest === 'string' && versions.some((v) => v.version === ch.latest)
					? ch.latest
					: versions[0]?.version || '';

			byId.set(id, { latest, versions });
		}
	}

	/** @type {ChannelEntry[]} */
	const channels = CHANNEL_DEFINITIONS.map((def) => {
		const found = byId.get(def.id);
		return {
			id: def.id,
			label: def.label,
			latest: found?.latest || '',
			versions: found?.versions || [],
		};
	});

	return { defaultChannel, channels };
}

/**
 * Ensure the currently rendered build's `{ channel, version, base }` is present in the manifest
 * so the selector always reflects the active page accurately even offline or before `versions.json` refreshes.
 *
 * @param {VersionsManifest} manifest
 * @param {{ channel: ChannelId, version: string, base: string }} current
 * @returns {VersionsManifest}
 */
export function ensureCurrentInManifest(manifest, current) {
	const safeBase = normalizeBase(current.base);
	const channels = manifest.channels.map((ch) => {
		if (ch.id === 'main' && ch.versions.length === 0) {
			return {
				...ch,
				latest: 'main',
				versions: [{ version: 'main', label: MAIN_VERSION_LABEL, path: ROOT_BASE }],
			};
		}
		if (ch.id !== current.channel) return ch;
		if (ch.versions.some((v) => v.version === current.version)) return ch;

		const isCurrentNewer =
			!ch.latest || (compareReleaseVersions(current.version, ch.latest) ?? -1) > 0;
		const defaultLabel =
			current.channel === 'main'
				? MAIN_VERSION_LABEL
				: isCurrentNewer
					? `${current.version} (latest)`
					: current.version;
		const added = { version: current.version, label: defaultLabel, path: safeBase };
		const versions = isCurrentNewer ? [added, ...ch.versions] : [...ch.versions, added];
		return {
			...ch,
			latest: isCurrentNewer ? current.version : ch.latest,
			versions,
		};
	});

	return {
		...manifest,
		channels,
	};
}
