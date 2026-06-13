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
 * Shared markdown rendering helper.
 *
 * Renders raw markdown text to sanitized HTML using marked + DOMPurify. Both
 * libraries are lazy-loaded on first use to keep the main bundle small, and the
 * resolved renderer is cached for the lifetime of the page. The output is always
 * passed through DOMPurify, so it is safe to inject via Lit's `unsafeHTML`
 * directive or `.innerHTML`.
 */

interface MarkdownRenderer {
  render(markdown: string): string;
}

let rendererPromise: Promise<MarkdownRenderer> | null = null;

async function loadRenderer(): Promise<MarkdownRenderer> {
  if (!rendererPromise) {
    rendererPromise = (async () => {
      const [{ marked }, DOMPurify] = await Promise.all([import('marked'), import('dompurify')]);

      const purify = DOMPurify.default ?? DOMPurify;

      return {
        render(markdown: string): string {
          // marked's overload for { async: false } already returns string, so
          // no cast is needed here (verified by tsc + typed lint).
          const rawHtml = marked.parse(markdown, { async: false });
          return purify.sanitize(rawHtml);
        },
      };
    })();
  }
  return rendererPromise;
}

/**
 * Render markdown text to sanitized HTML.
 *
 * The returned HTML has been sanitized with DOMPurify and is safe to inject.
 */
export async function renderMarkdown(markdown: string): Promise<string> {
  const renderer = await loadRenderer();
  return renderer.render(markdown);
}
