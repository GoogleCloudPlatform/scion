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
import { defineHastPlugin, defineMdastPlugin } from 'satteri';
import { normalizeBase, rewriteScionUrl, ROOT_BASE } from '../lib/versioning.mjs';

const HTML_ATTR_RE = /\b(href|src)=(["'])(\/scion(?:[/?#][^"']*)?)\2/g;
const JSX_URL_ATTRS = new Set(['href', 'src', 'link']);

/**
 * Rewrite `href="/scion/..."` and `src="/scion/..."` attributes inside raw HTML snippets.
 *
 * @param {string} rawHtml
 * @param {string} targetBase
 * @returns {string}
 */
export function rewriteHtmlSnippet(rawHtml, targetBase) {
	if (!rawHtml || !rawHtml.includes('/scion')) return rawHtml;
	return rawHtml.replace(HTML_ATTR_RE, (_match, attr, quote, url) => {
		return `${attr}=${quote}${rewriteScionUrl(url, targetBase)}${quote}`;
	});
}

/**
 * Rewrite Starlight frontmatter link fields (`hero.actions[].link`, `prev.link`, `next.link`).
 *
 * @param {Record<string, unknown> | undefined} frontmatter
 * @param {string} targetBase
 */
export function rewriteFrontmatterLinks(frontmatter, targetBase) {
	if (!frontmatter || typeof frontmatter !== 'object') return;
	const normalizedTarget = normalizeBase(targetBase);
	if (normalizedTarget === ROOT_BASE) return;

	const hero = frontmatter.hero;
	if (hero && typeof hero === 'object' && Array.isArray(/** @type {any} */ (hero).actions)) {
		for (const action of /** @type {any} */ (hero).actions) {
			if (action && typeof action === 'object' && typeof action.link === 'string') {
				action.link = rewriteScionUrl(action.link, normalizedTarget);
			}
		}
	}

	for (const key of ['prev', 'next']) {
		const nav = frontmatter[key];
		if (nav && typeof nav === 'object' && typeof (/** @type {any} */ (nav).link) === 'string') {
			/** @type {any} */ (nav).link = rewriteScionUrl(/** @type {any} */ (nav).link, normalizedTarget);
		}
	}
}

/**
 * Rewrite URL-bearing MDX JSX attributes (`href`, `src`, `link`), returning a new array only when changed.
 *
 * @param {readonly any[] | undefined} attributes
 * @param {string} targetBase
 * @returns {any[] | null}
 */
function rewriteJsxAttributes(attributes, targetBase) {
	if (!Array.isArray(attributes) || attributes.length === 0) return null;
	let mutated = false;
	const nextAttrs = attributes.map((attr) => {
		if (
			attr?.type === 'mdxJsxAttribute' &&
			JSX_URL_ATTRS.has(attr.name) &&
			typeof attr.value === 'string'
		) {
			const nextVal = rewriteScionUrl(attr.value, targetBase);
			if (nextVal !== attr.value) {
				mutated = true;
				return { ...attr, value: nextVal };
			}
		}
		return attr;
	});
	return mutated ? nextAttrs : null;
}

/**
 * Create a Sätteri MDAST plugin that rewrites `/scion/...` links, images, definitions,
 * HTML blocks, and MDX JSX attributes when `targetBase !== '/scion'`.
 *
 * @param {string} targetBase
 */
export function createSatteriMdastRewritePlugin(targetBase) {
	const normalizedTarget = normalizeBase(targetBase);

	return defineMdastPlugin({
		name: 'scion-rewrite-base-mdast',
		link(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const next = rewriteScionUrl(node.url, normalizedTarget);
			if (next !== node.url) ctx.setProperty(node, 'url', next);
		},
		image(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const next = rewriteScionUrl(node.url, normalizedTarget);
			if (next !== node.url) ctx.setProperty(node, 'url', next);
		},
		definition(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const next = rewriteScionUrl(node.url, normalizedTarget);
			if (next !== node.url) ctx.setProperty(node, 'url', next);
		},
		html(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const next = rewriteHtmlSnippet(node.value, normalizedTarget);
			if (next !== node.value) ctx.setProperty(node, 'value', next);
		},
		mdxJsxFlowElement(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const nextAttrs = rewriteJsxAttributes(node.attributes, normalizedTarget);
			if (nextAttrs) ctx.replaceNode(node, { ...node, attributes: nextAttrs });
		},
		mdxJsxTextElement(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const nextAttrs = rewriteJsxAttributes(node.attributes, normalizedTarget);
			if (nextAttrs) ctx.replaceNode(node, { ...node, attributes: nextAttrs });
		},
	});
}

/**
 * Create a Sätteri HAST plugin that ensures any `<a>`, `<img>`, `<source>`, MDX JSX element,
 * or raw HTML fragment with `/scion/...` is rewritten before `starlight-links-validator` runs.
 *
 * @param {string} targetBase
 */
export function createSatteriHastRewritePlugin(targetBase) {
	const normalizedTarget = normalizeBase(targetBase);

	return defineHastPlugin({
		name: 'scion-rewrite-base-hast',
		element: {
			filter: ['a', 'img', 'source'],
			visit(node, ctx) {
				if (normalizedTarget === ROOT_BASE || !node.properties) return;
				if (typeof node.properties.href === 'string') {
					const next = rewriteScionUrl(node.properties.href, normalizedTarget);
					if (next !== node.properties.href) ctx.setProperty(node, 'href', next);
				}
				if (typeof node.properties.src === 'string') {
					const next = rewriteScionUrl(node.properties.src, normalizedTarget);
					if (next !== node.properties.src) ctx.setProperty(node, 'src', next);
				}
			},
		},
		mdxJsxFlowElement: {
			filter: [],
			visit(node, ctx) {
				if (normalizedTarget === ROOT_BASE) return;
				const nextAttrs = rewriteJsxAttributes(node.attributes, normalizedTarget);
				if (nextAttrs) ctx.replaceNode(node, { ...node, attributes: nextAttrs });
			},
		},
		mdxJsxTextElement: {
			filter: [],
			visit(node, ctx) {
				if (normalizedTarget === ROOT_BASE) return;
				const nextAttrs = rewriteJsxAttributes(node.attributes, normalizedTarget);
				if (nextAttrs) ctx.replaceNode(node, { ...node, attributes: nextAttrs });
			},
		},
		raw(node, ctx) {
			if (normalizedTarget === ROOT_BASE) return;
			const next = rewriteHtmlSnippet(node.value, normalizedTarget);
			if (next !== node.value) ctx.replaceNode(node, { ...node, value: next });
		},
	});
}

/**
 * Attach `/scion` base-path rewriters to Astro 7's Sätteri Markdown processor.
 * Must be registered in `integrations` BEFORE `starlight()` so rewritten URLs are visible
 * to `starlight-links-validator`.
 *
 * @param {{ base?: string }} [options]
 * @returns {import('astro').AstroIntegration}
 */
export function scionRewriteBaseIntegration(options = {}) {
	const targetBase = normalizeBase(options.base);

	return {
		name: 'scion-rewrite-base',
		hooks: {
			'astro:config:setup': async ({ command, config: astroConfig }) => {
				if (targetBase === ROOT_BASE) return;
				if (command !== 'build' && command !== 'dev') return;

				// Clear cached content-layer HTML when building/serving a non-root base so
				// previously rendered `/scion/...` links from a default build are never reused.
				try {
					const dataStoreUrl = new URL('data-store.json', astroConfig.cacheDir);
					await fs.rm(dataStoreUrl, { force: true });
				} catch {
					// Ignore missing cache file.
				}

				const processor = /** @type {any} */ (astroConfig.markdown?.processor);
				if (
					processor &&
					processor.name === 'satteri' &&
					Array.isArray(processor.options?.mdastPlugins) &&
					Array.isArray(processor.options?.hastPlugins)
				) {
					processor.options.mdastPlugins.push(() => createSatteriMdastRewritePlugin(targetBase));
					processor.options.hastPlugins.push(() => createSatteriHastRewritePlugin(targetBase));

					// Starlight reads page-level frontmatter (`hero.actions[].link`, `prev.link`, `next.link`)
					// from the `frontmatter` object passed into `renderer.render(content, renderOpts)` and
					// returned in `metadata.frontmatter`. Rewriting once per document here covers pages
					// even when their Markdown body contains zero links.
					const origCreateRenderer = processor.createRenderer.bind(processor);
					processor.createRenderer = async (/** @type {any} */ shared) => {
						const renderer = await origCreateRenderer(shared);
						const origRender = renderer.render.bind(renderer);
						renderer.render = async (/** @type {string} */ content, /** @type {any} */ renderOpts) => {
							rewriteFrontmatterLinks(renderOpts?.frontmatter, targetBase);
							return origRender(content, renderOpts);
						};
						return renderer;
					};
				} else {
					throw new Error(
						`Unsupported Astro markdown processor "${processor?.name ?? 'unknown'}" in scion-rewrite-base.`,
					);
				}
			},
		},
	};
}
