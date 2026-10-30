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
 * Runtime Broker version against the hub's (ptone/scion#5972).
 *
 * The hub compares versions and sends the result (versionStatus on the
 * runtime broker list, version_status on the health summary); this module
 * only renders it, so the Runtime Brokers lists and the health dashboard
 * show the same thing. Text goes through Lit interpolation and attribute
 * bindings only.
 */

import { html, css, nothing, type TemplateResult } from 'lit';

/** The hub's comparison result; a missing value counts as unknown. */
export type BrokerVersionStatus = 'match' | 'mismatch' | 'unknown';

/** The fleet hub version the hub sends when its instances disagree. */
export const HUB_VERSION_MIXED = 'mixed';

/**
 * True when the hub version is a usable token: not empty and not "unknown" (a
 * hub built without build information reports "unknown"). A type guard, so
 * callers that return early on false see a string afterwards.
 */
function hubVersionKnown(hubVersion: string | null | undefined): hubVersion is string {
  return !!hubVersion && hubVersion !== 'unknown';
}

/** A missing or unexpected status is unknown, never match. */
export function normalizeVersionStatus(status: string | null | undefined): BrokerVersionStatus {
  return status === 'match' || status === 'mismatch' ? status : 'unknown';
}

/** The hub version as shown after "Hub: ". */
export function hubVersionText(hubVersion: string | null | undefined): string {
  if (!hubVersionKnown(hubVersion)) return 'unknown';
  if (hubVersion === HUB_VERSION_MIXED) return 'mixed versions (upgrade in progress)';
  return hubVersion;
}

/** The tooltip describing the hub version. */
export function hubVersionTitle(hubVersion: string | null | undefined): string {
  if (!hubVersionKnown(hubVersion)) return 'Hub version unknown';
  if (hubVersion === HUB_VERSION_MIXED) return 'Hub instances run different versions';
  return `Hub runs ${hubVersion}`;
}

/** The tooltip of a broker's version cell. */
export function brokerVersionTitle(
  version: string | null | undefined,
  status: string | null | undefined,
  hubVersion: string | null | undefined
): string {
  if (!version) return 'Broker has not reported a version';
  const s = normalizeVersionStatus(status);
  if (s === 'mismatch') return `Differs from the hub. ${hubVersionTitle(hubVersion)}`;
  // Unknown although both sides report a version: the broker's token cannot
  // be compared (for example "unknown", a build without build information).
  if (s === 'unknown' && hubVersionKnown(hubVersion) && hubVersion !== HUB_VERSION_MIXED) {
    return `Cannot be compared with the hub. ${hubVersionTitle(hubVersion)}`;
  }
  return hubVersionTitle(hubVersion);
}

/**
 * A broker's version (as reported, no prefix), with a mark when it differs
 * from the hub's or cannot be compared.
 */
export function renderBrokerVersion(
  version: string | null | undefined,
  status: string | null | undefined,
  hubVersion: string | null | undefined
): TemplateResult {
  const s = normalizeVersionStatus(status);
  return html`<span
    class="broker-version-cell version-${s}"
    data-version-status=${s}
    title=${brokerVersionTitle(version, status, hubVersion)}
    ><span class="version-token">${version || '—'}</span>${s === 'mismatch'
      ? html`<sl-icon
          class="version-mark"
          name="exclamation-triangle"
          label="Differs from the hub's version"
        ></sl-icon>`
      : s === 'unknown'
        ? html`<sl-icon
            class="version-mark"
            name="question-circle"
            label="Version unknown"
          ></sl-icon>`
        : nothing}</span
  >`;
}

/** The "Hub: <version>" note shown next to a broker list. */
export function renderHubVersionNote(hubVersion: string | null | undefined): TemplateResult {
  return html`<span class="hub-version-note" title=${hubVersionTitle(hubVersion)}
    >Hub: <span class="version-token">${hubVersionText(hubVersion)}</span></span
  >`;
}

/** Styles for renderBrokerVersion and renderHubVersionNote. */
export const brokerVersionStyles = css`
  .broker-version-cell {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
  }

  .version-token {
    font-family: var(--scion-font-mono, monospace);
    overflow-wrap: anywhere;
  }

  .version-mismatch .version-mark {
    color: var(--scion-badge-warning-text);
  }

  .version-unknown .version-mark {
    color: var(--scion-text-muted);
  }

  .hub-version-note {
    font-size: 0.8125rem;
    color: var(--scion-text-muted);
  }
`;
