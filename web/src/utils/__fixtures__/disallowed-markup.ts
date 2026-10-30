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
 * Disallowed markup cases shared by the sanitizer tests. Each entry is HTML
 * handed straight to the sanitizer, as if the markdown step had let it
 * through. `mutation` marks forms that only turn disallowed when a browser
 * parser reads them again after serializing; the test DOM's parser does not
 * reproduce that, so for them the tests check the output after a round trip
 * instead of first proving the input looks disallowed.
 */
export const DISALLOWED_CASES: readonly { name: string; html: string; mutation?: boolean }[] = [
  { name: 'script element', html: '<script>alert(1)</script><p>ok</p>' },
  { name: 'img onerror', html: '<img src="/x.png" onerror="alert(1)">' },
  { name: 'p onclick', html: '<p onclick="alert(1)">t</p>' },
  {
    name: 'svg animate onbegin',
    html: '<svg><animate onbegin="alert(1)" attributeName="x"></svg>',
  },
  { name: 'javascript: href', html: '<a href="javascript:alert(1)">j</a>' },
  { name: 'mixed-case javascript: href', html: '<a href="JaVaScRiPt:alert(1)">j</a>' },
  { name: 'spaced javascript: href', html: '<a href=" javascript:alert(1)">j</a>' },
  { name: 'data: href', html: '<a href="data:text/html,<script>alert(1)</script>">d</a>' },
  { name: 'javascript: img src', html: '<img src="javascript:alert(1)">' },
  { name: 'javascript: iframe src', html: '<iframe src="javascript:alert(1)"></iframe>' },
  { name: 'svg script', html: '<svg><script>alert(1)</script></svg>' },
  { name: 'svg javascript: link', html: '<svg><a href="javascript:alert(1)">s</a></svg>' },
  {
    name: 'svg foreignObject nesting',
    html: '<svg><foreignObject><img src="/x.png" onerror="alert(1)"></foreignObject></svg>',
  },
  {
    name: 'mathml mglyph/style nesting',
    html: '<math><mtext><table><mglyph><style><img src="/x.png" onerror="alert(1)"></style></mglyph></table></mtext></math>',
    mutation: true,
  },
  {
    name: 'svg/style namespace confusion',
    html: '<svg></p><style><a id="</style><img src="/x.png" onerror="alert(1)">">',
    mutation: true,
  },
  {
    name: 'form/math namespace confusion',
    html: '<form><math><mtext></form><form><mglyph><style></math><img src="/x.png" onerror="alert(1)">',
    mutation: true,
  },
  { name: 'iframe srcdoc', html: '<iframe srcdoc="<script>alert(1)</script>"></iframe>' },
  { name: 'vbscript: href', html: '<a href="vbscript:msgbox(1)">v</a>' },
  { name: 'base element', html: '<base href="https://elsewhere.example/"><a href="x">b</a>' },
  {
    name: 'meta refresh',
    html: '<meta http-equiv="refresh" content="0;url=javascript:alert(1)"><p>m</p>',
  },
  { name: 'object data', html: '<object data="javascript:alert(1)"></object>' },
  {
    name: 'noscript/style nesting',
    html: '<noscript><style></noscript><img src="/x.png" onerror="alert(1)">',
  },
];

/** Problems in a sanitized tree; empty when nothing disallowed is left. */
export function disallowedLeft(root: ParentNode): string[] {
  const problems: string[] = [];
  for (const el of Array.from(root.querySelectorAll('*'))) {
    const tag = el.localName.toLowerCase();
    if (['script', 'iframe', 'object', 'embed', 'noscript', 'base', 'meta'].includes(tag)) {
      problems.push(`<${tag}>`);
    }
    for (const attr of Array.from(el.attributes)) {
      const name = attr.name.toLowerCase();
      // Drop spaces and control characters, as browsers do when reading a URL.
      const value = Array.from(attr.value)
        .filter((ch) => ch.charCodeAt(0) > 0x20)
        .join('')
        .toLowerCase();
      if (name.startsWith('on')) problems.push(`${tag}[${name}]`);
      if (name === 'srcdoc') problems.push(`${tag}[srcdoc]`);
      if (/(^|:)href$|^src$|^action$|^formaction$|^data$/.test(name)) {
        if (value.startsWith('javascript:')) problems.push(`${tag}[${name}=javascript:]`);
        if (value.startsWith('vbscript:')) problems.push(`${tag}[${name}=vbscript:]`);
        if (value.startsWith('data:') && !(tag === 'img' && value.startsWith('data:image/'))) {
          problems.push(`${tag}[${name}=data:]`);
        }
      }
    }
  }
  return problems;
}

/** Serializes root and parses the markup again, as a string round trip would. */
export function roundTrip(root: Node): DocumentFragment {
  const out = document.createElement('template');
  out.content.append(root.cloneNode(true));
  const back = document.createElement('template');
  back.innerHTML = out.innerHTML;
  return back.content;
}
