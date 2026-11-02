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
 * Counts the markup-parsing calls a chat page makes per message.
 *
 * A browser that checks Trusted Types flags every plain string handed to an
 * API that parses markup (innerHTML, DOMParser and the like). Values a named
 * policy produced pass. This file installs a stand-in `trustedTypes` before
 * any module loads, so the libraries that create their own policy (Lit for
 * its templates, DOMPurify for its parser) create it here, and wraps each
 * markup-parsing API to record whether it got a plain string or a policy
 * value. The real markdown renderer is used, not a stand-in.
 */

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { vi } from 'vitest';
import '../../../utils/__fixtures__/sanitizer-node-name.js';

interface SinkCall {
  sink: string;
  /** The policy that produced the value, or null for a plain string. */
  policy: string | null;
}

const sinkState = vi.hoisted(() => {
  class FakeTrustedHTML {
    constructor(
      readonly value: string,
      readonly policy: string
    ) {}
    toString(): string {
      return this.value;
    }
  }
  interface PolicyRules {
    createHTML?: (s: string) => string;
    createScriptURL?: (s: string) => string;
  }
  interface FakePolicy {
    createHTML: (s: string) => FakeTrustedHTML;
    createScriptURL: (s: string) => string;
  }
  const policies: string[] = [];
  (globalThis as unknown as { trustedTypes: unknown }).trustedTypes = {
    createPolicy(name: string, rules: PolicyRules): FakePolicy {
      policies.push(name);
      return {
        createHTML: (s: string): FakeTrustedHTML =>
          new FakeTrustedHTML(rules.createHTML?.(s) ?? s, name),
        createScriptURL: (s: string): string => rules.createScriptURL?.(s) ?? s,
      };
    },
    getAttributeType: (): null => null,
  };
  return { FakeTrustedHTML, policies, calls: [] as SinkCall[] };
});

function record(sink: string, value: unknown): string {
  const trusted = value instanceof sinkState.FakeTrustedHTML;
  sinkState.calls.push({ sink, policy: trusted ? value.policy : null });
  return String(value);
}

/** Each wrapped descriptor, by owner and name, so none is wrapped twice. */
const wrapped = new Set<string>();
const owners = new Map<object, number>();

function ownerKey(owner: object, name: string): string {
  if (!owners.has(owner)) owners.set(owner, owners.size);
  return `${owners.get(owner)}:${name}`;
}

/** Wrap a setter wherever it is defined on the prototype chain. */
function wrapSetter(proto: object | undefined, name: string, sink: string): boolean {
  for (let p: object | null = proto ?? null; p; p = Object.getPrototypeOf(p) as object | null) {
    const desc = Object.getOwnPropertyDescriptor(p, name);
    if (desc?.set) {
      const key = ownerKey(p, name);
      if (wrapped.has(key)) return true;
      wrapped.add(key);
      const set = desc.set;
      Object.defineProperty(p, name, {
        ...desc,
        set(this: unknown, value: unknown) {
          set.call(this, record(sink, value));
        },
      });
      return true;
    }
  }
  return false;
}

/** Wrap a method whose argument at argIndex is markup. */
function wrapMethod(proto: object | undefined, name: string, sink: string, argIndex = 0): boolean {
  for (let p: object | null = proto ?? null; p; p = Object.getPrototypeOf(p) as object | null) {
    const desc = Object.getOwnPropertyDescriptor(p, name);
    if (typeof desc?.value === 'function') {
      const key = ownerKey(p, name);
      if (wrapped.has(key)) return true;
      wrapped.add(key);
      const fn = desc.value as (...args: unknown[]) => unknown;
      Object.defineProperty(p, name, {
        ...desc,
        value(this: unknown, ...args: unknown[]) {
          args[argIndex] = record(sink, args[argIndex]);
          return fn.apply(this, args);
        },
      });
      return true;
    }
  }
  return false;
}

// Every markup-parsing API; `missing` lists the ones the test DOM lacks.
const sinkWrappers: [string, () => boolean][] = [
  [
    'HTMLTemplateElement.innerHTML',
    () => wrapSetter(HTMLTemplateElement.prototype, 'innerHTML', 'Element.innerHTML'),
  ],
  [
    'HTMLElement.innerHTML',
    () => wrapSetter(HTMLElement.prototype, 'innerHTML', 'Element.innerHTML'),
  ],
  [
    'HTMLElement.outerHTML',
    () => wrapSetter(HTMLElement.prototype, 'outerHTML', 'Element.outerHTML'),
  ],
  [
    'ShadowRoot.innerHTML',
    () => wrapSetter(ShadowRoot.prototype, 'innerHTML', 'ShadowRoot.innerHTML'),
  ],
  [
    'HTMLIFrameElement.srcdoc',
    () => wrapSetter(HTMLIFrameElement.prototype, 'srcdoc', 'HTMLIFrameElement.srcdoc'),
  ],
  [
    'Element.insertAdjacentHTML',
    () => wrapMethod(HTMLElement.prototype, 'insertAdjacentHTML', 'Element.insertAdjacentHTML', 1),
  ],
  [
    'Element.setHTMLUnsafe',
    () => wrapMethod(HTMLElement.prototype, 'setHTMLUnsafe', 'Element.setHTMLUnsafe'),
  ],
  [
    'ShadowRoot.setHTMLUnsafe',
    () => wrapMethod(ShadowRoot.prototype, 'setHTMLUnsafe', 'ShadowRoot.setHTMLUnsafe'),
  ],
  ['Document.write', () => wrapMethod(Document.prototype, 'write', 'Document.write')],
  ['Document.writeln', () => wrapMethod(Document.prototype, 'writeln', 'Document.writeln')],
  [
    'Document.parseHTMLUnsafe',
    () => wrapMethod(Document, 'parseHTMLUnsafe', 'Document.parseHTMLUnsafe'),
  ],
  [
    'DOMParser.parseFromString',
    () => wrapMethod(DOMParser.prototype, 'parseFromString', 'DOMParser.parseFromString'),
  ],
  [
    'Range.createContextualFragment',
    () => wrapMethod(Range.prototype, 'createContextualFragment', 'Range.createContextualFragment'),
  ],
];
const missingSinks = sinkWrappers.filter(([, wrap]) => !wrap()).map(([name]) => name);

// Imported after the stand-in is installed, so the libraries see it.
await import('./chat-message.js');
await import('./chat-interagent-marker.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

const BODY = [
  'Hello @lead, see owner/repo#12 and sess_abcd1234.',
  '',
  '```',
  'go test ./pkg/... @not-a-mention',
  '```',
  '',
  '```go',
  'func main() {}',
  '```',
  '',
  '| a | b |',
  '|---|---|',
  '| 1 | 2 |',
  '',
  '[docs](https://example.com/docs) ![logo](https://elsewhere.example/logo.png)',
].join('\n');

/** Plain-string calls only: the ones a browser checking Trusted Types flags. */
function flagged(): SinkCall[] {
  return sinkState.calls.filter((c) => c.policy === null);
}

async function settle(el: ScionChatMessage): Promise<void> {
  await el.updateComplete;
  // renderContent() awaits the renderer, then re-renders.
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await el.updateComplete;
  await editorsReady(el);
}

/**
 * Waits until every code block editor in the message has mounted its
 * CodeMirror view: each editor loads CodeMirror, then clears its mount
 * point and builds the view.
 */
async function editorsReady(el: ScionChatMessage): Promise<void> {
  await vi.waitFor(
    () => {
      const editors = Array.from(el.shadowRoot?.querySelectorAll('scion-code-editor') ?? []);
      if (!editors.every((ed) => ed.shadowRoot?.querySelector('.editor-mount .cm-editor'))) {
        throw new Error('a code block editor has not mounted yet');
      }
    },
    { timeout: 2000, interval: 10 }
  );
}

async function mount(body: string): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  el.senderIsAgent = true;
  document.body.appendChild(el);
  await settle(el);
  // The first load of marked and DOMPurify takes a few more turns.
  await new Promise((r) => setTimeout(r, 0));
  await settle(el);
  return el;
}

describe('chat message markup boundary', () => {
  beforeEach(async () => {
    document.body.replaceChildren();
    // Warm the shared renderer and Lit's templates outside the counted window.
    const warm = await mount('warm up @x');
    warm.remove();
    sinkState.calls.length = 0;
  });

  afterEach(() => {
    document.body.replaceChildren();
  });

  it('renders a message with no plain-string markup parsing', async () => {
    const el = await mount(BODY);

    const content = el.shadowRoot?.querySelector('.md-content');
    expect(content?.querySelector('.mention')?.textContent).toBe('@lead');
    expect(content?.querySelector('.gh-ref-link')?.textContent).toBe('owner/repo#12');
    expect(content?.querySelectorAll('scion-code-editor')).toHaveLength(1);
    expect(content?.querySelector('a[href="https://example.com/docs"]')?.getAttribute('rel')).toBe(
      'noopener noreferrer'
    );
    expect(flagged()).toEqual([]);
  });

  it('parses markup once per body change while a reply streams, inside the sanitizer', async () => {
    const el = await mount('Hello');
    sinkState.calls.length = 0;

    const steps = [
      'Hello @lead',
      'Hello @lead, see the draft',
      'Hello @lead, see\n\n```go\nfunc draft() {}\n```',
      BODY,
    ];
    for (const step of steps) {
      el.body = step;
      await settle(el);
    }

    const contents = el.shadowRoot?.querySelectorAll('.md-content') ?? [];
    expect(contents).toHaveLength(1);
    const content = contents[0];
    expect(content.querySelector('.mention')?.textContent).toBe('@lead');
    expect(content.querySelector('table')).not.toBeNull();
    expect(content.querySelector('a[href="https://example.com/docs"]')).not.toBeNull();
    const editors = content.querySelectorAll<HTMLElement & { content: string }>(
      'scion-code-editor'
    );
    expect(editors).toHaveLength(1);
    // Code in an editor is not in textContent; the editor holds the final block.
    expect(editors[0].content).toContain('func main()');
    expect(editors[0].content).not.toContain('draft');
    expect(content.textContent).not.toContain('draft');
    expect(flagged()).toEqual([]);
    // One parse per body change, and only through the sanitizer's own policy.
    expect(sinkState.calls).toEqual(
      steps.map(() => ({ sink: 'DOMParser.parseFromString', policy: 'dompurify' }))
    );
  });

  it('counts every markup-parsing API the test DOM has', () => {
    // The test DOM lacks these, so calls to them are not counted in this
    // suite. A test DOM upgrade that adds one fails this check (the safe
    // direction): add its wrapper's name to the counted set by updating
    // the list.
    expect(missingSinks).toEqual([
      'Element.setHTMLUnsafe',
      'Document.writeln',
      'Document.parseHTMLUnsafe',
    ]);
  });

  it('creates no policy of its own: only the libraries that parse markup do', () => {
    expect([...new Set(sinkState.policies)].sort()).toEqual(['dompurify', 'lit-html']);
  });

  it('re-renders that do not change the body parse nothing', async () => {
    const el = await mount(BODY);
    sinkState.calls.length = 0;

    el.senderName = 'Someone else';
    el.starred = true;
    await settle(el);

    // Lit compiles a template the first time it is shown, through its own
    // policy; the message content is not parsed again.
    expect(sinkState.calls.filter((c) => c.policy !== 'lit-html')).toEqual([]);
  });

  it('renders a markdown attachment preview with no plain-string markup parsing', async () => {
    type Preview = HTMLElement & { content: string; updateComplete: Promise<boolean> };
    const preview = document.createElement('scion-markdown-preview') as Preview;
    preview.content = '# Notes\n\nSee [docs](https://example.com/docs).';
    document.body.appendChild(preview);
    for (let i = 0; i < 3; i++) {
      await preview.updateComplete;
      await new Promise((r) => setTimeout(r, 0));
    }

    const container = preview.shadowRoot?.querySelector('.preview-container');
    expect(container?.getAttribute('part')).toBe('container');
    expect(container?.querySelector('h1')?.textContent).toBe('Notes');
    expect(flagged()).toEqual([]);
    // Parsed once on mount, not once per lifecycle callback.
    expect(sinkState.calls.filter((c) => c.policy === 'dompurify')).toHaveLength(1);
  });

  it('opens an inter-agent message preview with no plain-string markup parsing', async () => {
    type Marker = HTMLElement & {
      messages: unknown[];
      expanded: boolean;
      updateComplete: Promise<boolean>;
    };
    const marker = document.createElement('scion-chat-interagent-marker') as Marker;
    marker.messages = [
      {
        id: 'm1',
        sender: 'agent:a',
        recipient: 'agent:b',
        msg: 'Hi **there**, @lead',
        createdAt: '2026-01-01T00:00:00Z',
      },
    ];
    marker.expanded = true;
    document.body.appendChild(marker);
    await marker.updateComplete;
    sinkState.calls.length = 0;

    const open = (
      marker as unknown as {
        openMessagePreview(msg: unknown, e: Event): Promise<void>;
      }
    ).openMessagePreview.bind(marker);
    await open(marker.messages[0], new Event('click'));
    await marker.updateComplete;

    expect(marker.shadowRoot?.querySelector('.ia-full-body strong')?.textContent).toBe('there');
    expect(flagged()).toEqual([]);
  });
});
