// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Browser side of markdown-sanitize.pw.ts. Loaded on demand from the
 * extraction fixture page (Vite serves it), it runs disallowed markup through
 * the real renderFragment and reports what is left.
 *
 * marked shows raw HTML in markdown as text, so disallowed tags never reach the
 * sanitizer through it. For the length of one call, marked.parse is
 * replaced by a pass-through, so each case reaches the real sanitizer
 * (DOMPurify, its configuration and the link hook) exactly as written.
 */

import { marked } from 'marked';
import { getMarkdownRenderer } from '../../src/utils/markdown.js';
import {
  DISALLOWED_CASES,
  disallowedLeft,
  roundTrip,
} from '../../src/utils/__fixtures__/disallowed-markup.js';

/** What one case looks like before and after the renderer. */
export interface CaseResult {
  name: string;
  mutation: boolean;
  /** Disallowed items in the raw input, parsed once by the browser. */
  rawParsed: string[];
  /** Disallowed items after the raw input is serialized and parsed again. */
  rawRoundTrip: string[];
  /** Disallowed items in renderFragment's nodes. */
  rendered: string[];
  /** Disallowed items after those nodes are serialized and parsed again. */
  renderedRoundTrip: string[];
}

function parse(html: string): DocumentFragment {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  return tpl.content;
}

export async function runDisallowedCases(): Promise<CaseResult[]> {
  const renderer = await getMarkdownRenderer();
  const parseMarkdown = marked.parse;
  marked.parse = ((source: string) => source) as typeof marked.parse;
  try {
    return DISALLOWED_CASES.map(({ name, html, mutation }) => {
      const rendered = renderer.renderFragment(html);
      return {
        name,
        mutation: !!mutation,
        rawParsed: disallowedLeft(parse(html)),
        rawRoundTrip: disallowedLeft(roundTrip(parse(html))),
        rendered: disallowedLeft(rendered),
        renderedRoundTrip: disallowedLeft(roundTrip(rendered)),
      };
    });
  } finally {
    marked.parse = parseMarkdown;
  }
}
