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

import { describe, it, expect, afterEach } from 'vitest';
import { render } from 'lit';
import { agentBadges, type ScionAgentBadges } from './agent-badges.js';
import type { Agent } from '../../shared/types.js';

async function mount(props: Partial<Pick<ScionAgentBadges, 'vendor' | 'size' | 'harness'>>) {
  const el = document.createElement('scion-agent-badges');
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const mark = (el: HTMLElement) => el.shadowRoot!.querySelector<HTMLElement>('.mark');
const chip = (el: HTMLElement) => el.shadowRoot!.querySelector<HTMLElement>('.size');
const label = (el: HTMLElement) =>
  el.shadowRoot!.querySelector('.badges')?.getAttribute('title') ?? '';

describe('scion-agent-badges', () => {
  afterEach(() => {
    document.body.querySelectorAll('scion-agent-badges').forEach((n) => n.remove());
    document.body.querySelectorAll('div[data-host]').forEach((n) => n.remove());
    delete window.__SCION_FEATURES__;
  });

  it('renders nothing while the experiment is off', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_badges': false };
    const el = await mount({ vendor: 'anthropic', size: 'large' });
    expect(el.shadowRoot!.querySelector('.badges')).toBeNull();
    expect(el.shadowRoot!.textContent?.trim()).toBe('');
  });

  it('renders nothing when the experiment is unset (default off)', async () => {
    const el = await mount({ vendor: 'anthropic', size: 'large' });
    expect(el.shadowRoot!.querySelector('.badges')).toBeNull();
  });

  describe('experiment on', () => {
    afterEach(() => delete window.__SCION_FEATURES__);
    const on = () => (window.__SCION_FEATURES__ = { 'web.agent_badges': true });

    it('shows the vendor monogram and the size chip, with both in the tooltip', async () => {
      on();
      const el = await mount({ vendor: 'anthropic', size: 'extra-large' });
      expect(mark(el)?.textContent).toBe('A');
      expect(mark(el)?.classList.contains('multi')).toBe(false);
      expect(chip(el)?.textContent).toBe('XL');
      expect(label(el)).toBe('Harness vendor: Anthropic\nConfigured model size: Extra large');
      expect(el.shadowRoot!.querySelector('.badges')?.getAttribute('aria-label')).toBe(label(el));
    });

    it('omits the size chip when the size is empty', async () => {
      on();
      const el = await mount({ vendor: 'openai', size: '' });
      expect(mark(el)?.textContent).toBe('O');
      expect(chip(el)).toBeNull();
      expect(label(el)).toBe('Harness vendor: OpenAI');
    });

    it('shows only the outlined harness mark for a multi-vendor harness, even with a size', async () => {
      on();
      const el = await mount({ vendor: 'opencode', size: 'large' });
      expect(mark(el)?.textContent).toBe('O');
      expect(mark(el)?.classList.contains('multi')).toBe(true);
      expect(chip(el)).toBeNull();
      expect(label(el)).toBe('Harness: OpenCode');
    });

    it('shows a neutral ? for an unknown or missing vendor, keeping the size', async () => {
      on();
      const unknown = await mount({ vendor: 'acme', size: 'small' });
      expect(mark(unknown)?.textContent).toBe('?');
      expect(chip(unknown)?.textContent).toBe('S');
      expect(label(unknown)).toBe('Harness: unknown\nConfigured model size: Small');
      const missing = await mount({});
      expect(mark(missing)?.textContent).toBe('?');
      expect(chip(missing)).toBeNull();
    });

    it('names the harness in the tooltip when the row carries it', async () => {
      on();
      const el = await mount({ vendor: 'google', size: 'medium', harness: 'gemini-cli' });
      expect(label(el)).toBe('Harness: Gemini CLI (Google)\nConfigured model size: Medium');
    });

    it('agentBadges() binds the row fields, treating absent and cleared values as empty', async () => {
      on();
      const host = document.createElement('div');
      host.dataset.host = '';
      document.body.appendChild(host);
      const row = {
        id: 'a1',
        harnessVendor: 'xai',
        modelSize: '',
        appliedConfig: { harness: 'grok-build' },
      } as unknown as Agent;
      render(agentBadges(row), host);
      const el = host.querySelector('scion-agent-badges')!;
      await el.updateComplete;
      expect(el.vendor).toBe('xai');
      expect(el.size).toBe('');
      expect(el.harness).toBe('grok-build');
      expect(mark(el)?.textContent).toBe('x');
      expect(chip(el)).toBeNull();

      render(agentBadges({ id: 'a2' } as Agent), host);
      await el.updateComplete;
      expect(el.vendor).toBe('');
      expect(el.harness).toBe('');
      expect(mark(el)?.textContent).toBe('?');
    });
  });
});
