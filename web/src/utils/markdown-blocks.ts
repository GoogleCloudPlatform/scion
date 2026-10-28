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
 * The number of top-level blocks of a Markdown text (headings, paragraphs,
 * lists, tables, code blocks and the like; blank lines are not blocks), as
 * the app's Markdown renderer reads it. It is quick and loads no editor
 * code, so a page can size a document before opening it in the review
 * editor. It is close to the review editor's own count, not always equal
 * to it: use it for thresholds only.
 */
export async function countTopLevelBlocks(markdown: string): Promise<number> {
  const { marked } = await import('marked');
  return marked.lexer(markdown).filter((t) => t.type !== 'space').length;
}
