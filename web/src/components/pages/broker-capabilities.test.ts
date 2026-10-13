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
 * WebPTY capability tag on the broker list (card and table) and the broker
 * detail page (ptone/scion#4922). The hub reports web terminal support as
 * capabilities.webPty; the tag carries the enabled class only when it is true.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { BrokerCapabilities, RuntimeBroker } from '../../shared/types.js';

function makeBroker(capabilities: BrokerCapabilities): RuntimeBroker {
  return {
    id: 'b-1',
    name: 'broker-1',
    slug: 'broker-1',
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-10-13T12:00:00Z',
    autoProvide: false,
    capabilities,
  };
}

function caps(webPty: boolean): BrokerCapabilities {
  return { webPty, sync: false, attach: false };
}

/** Return the capability tag labelled WebPTY inside `scope`. */
function webPtyTag(scope: ParentNode, selector: string): Element {
  const tag = Array.from(scope.querySelectorAll(`${selector} .capability-tag`)).find(
    (t) => t.textContent?.trim() === 'WebPTY'
  );
  expect(tag).toBeDefined();
  return tag!;
}

type BrokersPage = HTMLElement & {
  renderBrokerCard(broker: RuntimeBroker): TemplateResult;
  renderBrokerRow(broker: RuntimeBroker): TemplateResult;
};

describe('broker list WebPTY capability tag', () => {
  let page: BrokersPage;

  beforeAll(async () => {
    await import('./brokers.js');
    page = document.createElement('scion-page-brokers') as unknown as BrokersPage;
  }, 30_000);

  function cardTag(webPty: boolean): Element {
    const host = document.createElement('div');
    render(page.renderBrokerCard(makeBroker(caps(webPty))), host);
    return webPtyTag(host, '.broker-details');
  }

  function rowTag(webPty: boolean): Element {
    const tbody = document.createElement('tbody');
    document.createElement('table').appendChild(tbody);
    render(page.renderBrokerRow(makeBroker(caps(webPty))), tbody);
    return webPtyTag(tbody, '.capability-tags-inline');
  }

  it('card: marks WebPTY enabled when capabilities.webPty is true', () => {
    expect(cardTag(true).classList.contains('enabled')).toBe(true);
  });

  it('card: does not mark WebPTY enabled when capabilities.webPty is false', () => {
    expect(cardTag(false).classList.contains('enabled')).toBe(false);
  });

  it('table row: marks WebPTY enabled when capabilities.webPty is true', () => {
    expect(rowTag(true).classList.contains('enabled')).toBe(true);
  });

  it('table row: does not mark WebPTY enabled when capabilities.webPty is false', () => {
    expect(rowTag(false).classList.contains('enabled')).toBe(false);
  });
});

describe('broker detail WebPTY capability tag', () => {
  beforeAll(async () => {
    await import('./broker-detail.js');
  }, 30_000);

  function detailTag(webPty: boolean): Element {
    const el = document.createElement('scion-page-broker-detail');
    const page = el as unknown as {
      loading: boolean;
      broker: RuntimeBroker | null;
      render(): TemplateResult;
    };
    page.loading = false;
    page.broker = makeBroker(caps(webPty));
    const host = document.createElement('div');
    render(page.render(), host);
    return webPtyTag(host, '.capabilities-row');
  }

  it('marks WebPTY enabled when capabilities.webPty is true', () => {
    expect(detailTag(true).classList.contains('enabled')).toBe(true);
  });

  it('does not mark WebPTY enabled when capabilities.webPty is false', () => {
    expect(detailTag(false).classList.contains('enabled')).toBe(false);
  });
});
