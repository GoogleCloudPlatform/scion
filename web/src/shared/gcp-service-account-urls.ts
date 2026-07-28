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
 * THE ONE PLACE THAT CHOOSES A SERVICE ACCOUNT'S ADDRESS.
 *
 * A GCP service account has two possible addresses and the choice is not the
 * caller's to make freely:
 *
 *   - project-scoped -> /api/v1/projects/{scionProjectId}/gcp-service-accounts/{id}
 *   - hub- or user-scoped -> /api/v1/gcp-service-accounts/{id}
 *
 * Hub- and user-scoped accounts are PARENTLESS. The flat route exists because
 * they have no project to be nested under, and it answers 404 -- not 403 -- for
 * a project-scoped id, so an address chosen wrongly does not fail loudly, it
 * fails as "no such account".
 *
 * THE TRAP THIS MODULE CLOSES is the same one pkg/hubclient closed with
 * GCPServiceAccount.Ref(): the account carries BOTH `scopeId` (the Scion
 * project that owns the registration) and `projectId` (the GCP project the
 * service account itself lives in). The obvious line
 *
 *     `/api/v1/projects/${account.projectId}/gcp-service-accounts/${account.id}`
 *
 * reads correctly, uses the field whose name matches the path segment, and
 * 404s. So callers do not assemble these paths; they call saRef(account) and
 * let the account's own scope pick the address, exactly as the Go client does.
 */

import type { GCPServiceAccount } from './types.js';

/**
 * The scopes a service account LIST can be asked for from the UI.
 *
 * Deliberately not the wider ResourceScope: 'user' accounts are reachable
 * per-id through the flat route, but no UI surface lists them yet, and a value
 * this module accepts is a value some component will eventually pass.
 */
export type GCPSAListScope = 'project' | 'hub';

const FLAT = '/api/v1/gcp-service-accounts';

function nested(scionProjectId: string): string {
  return `/api/v1/projects/${scionProjectId}/gcp-service-accounts`;
}

/**
 * requireScopeId refuses an empty project id instead of building
 * `/api/v1/projects//gcp-service-accounts`, which is a URL the Hub answers --
 * with someone else's 404 or, worse, a route match nobody intended. A component
 * whose `scopeId` property has not arrived yet is the common cause, and the
 * failure to want is the one that never leaves the browser.
 */
function requireScopeId(scope: GCPSAListScope, scopeId: string): string {
  if (scope === 'project' && !scopeId) {
    throw new Error('gcp-service-accounts: scope="project" requires a scopeId');
  }
  return scopeId;
}

/**
 * saListUrl builds the collection URL for a scope.
 *
 * Project scope keeps the NESTED collection rather than the flat one with
 * ?scope=project. Both return the same rows, but the nested route also returns
 * mint_quota, which the flat route deliberately omits (it is scope-general and
 * has no single project to report a per-project quota against). Sending the
 * project list through the flat route would silently empty the quota line.
 *
 * includeHubScoped asks the Hub for the union "registered to this project, plus
 * the hub-scoped accounts usable from it". It is only defined at project scope
 * -- at hub scope it is already implied -- and the Hub rejects the incoherent
 * combination rather than ignoring it, so this function does not paper over it
 * either.
 */
export function saListUrl(
  scope: GCPSAListScope,
  scopeId: string,
  includeHubScoped = false
): string {
  requireScopeId(scope, scopeId);

  if (scope === 'hub') {
    if (includeHubScoped) {
      throw new Error('gcp-service-accounts: includeHubScoped is meaningless at scope="hub"');
    }
    return `${FLAT}?scope=hub`;
  }

  return includeHubScoped
    ? `${FLAT}?scope=project&scopeId=${encodeURIComponent(scopeId)}&includeHubScoped=true`
    : nested(scopeId);
}

/**
 * saCreateUrl builds the collection URL a registration POST goes to.
 *
 * AT HUB SCOPE THIS POINTS AT THE HUB'S REFUSAL, ON PURPOSE. Hub-scoped
 * creation is not enabled: the flat collection answers 400 invalid_request to
 * POST ?scope=hub (svc-accnt #19 holds the enabling change). No UI renders a
 * create affordance at hub scope -- see the list component -- but if one is
 * ever added, this is the address it will use, and the server will refuse it.
 * The alternative, which is what makes this function worth writing down, is a
 * create button that quietly posts to some project's collection and succeeds at
 * making the WRONG THING: a project-scoped account on a hub-scoped screen.
 */
export function saCreateUrl(scope: GCPSAListScope, scopeId: string): string {
  requireScopeId(scope, scopeId);
  return scope === 'hub' ? `${FLAT}?scope=hub` : nested(scopeId);
}

/**
 * saMintUrl returns the mint URL, or null where minting has no meaning.
 *
 * Mint is a per-project quota operation against the Hub's own GCP project, and
 * the flat route has no mint endpoint at all -- /api/v1/gcp-service-accounts/mint
 * parses as an account whose id is "mint" and 404s. Returning null rather than
 * a string makes "there is nowhere to send this" a value the caller has to
 * handle, instead of a URL that looks plausible.
 */
export function saMintUrl(scope: GCPSAListScope, scopeId: string): string | null {
  if (scope !== 'project') return null;
  requireScopeId(scope, scopeId);
  return `${nested(scopeId)}/mint`;
}

/**
 * saRef returns the by-id address of an account, chosen from the ACCOUNT rather
 * than from whatever screen it is being rendered on.
 *
 * This matters as soon as one list can hold accounts of more than one scope --
 * a project list asked with includeHubScoped is exactly that -- because the
 * screen's scope is then the wrong answer for some of its own rows. The Go
 * client makes the same choice for the same reason; see
 * (*GCPServiceAccount).Ref in pkg/hubclient.
 *
 * `scopeId` is used for project scope and `projectId` never is: the route names
 * the Scion project that owns the registration, not the GCP project the service
 * account lives in.
 */
export function saRef(account: Pick<GCPServiceAccount, 'id' | 'scope' | 'scopeId'>): string {
  if (!account.id) {
    throw new Error('gcp-service-accounts: cannot address an account with no id');
  }
  if (account.scope === 'project') {
    if (!account.scopeId) {
      throw new Error(`gcp-service-accounts: project-scoped account ${account.id} has no scopeId`);
    }
    return `${nested(account.scopeId)}/${account.id}`;
  }
  return `${FLAT}/${account.id}`;
}

/** saVerifyUrl is the re-verification POST for an account, at its own address. */
export function saVerifyUrl(account: Pick<GCPServiceAccount, 'id' | 'scope' | 'scopeId'>): string {
  return `${saRef(account)}/verify`;
}

/**
 * saDetailPath is the UI route for one account.
 *
 * Only parentless accounts get a detail page today; see the page component for
 * why (the nested GET returns no capabilities, so a project-scoped detail page
 * could only render its buttons from existence, which is the thing this feature
 * is under instruction not to do). Project-scoped accounts are managed from
 * their project's settings tab, which is where their capabilities come from.
 */
export function saDetailPath(
  account: Pick<GCPServiceAccount, 'id' | 'scope' | 'scopeId'>
): string | null {
  if (account.scope === 'project') return null;
  return `/settings/service-accounts/${account.id}`;
}
