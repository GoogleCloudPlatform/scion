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
 * Shared markdown rendering utility.
 *
 * Provides a singleton lazy-loaded renderer using marked + DOMPurify.
 * Both the markdown-preview component and the chat components share
 * this instance so the parser is loaded at most once.
 *
 * All rendered HTML is sanitized via DOMPurify. A DOMPurify hook
 * ensures anchors open in a new tab with rel="noopener noreferrer".
 */

import { criticToSentinels, renderCriticSentinels } from './critic.js';

/** Options for one render call. */
export interface MarkdownRenderOptions {
  /**
   * Keep only images the page's own origin serves (relative and
   * same-origin URLs, and data: images). Images from any other host are
   * replaced by their alt text, so showing the content makes the viewer's
   * browser fetch nothing from elsewhere.
   */
  sameOriginImagesOnly?: boolean;
  /**
   * Render CriticMarkup marks (utils/critic.ts): insertions as <ins>,
   * deletions as <del>, highlights as <mark> and comments as numbered
   * notes. The marks become elements before sanitizing, so the output is
   * sanitized like any other.
   */
  criticMarks?: boolean;
}

/** Result of the lazy-loaded renderer. */
export interface MarkdownRenderer {
  render(markdown: string, options?: MarkdownRenderOptions): string;
}

/**
 * Reports whether an image source is served by the page's own origin:
 * a relative or same-origin URL, or a data: image.
 */
export function isSameOriginImageSrc(src: string, origin: string): boolean {
  const value = src.trim();
  if (value === '') return false;
  if (/^data:image\//i.test(value)) return true;
  try {
    return new URL(value, origin + '/').origin === origin;
  } catch {
    return false;
  }
}

/** Replaces every image not served by origin with its alt text. */
function dropOffOriginImages(html: string, origin: string): string {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  for (const img of Array.from(tpl.content.querySelectorAll('img'))) {
    const src = img.getAttribute('src') ?? '';
    if (img.hasAttribute('srcset') || !isSameOriginImageSrc(src, origin)) {
      img.replaceWith(document.createTextNode(img.getAttribute('alt') ?? ''));
    }
  }
  return tpl.innerHTML;
}

let rendererPromise: Promise<MarkdownRenderer> | null = null;

/**
 * Lazily load and return the shared markdown renderer.
 * Both `marked` and `dompurify` are loaded on first call; subsequent
 * calls return the cached promise.
 */
export async function getMarkdownRenderer(): Promise<MarkdownRenderer> {
  if (!rendererPromise) {
    rendererPromise = (async () => {
      const [{ marked }, DOMPurify] = await Promise.all([import('marked'), import('dompurify')]);

      const purify = DOMPurify.default ?? DOMPurify;

      // Add a hook so all anchors open in a new tab with noopener noreferrer.
      purify.addHook('afterSanitizeAttributes', (node: Element) => {
        if (node.tagName === 'A') {
          node.setAttribute('target', '_blank');
          node.setAttribute('rel', 'noopener noreferrer');
        }
      });

      // Escape raw HTML tokens so angle-bracket text like <template> is
      // rendered as visible text instead of being interpreted as real HTML
      // elements (which can truncate content — see miller79/scion#10).
      marked.use({
        renderer: {
          html({ text }: { text: string }): string {
            return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
          },
        },
      });

      return {
        render(markdown: string, options?: MarkdownRenderOptions): string {
          const source = options?.criticMarks ? criticToSentinels(markdown) : markdown;
          let rawHtml = marked.parse(source, { async: false }) as string;
          if (options?.criticMarks) rawHtml = renderCriticSentinels(rawHtml);
          const clean = purify.sanitize(rawHtml);
          return options?.sameOriginImagesOnly
            ? dropOffOriginImages(clean, window.location.origin)
            : clean;
        },
      };
    })();
  }
  return rendererPromise;
}
