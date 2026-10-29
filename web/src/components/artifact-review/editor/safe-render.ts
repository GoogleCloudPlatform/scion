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

/**
 * How the editor turns a document into DOM.
 *
 * - Every node and mark is rendered from a fixed set of element names and
 *   attribute names. Values that come from the document (link targets,
 *   image sources, heading ids, list labels, code languages) are dropped,
 *   except where this module computes them itself: link targets follow
 *   artifactLinkTarget and image sources follow imageSource, the rules of
 *   the read-only preview.
 * - Raw HTML in the Markdown is shown as literal text.
 * - Strikethrough is an s element with a fixed class, so it reads apart
 *   from a suggested deletion (a del element).
 * - Images load only from the version's files (or a few inline data image
 *   types); anything else is a placeholder with the preview's wording.
 */

import type { Ctx, MilkdownPlugin } from '@milkdown/ctx';
import { SchemaReady, nodeViewCtx, schemaCtx } from '@milkdown/core';
import type { DOMOutputSpec, Mark, Node, Schema } from '@milkdown/prose/model';
import type { NodeView, NodeViewConstructor } from '@milkdown/prose/view';

import {
  PLACEHOLDER_TEXT,
  artifactLinkTarget,
  imageSource,
} from '../../../client/artifact-preview.js';
import type { ArtifactLinkContext } from '../../../client/artifact-preview.js';
import { artifactFileUrl } from '../../../client/artifacts.js';

/** Element names the editor may render. */
export const ALLOWED_TAGS: ReadonlySet<string> = new Set([
  'p',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'blockquote',
  'pre',
  'code',
  'hr',
  'br',
  'ul',
  'ol',
  'li',
  'em',
  'strong',
  'a',
  's',
  'del',
  'ins',
  'mark',
  'span',
  'sl-icon',
  'img',
  'table',
  'thead',
  'tbody',
  'tr',
  'th',
  'td',
  'colgroup',
  'col',
  'sup',
  'dl',
  'dt',
  'dd',
  'div',
]);

const FIXED_TOKEN = /^[a-z0-9 _-]*$/i;

/**
 * Attribute names a node or mark may set, each with the values it may
 * take. Anything else is dropped.
 */
const ATTRIBUTE_RULES: Record<string, (value: string, tag: string) => boolean> = {
  class: (v) => FIXED_TOKEN.test(v),
  start: (v, tag) => tag === 'ol' && /^\d{1,9}$/.test(v),
  colspan: (v) => /^\d{1,3}$/.test(v),
  rowspan: (v) => /^\d{1,3}$/.test(v),
  style: (v) => /^\s*text-align:\s*(left|right|center)\s*;?\s*$/.test(v),
  'data-item-type': (v) => v === 'task',
  'data-checked': (v) => v === 'true' || v === 'false',
  'data-is-header': (v) => v === 'true',
  role: (v) => v === 'note',
  tabindex: (v) => v === '0',
  'aria-hidden': (v) => v === 'true',
  'aria-label': () => true,
  title: (v) => FIXED_TOKEN.test(v.replace(/,/g, '')),
  name: (v, tag) => tag === 'sl-icon' && (v === 'chat-text' || v === 'image'),
};

/** The class of a strikethrough element, a fixed literal. */
export const STRIKE_CLASS = 'gfm-strike';

type AttributeRule = (value: string, tag: string) => boolean;

/**
 * Elements whose attributes follow their own rules instead of
 * ATTRIBUTE_RULES: anything not listed for them is dropped.
 */
const TAG_ATTRIBUTE_RULES: Record<string, Record<string, AttributeRule>> = {
  s: { class: (v) => v === STRIKE_CLASS },
};

/** Attribute names the editor may render, for tests. */
export const ALLOWED_ATTRIBUTES: ReadonlySet<string> = new Set([
  ...Object.keys(ATTRIBUTE_RULES),
  // Set by this module's own link and image rendering only.
  'href',
  'target',
  'rel',
  'referrerpolicy',
  'src',
  'alt',
  'loading',
  'decoding',
  // Set by ProseMirror itself on the editor root and on node views.
  'contenteditable',
  'draggable',
  'translate',
  'spellcheck',
]);

type SpecArray = readonly [string, ...unknown[]];

/** Returns spec with element names and attributes limited to the rules above. */
export function fixedSpec(spec: DOMOutputSpec, hasContent: boolean): DOMOutputSpec {
  if (typeof spec === 'string') return spec;
  if (!Array.isArray(spec)) {
    // A DOM node or {dom, contentDOM} from a renderer we do not control.
    return hasContent ? ['span', 0] : ['span'];
  }
  const arr = spec as unknown as SpecArray;
  const tag = String(arr[0]).toLowerCase();
  if (!ALLOWED_TAGS.has(tag)) return hasContent ? ['span', 0] : ['span'];
  const out: unknown[] = [tag];
  let rest = arr.slice(1);
  const first = rest[0];
  if (
    first &&
    typeof first === 'object' &&
    !Array.isArray(first) &&
    typeof (first as { nodeType?: unknown }).nodeType !== 'number'
  ) {
    const attrs: Record<string, string> = {};
    for (const [name, raw] of Object.entries(first as Record<string, unknown>)) {
      if (typeof raw !== 'string' && typeof raw !== 'number' && raw !== true) continue;
      const value = String(raw);
      const key = name.toLowerCase();
      const table = Object.hasOwn(TAG_ATTRIBUTE_RULES, tag)
        ? TAG_ATTRIBUTE_RULES[tag]
        : ATTRIBUTE_RULES;
      const rule = Object.hasOwn(table, key) ? table[key] : undefined;
      if (rule && rule(value, tag)) attrs[key] = value;
    }
    out.push(attrs);
    rest = rest.slice(1);
  }
  for (const child of rest) {
    if (child === 0) out.push(0);
    else if (typeof child === 'string') out.push(child);
    else if (Array.isArray(child))
      out.push(fixedSpec(child as unknown as DOMOutputSpec, hasContent));
  }
  return out as unknown as DOMOutputSpec;
}

/** The parts of the editor's context its rendering needs. */
export type RenderContext = ArtifactLinkContext;

/** Inline data images the editor shows. */
const DATA_IMAGE = /^data:image\/(png|jpeg|gif|webp)[;,]/i;

/** The DOM of a link mark: a target only when artifactLinkTarget gives one. */
export function linkSpec(mark: Mark, rc: RenderContext): DOMOutputSpec {
  const target = artifactLinkTarget(String(mark.attrs.href ?? ''), rc);
  if (target.kind === 'external' || target.kind === 'file') {
    return [
      'a',
      {
        href: target.href,
        target: '_blank',
        rel: 'noopener noreferrer',
        referrerpolicy: 'no-referrer',
      },
      0,
    ];
  }
  if (target.kind === 'fragment') return ['a', { class: 'fragment-link' }, 0];
  return ['a', { class: 'inert-link' }, 0];
}

/** What an image node shows: an image URL, or a placeholder text. */
export function imageDisplay(
  src: string,
  alt: string,
  rc: RenderContext
): { kind: 'image'; url: string } | { kind: 'placeholder'; text: string; failed: boolean } {
  const where = imageSource(src, rc.entryPath, rc.files);
  if (where.kind === 'file' || where.kind === 'remote') {
    return { kind: 'image', url: artifactFileUrl(rc.id, rc.seq, where.path, true) };
  }
  if (where.kind === 'data') {
    const value = src.replace(/[\t\n\r]/g, '').trim();
    if (DATA_IMAGE.test(value)) return { kind: 'image', url: value };
  }
  const key = where.kind === 'not-fetched' || where.kind === 'failed' ? where.kind : 'missing';
  const base = PLACEHOLDER_TEXT[key];
  return { kind: 'placeholder', text: alt ? `${base}: ${alt}` : base, failed: key === 'failed' };
}

/** Builds the DOM of an image node. */
function imageDom(node: Node, rc: RenderContext): HTMLElement {
  const alt = String(node.attrs.alt ?? '');
  const shown = imageDisplay(String(node.attrs.src ?? ''), alt, rc);
  if (shown.kind === 'image') {
    const img = document.createElement('img');
    img.setAttribute('src', shown.url);
    img.setAttribute('alt', alt);
    img.setAttribute('referrerpolicy', 'no-referrer');
    img.setAttribute('loading', 'lazy');
    img.setAttribute('decoding', 'async');
    return img;
  }
  const span = document.createElement('span');
  span.className = shown.failed ? 'image-placeholder failed' : 'image-placeholder';
  const icon = document.createElement('sl-icon');
  icon.setAttribute('name', 'image');
  icon.setAttribute('aria-hidden', 'true');
  span.append(icon, document.createTextNode(shown.text));
  return span;
}

/** The node view of images: built from imageDisplay only. */
export function imageView(getContext: () => RenderContext): NodeViewConstructor {
  return (initial: Node): NodeView => {
    let current = initial;
    const view: NodeView = {
      dom: imageDom(initial, getContext()),
      update(next: Node): boolean {
        if (next.type !== current.type) return false;
        if (next.attrs.src !== current.attrs.src || next.attrs.alt !== current.attrs.alt) {
          return false;
        }
        current = next;
        return true;
      },
      ignoreMutation: (): boolean => true,
    };
    return view;
  };
}

/** The DOM of a strikethrough mark. */
export const STRIKE_SPEC: DOMOutputSpec = ['s', { class: STRIKE_CLASS }, 0];

/** Marks the toDOM functions this module has replaced, for tests. */
export const HARDENED = Symbol('hardened');

type ToDom = ((...args: never[]) => DOMOutputSpec) & { [HARDENED]?: true };

/**
 * Replaces the toDOM of every node and mark of schema: html, link and
 * strikethrough get this module's renderers, every other one is limited
 * by fixedSpec.
 */
export function hardenSchema(schema: Schema, getContext: () => RenderContext): void {
  for (const type of Object.values(schema.nodes)) {
    const spec = type.spec as unknown as { toDOM?: ToDom };
    if (!spec.toDOM) continue;
    let next: ToDom;
    if (type.name === 'html') {
      next = ((n: Node): DOMOutputSpec => [
        'code',
        { class: 'raw-html' },
        String(n.attrs.value ?? ''),
      ]) as ToDom;
    } else if (type.name === 'image') {
      // Clipboard serialization only; the view uses imageView.
      next = ((n: Node): DOMOutputSpec => {
        const shown = imageDisplay(
          String(n.attrs.src ?? ''),
          String(n.attrs.alt ?? ''),
          getContext()
        );
        return shown.kind === 'image'
          ? ['img', { src: shown.url, alt: String(n.attrs.alt ?? '') }]
          : ['span', { class: 'image-placeholder' }, shown.text];
      }) as ToDom;
    } else {
      const prev = spec.toDOM.bind(spec) as unknown as (n: Node) => DOMOutputSpec;
      const leaf = type.isLeaf;
      next = ((n: Node): DOMOutputSpec => fixedSpec(prev(n), !leaf)) as ToDom;
    }
    next[HARDENED] = true;
    spec.toDOM = next;
  }
  for (const type of Object.values(schema.marks)) {
    const spec = type.spec as unknown as { toDOM?: ToDom };
    if (!spec.toDOM) continue;
    let next: ToDom;
    if (type.name === 'link') {
      next = ((m: Mark): DOMOutputSpec => linkSpec(m, getContext())) as ToDom;
    } else if (type.name === 'strike_through') {
      next = ((): DOMOutputSpec => fixedSpec(STRIKE_SPEC, true)) as ToDom;
    } else {
      const prev = spec.toDOM.bind(spec) as unknown as (m: Mark, inline: boolean) => DOMOutputSpec;
      next = ((m: Mark, inline: boolean): DOMOutputSpec =>
        fixedSpec(prev(m, inline), true)) as ToDom;
    }
    next[HARDENED] = true;
    spec.toDOM = next;
  }
}

/** A Milkdown plugin that hardens the schema and installs the image view. */
export function safeRender(getContext: () => RenderContext): MilkdownPlugin {
  return (ctx: Ctx) => async (): Promise<void> => {
    await ctx.wait(SchemaReady);
    hardenSchema(ctx.get(schemaCtx), getContext);
    ctx.update(nodeViewCtx, (views) => [
      ...views,
      ['image', imageView(getContext)] as [string, NodeViewConstructor],
    ]);
  };
}
