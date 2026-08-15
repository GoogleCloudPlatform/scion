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

/** Result of the lazy-loaded renderer. */
export interface MarkdownRenderer {
  render(markdown: string): string;
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

      return {
        render(markdown: string): string {
          const rawHtml = marked.parse(markdown, { async: false, breaks: true }) as string;
          return purify.sanitize(rawHtml);
        },
      };
    })();
  }
  return rendererPromise;
}
