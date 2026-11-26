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
 * Tests for <scion-artifact-transfer-dialog> and transfer client helpers
 * (experiment hub.artifacts_transfer).
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { Artifact, ArtifactResponse, TransferCandidate } from '../../client/artifacts.js';
import type { ScionArtifactTransferDialog } from './artifact-transfer-dialog.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const ID = '00000000-0000-4000-8000-000000000042';
const OLD_OWNER = '11111111-1111-4111-8111-111111111111';
const NEW_OWNER = '22222222-2222-4222-8222-222222222222';
const ALT_OWNER = '33333333-3333-4333-8333-333333333333';

function makeArtifact(overrides: Partial<Artifact> = {}): Artifact {
  return {
    id: ID,
    ref: `scion://artifact/${ID}`,
    scopeKind: 'project',
    scopeRef: 'proj-1',
    ownerKind: 'agent',
    ownerRef: OLD_OWNER,
    key: 'daily-report',
    title: 'Daily status report',
    currentSeq: 2,
    createdAt: '2026-11-20T10:00:00Z',
    updatedAt: '2026-11-25T10:00:00Z',
    ownerDeleted: true,
    ...overrides,
  };
}

function makeResponse(
  artOverrides: Partial<Artifact> = {},
  respOverrides: Partial<ArtifactResponse> = {}
): ArtifactResponse {
  return {
    artifact: makeArtifact(artOverrides),
    canTransfer: true,
    canShare: true,
    canRetire: true,
    ...respOverrides,
  };
}

const TARGETS: TransferCandidate[] = [
  { id: NEW_OWNER, slug: 'builder-2', name: 'Builder Two', phase: 'running' },
  { id: ALT_OWNER, slug: 'worker-3', name: 'worker-3', phase: 'suspended' },
];

interface Call {
  method: string;
  url: string;
  body: unknown;
}

interface MockOptions {
  targets?: Response | TransferCandidate[];
  truncated?: boolean;
  transfer?: Response | ((body: Record<string, unknown>, callCount: number) => Response) | 'hold';
  reloadArtifact?: Response;
}

interface Mock {
  calls: Call[];
  release: () => void;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function mockFetch(opts: MockOptions = {}): Mock {
  let release: () => void = () => {};
  let transferCalls = 0;
  const m: Mock = { calls: [], release: () => release() };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      m.calls.push({ method, url, body });

      if (url === `/api/v1/artifacts/${ID}/transfer-targets` && method === 'GET') {
        if (opts.targets instanceof Response) return Promise.resolve(opts.targets.clone());
        return Promise.resolve(
          json({ agents: opts.targets ?? TARGETS, truncated: opts.truncated ?? false })
        );
      }

      if (url === `/api/v1/artifacts/${ID}/transfer` && method === 'POST') {
        transferCalls++;
        if (opts.transfer === 'hold') {
          return new Promise<Response>((resolve) => {
            release = (): void =>
              resolve(
                json({
                  artifact: makeResponse({
                    ownerRef: NEW_OWNER,
                    ownerDeleted: false,
                    formerOwnerKind: 'agent',
                    formerOwnerRef: OLD_OWNER,
                    transferredAt: '2026-11-26T04:00:00Z',
                  }),
                  notified: { newOwner: true, formerOwner: false },
                })
              );
          });
        }
        if (typeof opts.transfer === 'function') {
          return Promise.resolve(opts.transfer(body as Record<string, unknown>, transferCalls));
        }
        if (opts.transfer instanceof Response) {
          return Promise.resolve(opts.transfer.clone());
        }
        return Promise.resolve(
          json({
            artifact: makeResponse({
              ownerRef: NEW_OWNER,
              ownerDeleted: false,
              formerOwnerKind: 'agent',
              formerOwnerRef: OLD_OWNER,
              transferredAt: '2026-11-26T04:00:00Z',
            }),
            notified: { newOwner: true, formerOwner: false },
          })
        );
      }

      if (url === `/api/v1/artifacts/${ID}` && method === 'GET') {
        if (opts.reloadArtifact) return Promise.resolve(opts.reloadArtifact.clone());
        return Promise.resolve(json(makeResponse({ ownerRef: ALT_OWNER, ownerDeleted: false })));
      }

      return Promise.resolve(json({ error: { code: 'not_found', message: 'not found' } }, 404));
    })
  );
  return m;
}

async function settle(el: ScionArtifactTransferDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(
  props: Partial<{
    artifact: Artifact;
    ownerName: string;
    projectName: string;
    shareAuthority: string;
  }> = {}
): Promise<ScionArtifactTransferDialog> {
  const el = document.createElement(
    'scion-artifact-transfer-dialog'
  ) as ScionArtifactTransferDialog;
  el.artifact = props.artifact ?? makeArtifact();
  el.ownerName = props.ownerName ?? 'old-builder';
  el.projectName = props.projectName ?? 'Platform';
  el.shareAuthority = props.shareAuthority ?? 'delegator';
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function button(el: ScionArtifactTransferDialog, text: string): HTMLElement {
  const b = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
    (x) => x.textContent!.trim() === text
  );
  if (!b) throw new Error(`no button ${text}`);
  return b as HTMLElement;
}

function choose(el: ScionArtifactTransferDialog, value: string): void {
  const sel = el.shadowRoot!.querySelector('sl-select') as HTMLElement & { value: string };
  sel.value = value;
  sel.dispatchEvent(new Event('sl-change'));
}

function events(el: ScionArtifactTransferDialog): {
  changed: ArtifactResponse[];
  reloaded: ArtifactResponse[];
  closed: number;
} {
  const out = {
    changed: [] as ArtifactResponse[],
    reloaded: [] as ArtifactResponse[],
    closed: 0,
  };
  el.addEventListener('artifact-changed', (e) =>
    out.changed.push((e as CustomEvent<ArtifactResponse>).detail)
  );
  el.addEventListener('artifact-reloaded', (e) =>
    out.reloaded.push((e as CustomEvent<ArtifactResponse>).detail)
  );
  el.addEventListener('artifact-transfer-closed', () => out.closed++);
  return out;
}

describe('artifact transfer dialog', () => {
  beforeAll(async () => {
    await import('./artifact-transfer-dialog.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it('loads transfer-targets and renders agent name, slug, and phase badge', async () => {
    const m = mockFetch({ targets: TARGETS, truncated: true });
    const el = await mount();
    expect(m.calls[0]).toEqual({
      method: 'GET',
      url: `/api/v1/artifacts/${ID}/transfer-targets`,
      body: undefined,
    });
    const options = Array.from(el.shadowRoot!.querySelectorAll('sl-option'));
    expect(options).toHaveLength(2);
    expect(options[0].getAttribute('value')).toBe(NEW_OWNER);
    expect(options[0].textContent).toContain('Builder Two');
    expect(options[0].textContent).toContain('(builder-2)');
    expect(options[0].querySelector('sl-badge')!.getAttribute('variant')).toBe('success');
    expect(options[0].querySelector('sl-badge')!.textContent!.trim()).toBe('running');

    expect(options[1].getAttribute('value')).toBe(ALT_OWNER);
    expect(options[1].textContent).toContain('worker-3');
    expect(options[1].querySelector('.option-slug')).toBeNull();
    expect(options[1].querySelector('sl-badge')!.getAttribute('variant')).toBe('warning');
    expect(options[1].querySelector('sl-badge')!.textContent!.trim()).toBe('suspended');

    expect(el.shadowRoot!.textContent).toContain('Showing the first 2 eligible agents.');
    expect(button(el, 'Transfer ownership').hasAttribute('disabled')).toBe(true);
  });

  it('handles missing or null agent phase without throwing', async () => {
    mockFetch({
      targets: [
        {
          id: NEW_OWNER,
          slug: 'agent-no-phase',
          name: 'Agent No Phase',
          phase: undefined as unknown as string,
        },
      ],
    });
    const el = await mount();
    const badge = el.shadowRoot!.querySelector('sl-option sl-badge')!;
    expect(badge.getAttribute('variant')).toBe('neutral');
    expect(badge.textContent!.trim()).toBe('unknown');
  });

  it('shows empty state with project name and omits submit when no eligible agents exist', async () => {
    mockFetch({ targets: [] });
    const el = await mount({ projectName: 'Docs' });
    expect(el.shadowRoot!.querySelector('sl-select')).toBeNull();
    expect(el.shadowRoot!.querySelector('.empty-state')!.textContent!.trim()).toBe(
      'You have no agents in Docs that can take ownership.'
    );
    const submit = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent!.trim() === 'Transfer ownership'
    );
    expect(submit).toBeUndefined();
  });

  it('shows struck-through owner with deleted badge when ownerDeleted is true', async () => {
    mockFetch();
    const el = await mount({
      artifact: makeArtifact({ ownerDeleted: true }),
      ownerName: 'old-builder',
    });
    expect(el.shadowRoot!.querySelector('.owner-gone')!.textContent).toBe('old-builder');
    expect(el.shadowRoot!.querySelector('sl-badge.owner-deleted')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.live-owner-warning')).toBeNull();
  });

  it('shows live-owner warning when ownerDeleted is false', async () => {
    mockFetch();
    const el = await mount({
      artifact: makeArtifact({ ownerDeleted: false }),
      ownerName: 'live-agent',
    });
    expect(el.shadowRoot!.querySelector('sl-badge.owner-deleted')).toBeNull();
    const warn = el.shadowRoot!.querySelector('.live-owner-warning');
    expect(warn).not.toBeNull();
    expect(warn!.textContent!.replace(/\s+/g, ' ').trim()).toContain(
      'Agent live-agent is running and will no longer be able to publish new versions of this artifact.'
    );
  });

  it('notes steward handoff when shareAuthority is project_owner', async () => {
    mockFetch();
    const el = await mount({ shareAuthority: 'project_owner' });
    expect(
      el.shadowRoot!.querySelector('.steward-note')!.textContent!.replace(/\s+/g, ' ').trim()
    ).toContain('the previous delegator will no longer be a steward');
  });

  it('submits POST /transfer with to and expectOwner and emits artifact-changed', async () => {
    const m = mockFetch({ transfer: 'hold' });
    const el = await mount();
    const ev = events(el);
    choose(el, NEW_OWNER);
    await settle(el);
    button(el, 'Transfer ownership').click();
    await settle(el);

    const post = m.calls.find((c) => c.method === 'POST')!;
    expect(post.url).toBe(`/api/v1/artifacts/${ID}/transfer`);
    expect(post.body).toEqual({
      to: `agent:${NEW_OWNER}`,
      expectOwner: `agent:${OLD_OWNER}`,
    });
    expect(button(el, 'Cancel').hasAttribute('disabled')).toBe(true);

    m.release();
    await settle(el);
    expect(ev.changed).toHaveLength(1);
    expect(ev.changed[0].artifact.ownerRef).toBe(NEW_OWNER);
    expect(ev.changed[0].artifact.formerOwnerRef).toBe(OLD_OWNER);
    expect(button(el, 'Cancel').hasAttribute('disabled')).toBe(false);
  });

  it('handles 409 key_conflict by offering Remove the key and transfer with clearKey: true', async () => {
    const m = mockFetch({
      transfer: (body) => {
        if (!body.clearKey) {
          return json(
            {
              error: {
                code: 'key_conflict',
                message: 'target agent already owns an artifact with this key',
              },
            },
            409
          );
        }
        return json({
          artifact: makeResponse({
            ownerRef: NEW_OWNER,
            ownerDeleted: false,
            key: undefined,
            formerOwnerKind: 'agent',
            formerOwnerRef: OLD_OWNER,
            transferredAt: '2026-11-26T04:00:00Z',
          }),
          notified: { newOwner: true, formerOwner: false },
        });
      },
    });
    const el = await mount();
    const ev = events(el);
    choose(el, NEW_OWNER);
    await settle(el);
    button(el, 'Transfer ownership').click();
    await settle(el);

    const conflictAlert = el.shadowRoot!.querySelector('.key-conflict-alert');
    expect(conflictAlert).not.toBeNull();
    expect(conflictAlert!.textContent).toContain('daily-report');
    expect(ev.changed).toHaveLength(0);

    button(el, 'Remove the key and transfer').click();
    await settle(el);

    const posts = m.calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts[1].body).toEqual({
      to: `agent:${NEW_OWNER}`,
      expectOwner: `agent:${OLD_OWNER}`,
      clearKey: true,
    });
    expect(ev.changed).toHaveLength(1);
    expect(ev.changed[0].artifact.ownerRef).toBe(NEW_OWNER);
  });

  it('handles 409 owner_changed by reloading artifact and targets and asking again', async () => {
    const m = mockFetch({
      transfer: json(
        { error: { code: 'owner_changed', message: 'the artifact owner has changed' } },
        409
      ),
      reloadArtifact: json(
        makeResponse({ ownerRef: ALT_OWNER, ownerDeleted: false }, { canTransfer: true })
      ),
    });
    const el = await mount();
    const ev = events(el);
    choose(el, NEW_OWNER);
    await settle(el);
    button(el, 'Transfer ownership').click();
    await settle(el);

    expect(m.calls.some((c) => c.method === 'GET' && c.url === `/api/v1/artifacts/${ID}`)).toBe(
      true
    );
    expect(
      m.calls.filter((c) => c.method === 'GET' && c.url.endsWith('/transfer-targets'))
    ).toHaveLength(2);
    expect(ev.reloaded).toHaveLength(1);
    expect(ev.reloaded[0].artifact.ownerRef).toBe(ALT_OWNER);
    expect(ev.changed).toHaveLength(0);
    expect(el.shadowRoot!.querySelector('.owner-changed-alert')!.textContent).toContain(
      'Review the updated owner and confirm again'
    );
    expect(button(el, 'Transfer ownership').hasAttribute('disabled')).toBe(true);
  });

  it('disables transfer when 409 owner_changed reload reports canTransfer: false', async () => {
    mockFetch({
      transfer: json(
        { error: { code: 'owner_changed', message: 'the artifact owner has changed' } },
        409
      ),
      reloadArtifact: json(
        makeResponse({ ownerRef: ALT_OWNER, ownerDeleted: false }, { canTransfer: false })
      ),
    });
    const el = await mount();
    choose(el, NEW_OWNER);
    await settle(el);
    button(el, 'Transfer ownership').click();
    await settle(el);

    expect(el.shadowRoot!.querySelector('.transfer-error')!.textContent).toContain(
      'you can no longer transfer it'
    );
    expect(el.shadowRoot!.querySelector('.confirm-transfer')).toBeNull();
  });

  for (const [status, code, expectedText] of [
    [403, 'owner_live', 'Cannot transfer ownership from a live agent you do not control.'],
    [403, 'target_ineligible', 'not eligible to take ownership'],
    [403, 'transfer_not_allowed', 'do not have permission to transfer'],
    [403, 'forbidden', 'requires an interactive browser session'],
    [429, 'rate_limited', 'Too many transfer attempts'],
    [503, 'unavailable', 'Could not complete the ownership transfer right now'],
  ] as const) {
    it(`shows inline error for ${code} (${status}) and stays open`, async () => {
      mockFetch({
        transfer: json({ error: { code, message: 'server msg' } }, status),
      });
      const el = await mount();
      const ev = events(el);
      choose(el, NEW_OWNER);
      await settle(el);
      button(el, 'Transfer ownership').click();
      await settle(el);

      expect(el.shadowRoot!.querySelector('.transfer-error')!.textContent).toContain(expectedText);
      expect(ev.changed).toHaveLength(0);
      expect(ev.closed).toBe(0);
      expect(el.open).toBe(true);
    });
  }

  it('shows load error when GET transfer-targets fails', async () => {
    mockFetch({
      targets: json({ error: { code: 'owner_live', message: 'owner is live' } }, 403),
    });
    const el = await mount();
    expect(el.shadowRoot!.querySelector('.load-error')!.textContent).toContain(
      'Cannot transfer ownership from a live agent you do not control.'
    );
  });

  it('ignores bubbled sl-after-hide from nested sl-select and closes on Cancel', async () => {
    mockFetch();
    const el = await mount();
    const ev = events(el);
    const select = el.shadowRoot!.querySelector('sl-select')!;
    select.dispatchEvent(new Event('sl-after-hide', { bubbles: true, composed: true }));
    expect(ev.closed).toBe(0);

    button(el, 'Cancel').click();
    expect(ev.closed).toBe(1);
  });
});
