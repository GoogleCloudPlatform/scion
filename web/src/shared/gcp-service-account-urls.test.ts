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
 * Tests for the one module that chooses a service account's address.
 *
 * These are pure-function tests on purpose: the failures they guard against are
 * URLs that LOOK right, and a URL that looks right is exactly what a rendering
 * test will not notice.
 */

import { describe, it, expect } from 'vitest';

import {
  saCreateUrl,
  saDetailPath,
  saListUrl,
  saMintUrl,
  saRef,
  saVerifyUrl,
} from './gcp-service-account-urls.js';

/**
 * An account whose two project-ish fields DIFFER. Every test below depends on
 * that difference: a fixture where scopeId === projectId cannot fail the way
 * production does.
 */
const projectScoped = {
  id: 'sa-1',
  scope: 'project' as const,
  scopeId: 'scion-project-abc', // the Scion project that owns the registration
  projectId: 'gcp-project-xyz', // the GCP project the SA lives in
};

const hubScoped = {
  id: 'sa-2',
  scope: 'hub' as const,
  scopeId: 'hub-instance-1',
  projectId: 'gcp-project-xyz',
};

const userScoped = {
  id: 'sa-3',
  scope: 'user' as const,
  scopeId: 'user-7',
  projectId: 'gcp-project-xyz',
};

describe('saRef — the address comes from the account, not from the screen', () => {
  /**
   * THE CAREFUL-CALLER TRAP, PINNED AS A DIVERGENCE.
   *
   * `/api/v1/projects/${account.projectId}/...` compiles, reads correctly, and
   * uses the field whose NAME matches the path segment. It is also wrong, and
   * the Hub answers 404 rather than complaining. Two separate assertions ("uses
   * scopeId" and "does not use projectId") could each be satisfied by a
   * different mistake; one assertion that the two addresses DIFFER cannot.
   */
  it('uses scopeId, never the GCP projectId', () => {
    const ref = saRef(projectScoped);
    const trap = `/api/v1/projects/${projectScoped.projectId}/gcp-service-accounts/${projectScoped.id}`;

    expect(ref).not.toBe(trap);
    expect(ref).toBe(
      `/api/v1/projects/${projectScoped.scopeId}/gcp-service-accounts/${projectScoped.id}`
    );
  });

  it('addresses parentless accounts flat — hub and user alike', () => {
    // One assertion that the two parentless scopes agree, rather than two
    // assertions of the same literal: a change that nests one of them again
    // must fail here, whichever one it is.
    expect(saRef(hubScoped)).toBe(`/api/v1/gcp-service-accounts/${hubScoped.id}`);
    expect(saRef(userScoped)).toBe(`/api/v1/gcp-service-accounts/${userScoped.id}`);
    expect(saRef(hubScoped).replace(hubScoped.id, 'X')).toBe(
      saRef(userScoped).replace(userScoped.id, 'X')
    );
  });

  it('refuses a project-scoped account with no scopeId instead of building /projects//…', () => {
    expect(() => saRef({ id: 'sa-9', scope: 'project', scopeId: '' })).toThrow(/scopeId/);
  });

  it('verify hangs off the same address', () => {
    expect(saVerifyUrl(hubScoped)).toBe(`${saRef(hubScoped)}/verify`);
    expect(saVerifyUrl(projectScoped)).toBe(`${saRef(projectScoped)}/verify`);
  });
});

describe('saListUrl', () => {
  it('asks two different questions for two different scopes', () => {
    const hub = saListUrl('hub', '');
    const project = saListUrl('project', 'scion-project-abc');

    // The subject is that they differ, and how: the hub list names a scope and
    // no project, the project list names a project and no scope.
    expect(hub).not.toBe(project);
    expect(hub).toBe('/api/v1/gcp-service-accounts?scope=hub');
    expect(hub).not.toContain('/projects/');
    expect(project).toBe('/api/v1/projects/scion-project-abc/gcp-service-accounts');
    expect(project).not.toContain('scope=');
  });

  it('keeps project scope on the nested collection, which is the one that reports mint quota', () => {
    // Not a style preference: the flat route omits mint_quota deliberately, so
    // routing the project list through it would empty the quota line with no
    // other visible symptom.
    expect(saListUrl('project', 'p1')).not.toContain('/api/v1/gcp-service-accounts');
  });

  it('asks for the union only when a project is named', () => {
    expect(saListUrl('project', 'p1', true)).toBe(
      '/api/v1/gcp-service-accounts?scope=project&scopeId=p1&includeHubScoped=true'
    );
    // At hub scope the union is already implied, so asking for it says
    // something the caller does not mean. The Hub rejects it; so do we, rather
    // than dropping the flag and answering a different question silently.
    expect(() => saListUrl('hub', '', true)).toThrow(/includeHubScoped/);
  });

  it('refuses project scope with no project id, before any request exists', () => {
    expect(() => saListUrl('project', '')).toThrow(/scopeId/);
  });
});

describe('saCreateUrl — hub-scoped creation must reach the Hub’s refusal', () => {
  /**
   * The requirement is not "creation fails at hub scope". It is that it fails
   * AT THE SERVER, where the refusal is implemented and where lifting the hold
   * (#19) will change it. A create URL that pointed at some project's
   * collection would SUCCEED, and succeed at making the wrong thing: a
   * project-scoped account registered from a hub-scoped screen.
   */
  it('points at the flat collection with scope=hub, never at a project', () => {
    const url = saCreateUrl('hub', '');
    expect(url).toBe('/api/v1/gcp-service-accounts?scope=hub');
    expect(url).not.toContain('/projects/');
  });

  it('still posts project registrations to the project collection', () => {
    expect(saCreateUrl('project', 'p1')).toBe('/api/v1/projects/p1/gcp-service-accounts');
  });
});

describe('saMintUrl — “nowhere to send this” is a value, not a URL', () => {
  it('returns null at hub scope', () => {
    // The flat route has no mint endpoint: /api/v1/gcp-service-accounts/mint
    // parses as an account whose id is "mint". A plausible-looking string here
    // would produce a 404 that reads like a missing account.
    expect(saMintUrl('hub', '')).toBeNull();
  });

  it('returns the project mint endpoint at project scope', () => {
    expect(saMintUrl('project', 'p1')).toBe('/api/v1/projects/p1/gcp-service-accounts/mint');
  });
});

describe('saDetailPath', () => {
  it('offers a detail page for parentless accounts only', () => {
    expect(saDetailPath(hubScoped)).toBe('/settings/service-accounts/sa-2');
    expect(saDetailPath(userScoped)).toBe('/settings/service-accounts/sa-3');
    // Project-scoped accounts are managed from their project's settings tab,
    // which is the surface whose list route computes their capabilities. A
    // detail page for them could only render buttons from existence.
    expect(saDetailPath(projectScoped)).toBeNull();
  });
});
