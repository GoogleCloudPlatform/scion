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
 * Project visitors (ptone/scion#4603): types and helpers shared by the
 * project Visitors tab and the Admin > Federation page.
 *
 * Hub routes (pkg/hub/handlers_project_visitors.go and
 * handlers_project_visitor_rules.go), all 404 while the
 * federation.project_visitors experiment is off and all needing
 * project.manage_visitors on the project:
 *
 *  - GET/POST     /api/v1/projects/{id}/visitors              grants by email
 *  - PATCH/DELETE /api/v1/projects/{id}/visitors/{visitorId}  window / revoke
 *  - GET/POST     /api/v1/projects/{id}/visitors/rules        rules by domain
 *  - PATCH/DELETE /api/v1/projects/{id}/visitors/rules/{id}   window / remove
 *
 * The experiment check and the permission check here only decide what the
 * page shows; the hub enforces both on every request.
 */

import { apiFetch } from '../client/api.js';
import { hasAnyPermission, type AdminStatus } from '../lib/admin-permissions.js';
import { isFeatureEnabled } from '../utils/feature-flags.js';
import { can, type Capabilities } from './types.js';

/**
 * The registered experiment (pkg/experiments/registry.go). It has the web
 * and server layers, so GET /api/v1/experiments carries it to the browser.
 */
export const PROJECT_VISITORS_FLAG = 'federation.project_visitors';

/** The project capability action of project.manage_visitors. */
export const MANAGE_VISITORS_ACTION = 'manage_visitors';

/** One visitor grant, as GET /visitors returns it (ProjectVisitor). */
export interface ProjectVisitor {
  /** The federated account ID; the item routes address it. */
  id: string;
  bindingId: string;
  email: string;
  issuer: string;
  /** pending | active | suspended */
  status: string;
  /** Whether the hub accepts visitors from the issuer now. */
  issuerAccepted: boolean;
  grantedBy?: string;
  notBefore?: string;
  expiresAt?: string;
  createdAt: string;
  /** "email" for a grant by email, "rule" for a binding a visitor rule created. */
  source: 'email' | 'rule';
  sourceRuleId?: string;
}

/** One visitor rule, as GET /visitors/rules returns it (ProjectVisitorRuleView). */
export interface ProjectVisitorRule {
  id: string;
  issuer: string;
  domain: string;
  issuerAccepted: boolean;
  createdBy?: string;
  notBefore?: string;
  expiresAt?: string;
  createdAt: string;
}

/**
 * Whether the page shows the project's Visitors tab: the experiment is on
 * and the caller holds project.manage_visitors on the project.
 */
export function projectVisitorsVisible(capabilities: Capabilities | undefined): boolean {
  return isFeatureEnabled(PROJECT_VISITORS_FLAG) && can(capabilities, MANAGE_VISITORS_ACTION);
}

/** The visitors collection URL of a project. */
export function projectVisitorsUrl(projectId: string): string {
  return `/api/v1/projects/${encodeURIComponent(projectId)}/visitors`;
}

/** The visitor rules collection URL of a project. */
export function projectVisitorRulesUrl(projectId: string): string {
  return `${projectVisitorsUrl(projectId)}/rules`;
}

/** An issuer URL as the hub compares it: trimmed, without trailing slashes. */
export function normalizeIssuer(issuer: string): string {
  return issuer.trim().replace(/\/+$/, '');
}

/**
 * Whether allow_visitor_grants applies to an issuer type ("" means hub),
 * mirroring config.IssuerAllowsVisitorGrantsType.
 */
export function issuerTypeAllowsVisitorGrants(issuerType: string | undefined): boolean {
  return !issuerType || issuerType === 'hub' || issuerType === 'user';
}

/** The trusted issuer fields the visitor issuer choice reads. */
export interface VisitorIssuerSettings {
  issuer_url: string;
  issuer_type?: string;
  allow_visitor_grants?: boolean;
}

/**
 * The issuers a project owner may pick for a new grant or rule: trusted
 * issuers of type user or hub whose allow_visitor_grants toggle is on.
 */
export function visitorGrantIssuers(issuers: readonly VisitorIssuerSettings[]): string[] {
  const out: string[] = [];
  for (const issuer of issuers) {
    if (!issuer.allow_visitor_grants || !issuerTypeAllowsVisitorGrants(issuer.issuer_type)) {
      continue;
    }
    const url = normalizeIssuer(issuer.issuer_url);
    if (url && !out.includes(url)) out.push(url);
  }
  return out;
}

/**
 * Issuers of the project's grants and rules that the hub accepts now, for
 * callers who cannot read the hub's issuer list.
 */
export function acceptedProjectIssuers(
  visitors: readonly ProjectVisitor[],
  rules: readonly ProjectVisitorRule[]
): string[] {
  const out: string[] = [];
  for (const item of [...visitors, ...rules]) {
    if (!item.issuerAccepted) continue;
    const url = normalizeIssuer(item.issuer);
    if (url && !out.includes(url)) out.push(url);
  }
  return out.sort();
}

/**
 * How the Add visitor dialog offers issuers. "picker": a closed list of
 * the issuers that accept visitor grants (hub admins only). "text": a free
 * issuer field with suggestions from the project's own grants and rules;
 * the hub refuses an issuer it does not accept.
 */
export interface VisitorIssuerChoices {
  mode: 'picker' | 'text';
  issuers: string[];
}

/** The hub permission of the Admin > Federation page (hub.config.read). */
export const HUB_CONFIG_READ_PERMISSION = 'hub.config.read';

/**
 * Whether the caller is a hub admin who may read the hub's trusted issuer
 * list (the admin settings route). Only such a caller gets the picker; no
 * other caller sends a request to an admin route.
 */
export function canReadHubIssuers(adminStatus: AdminStatus | null): boolean {
  return (
    adminStatus !== null &&
    (adminStatus.isAdmin || adminStatus.isSuperAdmin) &&
    hasAnyPermission(adminStatus, [HUB_CONFIG_READ_PERMISSION])
  );
}

/**
 * Loads the issuer choices of the Add visitor dialog. A hub admin
 * (canReadHubIssuers) gets a picker of the trusted issuers whose toggle is
 * on, read from the admin settings route. Any other caller gets a text
 * field whose suggestions come only from the project's grants and rules
 * that the caller already reads.
 *
 * Kept in one place so that a later project-scoped issuer route only
 * changes this function.
 */
export async function loadVisitorIssuerChoices(
  adminStatus: AdminStatus | null,
  visitors: readonly ProjectVisitor[],
  rules: readonly ProjectVisitorRule[]
): Promise<VisitorIssuerChoices> {
  const text: VisitorIssuerChoices = {
    mode: 'text',
    issuers: acceptedProjectIssuers(visitors, rules),
  };
  if (!canReadHubIssuers(adminStatus)) return text;
  try {
    const res = await apiFetch('/api/v1/admin/server-config', {
      suppressAccessDeniedToast: true,
    });
    if (res.ok) {
      const data = (await res.json()) as {
        federation?: { enabled?: boolean; trusted_issuers?: VisitorIssuerSettings[] };
      };
      // While federation is off, the hub accepts no issuer at all.
      if (data.federation?.enabled !== true) return { mode: 'picker', issuers: [] };
      return {
        mode: 'picker',
        issuers: visitorGrantIssuers(data.federation.trusted_issuers ?? []),
      };
    }
  } catch {
    // Fall through to the text field.
  }
  return text;
}

/** Refusal texts of issuerShapeError. */
export const ISSUER_EMPTY_ERROR = 'Enter the issuer URL';
export const ISSUER_NOT_HTTPS_ERROR = 'The issuer must be an https:// URL';
export const ISSUER_PARTS_ERROR = 'The issuer URL cannot have credentials, a query or a fragment';
export const ISSUER_METADATA_PATH_ERROR =
  'Enter the issuer URL itself, not its discovery document or key set URL';

/**
 * Why an issuer typed into the text field is refused before it is sent, or
 * null when its shape is acceptable: an absolute https URL with a host and
 * no credentials, query or fragment. A path is allowed (some identity
 * providers have one), except a path that names the issuer's metadata
 * rather than the issuer: one containing a /.well-known/ segment, or ending
 * in /openid-configuration or /jwks.json. Only surrounding whitespace is
 * trimmed; the value is sent as typed otherwise, since the hub compares it
 * with its trusted issuers. Whether the hub accepts the issuer is the hub's
 * decision; its refusal is shown as it sends it.
 */
export function issuerShapeError(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return ISSUER_EMPTY_ERROR;
  let url: URL;
  try {
    url = new URL(trimmed);
  } catch {
    return ISSUER_NOT_HTTPS_ERROR;
  }
  if (url.protocol !== 'https:' || !url.hostname) return ISSUER_NOT_HTTPS_ERROR;
  // The URL parser drops an empty userinfo ("https://@host"), an empty
  // query and an empty fragment, so the typed text is checked as well.
  const authority = trimmed.replace(/^[^:]*:\/\//, '').split(/[/?#]/, 1)[0] ?? '';
  if (
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    authority.includes('@') ||
    /[?#]/.test(trimmed)
  ) {
    return ISSUER_PARTS_ERROR;
  }
  const path = url.pathname.toLowerCase().replace(/\/+$/, '');
  if (
    `${path}/`.includes('/.well-known/') ||
    path.endsWith('/openid-configuration') ||
    path.endsWith('/jwks.json')
  ) {
    return ISSUER_METADATA_PATH_ERROR;
  }
  return null;
}

/** Status text shown when the hub admin has turned the issuer's toggle off. */
export const ISSUER_DISABLED_LABEL = 'Issuer disabled by hub admin';

/** Status text of a grant or rule whose expiry has passed. */
export const EXPIRED_LABEL = 'Expired';

/** Status text of a grant or rule whose start is still in the future. */
export const NOT_YET_ACTIVE_LABEL = 'Not yet active';

/** The access window of a grant or rule, as the hub returns it. */
export interface VisitorWindow {
  notBefore?: string;
  expiresAt?: string;
}

/**
 * Where now falls in a grant's or rule's window: "expired" once expiresAt
 * is at or before now, "not_yet_active" while notBefore is after now,
 * "current" otherwise. The hub lists every grant and rule whatever its
 * window, and refuses a visitor outside it. A bound the browser cannot
 * parse is ignored.
 */
export function visitorWindowState(
  bounds: VisitorWindow,
  now: Date = new Date()
): 'expired' | 'not_yet_active' | 'current' {
  const t = now.getTime();
  const expires = bounds.expiresAt ? Date.parse(bounds.expiresAt) : Number.NaN;
  if (!Number.isNaN(expires) && expires <= t) return 'expired';
  const start = bounds.notBefore ? Date.parse(bounds.notBefore) : Number.NaN;
  if (!Number.isNaN(start) && start > t) return 'not_yet_active';
  return 'current';
}

/**
 * The status shown for a grant or rule (rules have no account status). The
 * issuer label comes first, then the window, then the account status.
 */
export function visitorStatusLabel(
  status: string | undefined,
  issuerAccepted: boolean,
  bounds: VisitorWindow = {},
  now: Date = new Date()
): string {
  if (!issuerAccepted) return ISSUER_DISABLED_LABEL;
  const windowState = visitorWindowState(bounds, now);
  if (windowState === 'expired') return EXPIRED_LABEL;
  if (windowState === 'not_yet_active') return NOT_YET_ACTIVE_LABEL;
  switch (status) {
    case 'pending':
      return 'Pending';
    case 'active':
      return 'Active';
    case 'suspended':
      return 'Suspended';
    case undefined:
    case '':
      return 'Active';
    default:
      return status;
  }
}

/** Expiry choices of the add and extend dialogs; '' is no expiry. */
export const VISITOR_EXPIRY_CHOICES: ReadonlyArray<{ value: string; label: string }> = [
  { value: '', label: 'No expiry' },
  { value: '7', label: '7 days' },
  { value: '30', label: '30 days' },
  { value: '90', label: '90 days' },
  { value: '365', label: '1 year' },
];

/** The expiry instant of an expiry choice (days from now), or null for no expiry. */
export function expiryFromChoice(days: string, now: Date = new Date()): string | null {
  const n = Number.parseInt(days, 10);
  if (!days || !Number.isFinite(n) || n <= 0) return null;
  return new Date(now.getTime() + n * 24 * 60 * 60 * 1000).toISOString();
}

/** The domain form the hub expects for a rule: trimmed, lower case, no leading "@". */
export function normalizeVisitorDomain(domain: string): string {
  return domain.trim().replace(/^@/, '').toLowerCase();
}
