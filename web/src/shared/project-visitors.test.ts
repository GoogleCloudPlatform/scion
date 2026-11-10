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
 * Project visitor helpers (ptone/scion#4603): the Visitors tab gating, the
 * issuer choice of the add dialog, and the status labels.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { apiFetch } from '../client/api.js';
import type { AdminStatus } from '../lib/admin-permissions.js';
import {
  EXPIRED_LABEL,
  ISSUER_DISABLED_LABEL,
  ISSUER_EMPTY_ERROR,
  ISSUER_METADATA_PATH_ERROR,
  ISSUER_NOT_HTTPS_ERROR,
  ISSUER_PARTS_ERROR,
  NOT_YET_ACTIVE_LABEL,
  PROJECT_VISITORS_FLAG,
  acceptedProjectIssuers,
  canReadHubIssuers,
  expiryFromChoice,
  issuerShapeError,
  issuerTypeAllowsVisitorGrants,
  loadVisitorIssuerChoices,
  normalizeIssuer,
  normalizeVisitorDomain,
  projectVisitorsVisible,
  visitorGrantIssuers,
  visitorStatusLabel,
  visitorWindowState,
  type ProjectVisitor,
  type ProjectVisitorRule,
} from './project-visitors.js';

vi.mock('../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../client/api.js')>()),
  apiFetch: vi.fn(),
}));

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function visitor(issuer: string, issuerAccepted: boolean): ProjectVisitor {
  return {
    id: `v-${issuer}`,
    bindingId: 'b',
    email: 'a@example.com',
    issuer,
    status: 'active',
    issuerAccepted,
    createdAt: '2026-10-01T00:00:00Z',
    source: 'email',
  };
}

function rule(issuer: string, issuerAccepted: boolean): ProjectVisitorRule {
  return {
    id: `r-${issuer}`,
    issuer,
    domain: 'example.com',
    issuerAccepted,
    createdAt: '2026-10-01T00:00:00Z',
  };
}

const HUB_ADMIN: AdminStatus = {
  isAdmin: true,
  isSuperAdmin: false,
  permissions: ['hub.config.read'],
};
const NOT_ADMIN: AdminStatus = { isAdmin: false, isSuperAdmin: false, permissions: [] };

function setFlag(value: boolean | undefined): void {
  const bag = { ...(window.__SCION_FEATURES__ ?? {}) };
  if (value === undefined) delete bag[PROJECT_VISITORS_FLAG];
  else bag[PROJECT_VISITORS_FLAG] = value;
  window.__SCION_FEATURES__ = bag;
}

describe('projectVisitorsVisible', () => {
  afterEach(() => setFlag(undefined));

  it('needs both the experiment and manage_visitors', () => {
    setFlag(true);
    expect(projectVisitorsVisible({ actions: ['read', 'manage_visitors'] })).toBe(true);
    expect(projectVisitorsVisible({ actions: ['read', 'update', 'manage'] })).toBe(false);
    expect(projectVisitorsVisible(undefined)).toBe(false);
  });

  it('is hidden while the experiment is off, whatever the capabilities', () => {
    setFlag(false);
    expect(projectVisitorsVisible({ actions: ['manage_visitors'] })).toBe(false);
  });
});

describe('visitorGrantIssuers', () => {
  it('lists only hub and user issuers whose toggle is on, normalized and deduplicated', () => {
    expect(
      visitorGrantIssuers([
        {
          issuer_url: 'https://hub-a.example.com/',
          issuer_type: 'hub',
          allow_visitor_grants: true,
        },
        { issuer_url: 'https://hub-a.example.com', allow_visitor_grants: true },
        { issuer_url: 'https://idp.example.com', issuer_type: 'user', allow_visitor_grants: true },
        { issuer_url: 'https://off.example.com', issuer_type: 'user', allow_visitor_grants: false },
        { issuer_url: 'https://unset.example.com', issuer_type: 'hub' },
        {
          issuer_url: 'https://sa.example.com',
          issuer_type: 'service_account',
          allow_visitor_grants: true,
        },
      ])
    ).toEqual(['https://hub-a.example.com', 'https://idp.example.com']);
  });

  it('issuer types follow the hub rule ("" means hub)', () => {
    expect(issuerTypeAllowsVisitorGrants(undefined)).toBe(true);
    expect(issuerTypeAllowsVisitorGrants('')).toBe(true);
    expect(issuerTypeAllowsVisitorGrants('hub')).toBe(true);
    expect(issuerTypeAllowsVisitorGrants('user')).toBe(true);
    expect(issuerTypeAllowsVisitorGrants('service_account')).toBe(false);
  });
});

describe('acceptedProjectIssuers', () => {
  it('suggests only issuers the hub accepts now, from the project grants and rules', () => {
    expect(
      acceptedProjectIssuers(
        [visitor('https://b.example.com', true), visitor('https://off.example.com', false)],
        [rule('https://a.example.com/', true), rule('https://b.example.com', true)]
      )
    ).toEqual(['https://a.example.com', 'https://b.example.com']);
  });
});

describe('canReadHubIssuers', () => {
  it('is true only for a hub admin with hub.config.read', () => {
    expect(canReadHubIssuers(HUB_ADMIN)).toBe(true);
    expect(canReadHubIssuers({ isAdmin: true, isSuperAdmin: true, permissions: [] })).toBe(true);
    expect(canReadHubIssuers(NOT_ADMIN)).toBe(false);
    expect(
      canReadHubIssuers({ isAdmin: false, isSuperAdmin: false, permissions: ['hub.config.read'] })
    ).toBe(false);
    expect(canReadHubIssuers({ isAdmin: true, isSuperAdmin: false, permissions: [] })).toBe(false);
    expect(canReadHubIssuers(null)).toBe(false);
  });
});

describe('loadVisitorIssuerChoices', () => {
  beforeEach(() => {
    vi.mocked(apiFetch).mockReset();
  });

  it('a caller who is not a hub admin gets a text field and sends no admin request', async () => {
    const choices = await loadVisitorIssuerChoices(
      NOT_ADMIN,
      [visitor('https://b.example.com', true)],
      [rule('https://off.example.com', false)]
    );
    expect(choices).toEqual({ mode: 'text', issuers: ['https://b.example.com'] });
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('without an admin status, no admin request is sent either', async () => {
    const choices = await loadVisitorIssuerChoices(null, [], []);
    expect(choices).toEqual({ mode: 'text', issuers: [] });
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('a hub admin gets a picker of the issuers whose toggle is on', async () => {
    vi.mocked(apiFetch).mockResolvedValue(
      jsonResponse(200, {
        federation: {
          enabled: true,
          trusted_issuers: [
            {
              issuer_url: 'https://on.example.com',
              issuer_type: 'hub',
              allow_visitor_grants: true,
            },
            { issuer_url: 'https://off.example.com', issuer_type: 'hub' },
          ],
        },
      })
    );
    const choices = await loadVisitorIssuerChoices(HUB_ADMIN, [], []);
    expect(choices).toEqual({ mode: 'picker', issuers: ['https://on.example.com'] });
    expect(vi.mocked(apiFetch).mock.calls[0]![0]).toBe('/api/v1/admin/server-config');
  });

  it('a hub admin gets an empty picker while federation is off on the hub', async () => {
    vi.mocked(apiFetch).mockResolvedValue(
      jsonResponse(200, {
        federation: {
          enabled: false,
          trusted_issuers: [
            {
              issuer_url: 'https://on.example.com',
              issuer_type: 'hub',
              allow_visitor_grants: true,
            },
          ],
        },
      })
    );
    const choices = await loadVisitorIssuerChoices(HUB_ADMIN, [], []);
    expect(choices).toEqual({ mode: 'picker', issuers: [] });
  });

  it('a hub admin whose settings read fails falls back to the text field', async () => {
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse(403, {}));
    const choices = await loadVisitorIssuerChoices(
      HUB_ADMIN,
      [visitor('https://b.example.com', true)],
      []
    );
    expect(choices).toEqual({ mode: 'text', issuers: ['https://b.example.com'] });
  });
});

describe('issuerShapeError', () => {
  it('accepts an https issuer URL, with or without a path', () => {
    expect(issuerShapeError('https://hub.example.com')).toBeNull();
    expect(issuerShapeError(' https://hub.example.com/ ')).toBeNull();
    expect(issuerShapeError('https://securetoken.google.com/my-project')).toBeNull();
    expect(issuerShapeError('https://idp.example.com/realms/corp')).toBeNull();
  });

  it('refuses an empty value', () => {
    expect(issuerShapeError('')).toBe(ISSUER_EMPTY_ERROR);
    expect(issuerShapeError('   ')).toBe(ISSUER_EMPTY_ERROR);
  });

  it('refuses a URL that is not https', () => {
    expect(issuerShapeError('http://hub.example.com')).toBe(ISSUER_NOT_HTTPS_ERROR);
    expect(issuerShapeError('ftp://hub.example.com')).toBe(ISSUER_NOT_HTTPS_ERROR);
  });

  it('refuses a value with no host', () => {
    expect(issuerShapeError('hub.example.com')).toBe(ISSUER_NOT_HTTPS_ERROR);
    expect(issuerShapeError('https://')).toBe(ISSUER_NOT_HTTPS_ERROR);
    expect(issuerShapeError('/realms/corp')).toBe(ISSUER_NOT_HTTPS_ERROR);
  });

  it('refuses a query', () => {
    expect(issuerShapeError('https://hub.example.com/?a=b')).toBe(ISSUER_PARTS_ERROR);
    expect(issuerShapeError('https://hub.example.com?')).toBe(ISSUER_PARTS_ERROR);
  });

  it('refuses a fragment', () => {
    expect(issuerShapeError('https://hub.example.com/#x')).toBe(ISSUER_PARTS_ERROR);
    expect(issuerShapeError('https://hub.example.com#')).toBe(ISSUER_PARTS_ERROR);
  });

  it('refuses userinfo', () => {
    expect(issuerShapeError('https://user:pw@hub.example.com')).toBe(ISSUER_PARTS_ERROR);
    expect(issuerShapeError('https://user@hub.example.com')).toBe(ISSUER_PARTS_ERROR);
    expect(issuerShapeError('https://@hub.example.com')).toBe(ISSUER_PARTS_ERROR);
  });

  it('refuses a path with a /.well-known/ segment', () => {
    expect(issuerShapeError('https://hub.example.com/.well-known/openid-configuration')).toBe(
      ISSUER_METADATA_PATH_ERROR
    );
    expect(issuerShapeError('https://hub.example.com/.well-known/')).toBe(
      ISSUER_METADATA_PATH_ERROR
    );
    expect(issuerShapeError('https://idp.example.com/realms/corp/.well-known')).toBe(
      ISSUER_METADATA_PATH_ERROR
    );
  });

  it('refuses a path ending in /openid-configuration', () => {
    expect(issuerShapeError('https://idp.example.com/corp/openid-configuration')).toBe(
      ISSUER_METADATA_PATH_ERROR
    );
  });

  it('refuses a path ending in /jwks.json', () => {
    expect(issuerShapeError('https://hub.example.com/jwks.json')).toBe(ISSUER_METADATA_PATH_ERROR);
    expect(issuerShapeError('https://idp.example.com/realms/corp/jwks.json/')).toBe(
      ISSUER_METADATA_PATH_ERROR
    );
  });
});

describe('visitorStatusLabel', () => {
  const NOW = new Date('2026-10-18T12:00:00Z');
  const BEFORE = '2026-10-17T12:00:00Z';
  const AFTER = '2026-10-19T12:00:00Z';

  it('labels the account status', () => {
    expect(visitorStatusLabel('pending', true)).toBe('Pending');
    expect(visitorStatusLabel('active', true)).toBe('Active');
    expect(visitorStatusLabel('suspended', true)).toBe('Suspended');
    expect(visitorStatusLabel(undefined, true)).toBe('Active');
  });

  it('says the issuer is disabled while the hub does not accept it', () => {
    expect(visitorStatusLabel('active', false)).toBe(ISSUER_DISABLED_LABEL);
    expect(visitorStatusLabel(undefined, false)).toBe('Issuer disabled by hub admin');
  });

  it('a grant: expired, not yet active, or its account status inside the window', () => {
    expect(visitorStatusLabel('active', true, { expiresAt: BEFORE }, NOW)).toBe(EXPIRED_LABEL);
    expect(visitorStatusLabel('active', true, { expiresAt: NOW.toISOString() }, NOW)).toBe(
      EXPIRED_LABEL
    );
    expect(visitorStatusLabel('pending', true, { notBefore: AFTER }, NOW)).toBe(
      NOT_YET_ACTIVE_LABEL
    );
    expect(visitorStatusLabel('active', true, { notBefore: NOW.toISOString() }, NOW)).toBe(
      'Active'
    );
    expect(visitorStatusLabel('active', true, { notBefore: BEFORE, expiresAt: AFTER }, NOW)).toBe(
      'Active'
    );
    expect(visitorStatusLabel('pending', true, { expiresAt: AFTER }, NOW)).toBe('Pending');
  });

  it('a rule: expired, not yet active, or active inside the window', () => {
    expect(visitorStatusLabel(undefined, true, { expiresAt: BEFORE }, NOW)).toBe(EXPIRED_LABEL);
    expect(visitorStatusLabel(undefined, true, { notBefore: AFTER, expiresAt: AFTER }, NOW)).toBe(
      NOT_YET_ACTIVE_LABEL
    );
    expect(visitorStatusLabel(undefined, true, { notBefore: BEFORE }, NOW)).toBe('Active');
    expect(visitorStatusLabel(undefined, true, { notBefore: NOW.toISOString() }, NOW)).toBe(
      'Active'
    );
  });

  it('the issuer label comes before the window', () => {
    expect(visitorStatusLabel('active', false, { expiresAt: BEFORE }, NOW)).toBe(
      ISSUER_DISABLED_LABEL
    );
  });

  it('visitorWindowState ignores a bound it cannot parse', () => {
    expect(visitorWindowState({ expiresAt: 'not a date' }, NOW)).toBe('current');
    expect(visitorWindowState({}, NOW)).toBe('current');
  });
});

describe('small helpers', () => {
  it('expiryFromChoice counts days from now; "" is no expiry', () => {
    const now = new Date('2026-10-18T00:00:00Z');
    expect(expiryFromChoice('', now)).toBeNull();
    expect(expiryFromChoice('30', now)).toBe('2026-11-17T00:00:00.000Z');
  });

  it('normalizes issuers and domains as the hub compares them', () => {
    expect(normalizeIssuer(' https://hub.example.com// ')).toBe('https://hub.example.com');
    expect(normalizeVisitorDomain(' @Example.COM ')).toBe('example.com');
  });
});
