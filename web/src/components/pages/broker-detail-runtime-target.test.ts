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
 * The read-only "Runtime target" card on the broker detail page
 * (ptone/scion#3282, ptone/scion#3283): the stored target, then the
 * defaults the instance reported, with safe_to_evict as unset/true/false
 * and no field FC4-a withholds anywhere in the DOM.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { RuntimeBroker, RuntimeTargetPresentation } from '../../shared/types.js';
import type { ScionPageBrokerDetail } from './broker-detail.js';
import {
  fullPresentation,
  presentationWithNeverFields,
} from '../../shared/__fixtures__/runtime-target-presentation.js';

function flatBroker(presentation?: RuntimeTargetPresentation): RuntimeBroker {
  return {
    id: 'b-1',
    name: 'flat-k8s',
    slug: 'flat-k8s',
    version: '1.2.3',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-10-14T12:00:00Z',
    autoProvide: false,
    runtimeTarget: { id: 'rt-1234', type: 'kubernetes', displayName: 'Cluster A' },
    runtimeTargetPresentation: presentation,
    runtimeTargetPresentationObservedAt: presentation ? '2026-10-14T12:00:00Z' : undefined,
  } as RuntimeBroker;
}

interface CardRenderer {
  renderRuntimeTargetCard(b: RuntimeBroker): TemplateResult | typeof nothing;
}

/** Render the card for `broker` into a detached host (null when there is none). */
function renderCard(broker: RuntimeBroker): HTMLElement | null {
  const el = document.createElement('scion-page-broker-detail') as ScionPageBrokerDetail;
  const tpl = (el as unknown as CardRenderer).renderRuntimeTargetCard(broker);
  if (tpl === nothing) return null;
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

/** The card's rows of one list (dt/dd pairs) as a label -> value map. */
function rows(host: HTMLElement, cls: string): Record<string, string> {
  const dl = host.querySelector(`dl.${cls}`);
  expect(dl, cls).not.toBeNull();
  const out: Record<string, string> = {};
  const dts = Array.from(dl!.querySelectorAll('dt'));
  const dds = Array.from(dl!.querySelectorAll('dd'));
  expect(dts).toHaveLength(dds.length);
  dts.forEach((dt, i) => {
    out[dt.textContent!.trim()] = dds[i].textContent!.trim();
  });
  return out;
}

describe('broker detail Runtime target card', () => {
  beforeAll(async () => {
    await import('./broker-detail.js');
  }, 30_000);

  it('shows the stored target, the reported defaults and the caption', () => {
    const host = renderCard(flatBroker(fullPresentation()))!;
    expect(host.querySelector('h2')?.textContent?.trim()).toBe('Runtime target');
    expect(host.querySelector('sl-icon')?.getAttribute('name')).toBe('hdd-rack');
    expect(rows(host, 'target-rows')).toEqual({
      ID: 'rt-1234',
      Type: 'kubernetes',
      'Display name': 'Cluster A',
      Namespace: 'agents',
    });
    const reported = rows(host, 'presentation-rows');
    expect(reported['Safe to evict']).toBe('false');
    expect(reported['Harness override: claude']).toContain('env keys EMPTY, TOKEN');
    const caption = host.querySelector('.presentation-caption')?.textContent ?? '';
    expect(caption).toContain('Reported by the instance at');
    expect(caption).toContain('2026');
    expect(caption).toContain('not the configuration of existing agents');
    expect(caption).toContain('not a check of cluster permissions');
    // Read-only: nothing to edit.
    expect(host.querySelector('sl-input, sl-button, input, button, form')).toBeNull();
  });

  it('keeps safe_to_evict tri-state in the DOM', () => {
    const cases: Record<string, boolean | undefined> = {
      unset: undefined,
      true: true,
      false: false,
    };
    for (const [want, value] of Object.entries(cases)) {
      const p = fullPresentation();
      if (value === undefined) delete p.target!.safeToEvict;
      else p.target!.safeToEvict = value;
      expect(rows(renderCard(flatBroker(p))!, 'presentation-rows')['Safe to evict']).toBe(want);
    }
  });

  it('never puts a withheld field in the DOM, even when the payload carries it', () => {
    const host = renderCard(flatBroker(presentationWithNeverFields()))!;
    expect(host.innerHTML).not.toContain('NEVER');
    expect(host.textContent).toContain('API_KEY');
    expect(rows(host, 'presentation-rows')).toEqual(
      rows(renderCard(flatBroker(fullPresentation()))!, 'presentation-rows')
    );
  });

  it('says when nothing was reported, and has no card on a legacy broker', () => {
    const host = renderCard(flatBroker(undefined))!;
    expect(host.querySelector('dl.presentation-rows')).toBeNull();
    expect(host.querySelector('.presentation-caption')?.textContent?.trim()).toBe(
      'No defaults reported yet.'
    );
    const legacy = { ...flatBroker(fullPresentation()), runtimeTarget: undefined };
    expect(renderCard(legacy)).toBeNull();
  });
});
