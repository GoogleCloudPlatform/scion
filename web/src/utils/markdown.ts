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
 *
 * `renderFragment` is the one place where rendered markdown becomes markup:
 * the sanitizer parses it once and hands back DOM nodes. Callers change
 * those nodes with DOM calls only and attach them as nodes, so no step after
 * it parses markup again.
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
  /** With criticMarks, the author shown in each comment's note header. */
  criticAuthor?: string;
}

/** Result of the lazy-loaded renderer. */
export interface MarkdownRenderer {
  /** Renders and sanitizes markdown to an HTML string. */
  render(markdown: string, options?: MarkdownRenderOptions): string;
  /**
   * Renders and sanitizes markdown to nodes owned by this document, parsing
   * the markup exactly once. The sanitizing and image rules are the same as
   * `render`. Change the result with DOM calls only (never by setting markup
   * from strings) and attach it as nodes.
   */
  renderFragment(markdown: string, options?: MarkdownRenderOptions): DocumentFragment;
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

/**
 * Replaces every image in root not served by origin with its alt text.
 * Call it before the nodes join the page's document (see toInertFragment).
 */
function dropOffOriginImages(root: DocumentFragment, origin: string): void {
  for (const img of Array.from(root.querySelectorAll('img'))) {
    const src = img.getAttribute('src') ?? '';
    if (img.hasAttribute('srcset') || !isSameOriginImageSrc(src, origin)) {
      img.replaceWith(root.ownerDocument.createTextNode(img.getAttribute('alt') ?? ''));
    }
  }
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

      /** Markdown to unsanitized HTML; only ever handed to the sanitizer. */
      const toRawHtml = (markdown: string, options?: MarkdownRenderOptions): string => {
        const source = options?.criticMarks ? criticToSentinels(markdown) : markdown;
        const rawHtml = marked.parse(source, { async: false });
        return options?.criticMarks
          ? renderCriticSentinels(rawHtml, options.criticAuthor)
          : rawHtml;
      };

      /**
       * The single parse: the sanitizer turns the markup into nodes in its
       * own inert document, with the default configuration (the same one
       * `sanitize` uses for strings). The image rule then runs on those
       * nodes while they are still outside the page's document, so the page
       * never loads an image that is then replaced.
       */
      const toInertFragment = (
        markdown: string,
        options?: MarkdownRenderOptions
      ): DocumentFragment => {
        const fragment = purify.sanitize(toRawHtml(markdown, options), {
          RETURN_DOM_FRAGMENT: true,
        });
        if (options?.sameOriginImagesOnly) dropOffOriginImages(fragment, window.location.origin);
        return fragment;
      };

      return {
        render(markdown: string, options?: MarkdownRenderOptions): string {
          if (!options?.sameOriginImagesOnly) return purify.sanitize(toRawHtml(markdown, options));
          // Serialize from an inert template, so no kept image is requested
          // before the caller shows the result.
          const tpl = document.createElement('template');
          tpl.content.append(toInertFragment(markdown, options));
          return tpl.innerHTML;
        },
        renderFragment(markdown: string, options?: MarkdownRenderOptions): DocumentFragment {
          return document.adoptNode(toInertFragment(markdown, options));
        },
      };
    })();
  }
  return rendererPromise;
}
