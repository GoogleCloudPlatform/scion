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

import { describe, it, expect, afterEach, vi } from 'vitest';
import { createHmac } from 'node:crypto';
import './webhook-endpoint-list.js';
import {
  buildScionV1CurlExample,
  buildWebhookCurlExample,
  SUPPORTED_SIGNATURE_MODES,
  type ScionWebhookEndpointList,
} from './webhook-endpoint-list.js';
import type { TriggerDefinition, WebhookEndpoint } from './types.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const sampleEndpoint: WebhookEndpoint = {
  id: 'ep-1',
  projectId: 'proj-1',
  name: 'ci-deploys',
  description: 'CI deploy hook',
  url: 'https://hub.example/api/v1/hooks/ep-1',
  auth: 'hmac',
  signatureMode: 'scion_v1',
  maxBodyBytes: 65536,
  replayWindowSeconds: 300,
  secretVersion: 1,
  configVersion: 1,
  status: 'active',
  receiveCount: 12,
  rejectCount: 1,
  createdAt: '2026-11-20T10:00:00Z',
  updatedAt: '2026-11-20T10:00:00Z',
};

describe('scion-webhook-endpoint-list', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('shows created secret once and clears it from component state when dialog closes', async () => {
    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.endsWith('/webhook-endpoints')) {
        return Promise.resolve(
          jsonResponse(
            {
              ...sampleEndpoint,
              id: 'ep-new',
              name: 'new-hook',
              secret: 'whsec_ONCE_ONLY_SECRET_12345',
            },
            201
          )
        );
      }
      return Promise.resolve(jsonResponse({ endpoints: [sampleEndpoint], totalCount: 1 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-endpoint-row="ep-1"]')).toBeTruthy();

    // Open create dialog and submit
    (el.shadowRoot?.querySelector('[data-create-endpoint]') as HTMLElement).click();
    await el.updateComplete;

    const nameInput = el.shadowRoot?.querySelector(
      '[data-endpoint-name-input]'
    ) as HTMLInputElement;
    nameInput.value = 'new-hook';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-submit-endpoint]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    // Secret is shown once in the dialog along with scion_v1 curl/openssl example
    const revealed = el.shadowRoot?.querySelector('[data-revealed-secret]');
    expect(revealed?.textContent).toBe('whsec_ONCE_ONLY_SECRET_12345');
    const curlExample = el.shadowRoot?.querySelector('[data-curl-example]')?.textContent ?? '';
    expect(curlExample).toContain("printf 'v1\\n%s\\n%s\\n%s\\n%s\\n%s'");
    expect(curlExample).toContain(`openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}'`);
    expect(curlExample).not.toContain(`-r | awk '{print $1}'`);
    expect(curlExample).toContain('X-Scion-Signature: t=${TS},v1=${SIG}');
    expect(curlExample).toContain('X-Scion-Delivery: ${DELIVERY}');
    expect(curlExample).toContain('X-Scion-Event: ${EVENT}');
    expect(curlExample).toContain('X-Scion-Subject: ${SUBJECT}');
    expect(curlExample).toContain('--data-raw "$BODY"');
    expect(curlExample).not.toContain('X-Scion-Timestamp');

    // Close dialog -> secret is cleared from DOM and component state
    (el.shadowRoot?.querySelector('[data-close-secret-dialog]') as HTMLElement).click();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-revealed-secret]')).toBeNull();
    expect(el.shadowRoot?.textContent).not.toContain('whsec_ONCE_ONLY_SECRET_12345');
    // Verify private state field is also cleared
    expect((el as unknown as { revealedSecret: string | null }).revealedSecret).toBeNull();
  });

  it('rotates secret with 24h grace note and clears secret on close', async () => {
    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.includes('/rotate-secret')) {
        return Promise.resolve(
          jsonResponse({
            ...sampleEndpoint,
            secretVersion: 2,
            secret: 'whsec_ROTATED_SECRET_999',
            graceWindowExpires: '2026-11-21T10:00:00Z',
          })
        );
      }
      return Promise.resolve(jsonResponse({ endpoints: [sampleEndpoint], totalCount: 1 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-rotate-endpoint="ep-1"]') as HTMLElement).click();
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-confirm-rotate]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-revealed-secret]')?.textContent).toBe(
      'whsec_ROTATED_SECRET_999'
    );
    expect(el.shadowRoot?.textContent).toContain('24-hour grace window');

    (el.shadowRoot?.querySelector('[data-close-secret-dialog]') as HTMLElement).click();
    await el.updateComplete;
    expect((el as unknown as { revealedSecret: string | null }).revealedSecret).toBeNull();
  });

  it('lists affected rules in the delete confirmation dialog', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ endpoints: [sampleEndpoint], totalCount: 1 }))
    );

    const dependentTrigger: TriggerDefinition = {
      id: 'tr-1',
      projectId: 'proj-1',
      name: 'deploy-notifier',
      source: { kind: 'webhook', id: 'ep-1' },
      match: { types: ['webhook.received'], rules: [] },
      action: { type: 'message', agentId: 'ag-1', message: 'hi' },
      subject: { mode: 'new' },
      status: 'active',
      authorizationRevision: 1,
      fireCount: 0,
      errorCount: 0,
      createdAt: '2026-11-20T10:00:00Z',
      updatedAt: '2026-11-20T10:00:00Z',
    };

    const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
    el.projectId = 'proj-1';
    el.triggers = [dependentTrigger];
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-delete-endpoint="ep-1"]') as HTMLElement).click();
    await el.updateComplete;

    expect(el.shadowRoot?.textContent).toContain('deploy-notifier');
    expect(el.shadowRoot?.textContent).toContain('source_deleted');
  });

  it('builds a scion_v1 curl example matching the hub canonical string and verified test vector (R2)', () => {
    const snippet = buildScionV1CurlExample(
      'https://hub.example/api/v1/hooks/ep-1',
      'test-secret-key'
    );
    expect(snippet).toContain('EVENT=""');
    expect(snippet).toContain('SUBJECT=""');
    expect(snippet).toContain(
      '# X-Scion-Event: <name> sets event.type=webhook.<name> (default webhook.received rules then stop matching; use --type webhook.<name>)'
    );
    expect(snippet).toContain(
      '# X-Scion-Subject is stored as event.subject=webhook:<endpoint-id>/<subject>'
    );
    expect(snippet).toContain(
      `printf 'v1\\n%s\\n%s\\n%s\\n%s\\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY"`
    );
    expect(snippet).toContain(`openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}'`);
    expect(snippet).not.toContain(`-r | awk '{print $1}'`);
    expect(snippet).toContain('-H "X-Scion-Signature: t=${TS},v1=${SIG}"');
    expect(snippet).toContain('-H "X-Scion-Delivery: ${DELIVERY}"');
    expect(snippet).toContain('-H "X-Scion-Event: ${EVENT}"');
    expect(snippet).toContain('-H "X-Scion-Subject: ${SUBJECT}"');
    expect(snippet).toContain('--data-raw "$BODY"');
    expect(snippet).not.toContain('X-Scion-Timestamp');

    // Verify canonical string HMAC (including a body ending in a trailing newline)
    // matches the hub's VerifyScionV1Signature vector in pkg/hub/trigger/signature_test.go.
    const secret = 'test-secret-key';
    const ts = '1700000000';
    const delivery = 'deliv-42';
    const event = '';
    const subject = 'service/api';
    const bodyWithNewline = '{"status":"ok"}\n';
    const canonical = `v1\n${ts}\n${delivery}\n${event}\n${subject}\n${bodyWithNewline}`;
    const sigHex = createHmac('sha256', secret).update(canonical, 'utf8').digest('hex');
    expect(sigHex).toBe('bab6dadd269fac38afc019afc0b0f443a054772d5b0ba57837135717351de9de');
  });

  it('omits the scion_v1 curl example for non-scion_v1 endpoints (R2)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string, init?: RequestInit) => {
        if (init?.method === 'POST' && url.endsWith('/webhook-endpoints')) {
          return Promise.resolve(
            jsonResponse(
              {
                ...sampleEndpoint,
                id: 'ep-body',
                name: 'body-only-hook',
                signatureMode: 'body_only',
                secret: 'whsec_BODY_ONLY_123',
              },
              201
            )
          );
        }
        return Promise.resolve(jsonResponse({ endpoints: [], totalCount: 0 }));
      })
    );

    const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-create-endpoint]') as HTMLElement).click();
    await el.updateComplete;
    const nameInput = el.shadowRoot?.querySelector(
      '[data-endpoint-name-input]'
    ) as HTMLInputElement;
    nameInput.value = 'body-only-hook';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-endpoint]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-revealed-secret]')?.textContent).toBe(
      'whsec_BODY_ONLY_123'
    );
    expect(el.shadowRoot?.querySelector('[data-curl-example]')).toBeNull();
  });

  it('copies the raw scion_v1 snippet byte-for-byte to the clipboard with all dollar signs intact', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const origClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    try {
      vi.stubGlobal(
        'fetch',
        vi.fn().mockImplementation((url: string, init?: RequestInit) => {
          if (init?.method === 'POST' && url.endsWith('/webhook-endpoints')) {
            return Promise.resolve(
              jsonResponse(
                {
                  ...sampleEndpoint,
                  id: 'ep-copy',
                  name: 'copy-hook',
                  url: 'https://hub.example/api/v1/hooks/ep-copy',
                  secret: 'whsec_COPY_SECRET_ABC',
                },
                201
              )
            );
          }
          return Promise.resolve(jsonResponse({ endpoints: [], totalCount: 0 }));
        })
      );

      const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
      el.projectId = 'proj-1';
      document.body.appendChild(el);
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      (el.shadowRoot?.querySelector('[data-create-endpoint]') as HTMLElement).click();
      await el.updateComplete;
      const nameInput = el.shadowRoot?.querySelector(
        '[data-endpoint-name-input]'
      ) as HTMLInputElement;
      nameInput.value = 'copy-hook';
      nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
      await el.updateComplete;
      (el.shadowRoot?.querySelector('[data-submit-endpoint]') as HTMLElement).click();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      const copyBtn = el.shadowRoot?.querySelector('[data-copy-curl-example]') as HTMLElement;
      expect(copyBtn).toBeTruthy();
      copyBtn.click();
      await new Promise((r) => setTimeout(r, 10));

      const expected = buildScionV1CurlExample(
        'https://hub.example/api/v1/hooks/ep-copy',
        'whsec_COPY_SECRET_ABC'
      );
      expect(writeText).toHaveBeenCalledTimes(1);
      expect(writeText).toHaveBeenCalledWith(expected);
      expect(writeText.mock.calls[0][0]).toContain('-H "X-Scion-Signature: t=${TS},v1=${SIG}"');
      expect(writeText.mock.calls[0][0]).toContain('-H "X-Scion-Event: ${EVENT}"');
      expect(writeText.mock.calls[0][0]).toContain('-H "X-Scion-Subject: ${SUBJECT}"');
      expect(writeText.mock.calls[0][0]).toContain(`awk '{print $NF}'`);
    } finally {
      if (origClipboard) {
        Object.defineProperty(navigator, 'clipboard', origClipboard);
      } else {
        delete (navigator as unknown as Record<string, unknown>).clipboard;
      }
    }
  });

  it('offers exactly the hub-supported signature modes in the form dropdown (no body_only)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ endpoints: [], totalCount: 0 }))
    );

    const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-create-endpoint]') as HTMLElement).click();
    await el.updateComplete;

    const select = el.shadowRoot?.querySelector('[data-signature-mode-select]');
    expect(select).toBeTruthy();
    const values = Array.from(select?.querySelectorAll('sl-option') ?? []).map((o) =>
      o.getAttribute('value')
    );
    expect(values).toEqual([...SUPPORTED_SIGNATURE_MODES]);
    expect(values).toEqual(['scion_v1', 'github_v1', 'stripe_v1']);
    expect(values).not.toContain('body_only');
  });

  it('shows the matching portable curl snippet for github_v1 and stripe_v1 endpoints', async () => {
    for (const mode of ['github_v1', 'stripe_v1'] as const) {
      vi.stubGlobal(
        'fetch',
        vi.fn().mockImplementation((url: string, init?: RequestInit) => {
          if (init?.method === 'POST' && url.endsWith('/webhook-endpoints')) {
            return Promise.resolve(
              jsonResponse(
                {
                  ...sampleEndpoint,
                  id: `ep-${mode}`,
                  name: `${mode}-hook`,
                  url: `https://hub.example/api/v1/hooks/ep-${mode}`,
                  signatureMode: mode,
                  secret: `whsec_${mode}_SECRET`,
                },
                201
              )
            );
          }
          return Promise.resolve(jsonResponse({ endpoints: [], totalCount: 0 }));
        })
      );

      const el = document.createElement('scion-webhook-endpoint-list') as ScionWebhookEndpointList;
      el.projectId = 'proj-1';
      document.body.appendChild(el);
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      (el.shadowRoot?.querySelector('[data-create-endpoint]') as HTMLElement).click();
      await el.updateComplete;
      const nameInput = el.shadowRoot?.querySelector(
        '[data-endpoint-name-input]'
      ) as HTMLInputElement;
      nameInput.value = `${mode}-hook`;
      nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
      await el.updateComplete;
      (el.shadowRoot?.querySelector('[data-submit-endpoint]') as HTMLElement).click();
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;

      const example = el.shadowRoot?.querySelector('[data-curl-example]')?.textContent ?? '';
      expect(example).toBe(
        buildWebhookCurlExample(
          mode,
          `https://hub.example/api/v1/hooks/ep-${mode}`,
          `whsec_${mode}_SECRET`
        )
      );
      expect(example).toContain(`openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}'`);
      expect(example).not.toContain(`-r | awk '{print $1}'`);
      if (mode === 'github_v1') {
        expect(example).toContain('X-Hub-Signature-256: sha256=${SIG}');
      } else {
        expect(example).toContain('Stripe-Signature: t=${TS},v1=${SIG}');
      }

      document.body.innerHTML = '';
      vi.restoreAllMocks();
    }
  });
});
