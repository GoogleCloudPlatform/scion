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
 * A fixed 200 kB Markdown document (about 3,600 top-level blocks: headings,
 * paragraphs with marks, lists, tables and code), the same every time.
 * With hazard, its first line holds a literal opener (design E7), so the
 * per-edit whole-text check runs. With refs, it also holds a numeric
 * character reference (the opening alphabet is then chosen by its probe)
 * and a reference link with its definition (read from the source itself).
 */
export function largeDoc(hazard: boolean, refs = false): string {
  const parts: string[] = [];
  parts.push(
    hazard
      ? 'Notes: {== an unclosed opener stays literal here.\n'
      : 'Notes: an opening paragraph.\n'
  );
  if (refs) parts.push('See [the guide][guide] &#38; the notes below.\n');
  // The length of parts joined by line breaks.
  let size = parts.reduce((n, p) => n + p.length, 0) + parts.length - 1;
  for (let i = 1; size < 200_000; i++) {
    const section = [
      `## Section ${i}\n`,
      `Paragraph ${i} with some words, a [link](https://example.com/${i}) and \`code ${i}\` and {++an insertion++} here.\n`,
      `* item ${i} one\n* item ${i} two with *emphasis*\n* item ${i} three\n`,
      `| col | val |\n|:----|----:|\n| a${i} | ${i} |\n| b${i} | {--old--} |\n`,
      '```js\nconst x = ' + i + ';\n```\n',
    ];
    for (const p of section) {
      parts.push(p);
      size += p.length + 1;
    }
  }
  if (refs) parts.push('[guide]: https://example.com/guide "Guide"\n');
  return parts.join('\n');
}
