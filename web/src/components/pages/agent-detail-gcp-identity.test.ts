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
 * GCP Identity card on the agent detail page (ptone/scion#4017).
 *
 * Rules: the card shows the mode label for every mode, including block. For
 * an assigned account it prefers the registered display name with the email
 * as secondary text, and shows verification as verified, not verified, or
 * unknown. Unknown is shown whenever the registered record is not available
 * or belongs to a different account; the card never guesses.
 */

import { describe, it, expect, beforeAll, beforeEach, vi } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { Agent, GCPIdentityConfig, GCPServiceAccount } from '../../shared/types.js';
import type { ScionPageAgentDetail } from './agent-detail.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const apiFetch = vi.fn();
vi.mock('../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

const SA_EMAIL = 'agent-runner@example-project.iam.gserviceaccount.com';

function makeAccount(overrides: Partial<GCPServiceAccount> = {}): GCPServiceAccount {
  return {
    id: 'sa-1',
    scope: 'project',
    scopeId: 'p-1',
    email: SA_EMAIL,
    projectId: 'example-project',
    displayName: 'Agent Runner',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    verificationStatus: 'verified',
    createdBy: 'u-1',
    createdAt: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

const ASSIGNED: GCPIdentityConfig = {
  metadataMode: 'assign',
  serviceAccountId: 'sa-1',
  serviceAccountEmail: SA_EMAIL,
  projectId: 'example-project',
};

type CardHost = {
  renderGCPIdentityCard(
    identity: GCPIdentityConfig | undefined,
    account?: GCPServiceAccount | null,
    accountLoading?: boolean
  ): TemplateResult | typeof nothing;
};

function renderCard(
  identity: GCPIdentityConfig | undefined,
  account: GCPServiceAccount | null = null,
  accountLoading = false
): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as unknown as CardHost;
  const host = document.createElement('div');
  render(el.renderGCPIdentityCard(identity, account, accountLoading), host);
  return host;
}

function text(host: HTMLElement, selector: string): string | null {
  return host.querySelector(selector)?.textContent?.trim() ?? null;
}

describe('agent detail GCP Identity card', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('renders nothing without an applied identity', () => {
    expect(renderCard(undefined).innerHTML.replace(/<!--.*?-->/g, '')).toBe('');
  });

  it('prefers the display name and shows the email as secondary text', () => {
    const host = renderCard(ASSIGNED, makeAccount());
    expect(text(host, '.gcp-mode')).toBe('Assign');
    expect(text(host, '.gcp-sa-name')).toBe('Agent Runner');
    expect(text(host, '.gcp-sa-email')).toBe(SA_EMAIL);
    expect(host.querySelector('.gcp-sa-email')?.classList.contains('info-subvalue')).toBe(true);
  });

  it('shows verified for a verified account', () => {
    const badge = renderCard(ASSIGNED, makeAccount()).querySelector('.gcp-verification');
    expect(badge?.getAttribute('data-state')).toBe('verified');
    expect(badge?.textContent?.trim()).toBe('Verified');
    expect(badge?.getAttribute('variant')).toBe('success');
  });

  it('shows not verified for an unverified account', () => {
    const badge = renderCard(
      ASSIGNED,
      makeAccount({ verified: false, verifiedAt: null, verificationStatus: 'unverified' })
    ).querySelector('.gcp-verification');
    expect(badge?.getAttribute('data-state')).toBe('not-verified');
    expect(badge?.textContent?.trim()).toBe('Not verified');
    expect(badge?.getAttribute('variant')).toBe('warning');
  });

  it('shows not verified with the error for a failed verification', () => {
    const host = renderCard(
      ASSIGNED,
      makeAccount({
        verified: false,
        verificationStatus: 'failed',
        verificationError: 'missing token creator role',
      })
    );
    const badge = host.querySelector('.gcp-verification');
    expect(badge?.getAttribute('data-state')).toBe('not-verified');
    expect(badge?.getAttribute('variant')).toBe('danger');
    expect(host.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'missing token creator role'
    );
  });

  it('falls back to the verified flag when the record has no status field', () => {
    const badge = renderCard(
      ASSIGNED,
      makeAccount({ verificationStatus: undefined, verified: true })
    ).querySelector('.gcp-verification');
    expect(badge?.getAttribute('data-state')).toBe('verified');
  });

  it('shows unknown and the email alone when the record is unavailable', () => {
    const host = renderCard(ASSIGNED, null);
    expect(host.querySelector('.gcp-sa-name')).toBeNull();
    expect(text(host, '.gcp-sa-email')).toBe(SA_EMAIL);
    const badge = host.querySelector('.gcp-verification');
    expect(badge?.getAttribute('data-state')).toBe('unknown');
    expect(badge?.textContent?.trim()).toBe('Unknown');
  });

  it('shows unknown without a load-failure tooltip while the record is loading', () => {
    const host = renderCard(ASSIGNED, null, true);
    expect(host.querySelector('.gcp-verification')?.getAttribute('data-state')).toBe('unknown');
    expect(host.querySelector('sl-tooltip')).toBeNull();
  });

  it('explains unknown once loading has finished without a record', () => {
    const host = renderCard(ASSIGNED, null, false);
    expect(host.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'The registered service account record could not be loaded.'
    );
  });

  it('ignores a record that belongs to a different account', () => {
    const host = renderCard(ASSIGNED, makeAccount({ id: 'sa-other', displayName: 'Other' }));
    expect(host.querySelector('.gcp-sa-name')).toBeNull();
    expect(host.querySelector('.gcp-verification')?.getAttribute('data-state')).toBe('unknown');
  });

  it('shows the email alone when the record has no display name', () => {
    const host = renderCard(ASSIGNED, makeAccount({ displayName: '  ' }));
    expect(host.querySelector('.gcp-sa-name')).toBeNull();
    expect(text(host, '.gcp-sa-email')).toBe(SA_EMAIL);
  });

  it('shows the block mode label and no account or verification', () => {
    const host = renderCard({ metadataMode: 'block' });
    expect(text(host, '.gcp-mode')).toBe('Block');
    expect(host.querySelector('.gcp-sa-email')).toBeNull();
    expect(host.querySelector('.gcp-verification')).toBeNull();
  });

  it('shows the passthrough mode label and no verification', () => {
    const host = renderCard({ metadataMode: 'passthrough' });
    expect(text(host, '.gcp-mode')).toBe('Passthrough');
    expect(host.querySelector('.gcp-verification')).toBeNull();
  });
});

type SyncHost = {
  agent: Agent | null;
  gcpServiceAccount: GCPServiceAccount | null;
  gcpServiceAccountLoading: boolean;
  syncGCPServiceAccount(): void;
};

function makeAgent(identity: GCPIdentityConfig | undefined): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    appliedConfig: identity ? { gcpIdentity: identity } : {},
  } as Agent;
}

function jsonResponse(body: unknown, ok = true): Response {
  return { ok, json: () => Promise.resolve(body) } as Response;
}

describe('agent detail GCP service account lookup', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  beforeEach(() => {
    apiFetch.mockReset();
  });

  function newHost(): SyncHost {
    return document.createElement('scion-page-agent-detail') as unknown as SyncHost;
  }

  it("loads the assigned account through the agent's project", async () => {
    apiFetch.mockResolvedValue(jsonResponse(makeAccount()));
    const el = newHost();
    el.agent = makeAgent(ASSIGNED);
    el.syncGCPServiceAccount();
    expect(el.gcpServiceAccountLoading).toBe(true);
    await vi.waitFor(() => expect(el.gcpServiceAccount?.id).toBe('sa-1'));
    expect(el.gcpServiceAccountLoading).toBe(false);
    // A refused lookup is shown inline as Unknown, so it must not also raise
    // the global access-denied toast.
    expect(apiFetch).toHaveBeenCalledWith('/api/v1/projects/p-1/gcp-service-accounts/sa-1', {
      suppressAccessDeniedToast: true,
    });
  });

  it('does not refetch while the assignment is unchanged', async () => {
    apiFetch.mockResolvedValue(jsonResponse(makeAccount()));
    const el = newHost();
    el.agent = makeAgent(ASSIGNED);
    el.syncGCPServiceAccount();
    el.agent = makeAgent(ASSIGNED);
    el.syncGCPServiceAccount();
    await vi.waitFor(() => expect(el.gcpServiceAccount).not.toBeNull());
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it('does not fetch for modes without an assigned account', () => {
    const el = newHost();
    el.agent = makeAgent({ metadataMode: 'block' });
    el.syncGCPServiceAccount();
    el.agent = makeAgent({ metadataMode: 'passthrough' });
    el.syncGCPServiceAccount();
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el.gcpServiceAccount).toBeNull();
    expect(el.gcpServiceAccountLoading).toBe(false);
  });

  it('leaves the record empty when the hub refuses the lookup', async () => {
    apiFetch.mockResolvedValue(jsonResponse({}, false));
    const el = newHost();
    el.agent = makeAgent(ASSIGNED);
    el.syncGCPServiceAccount();
    await vi.waitFor(() => expect(el.gcpServiceAccountLoading).toBe(false));
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(el.gcpServiceAccount).toBeNull();
  });

  it('clears the record when the assignment changes', async () => {
    apiFetch.mockResolvedValueOnce(jsonResponse(makeAccount()));
    const el = newHost();
    el.agent = makeAgent(ASSIGNED);
    el.syncGCPServiceAccount();
    await vi.waitFor(() => expect(el.gcpServiceAccount).not.toBeNull());
    el.agent = makeAgent({ metadataMode: 'block' });
    el.syncGCPServiceAccount();
    expect(el.gcpServiceAccount).toBeNull();
  });
});
