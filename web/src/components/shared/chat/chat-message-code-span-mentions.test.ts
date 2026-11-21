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
 * Cross-language vector tests for `<scion-chat-message>` standalone inline
 * code-span mentions (`hub.code_span_mentions`, ptone/scion#8783) using the
 * real CommonMark markdown renderer (`marked` + `DOMPurify`).
 */

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { marked } from 'marked';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { CODE_SPAN_MENTIONS_FLAG, setFeatureFlag } from '../../../utils/feature-flags.js';

// Use real `marked` for CommonMark parsing (backticks, double-backticks,
// escapes, fences, blockquotes, emphasis, GFM autolinks) without DOMPurify,
// which strips `<blockquote>` under happy-dom's DOM implementation.
vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (markdown: string) => marked.parse(markdown, { async: false }),
      renderFragment: (markdown: string) => {
        const tpl = document.createElement('template');
        tpl.innerHTML = marked.parse(markdown, { async: false });
        return tpl.content;
      },
    }),
}));

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));
vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-message.js');
const { buildKnownMentionSlugs, buildKnownUsersByEmail } = await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

interface SharedVectorCase {
  name: string;
  text: string;
  webRenderedOff: string[];
  webRenderedOn: string[];
}

interface SharedVectorFile {
  roster: {
    agents: Array<{ id: string; slug: string; name: string }>;
    users: Array<{ id: string; name: string; email: string }>;
  };
  cases: SharedVectorCase[];
}

const here = dirname(fileURLToPath(import.meta.url));
const sharedVectors: SharedVectorFile = JSON.parse(
  readFileSync(
    resolve(here, '../../../../../pkg/messages/testdata/code_span_mentions_vectors.json'),
    'utf8'
  )
);

const testMembers = sharedVectors.roster.users.map((u) => ({
  name: u.name,
  email: u.email,
  kind: 'user',
}));
const testAgents = sharedVectors.roster.agents.map((a) => ({
  id: a.id,
  slug: a.slug,
  name: a.name,
}));

const knownUsersByEmail = buildKnownUsersByEmail(testMembers, '');
const knownMentionSlugs = buildKnownMentionSlugs(testMembers, testAgents, '');

async function mountWithRoster(body: string): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  el.knownUsersByEmail = knownUsersByEmail;
  el.knownMentionSlugs = knownMentionSlugs;
  document.body.appendChild(el);
  await el.updateComplete;
  // Wait for async getMarkdownRenderer() + Lit re-render.
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  return el;
}

function renderedMentionSlugs(el: ScionChatMessage): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.md-content .mention') ?? []).map(
    (node) => node.getAttribute('data-mention') ?? ''
  );
}

describe('scion-chat-message code-span mentions (shared vectors + real marked)', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  for (const tc of sharedVectors.cases) {
    it(`${tc.name} — flag OFF`, async () => {
      setFeatureFlag(CODE_SPAN_MENTIONS_FLAG, false);
      const el = await mountWithRoster(tc.text);
      expect(renderedMentionSlugs(el)).toEqual(tc.webRenderedOff);
    });

    it(`${tc.name} — flag ON`, async () => {
      setFeatureFlag(CODE_SPAN_MENTIONS_FLAG, true);
      const el = await mountWithRoster(tc.text);
      expect(renderedMentionSlugs(el)).toEqual(tc.webRenderedOn);
    });
  }

  it('re-renders bubble when hub.code_span_mentions flag toggles dynamically', async () => {
    setFeatureFlag(CODE_SPAN_MENTIONS_FLAG, false);
    const el = await mountWithRoster('Ping `@coordinator` and `@state`.');
    expect(renderedMentionSlugs(el)).toEqual([]);
    expect(el.shadowRoot?.querySelectorAll('.md-content code').length).toBe(2);

    setFeatureFlag(CODE_SPAN_MENTIONS_FLAG, true);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    expect(renderedMentionSlugs(el)).toEqual(['coordinator']);
    // Unknown `@state` stays plain <code>.
    const remainingCode = Array.from(el.shadowRoot?.querySelectorAll('.md-content code') ?? []).map(
      (c) => c.textContent
    );
    expect(remainingCode).toEqual(['@state']);
  });

  it('rejects intraword underscore wrappers around code-span mentions', async () => {
    setFeatureFlag(CODE_SPAN_MENTIONS_FLAG, true);
    const el = await mountWithRoster('Not foo_`@alice`_ or _`@bob`_bar.');
    expect(renderedMentionSlugs(el)).toEqual([]);
  });
});
