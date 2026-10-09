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
 * Builds the document of an artifact's markdown preview frame
 * (.design/artifacts.md, section 8.4, "Preview").
 *
 * The preview is a srcdoc frame sandboxed without allow-scripts whose
 * document carries PREVIEW_CSP, so it loads images from the hub only.
 * Images are pointed at the version's own files:
 *
 * - a relative path resolves against the entry's folder in the version;
 * - an absolute http(s) URL loads the copy the hub fetched when the version
 *   was published, found by comparing the browser's parsed form of the
 *   image URL with that of each remote row's sourceUrl;
 * - a data: image stays as it is;
 * - anything else becomes a placeholder.
 */

import { artifactFileUrl, isRemoteFile } from './artifacts.js';
import type { ArtifactFile } from './artifacts.js';

/** Content-Security-Policy of the preview document. */
export const PREVIEW_CSP = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'";

/** Sandbox flags of the preview frame: never allow-scripts. */
export const PREVIEW_SANDBOX = 'allow-same-origin allow-popups allow-popups-to-escape-sandbox';

/** Where an image of the preview comes from. */
export type ImageSource =
  | { kind: 'file'; path: string }
  | { kind: 'remote'; path: string }
  | { kind: 'data' }
  | { kind: 'not-fetched' }
  | { kind: 'failed' }
  | { kind: 'missing' };

/** The parsed form the browser gives an absolute http(s) URL, or null. */
export function parsedHttpUrl(raw: string): string | null {
  try {
    const u = new URL(raw);
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.href : null;
  } catch {
    return null;
  }
}

/**
 * Resolves a relative image path against the entry's folder. Returns the
 * bundle path, or null when the path leaves the bundle, is rooted, or
 * cannot be decoded. A query or fragment is dropped.
 */
export function resolveBundlePath(entryPath: string, src: string): string | null {
  const bare = src.split(/[?#]/, 1)[0] ?? '';
  if (bare === '' || bare.startsWith('/') || bare.startsWith('\\')) return null;
  const parts = entryPath.split('/').slice(0, -1);
  for (const raw of bare.split('/')) {
    let seg: string;
    try {
      seg = decodeURIComponent(raw);
    } catch {
      return null;
    }
    if (seg === '' || seg === '.') continue;
    if (seg === '..') {
      if (parts.length === 0) return null;
      parts.pop();
      continue;
    }
    if (seg.includes('/') || seg.includes('\\')) return null;
    parts.push(seg);
  }
  return parts.length > 0 ? parts.join('/') : null;
}

/** Decides where one image's src points, given the version's files. */
export function imageSource(src: string, entryPath: string, files: ArtifactFile[]): ImageSource {
  // The URL parser drops ASCII tabs and line breaks anywhere in a URL and
  // trims surrounding spaces; read the value the same way.
  const value = src.replace(/[\t\n\r]/g, '').trim();
  if (/^data:image\//i.test(value)) return { kind: 'data' };
  const hasScheme = /^[a-z][a-z0-9+.-]*:/i.test(value) || value.startsWith('//');
  if (hasScheme) {
    const want = parsedHttpUrl(value.startsWith('//') ? `https:${value}` : value);
    if (want === null) return { kind: 'not-fetched' };
    const row = files.find(
      (f) => isRemoteFile(f) && f.sourceUrl && parsedHttpUrl(f.sourceUrl) === want
    );
    if (!row) return { kind: 'not-fetched' };
    return row.fetchStatus === 'ok' ? { kind: 'remote', path: row.path } : { kind: 'failed' };
  }
  const path = resolveBundlePath(entryPath, value);
  if (path === null) return { kind: 'missing' };
  const file = files.find((f) => !isRemoteFile(f) && f.path === path);
  return file ? { kind: 'file', path } : { kind: 'missing' };
}

const PLACEHOLDER_TEXT: Record<'not-fetched' | 'failed' | 'missing', string> = {
  'not-fetched': 'Image not fetched',
  failed: 'Image could not be fetched',
  missing: 'Image not in this version',
};

/**
 * Points every image of sanitized HTML at the version's files, replacing
 * the rest with placeholders. Returns the rewritten HTML.
 */
export function rewriteImages(
  cleanHtml: string,
  ctx: { id: string; seq: number; entryPath: string; files: ArtifactFile[] }
): string {
  const tpl = document.createElement('template');
  tpl.innerHTML = cleanHtml;
  for (const img of Array.from(tpl.content.querySelectorAll('img'))) {
    img.removeAttribute('srcset');
    img.removeAttribute('sizes');
    const src = img.getAttribute('src') ?? '';
    const where = imageSource(src, ctx.entryPath, ctx.files);
    if (where.kind === 'file' || where.kind === 'remote') {
      img.setAttribute('src', artifactFileUrl(ctx.id, ctx.seq, where.path, true));
      img.setAttribute('loading', 'lazy');
      continue;
    }
    if (where.kind === 'data') continue;
    const alt = img.getAttribute('alt') ?? '';
    const span = document.createElement('span');
    span.className = `image-placeholder ${where.kind}`;
    span.textContent = alt
      ? `${PLACEHOLDER_TEXT[where.kind]}: ${alt}`
      : PLACEHOLDER_TEXT[where.kind];
    img.replaceWith(span);
  }
  // A relative link to a file of the version opens that file; any other
  // relative link, including a #fragment (links open in a new tab), would
  // resolve against the app's own URL, so it is made plain text. Absolute
  // links are left as the sanitizer kept them.
  for (const a of Array.from(tpl.content.querySelectorAll('a[href]'))) {
    const href = a.getAttribute('href') ?? '';
    if (/^[a-z][a-z0-9+.-]*:/i.test(href) || href.startsWith('//')) continue;
    const path = href.startsWith('#') ? null : resolveBundlePath(ctx.entryPath, href);
    const file =
      path === null ? undefined : ctx.files.find((f) => !isRemoteFile(f) && f.path === path);
    if (file) {
      a.setAttribute('href', artifactFileUrl(ctx.id, ctx.seq, file.path));
    } else {
      a.removeAttribute('href');
    }
  }
  return tpl.innerHTML;
}

/** Colors the preview document takes from the page's theme. */
export interface PreviewTheme {
  text: string;
  muted: string;
  border: string;
  subtle: string;
  surface: string;
  link: string;
  fontMono: string;
}

const DEFAULT_THEME: PreviewTheme = {
  text: '#1e293b',
  muted: '#64748b',
  border: '#e2e8f0',
  subtle: '#f8fafc',
  surface: '#ffffff',
  link: '#2563eb',
  fontMono: "'SF Mono', 'Fira Code', monospace",
};

/** Keeps a CSS value from closing its declaration or the style element. */
function cssValue(v: string, fallback: string): string {
  const t = v.trim();
  return t !== '' && !/[;{}<>\\]/.test(t) ? t : fallback;
}

/** The full srcdoc of the preview frame for rewritten, sanitized HTML. */
/**
 * Styles of rendered CriticMarkup (utils/critic.ts): insertions green and
 * underlined, deletions red and struck through, highlights yellow, and
 * comments as numbered notes in a right-hand margin when the frame is wide
 * enough, otherwise as a block under the reference.
 */
export const CRITIC_CSS = `ins.critic-ins{color:#166534;background:#dcfce7;text-decoration:underline}
del.critic-del{color:#991b1b;background:#fee2e2;text-decoration:line-through}
mark.critic-hl{background:#fef08a;color:inherit}
sup.critic-ref{font-size:.7em;font-weight:600;color:#6d28d9;background:#ede9fe;border-radius:.25rem;padding:0 .25em;margin-left:.1em}
.critic-note{display:block;margin:.35em 0 .6em;padding:.4em .6em;font-size:.8125rem;line-height:1.45;color:#4c1d95;background:#f5f3ff;border-left:3px solid #8b5cf6;border-radius:.25rem}
.critic-note-n{font-weight:600;margin-right:.25em}
.critic-side{display:block;margin:0 0 1em;font-size:.8125rem;line-height:1.45;color:#64748b}
.critic-side.empty{padding:.75em 1em;border:1px dashed #cbd5e1;border-radius:.5rem;text-align:center}
body.notes-margin{padding-right:17rem}
body.notes-margin .critic-note,body.notes-margin .critic-side{float:right;clear:right;width:14rem;margin:0 -16rem .5em 0}`;

/** Options of a preview document. */
export interface PreviewOptions {
  /**
   * Lay comment notes out in a right-hand margin. The page decides from
   * its viewport width; without it, notes sit under their reference.
   */
  marginNotes?: boolean;
  /** A short note shown first in the margin (or above the text). */
  sideNote?: string;
  /** Style the side note as an empty-state placeholder. */
  sideNoteEmpty?: boolean;
}

function escapeText(text: string): string {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function previewDocument(
  bodyHtml: string,
  theme: Partial<PreviewTheme> = {},
  options: PreviewOptions = {}
): string {
  const t = { ...DEFAULT_THEME };
  for (const k of Object.keys(DEFAULT_THEME) as (keyof PreviewTheme)[]) {
    if (theme[k] !== undefined) t[k] = cssValue(theme[k]!, DEFAULT_THEME[k]);
  }
  return `<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="${PREVIEW_CSP}">
<base target="_blank">
<style>
html,body{margin:0;background:${t.surface};color:${t.text}}
body{padding:1.5rem 2rem;font:0.9375rem/1.7 system-ui,-apple-system,'Segoe UI',sans-serif;overflow-wrap:break-word}
h1,h2,h3,h4,h5,h6{margin:1.5em 0 .5em;font-weight:600;line-height:1.3}
body>:first-child{margin-top:0}
h1{font-size:1.75rem;border-bottom:1px solid ${t.border};padding-bottom:.3em}
h2{font-size:1.375rem;border-bottom:1px solid ${t.border};padding-bottom:.3em}
h3{font-size:1.125rem}h4{font-size:1rem}
p{margin:0 0 1em}
a{color:${t.link};text-decoration:none}a:hover{text-decoration:underline}
code{font-family:${t.fontMono};font-size:.85em;background:${t.subtle};padding:.15em .35em;border-radius:.25rem;border:1px solid ${t.border}}
pre{background:${t.subtle};border:1px solid ${t.border};border-radius:.5rem;padding:1rem;overflow-x:auto;margin:0 0 1em}
pre code{background:none;border:none;padding:0;font-size:.8125rem}
blockquote{border-left:4px solid ${t.border};margin:0 0 1em;padding:.5em 1em;color:${t.muted};background:${t.subtle}}
ul,ol{margin:0 0 1em;padding-left:1.5em}li{margin-bottom:.25em}
table{border-collapse:collapse;width:100%;margin:0 0 1em}
th,td{border:1px solid ${t.border};padding:.5em .75em;text-align:left}th{background:${t.subtle};font-weight:600}
hr{border:none;border-top:1px solid ${t.border};margin:1.5em 0}
img{max-width:100%;height:auto}
.image-placeholder{display:inline-block;padding:.2em .5em;border:1px dashed ${t.border};border-radius:.25rem;color:${t.muted};background:${t.subtle};font-size:.8125rem}
.image-placeholder.failed{border-color:#fca5a5;color:#991b1b;background:#fef2f2}
${CRITIC_CSS}
</style></head><body${options.marginNotes ? ' class="notes-margin"' : ''}>${
    options.sideNote
      ? `<aside class="critic-side${options.sideNoteEmpty ? ' empty' : ''}">${escapeText(options.sideNote)}</aside>`
      : ''
  }${bodyHtml}</body></html>`;
}
