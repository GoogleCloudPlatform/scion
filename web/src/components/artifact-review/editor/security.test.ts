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
 * The editor renders artifact text in the page, so it must show raw HTML
 * as text, render only a fixed set of elements and attributes, and give
 * links and images only the targets the read-only preview allows. Every
 * check is paired with a positive control in the same document, so a
 * blank render cannot pass.
 */

import { afterEach, describe, expect, it } from 'vitest';
import { TextSelection } from '@milkdown/prose/state';

import { parseCritic } from '../../../utils/critic.js';
import { SENTINEL_CANDIDATES } from '../../../utils/critic-sentinels.js';
import { ReviewController } from './controller.js';
import { docMarkKinds } from './guard.js';
import { LOAD_META } from './track.js';
import type { HintKind } from './policy.js';
import { reviewEditorStyles } from '../review-editor-styles.js';
import {
  ALLOWED_ATTRIBUTES,
  ALLOWED_TAGS,
  HARDENED,
  STRIKE_CLASS,
  STRIKE_SPEC,
  fixedSpec,
} from './safe-render.js';
import { renderContext } from './__fixtures__/core.js';

const made: ReviewController[] = [];
afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  document.body.replaceChildren();
});

async function open(md: string, hints: HintKind[] = []): Promise<ReviewController> {
  const root = document.createElement('div');
  document.body.append(root);
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: () => Promise.resolve('n'),
    onHint: (h) => hints.push(h),
    onChange: () => {},
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return c;
}

/** Fails on any element or attribute outside the editor's fixed sets. */
function assertFixedDom(root: Element): void {
  for (const el of Array.from(root.querySelectorAll('*'))) {
    const tag = el.tagName.toLowerCase();
    expect(ALLOWED_TAGS.has(tag), `tag ${tag}`).toBe(true);
    for (const attr of Array.from(el.attributes)) {
      expect(attr.name.startsWith('on'), `${tag}[${attr.name}]`).toBe(false);
      expect(ALLOWED_ATTRIBUTES.has(attr.name), `${tag}[${attr.name}]`).toBe(true);
    }
  }
}

const HUB_FILE = /^\/api\/v1\/artifacts\/art1\/versions\/3\/files\/[^?#]+(\?stream=1)?$/;

function assertTargets(root: Element): void {
  for (const a of Array.from(root.querySelectorAll('a[href]'))) {
    const href = a.getAttribute('href')!;
    expect(/^(https?:|mailto:)/.test(href) || HUB_FILE.test(href), href).toBe(true);
    expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    expect(a.getAttribute('referrerpolicy')).toBe('no-referrer');
  }
  // ProseMirror's own separator image (no source) is the only other image.
  for (const img of Array.from(root.querySelectorAll('img:not(.ProseMirror-separator)'))) {
    const src = img.getAttribute('src')!;
    expect(HUB_FILE.test(src) || /^data:image\/(png|jpeg|gif|webp)[;,]/.test(src), src).toBe(true);
    expect(img.hasAttribute('srcset')).toBe(false);
    expect(img.getAttribute('referrerpolicy')).toBe('no-referrer');
  }
}

const RAW_HTML = [
  '<script>window.__pwned=1</script>',
  '<img src=x onerror="window.__pwned=1">',
  '<svg onload="window.__pwned=1"></svg>',
  '<iframe src="https://evil.example/"></iframe>',
];

describe('raw HTML (S1)', () => {
  it('shows raw HTML as literal text, while Markdown beside it still renders', async () => {
    const md =
      RAW_HTML.map((h) => `Inline ${h} and *emphasis*.\n\n${h}\n`).join('\n') + '\n**strong**\n';
    const c = await open(md);
    const dom = c.view.dom;
    const raw = Array.from(dom.querySelectorAll('code.raw-html')).map((e) => e.textContent);
    for (const h of RAW_HTML) expect(raw.some((t) => t?.includes(h.slice(0, 7)))).toBe(true);
    expect(dom.querySelectorAll('script, iframe, svg').length).toBe(0);
    // Positive controls.
    expect(dom.querySelectorAll('em').length).toBe(RAW_HTML.length);
    expect(dom.querySelector('strong')?.textContent).toBe('strong');
    assertFixedDom(dom);
    // The document saves back unchanged.
    expect(c.flush()).toBe(md);
  });
});

describe('fixed attributes (S2)', () => {
  it('renders every node and mark through the hardened renderers', async () => {
    const c = await open('x\n');
    const schema = c.view.state.schema;
    for (const t of [...Object.values(schema.nodes), ...Object.values(schema.marks)]) {
      const toDOM = (t.spec as unknown as { toDOM?: Record<symbol, unknown> }).toDOM;
      if (toDOM) expect(toDOM[HARDENED], t.name).toBe(true);
    }
  });

  it('drops content-derived attributes and keeps comment notes as text', async () => {
    const note = '"><img src=x onerror=alert(1)>';
    const md = [
      '# Heading <b>one</b>',
      '',
      '```js" onmouseover="x',
      'code',
      '```',
      '',
      '3. ordered',
      '',
      `text {==mark==}{>>${note}<<} end`,
      '',
      '- [x] task',
      '',
      '| a | b |',
      '|:-|-:|',
      '| 1 | 2 |',
      '',
    ].join('\n');
    const c = await open(md);
    const dom = c.view.dom;
    assertFixedDom(dom);
    expect(dom.querySelector('h1')?.hasAttribute('id')).toBe(false);
    expect(dom.querySelector('pre')?.hasAttribute('data-language')).toBe(false);
    const comment = dom.querySelector('.critic-comment')!;
    expect(comment.getAttribute('aria-label')).toBe(`Comment: ${note}`);
    expect(comment.querySelector('img')).toBeNull();
    // Positive controls.
    expect(dom.querySelector('mark.critic-hl')?.textContent).toBe('mark');
    expect(dom.querySelector('ol')?.getAttribute('start')).toBe('3');
    expect(dom.querySelectorAll('td').length).toBe(2);
  });
});

describe('task lists and strikethrough (S1, S2)', () => {
  it('render no input, only fixed task attributes, and s with only its fixed class', async () => {
    const md = [
      '- [x] done <input type="checkbox" onclick="window.__pwned=1">',
      '- [ ] open ~~struck~~ and {--~~gone~~--}',
      '',
    ].join('\n');
    const c = await open(md);
    const dom = c.view.dom;
    assertFixedDom(dom);
    expect(dom.querySelectorAll('input').length).toBe(0);
    expect(Array.from(dom.querySelectorAll('code.raw-html')).map((e) => e.textContent)).toEqual([
      '<input type="checkbox" onclick="window.__pwned=1">',
    ]);
    for (const li of Array.from(dom.querySelectorAll('li'))) {
      for (const a of Array.from(li.attributes)) {
        expect(['data-item-type', 'data-checked'], a.name).toContain(a.name);
      }
    }
    // Positive controls.
    expect(dom.querySelector('li[data-item-type="task"][data-checked="true"]')).not.toBeNull();
    const strikes = Array.from(dom.querySelectorAll('s'));
    expect(strikes.map((x) => x.outerHTML)).toEqual([
      '<s class="gfm-strike">struck</s>',
      '<s class="gfm-strike">gone</s>',
    ]);
    expect(dom.querySelector('del.critic-del > s.gfm-strike')?.textContent).toBe('gone');
    expect(c.flush()).toBe(md);
  });

  it('keep the strikethrough class a fixed literal', () => {
    expect(STRIKE_CLASS).toBe('gfm-strike');
    expect(STRIKE_SPEC).toEqual(['s', { class: 'gfm-strike' }, 0]);
  });

  it('drop input elements, event handlers and anything not listed', () => {
    expect(fixedSpec(['input', { type: 'checkbox', checked: true }], false)).toEqual(['span']);
    expect(fixedSpec(['input', { type: 'checkbox' }, 0], true)).toEqual(['span', 0]);
    expect(
      fixedSpec(['li', { 'data-item-type': 'task', onclick: 'x', onmouseover: 'y' }, 0], true)
    ).toEqual(['li', { 'data-item-type': 'task' }, 0]);
    // Task attributes take their fixed values only.
    const wrong = { 'data-item-type': 'note', 'data-checked': 'yes' };
    expect(fixedSpec(['li', wrong, 0], true)).toEqual(['li', {}, 0]);
    expect(fixedSpec(['li', { 'data-checked': 'false' }, 0], true)).toEqual([
      'li',
      { 'data-checked': 'false' },
      0,
    ]);
    // s takes its fixed class and nothing else.
    expect(
      fixedSpec(
        ['s', { class: 'gfm-strike', title: 'x', style: 'text-align: left', id: 'a' }, 0],
        true
      )
    ).toEqual(['s', { class: 'gfm-strike' }, 0]);
    expect(fixedSpec(['s', { class: 'other' }, 0], true)).toEqual(['s', {}, 0]);
    expect(fixedSpec(['s', { onclick: 'x' }, 0], true)).toEqual(['s', {}, 0]);
    // Names that only exist on Object.prototype are not rules. "constructor"
    // survives the lowercasing of names; __proto__ is an own key only when
    // parsed from JSON.
    expect(fixedSpec(['p', { constructor: 'x' }, 0], true)).toEqual(['p', {}, 0]);
    const proto = JSON.parse('{"__proto__":"x"}') as Record<string, string>;
    expect(Object.keys(proto)).toEqual(['__proto__']);
    expect(fixedSpec(['p', proto, 0], true)).toEqual(['p', {}, 0]);
    expect(fixedSpec(['strike', { class: 'gfm-strike' }, 0], true)).toEqual(['span', 0]);
  });

  it('draw the checkbox from attribute selectors and fixed text only', () => {
    const css = reviewEditorStyles.cssText;
    expect(css).not.toMatch(/attr\s*\(/);
    const contents = css.match(/content:[^;]*;/g) ?? [];
    const task = contents.filter((x) => /[☐☑]/.test(x));
    expect(task).toEqual([
      "content: '☐';",
      "content: '☐' / 'Not done: ';",
      "content: '☑';",
      "content: '☑' / 'Done: ';",
    ]);
  });
});

describe('links (S3)', () => {
  const bad = [
    'javascript:window.__pwned=1',
    'JaVaScRiPt:window.__pwned=1',
    'java\tscript:window.__pwned=1',
    '&#106;avascript:window.__pwned=1',
    'data:text/html,window.__pwned=1',
    'vbscript:x',
    'blob:https://hub.example/x',
    'file:///etc/passwd',
    '//evil.example/x',
    'tel:+15551234',
    'missing.md',
    '../../outside.md',
    'java&#x09;script:window.__pwned=1',
    '&#x0A;javascript:window.__pwned=1',
    'jav&#x0D;ascript:window.__pwned=1',
  ];

  it('gives a target only to http, https, mailto and version files', async () => {
    const md =
      bad.map((h, i) => `[bad${i}](<${h}>) and [good${i}](https://ok.example/${i})`).join('\n\n') +
      '\n\n[file](notes.md) [mail](mailto:a@b.example) [frag](#top) <https://auto.example/>\n\n' +
      '[ref][r]\n\n[r]: javascript:window.__pwned=1\n';
    const c = await open(md);
    const dom = c.view.dom;
    assertFixedDom(dom);
    assertTargets(dom);
    const byText = (t: string): Element =>
      Array.from(dom.querySelectorAll('a')).find((a) => a.textContent === t)!;
    bad.forEach((_h, i) => {
      expect(byText(`bad${i}`).hasAttribute('href'), `bad${i}`).toBe(false);
      // Positive control beside each one.
      expect(byText(`good${i}`).getAttribute('href')).toBe(`https://ok.example/${i}`);
    });
    expect(byText('file').getAttribute('href')).toBe(
      '/api/v1/artifacts/art1/versions/3/files/docs/notes.md'
    );
    expect(byText('mail').getAttribute('href')).toBe('mailto:a@b.example');
    expect(byText('frag').hasAttribute('href')).toBe(false);
    expect(byText('ref').hasAttribute('href')).toBe(false);
    expect(byText('https://auto.example/').getAttribute('href')).toBe('https://auto.example/');
    expect(c.flush()).toBe(md);
  });
});

describe('links in autolink and reference form (S3)', () => {
  it('gives no target to other schemes, beside https links that keep theirs', async () => {
    const autos = [
      'javascript:window.__pwned=1',
      'JaVaScRiPt:window.__pwned=1',
      'data:text/html,x',
    ];
    const refs = [
      'data:text/html,x',
      'JaVaScRiPt:window.__pwned=1',
      '&#106;avascript:window.__pwned=1',
      'java&#x09;script:window.__pwned=1',
    ];
    const md =
      autos.map((h, i) => `<${h}> and [autoOk${i}](https://ok.example/a${i})`).join('\n\n') +
      '\n\n' +
      refs
        .map((_h, i) => `[ref${i}][r${i}] and [refOk${i}](https://ok.example/r${i})`)
        .join('\n\n') +
      '\n\n' +
      refs.map((h, i) => `[r${i}]: ${h}`).join('\n') +
      '\n';
    const c = await open(md);
    const dom = c.view.dom;
    assertFixedDom(dom);
    assertTargets(dom);
    const anchors = Array.from(dom.querySelectorAll('a'));
    const byText = (t: string): Element | undefined => anchors.find((a) => a.textContent === t);
    autos.forEach((h, i) => {
      const a = byText(h);
      // An autolink with another scheme is text, or a link without a target.
      if (a) expect(a.hasAttribute('href'), h).toBe(false);
      expect(dom.textContent).toContain(h);
      expect(byText(`autoOk${i}`)?.getAttribute('href')).toBe(`https://ok.example/a${i}`);
    });
    refs.forEach((_h, i) => {
      expect(byText(`ref${i}`)?.hasAttribute('href'), `ref${i}`).toBe(false);
      expect(byText(`refOk${i}`)?.getAttribute('href')).toBe(`https://ok.example/r${i}`);
    });
    expect(c.flush()).toBe(md);
  });
});

describe('images (S4)', () => {
  it('loads only version files, fetched copies and a few data images', async () => {
    const md = [
      '![ext](https://evil.example/a.png)',
      '![proto](//evil.example/a.png)',
      '![missing](nothere.png)',
      // The editor is stricter than the preview on purpose: raster data images only.
      '![svg](data:image/svg+xml;base64,PHN2Zy8+)',
      '![dot](img/dot.png)',
      '![remote](https://img.example/pic.png)',
      '![png](data:image/png;base64,iVBORw0KGgo=)',
      '',
    ].join('\n\n');
    const c = await open(md);
    const dom = c.view.dom;
    assertFixedDom(dom);
    assertTargets(dom);
    const placeholders = Array.from(dom.querySelectorAll('.image-placeholder')).map(
      (e) => e.textContent
    );
    expect(placeholders).toEqual([
      'Image not fetched: ext',
      'Image not fetched: proto',
      'Image not in this version: missing',
      'Image not in this version: svg',
    ]);
    // Positive controls.
    const srcs = Array.from(dom.querySelectorAll('img:not(.ProseMirror-separator)')).map((i) =>
      i.getAttribute('src')
    );
    expect(srcs).toEqual([
      '/api/v1/artifacts/art1/versions/3/files/docs/img/dot.png?stream=1',
      '/api/v1/artifacts/art1/versions/3/files/_remote/1/pic.png?stream=1',
      'data:image/png;base64,iVBORw0KGgo=',
    ]);
    expect(c.flush()).toBe(md);
  });
});

function pasteEvent(data: Record<string, string>): ClipboardEvent {
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      getData: (t: string) => data[t] ?? '',
      items: [],
      files: [],
      types: Object.keys(data),
    },
  });
  return event;
}

describe('paste and drop (S5)', () => {
  it('inserts the plain-text reading when HTML and plain text disagree', async () => {
    const c = await open('one two\n');
    const v = c.view;
    const p = v.state.doc.resolve(5).pos;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, p)));
    v.dom.dispatchEvent(
      pasteEvent({
        'text/html': '<img src=x onerror="window.__pwned=1"><a href="javascript:x">evil</a>',
        'text/plain': 'plain words',
      })
    );
    const out = c.flush()!;
    expect(out).toBe('one {++plain words++}two\n');
    expect(v.dom.querySelector('img, a')).toBeNull();
  });

  it('empties pasted HTML that reaches the default path', async () => {
    const c = await open('x\n');
    const transform = c.view.someProp('transformPastedHTML');
    expect(transform?.('<b>x</b>', c.view)).toBe('');
  });

  it('refuses a drop and an image file, with the image hint', async () => {
    const hints: HintKind[] = [];
    const c = await open('one two\n', hints);
    const drop = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(drop, 'dataTransfer', {
      value: { items: [{ kind: 'file', type: 'image/png' }], files: [], getData: () => '' },
    });
    const handled = c.view.someProp('handleDrop', (f) => f(c.view, drop, null as never, false));
    expect(handled).toBe(true);
    expect(hints).toEqual(['image']);
    expect(c.flush()).toBe('one two\n');
  });
});

describe('paste and drop never reach the default paths (S5)', () => {
  it('handles a paste on the element around the editor before ProseMirror, also mid-composition', async () => {
    const c = await open('one two\n');
    const v = c.view;
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, 5)));
    c.beginComposition();
    const event = pasteEvent({
      'text/html': '<img src="https://paste.invalid/x">',
      'text/plain': 'words',
    });
    v.dom.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(c.flush()).toBe('one {++words++}two\n');
    expect(v.dom.querySelector('img')).toBeNull();
    // The pasted insertion survives the end of the composition.
    c.endComposition();
    c.settleComposition();
    expect(c.flush()).toBe('one {++words++}two\n');
  });

  it('refuses a drop with no position and any drag over the editor', async () => {
    const hints: HintKind[] = [];
    const c = await open('one two\n', hints);
    const root = c.view.dom.parentElement!;
    const drop = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(drop, 'dataTransfer', {
      value: { items: [{ kind: 'file', type: 'image/png' }], files: [], getData: () => '<b>x</b>' },
    });
    root.dispatchEvent(drop);
    const over = new Event('dragover', { bubbles: true, cancelable: true });
    c.view.dom.dispatchEvent(over);
    expect(drop.defaultPrevented).toBe(true);
    expect(over.defaultPrevented).toBe(true);
    expect(hints).toEqual(['image']);
    expect(c.flush()).toBe('one two\n');
  });
});

describe("copying from the editor (S5, the editor's own copies)", () => {
  /** What a copy of [from, to) puts on the clipboard as text. */
  function copied(c: ReviewController, from: number, to: number): string {
    const slice = c.view.state.doc.slice(from, to);
    return c.view.someProp('clipboardTextSerializer', (f) => f(slice, c.view)) ?? '';
  }
  const inner = (c: ReviewController): [number, number] => [
    1,
    c.view.state.doc.firstChild!.nodeSize - 1,
  ];

  it('copies Markdown, so backslashed characters paste back literally', async () => {
    const c = await open('a\\*b\\*c\n');
    const text = copied(c, ...inner(c));
    expect(text).toBe('a\\*b\\*c');
    const d = await open('x\n');
    d.view.dom.dispatchEvent(pasteEvent({ 'text/plain': text }));
    expect(d.view.dom.querySelector('em')).toBeNull();
    expect(d.flush()).toBe('{++a\\*b\\*c++}x\n');
  });

  it('copies link text with its link, and leaves review marks out', async () => {
    const c = await open('see [docs](https://ok.example/) {++new++}{>>n<<}\n');
    const text = copied(c, ...inner(c));
    expect(text).toBe('see [docs](https://ok.example/) new');
    const d = await open('x\n');
    d.view.dom.dispatchEvent(pasteEvent({ 'text/plain': 'see [docs](https://ok.example/)' }));
    expect(d.view.dom.querySelector('a')?.getAttribute('href')).toBe('https://ok.example/');
  });
});

describe('sentinel characters in the text (S6)', () => {
  it('keeps them as text and still reads real marks', async () => {
    const chars = SENTINEL_CANDIDATES.slice(0, 10).join('');
    const md = `text ${chars} and {++real++} mark ${chars}\n`;
    const c = await open(md);
    expect(c.view.dom.querySelectorAll('ins.critic-ins').length).toBe(1);
    expect(c.view.dom.textContent).toContain(chars);
    expect(c.flush()).toBe(md);
  });

  it('keeps a sentinel character typed after opening as text when saving', async () => {
    const c = await open('abc\n');
    const v = c.view;
    const ch = SENTINEL_CANDIDATES[0];
    v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, 1)));
    v.dispatch(v.state.tr.insertText(`${ch}x`, 1, 1));
    const out = c.flush()!;
    expect(out).toBe(`{++${ch}x++}abc\n`);
    expect(parseCritic(out).filter((s) => s.kind !== 'text')).toHaveLength(1);
  });

  it('keeps character references of sentinel candidates as text, beside real marks', async () => {
    const md = '&#x2E00;x&#x2E01; &#x2E11; and {++real++} with {==hl==}{>>real note<<}\n';
    const c = await open(md);
    const dom = c.view.dom;
    expect(dom.querySelectorAll('ins.critic-ins').length).toBe(1);
    expect(dom.querySelector('ins.critic-ins')?.textContent).toBe('real');
    const comments = dom.querySelectorAll('.critic-comment');
    expect(comments.length).toBe(1);
    expect(comments[0].getAttribute('aria-label')).toBe('Comment: real note');
    expect(dom.textContent).toContain('\u2E00x\u2E01 \u2E11');
    expect(c.flush()).toBe(md);
  });

  // References hidden from the parser in the source (inside raw HTML or
  // indented code that a comment note opens) decode in the sentinel form,
  // where the note is gone. They must stay text too.
  const hidden = [
    [
      'raw HTML in a note',
      '{>><b title="<<} &#x2E00;fake&#x2E01; ">',
      '<b title="',
      '\u2E00fake\u2E01',
    ],
    [
      'indented code in a note',
      'x {>>a\n\n    <<} &#x2E00;fake&#x2E01;',
      'a\n\n    ',
      '\u2E00fake\u2E01',
    ],
    // The last candidates make the probe alphabet.
    [
      'raw HTML in a note, probe characters',
      '{>><b title="<<} &#x2CFF;fake&#x2CFE; ">',
      '<b title="',
      '\u2CFFfake\u2CFE',
    ],
  ];
  for (const [name, frag, note, shown] of hidden) {
    it(`keeps references hidden by ${name} as text, beside real marks`, async () => {
      const md = `${frag}\n\nand {++real++} with {==hl==}{>>real note<<}\n`;
      const c = await open(md);
      const dom = c.view.dom;
      expect(dom.querySelectorAll('ins.critic-ins').length).toBe(1);
      expect(dom.querySelector('ins.critic-ins')?.textContent).toBe('real');
      const labels = Array.from(dom.querySelectorAll('.critic-comment')).map((e) =>
        e.getAttribute('aria-label')
      );
      expect(labels).toEqual([`Comment: ${note}`, 'Comment: real note']);
      expect(dom.textContent).toContain(shown);
      expect(c.flush()).toBe(md);
    });
  }

  it('does not shift real notes when a hidden reference poses as a comment', async () => {
    const md = '{>><b title="<<} &#x2E11; "> then {>>real<<}\n';
    const c = await open(md);
    const labels = Array.from(c.view.dom.querySelectorAll('.critic-comment')).map((e) =>
      e.getAttribute('aria-label')
    );
    expect(labels).toEqual(['Comment: <b title="', 'Comment: real']);
    expect(c.flush()).toBe(md);
  });

  // Other constructs a note can open or close around a reference. Either
  // the document's marks are exactly the critic parser's ('marks'), or the
  // parser reads a mark token inside an attribute (raw HTML, an HTML
  // comment, a link target or title, an image's alt or source), which the
  // editor keeps as written: that block is read-only and saved from its
  // source, and the marks after it are still the parser's ('attribute').
  // In both, the text saves byte for byte, before and after an edit. A
  // note read both in a link's text and in its target does not open
  // ('refuses'): the notes would not line up.
  const constructs: [string, 'marks' | 'attribute' | 'refuses'][] = [
    ['<!-- {>>x<<} &#x2E00;f&#x2E01; &#x2E11; -->', 'attribute'],
    ['{>><!--<<} &#x2E00;f&#x2E01; &#x2E11; -->', 'marks'],
    ['[l](<{>>x<<}&#x2E00;f&#x2E01;&#x2E11;>)', 'attribute'],
    ['[l](u "{>>x<<} &#x2E00;f&#x2E01; &#x2E11;")', 'attribute'],
    ['[l](u "{>>"<<} &#x2E00;f&#x2E01; &#x2E11;")', 'attribute'],
    ['<http://a.example/{>>x<<}&#x2E00;f&#x2E01;&#x2E11;>', 'refuses'],
    ['`{>>x<<} &#x2E00;f&#x2E01; &#x2E11;`', 'marks'],
    ['{>>`<<} &#x2E00;f&#x2E01; &#x2E11;`', 'marks'],
    ['a {>>x\n===<<} &#x2E00;f&#x2E01; &#x2E11;', 'marks'],
    ['| a {>>x | b<<} | &#x2E00;f&#x2E01; &#x2E11; |\n|---|---|\n| 1 | 2 |', 'marks'],
    ['{>>x\n```\n<<} &#x2E00;f&#x2E01; &#x2E11;\n```', 'marks'],
    // Link reference definitions: the parser copies a definition onto its
    // references and drops it, so a mark or note in one would show in
    // another place (or nowhere). The load invariant refuses them.
    ['[r]: u "{>>x<<} &#x2E00;f&#x2E01; &#x2E11;"\n\n[r]', 'refuses'],
    ['[a][r]\n\nPara {==x==}{>>B<<}\n\n[r]: http://e.example/{>>A<<}', 'refuses'],
    ['[r]: /p "{>>A<<}"\n\nPara {==x==}{>>B<<}\n\n[t][r]', 'refuses'],
    ['![alt][r]\n\nPara {==x==}{>>B<<}\n\n[r]: i.png "{>>A<<}"', 'refuses'],
    ['[r]: /p "{>>R<<}"\n\n{>>X<<} hello\n\n[t][r]', 'refuses'],
    ['[a][r] [b][r] {>>X<<}\n\n[r]: /p "{>>R<<}"\n[q]: /p "{>>Q<<}"', 'refuses'],
    ['see <http://a.example/{>>A<<}> here {>>B<<}\n\n[u]: /x "{>>C<<}"', 'refuses'],
    ['[u]: /x "{++"\n\nhello ++} world', 'refuses'],
    // A definition inside the same block quote or list item as its reference.
    ['> [t][r] {>>B<<}\n>\n> [r]: /p "{>>A<<}"', 'refuses'],
    ['- [t][r] {>>B<<}\n\n  [r]: /p "{>>A<<}"', 'refuses'],
    // A link whose text ends in an image: its target is not restored, so
    // the document does not open (kept deliberately; it fails safe).
    ['[![b](i.png)](https://x.example/{++y++}) and {++real two++}', 'refuses'],
    ['[l](u "{++x++}") and {++real two++}', 'attribute'],
    ['<span title="{--x--}">s</span> and {++real two++}', 'attribute'],
    ['![i]({++x++}.png) and {++real two++}', 'attribute'],
    ['![{>>alt note<<}](a.png) and {++real two++}', 'attribute'],
    ['[l](https://a.example/{++x++}.html) and {++real two++}', 'attribute'],
    ['<div title="{>>html note<<}">\nblock\n</div>', 'attribute'],
  ];
  it('keeps comments in an HTML block, a tag attribute and an image alt as written, and the notes after them in place', async () => {
    const md =
      '<div title="{>>one<<}">\nblock\n</div>\n\nText <b title="{>>two<<}">b</b> here.\n\n' +
      '![alt {>>three<<}](a.png)\n\nThen {==x==}{>>note B<<} end.\n';
    const hints: HintKind[] = [];
    const c = await open(md, hints);
    expect(c.readOnlyBlocks()).toEqual([0, 1, 2]);
    const dom = c.view.dom;
    // The marks inside attributes show as written, each with its own note.
    const raw = Array.from(dom.querySelectorAll('code.raw-html')).map((e) => e.textContent);
    expect(raw.join('\n')).toContain('{>>one<<}');
    expect(raw.join('\n')).toContain('{>>two<<}');
    const labels = Array.from(dom.querySelectorAll('.critic-comment')).map((e) =>
      e.getAttribute('aria-label')
    );
    expect(labels).toEqual(['Comment: note B']);
    expect(dom.querySelector('mark.critic-hl, .critic-hl')?.textContent).toBe('x');
    expect(docMarkKinds(c.view.state.doc)).toEqual(['highlight', 'comment']);
    expect(c.flush()).toBe(md);
    const v = c.view;
    // Typing in the paragraph with the tag is refused.
    let here = -1;
    v.state.doc.descendants((n, pos) => {
      if (here < 0 && n.isText && n.text!.includes(' here')) here = pos + n.text!.indexOf(' here');
      return here < 0;
    });
    v.dispatch(v.state.tr.insertText('Q', here));
    expect(hints).toEqual(['generic']);
    // Typing in the last paragraph is recorded; the blocks before it are written as they were.
    let end = -1;
    v.state.doc.descendants((n, pos) => {
      if (end < 0 && n.isText && n.text!.includes(' end.')) end = pos + n.text!.indexOf(' end.');
      return end < 0;
    });
    v.dispatch(v.state.tr.insertText(' Z', end));
    expect(c.flush()).toBe(md.replace(' end.', '{++ Z++} end.'));
  });

  it('writes a read-only block from its source even if a change reaches it', async () => {
    const md = '![{>>alt note<<}](a.png) and text\n\nand {++real++}\n';
    const c = await open(md);
    expect(c.readOnlyBlocks()).toEqual([0]);
    const v = c.view;
    let at = -1;
    v.state.doc.child(0).descendants((n, pos) => {
      if (at < 0 && n.isText) at = pos + 1;
      return at < 0;
    });
    // A transaction the review policy does not see (as at load).
    v.dispatch(v.state.tr.insertText('Q', at + 1).setMeta(LOAD_META, true));
    expect(v.state.doc.child(0).textContent).toContain('Q');
    v.dispatch(v.state.tr.insertText('Z', v.state.doc.child(0).nodeSize + 1));
    expect(c.flush()).toBe('![{>>alt note<<}](a.png) and text\n\n{++Z++}and {++real++}\n');
  });

  for (const [frag, expected] of constructs) {
    it(`references around a note, ${expected}: ${JSON.stringify(frag).slice(0, 40)}`, async () => {
      const md = `${frag}\n\nand {++real++} with {==hl==}{>>real note<<}\n`;
      const root = document.createElement('div');
      document.body.append(root);
      const hints: HintKind[] = [];
      const c = await ReviewController.create({
        root,
        render: () => renderContext,
        askNote: () => Promise.resolve('n'),
        onHint: (h) => hints.push(h),
        onChange: () => {},
      });
      made.push(c);
      const opened = c.load(md);
      expect(opened ? (c.readOnlyBlocks().length ? 'attribute' : 'marks') : 'refuses').toBe(
        expected
      );
      if (!opened) return;
      const dom = c.view.dom;
      const segs = parseCritic(md).filter((x) => x.kind !== 'text');
      const labels = Array.from(dom.querySelectorAll('.critic-comment')).map(
        (e) => e.getAttribute('aria-label') ?? ''
      );
      const want = segs.filter((x) => x.kind === 'comment').map((x) => `Comment: ${x.text}`);
      if (expected === 'marks') {
        // The same kinds in order, and the comments with the parser's notes.
        expect(docMarkKinds(c.view.state.doc)).toEqual(segs.map((x) => x.kind));
        expect(labels).toEqual(want);
      } else {
        // The marks the editor shows are the parser's last ones (those
        // after the read-only block), with their notes.
        const kinds = docMarkKinds(c.view.state.doc);
        expect(segs.slice(-kinds.length).map((x) => x.kind)).toEqual(kinds);
        expect(want.slice(-labels.length)).toEqual(labels);
        expect(labels.at(-1)).toBe('Comment: real note');
        expect(c.readOnlyBlocks()).toEqual([0]);
        // Typing in the read-only block is refused, with the generic hint.
        let first = -1;
        c.view.state.doc.child(0).descendants((n, pos) => {
          if (first < 0 && n.isText) first = pos + 1;
          return first < 0;
        });
        if (first >= 0) {
          const v = c.view;
          v.dispatch(v.state.tr.insertText('Q', first + 1));
          expect(hints).toEqual(['generic']);
        }
      }
      expect(c.flush()).toBe(md);
      // An edit in the last paragraph (when it is one, not code): the
      // blocks before it are written as they were.
      let at = -1;
      c.view.state.doc.descendants((n, pos) => {
        if (at < 0 && n.isText && n.text!.startsWith('and ')) at = pos;
        return at < 0;
      });
      if (at < 0) return;
      const v = c.view;
      v.dispatch(v.state.tr.insertText('Z', at));
      expect(c.flush()).toBe(md.replace('\n\nand {++real++}', '\n\n{++Z++}and {++real++}'));
    });
  }
});
