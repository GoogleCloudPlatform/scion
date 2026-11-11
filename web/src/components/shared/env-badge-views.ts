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
 * Environment-override badges for the admin settings pages that read the
 * server-config GET (env_overrides and env_values). Server Config and
 * Federation use these templates, so one key reads the same on both
 * pages. Pages include `envBadgeStyles` in their static styles.
 */

import { html, css, nothing } from 'lit';

/** A rendered lit html template. */
type HtmlResult = ReturnType<typeof html>;

import {
  type EnvKeyValue,
  envValueFor,
  envValuesUnder,
  formatEnvValue,
  hasInEffect,
} from '../pages/admin-server-config-env-values.js';

/**
 * What wins over an environment value that is not in effect: 'saved', a
 * saved setting (a Layer-1 key on a DB-backed hub), or 'startup', a
 * start-up default or command-line flag.
 */
export type EnvNotInEffectCause = 'saved' | 'startup';

export const envBadgeStyles = css`
  .env-badge {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    font-size: 0.6875rem;
    font-weight: 500;
    color: var(--sl-color-warning-700, #b45309);
    background: var(--sl-color-warning-50, #fffbeb);
    border: 1px solid var(--sl-color-warning-200, #fde68a);
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    white-space: nowrap;
  }

  .env-value-in-effect {
    display: block;
    font-size: 0.8125rem;
    font-weight: 600;
    margin: 0.25rem 0;
  }

  .env-value-saved-label,
  .env-value-saved,
  .env-value-configured-label,
  .env-value-not-in-effect {
    display: block;
  }

  .env-values-under {
    margin: 0.25rem 0;
    padding-left: 1.25rem;
    font-size: 0.75rem;
  }

  .env-badge sl-icon {
    font-size: 0.75rem;
  }
`;

/** A read-only field's value and its lock badge. */
export const readOnlyFieldStyles = css`
  .read-only-value {
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
    padding: 0.5rem 0.75rem;
    background: var(--scion-bg, #f8fafc);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius, 0.5rem);
    min-height: 1.5em;
  }

  .read-only-badge {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    font-size: 0.6875rem;
    font-weight: 500;
    color: var(--sl-color-neutral-600, #475569);
    background: var(--sl-color-neutral-100, #f1f5f9);
    border: 1px solid var(--sl-color-neutral-200, #e2e8f0);
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    white-space: nowrap;
    font-style: italic;
  }
`;

/** The lock badge text of a field whose environment value is in effect. */
export const ENV_LOCK_BADGE_TEXT = '🔒 Set via environment variable';

/** The lock badge of a field whose environment value is in effect (read-only). */
export function renderEnvLockBadge(): HtmlResult {
  return html`<span class="read-only-badge">${ENV_LOCK_BADGE_TEXT}</span>`;
}

/**
 * The env-override badge. inEffect false (the server reports that another
 * value wins over the environment value) says so instead of claiming an
 * override.
 */
export function renderEnvBadge(inEffect = true, cause: EnvNotInEffectCause = 'saved'): HtmlResult {
  const notInEffect =
    cause === 'startup'
      ? 'Environment value not in effect on this node'
      : 'Environment value superseded by the saved setting';
  return html`
    <span class="env-badge">
      <sl-icon name="exclamation-triangle"></sl-icon>
      ${inEffect ? 'Overridden by environment on this node' : notInEffect}
    </span>
  `;
}

/**
 * The environment value of an editable field. In effect: the primary
 * "In effect on this node" line, and the control below holds the saved
 * value, which is not in effect here. Not in effect: a secondary line, the
 * saved value (or a start-up default) winning.
 */
export function renderEnvValueLine(
  ev: EnvKeyValue,
  cause: EnvNotInEffectCause = 'saved'
): HtmlResult {
  const text = formatEnvValue(ev) || '—';
  if (ev.in_effect) {
    // The control below holds the stored value; label it only when a
    // stored value exists and differs (saved_differs), never a default or
    // the environment value itself.
    return html`<span class="env-value-in-effect"
        >In effect on this node: <code>${text}</code> (environment variable)</span
      >${ev.saved_differs
        ? html`<span class="hint env-value-saved-label"
            >Saved value below, not in effect on this node:</span
          >`
        : nothing}`;
  }
  const why = cause === 'startup' ? 'a start-up default or flag wins' : 'the saved value wins';
  return html`<span class="hint env-value-not-in-effect"
    >Environment value on this node, not in effect: <code>${text}</code> (${why})</span
  >`;
}

/** The section badge and the list of env_values entries of a map section. */
export function renderEnvValuesList(entries: Array<{ key: string; ev: EnvKeyValue }>): HtmlResult {
  // Entries without an in-effect state (masked) make no claim; the badge
  // says "not in effect" only when every reported state is false.
  const known = entries.filter((e) => hasInEffect(e.ev));
  return html`${renderEnvBadge(known.length === 0 || known.some((e) => e.ev.in_effect))}
    <ul class="env-values-under">
      ${entries.map(
        (e) =>
          html`<li>
            <code>${e.key}</code> = <code>${formatEnvValue(e.ev) || '—'}</code>
            (${!hasInEffect(e.ev)
              ? 'masked: whether it is in effect is not reported'
              : e.ev.in_effect
                ? 'in effect on this node'
                : 'not in effect: the saved value wins'})
          </li>`
      )}
    </ul>`;
}

/**
 * The badge of a field whose koanf key(s) env_overrides lists, with the
 * environment value line when env_values reports one. Pass one or more
 * keys; the badge renders if any match, and only the first one env_values
 * reports is shown (envValueFor).
 */
export function renderKeysEnvBadge(
  envOverrides: readonly string[],
  envValues: Record<string, EnvKeyValue> | null | undefined,
  keys: readonly string[]
): HtmlResult | typeof nothing {
  if (!keys.some((k) => envOverrides.includes(k))) return nothing;
  const env = envValueFor(envValues, keys);
  // No entry, or a masked one (no in-effect state reported): the plain badge.
  if (!env || !hasInEffect(env.ev)) return renderEnvBadge();
  return html`${renderEnvBadge(env.ev.in_effect)}${renderEnvValueLine(env.ev)}`;
}

/**
 * The badge of a map-valued section (runtimes, profiles, harness configs)
 * whose env_overrides entries are leaf keys under prefix (for example
 * profiles.local.runtime from SCION_SERVER_PROFILES_LOCAL_RUNTIME).
 */
export function renderEnvBadgeUnder(
  envOverrides: readonly string[],
  envValues: Record<string, EnvKeyValue> | null | undefined,
  prefix: string
): HtmlResult | typeof nothing {
  if (!envOverrides.some((k) => k === prefix || k.startsWith(`${prefix}.`))) return nothing;
  const entries = envValuesUnder(envValues, prefix);
  if (entries.length === 0) return renderEnvBadge();
  return renderEnvValuesList(entries);
}
