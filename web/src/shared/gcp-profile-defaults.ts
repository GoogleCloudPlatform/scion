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
 * Pure helpers for the Project Settings editor of the per-profile default
 * GCP service account map (`defaultGCPIdentityServiceAccountIDByProfile`,
 * ptone/scion#3947): rows, the profiles the project's brokers report, the
 * save response's per-profile warnings and errors, and the identity each
 * profile resolves to.
 *
 * The hub's agent-create ladder is: explicit request, then the per-profile
 * default for the profile the agent runs under, then the project default,
 * then the hub default, then nothing (the broker applies its runtime
 * default). A per-profile entry wins over the project default, including an
 * explicit project "block".
 */

import type { GCPServiceAccount, RuntimeBroker } from './types.js';

/** The project settings field this module edits. */
export const PROFILE_DEFAULTS_FIELD = 'defaultGCPIdentityServiceAccountIDByProfile';

/** One editor row. `key` is stable across edits, for keyed rendering. */
export interface ProfileDefaultRow {
  key: number;
  profile: string;
  serviceAccountId: string;
}

/** A profile name reported by at least one of the project's brokers. */
export interface KnownProfile {
  name: string;
  /** Runtime types reported for this name, sorted (usually one). */
  types: string[];
  /** Every broker that reports this name reports it as Kubernetes. */
  kubernetes: boolean;
}

/** Rows for a stored map, sorted by profile name. */
export function rowsFromMap(
  map: Record<string, string> | null | undefined,
  nextKey: () => number
): ProfileDefaultRow[] {
  const m = map ?? {};
  return Object.keys(m)
    .sort()
    .map((profile) => ({ key: nextKey(), profile, serviceAccountId: m[profile] ?? '' }));
}

/** The map a set of complete rows stands for. Incomplete rows are left out. */
export function mapFromRows(rows: readonly ProfileDefaultRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of rows) {
    if (row.profile && row.serviceAccountId) out[row.profile] = row.serviceAccountId;
  }
  return out;
}

/** Whether two maps hold the same entries. */
export function sameProfileMap(
  a: Record<string, string> | null | undefined,
  b: Record<string, string> | null | undefined
): boolean {
  const ak = Object.keys(a ?? {});
  const bk = Object.keys(b ?? {});
  if (ak.length !== bk.length) return false;
  return ak.every((k) => (b ?? {})[k] === (a ?? {})[k]);
}

/**
 * The client-side problem with a row, or '' when it is complete. Duplicate
 * profiles cannot be chosen in the picker, but a row is checked anyway.
 */
export function rowProblem(row: ProfileDefaultRow, rows: readonly ProfileDefaultRow[]): string {
  if (!row.profile && !row.serviceAccountId) {
    return 'Choose a profile and a service account, or remove this row.';
  }
  if (!row.profile) return 'Choose a profile, or remove this row.';
  if (!row.serviceAccountId) return 'Choose a service account, or remove this row.';
  if (rows.some((r) => r !== row && r.profile === row.profile)) {
    return `Profile "${row.profile}" has more than one row.`;
  }
  return '';
}

/** The profiles the given brokers report, by name, sorted. */
export function knownProfiles(brokers: readonly Pick<RuntimeBroker, 'profiles'>[]): KnownProfile[] {
  const byName = new Map<string, { types: Set<string>; kubernetes: boolean }>();
  for (const broker of brokers) {
    for (const p of broker.profiles ?? []) {
      if (!p.name) continue;
      const entry = byName.get(p.name) ?? { types: new Set<string>(), kubernetes: true };
      const type = (p.type || '').toLowerCase();
      if (type) entry.types.add(type);
      if (type !== 'kubernetes') entry.kubernetes = false;
      byName.set(p.name, entry);
    }
  }
  return [...byName.entries()]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([name, e]) => ({ name, types: [...e.types].sort(), kubernetes: e.kubernetes }));
}

/**
 * The profiles a row's picker offers: every known profile no other row uses,
 * plus the row's own profile (even one no broker reports, so a stored entry
 * stays visible and removable).
 */
export function profileOptionsForRow(
  row: ProfileDefaultRow,
  rows: readonly ProfileDefaultRow[],
  known: readonly KnownProfile[]
): string[] {
  const taken = new Set(rows.filter((r) => r !== row).map((r) => r.profile));
  const names = known.map((p) => p.name).filter((n) => !taken.has(n));
  if (row.profile && !names.includes(row.profile)) names.push(row.profile);
  return names.sort();
}

/**
 * The service accounts a row's picker offers. One function so a later
 * per-profile "usable where" filter (ptone/scion#3329) only changes this.
 */
export function serviceAccountOptionsForProfile(
  accounts: readonly GCPServiceAccount[],
  _profile: string
): GCPServiceAccount[] {
  return accounts.filter((sa) => sa.verified);
}

/**
 * Kubernetes profiles with no row while some other profile has one. Such a
 * profile silently falls back to the project default, which may not be
 * mapped on that profile's cluster.
 */
export function profilesMissingDefault(
  rows: readonly ProfileDefaultRow[],
  known: readonly KnownProfile[]
): string[] {
  const set = new Set(rows.filter((r) => r.profile).map((r) => r.profile));
  if (set.size === 0) return [];
  return known.filter((p) => p.kubernetes && !set.has(p.name)).map((p) => p.name);
}

/** Messages split into those about one profile's entry and the rest. */
export interface ProfileMessages {
  byProfile: Record<string, string[]>;
  general: string[];
}

const ENTRY_PREFIX = new RegExp(`^${PROFILE_DEFAULTS_FIELD}\\[([^\\]]+)\\]`);
const PROFILE_PHRASE = /\bthe default for profile "([^"]+)"/;

function addMessage(out: ProfileMessages, profile: string | null, message: string): void {
  if (profile) {
    (out.byProfile[profile] ??= []).push(message);
  } else {
    out.general.push(message);
  }
}

/** The profile a field path names, '' for the whole map, null for another field. */
function profileFromField(field: unknown): string | null {
  if (typeof field !== 'string') return null;
  if (field === PROFILE_DEFAULTS_FIELD) return '';
  if (field.startsWith(PROFILE_DEFAULTS_FIELD + '.')) {
    return field.slice(PROFILE_DEFAULTS_FIELD.length + 1);
  }
  const m = ENTRY_PREFIX.exec(field);
  return m ? (m[1] ?? null) : null;
}

/** The profile a message names (by its entry prefix or wording), '' for the map, null otherwise. */
function profileFromMessage(message: string): string | null {
  const prefixed = ENTRY_PREFIX.exec(message);
  if (prefixed) return prefixed[1] ?? null;
  // The whole-map form is "<field>: ..."; a longer field name sharing the prefix is not it.
  if (message.startsWith(PROFILE_DEFAULTS_FIELD + ':')) return '';
  const phrase = PROFILE_PHRASE.exec(message);
  return phrase ? (phrase[1] ?? null) : null;
}

/**
 * Splits the save response's `warnings` into per-profile warnings and the
 * rest. Per-profile warnings start with `<field>[<profile>]:`.
 */
export function splitProfileWarnings(warnings: unknown): ProfileMessages {
  const out: ProfileMessages = { byProfile: {}, general: [] };
  if (!Array.isArray(warnings)) return out;
  for (const w of warnings) {
    if (typeof w !== 'string' || !w) continue;
    const profile = profileFromMessage(w);
    addMessage(out, profile || null, w);
  }
  return out;
}

/**
 * The editor's share of a failed save: errors about one profile's entry,
 * and errors about the map as a whole (in `general`). Errors about other
 * fields are not returned; the page shows the save error as before.
 *
 * Reads, in order: `error.details.errors[]` entries' `field`, then
 * `error.details.field`, then the message (`<field>[<profile>]: ...`,
 * `<field>: ...`, or `... the default for profile "<profile>" ...`). The
 * Project Settings PUT sends only the message today, so the fallback stays.
 */
export function profileErrorsFromBody(body: unknown): ProfileMessages {
  const out: ProfileMessages = { byProfile: {}, general: [] };
  if (!body || typeof body !== 'object') return out;
  const raw = (body as { error?: unknown; message?: unknown }).error;
  let message = '';
  let details: Record<string, unknown> | undefined;
  if (raw && typeof raw === 'object') {
    const e = raw as { message?: unknown; details?: unknown };
    if (typeof e.message === 'string') message = e.message;
    if (e.details && typeof e.details === 'object') details = e.details as Record<string, unknown>;
  } else if (typeof raw === 'string') {
    message = raw;
  } else if (typeof (body as { message?: unknown }).message === 'string') {
    message = (body as { message: string }).message;
  }

  const rawErrors: unknown = details?.['errors'];
  const entries: unknown[] = Array.isArray(rawErrors) ? rawErrors : [];
  let matched = false;
  for (const entry of entries) {
    if (!entry || typeof entry !== 'object') continue;
    const { field, message: m } = entry as { field?: unknown; message?: unknown };
    const profile = profileFromField(field);
    if (profile === null) continue;
    addMessage(out, profile || null, typeof m === 'string' && m ? m : message);
    matched = true;
  }
  if (matched) return out;

  const fieldProfile = profileFromField(details?.['field']);
  if (fieldProfile !== null) {
    addMessage(out, fieldProfile || null, message);
    return out;
  }

  if (message) {
    const profile = profileFromMessage(message);
    if (profile !== null) addMessage(out, profile || null, message);
  }
  return out;
}

/** Where a profile's effective default identity comes from. */
export type EffectiveSource = 'profile' | 'project' | 'hub' | 'none';

export interface EffectiveIdentity {
  source: EffectiveSource;
  /** block, passthrough or assign; '' for none. */
  mode: string;
  /** The assigned account's ID, for assign. */
  serviceAccountId: string;
}

/**
 * The identity a new agent on `profile` gets when it chooses none, following
 * the hub's ladder. `project` and `hub` are the mode and service account of
 * each tier ('' when unset). A tier set to assign with no account counts as
 * set and falls back to block, as the hub applies it.
 */
export function effectiveIdentityForProfile(
  profile: string,
  map: Record<string, string>,
  project: { mode: string; serviceAccountId: string },
  hub: { mode: string; serviceAccountId: string }
): EffectiveIdentity {
  const own = map[profile];
  if (own) return { source: 'profile', mode: 'assign', serviceAccountId: own };
  for (const [source, tier] of [
    ['project', project],
    ['hub', hub],
  ] as const) {
    if (!tier.mode) continue;
    if (tier.mode === 'assign' && !tier.serviceAccountId) {
      return { source, mode: 'block', serviceAccountId: '' };
    }
    return {
      source,
      mode: tier.mode,
      serviceAccountId: tier.mode === 'assign' ? tier.serviceAccountId : '',
    };
  }
  return { source: 'none', mode: '', serviceAccountId: '' };
}
