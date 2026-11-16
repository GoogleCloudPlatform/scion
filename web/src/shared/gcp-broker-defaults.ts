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
 * Pure helpers for the Project Settings editor of the per-broker default
 * GCP service account map (`defaultGCPIdentityServiceAccountIDByBroker`,
 * ptone/scion#8185 / ptone/scion#4895): rows, the flat Runtime Brokers that
 * provide to the project, the save response's per-broker warnings and errors,
 * and the effective default identity each flat broker resolves to.
 *
 * Mirrors `gcp-profile-defaults.ts` for flat Runtime Brokers (`isFlatBroker`).
 * Legacy profile-based brokers remain in `defaultGCPIdentityServiceAccountIDByProfile`
 * and never appear as selectable options here.
 */

import { isBrokerKubernetesOnly } from './runtime-kind.js';
import { isFlatBroker, runtimeTargetLabel } from './runtime-target.js';
import type { GCPServiceAccount, RuntimeBroker } from './types.js';

/** The project settings field this module edits. */
export const BROKER_DEFAULTS_FIELD = 'defaultGCPIdentityServiceAccountIDByBroker';

/** Hub-enforced cap on per-broker default entries. */
export const MAX_BROKER_DEFAULTS = 64;

/** One editor row. `key` is stable across edits, for keyed rendering. */
export interface BrokerDefaultRow {
  key: number;
  brokerId: string;
  serviceAccountId: string;
}

/** A flat Runtime Broker linked to / providing to the project. */
export interface KnownFlatBroker {
  id: string;
  name: string;
  targetLabel: string;
  kubernetes: boolean;
}

/** An option offered in a row's Runtime Broker picker. */
export interface BrokerOption {
  id: string;
  name: string;
  targetLabel: string;
  /** True when the stored broker ID is not among the project's active flat brokers. */
  stale: boolean;
}

/** Rows for a stored map, sorted by broker ID. */
export function brokerRowsFromMap(
  map: Record<string, string> | null | undefined,
  nextKey: () => number
): BrokerDefaultRow[] {
  const m = map ?? {};
  return Object.keys(m)
    .sort()
    .map((brokerId) => ({ key: nextKey(), brokerId, serviceAccountId: m[brokerId] ?? '' }));
}

/** The map a set of complete rows stands for. Incomplete rows are left out. */
export function brokerMapFromRows(rows: readonly BrokerDefaultRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of rows) {
    if (row.brokerId && row.serviceAccountId) out[row.brokerId] = row.serviceAccountId;
  }
  return out;
}

/** Whether two maps hold the same entries. */
export function sameBrokerMap(
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
 * brokers cannot be chosen in the picker, but a row is checked anyway.
 */
export function brokerRowProblem(row: BrokerDefaultRow, rows: readonly BrokerDefaultRow[]): string {
  if (!row.brokerId && !row.serviceAccountId) {
    return 'Choose a Runtime Broker and a service account, or remove this row.';
  }
  if (!row.brokerId) return 'Choose a Runtime Broker, or remove this row.';
  if (!row.serviceAccountId) return 'Choose a service account, or remove this row.';
  if (rows.some((r) => r !== row && r.brokerId === row.brokerId)) {
    return `Runtime Broker "${row.brokerId}" has more than one row.`;
  }
  return '';
}

/** Client-side collection problem (e.g. exceeding the 64-entry cap), or ''. */
export function brokerCollectionProblem(rows: readonly BrokerDefaultRow[]): string {
  if (rows.length > MAX_BROKER_DEFAULTS) {
    return `At most ${MAX_BROKER_DEFAULTS} per-broker default entries are allowed.`;
  }
  return '';
}

/**
 * The flat Runtime Brokers among `brokers`, sorted by name (case-insensitive)
 * then ID. Legacy profile-based brokers (`!isFlatBroker`) are excluded.
 */
export function knownFlatBrokers(
  brokers: readonly Pick<RuntimeBroker, 'id' | 'name' | 'profiles' | 'runtimeTarget'>[]
): KnownFlatBroker[] {
  const out: KnownFlatBroker[] = [];
  for (const b of brokers) {
    if (!b.id || !isFlatBroker(b)) continue;
    out.push({
      id: b.id,
      name: b.name || b.id,
      targetLabel: runtimeTargetLabel(b),
      kubernetes: isBrokerKubernetesOnly(b),
    });
  }
  return out.sort((a, b) => {
    const an = a.name.toLowerCase();
    const bn = b.name.toLowerCase();
    if (an !== bn) return an < bn ? -1 : 1;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

/**
 * The Runtime Brokers a row's picker offers: every known flat broker no other
 * row uses, plus the row's own broker ID when it is no longer in `known` (so a
 * stored entry for a deleted or unlinked broker stays visible and removable).
 */
export function brokerOptionsForRow(
  row: BrokerDefaultRow,
  rows: readonly BrokerDefaultRow[],
  known: readonly KnownFlatBroker[]
): BrokerOption[] {
  const taken = new Set(rows.filter((r) => r !== row && r.brokerId).map((r) => r.brokerId));
  const options: BrokerOption[] = known
    .filter((b) => !taken.has(b.id))
    .map((b) => ({
      id: b.id,
      name: b.name,
      targetLabel: b.targetLabel,
      stale: false,
    }));
  if (row.brokerId && !known.some((b) => b.id === row.brokerId)) {
    options.push({
      id: row.brokerId,
      name: row.brokerId,
      targetLabel: '',
      stale: true,
    });
  }
  return options;
}

/** The verified service accounts a row's picker offers. */
export function serviceAccountOptionsForBroker(
  accounts: readonly GCPServiceAccount[],
  _brokerId: string
): GCPServiceAccount[] {
  return accounts.filter((sa) => sa.verified);
}

/**
 * Kubernetes flat brokers with no row while some other flat broker has one.
 * Mirrors `profilesMissingDefault`.
 */
export function brokersMissingDefault(
  rows: readonly BrokerDefaultRow[],
  known: readonly KnownFlatBroker[]
): KnownFlatBroker[] {
  const set = new Set(rows.filter((r) => r.brokerId).map((r) => r.brokerId));
  if (set.size === 0) return [];
  return known.filter((b) => b.kubernetes && !set.has(b.id));
}

/** Messages split into those about one broker's entry and the rest. */
export interface BrokerMessages {
  byBroker: Record<string, string[]>;
  general: string[];
}

const ENTRY_PREFIX = new RegExp(`^${BROKER_DEFAULTS_FIELD}\\[([^\\]]+)\\]`);
const BROKER_PHRASE = /\bthe default for Runtime Broker "([^"]+)"/;

function resolveBrokerRef(ref: string, known: readonly KnownFlatBroker[]): string {
  const byId = known.find((b) => b.id === ref);
  if (byId) return byId.id;
  const byName = known.filter((b) => b.name === ref);
  if (byName.length === 1 && byName[0]) return byName[0].id;
  return ref;
}

function addMessage(out: BrokerMessages, brokerId: string | null, message: string): void {
  if (brokerId) {
    (out.byBroker[brokerId] ??= []).push(message);
  } else {
    out.general.push(message);
  }
}

function brokerFromField(field: unknown, known: readonly KnownFlatBroker[]): string | null {
  if (typeof field !== 'string') return null;
  if (field === BROKER_DEFAULTS_FIELD) return '';
  if (field.startsWith(BROKER_DEFAULTS_FIELD + '.')) {
    return resolveBrokerRef(field.slice(BROKER_DEFAULTS_FIELD.length + 1), known);
  }
  const m = ENTRY_PREFIX.exec(field);
  return m?.[1] ? resolveBrokerRef(m[1], known) : null;
}

function brokerFromMessage(message: string, known: readonly KnownFlatBroker[]): string | null {
  const prefixed = ENTRY_PREFIX.exec(message);
  if (prefixed?.[1]) return resolveBrokerRef(prefixed[1], known);
  if (message.startsWith(BROKER_DEFAULTS_FIELD + ':')) return '';
  const phrase = BROKER_PHRASE.exec(message);
  return phrase?.[1] ? resolveBrokerRef(phrase[1], known) : null;
}

/** Whether a warning or error message specifically addresses per-broker defaults. */
export function isBrokerDefaultMessage(message: string): boolean {
  return (
    ENTRY_PREFIX.test(message) ||
    message.startsWith(BROKER_DEFAULTS_FIELD + ':') ||
    BROKER_PHRASE.test(message)
  );
}

/**
 * Splits `warnings` into per-broker warnings and general warnings.
 */
export function splitBrokerWarnings(
  warnings: unknown,
  known: readonly KnownFlatBroker[] = []
): BrokerMessages {
  const out: BrokerMessages = { byBroker: {}, general: [] };
  if (!Array.isArray(warnings)) return out;
  for (const w of warnings) {
    if (typeof w !== 'string' || !w) continue;
    const brokerId = brokerFromMessage(w, known);
    addMessage(out, brokerId || null, w);
  }
  return out;
}

/**
 * Extracts per-broker and whole-map validation errors from a failed save body.
 */
export function brokerErrorsFromBody(
  body: unknown,
  known: readonly KnownFlatBroker[] = []
): BrokerMessages {
  const out: BrokerMessages = { byBroker: {}, general: [] };
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
    const brokerId = brokerFromField(field, known);
    if (brokerId === null) continue;
    addMessage(out, brokerId || null, typeof m === 'string' && m ? m : message);
    matched = true;
  }
  if (matched) return out;

  const fieldBroker = brokerFromField(details?.['field'], known);
  if (fieldBroker !== null) {
    addMessage(out, fieldBroker || null, message);
    return out;
  }

  if (message) {
    const brokerId = brokerFromMessage(message, known);
    if (brokerId !== null) addMessage(out, brokerId || null, message);
  }
  return out;
}

/** Where a flat broker's effective default identity comes from. */
export type BrokerEffectiveSource = 'broker' | 'project' | 'hub' | 'none';

export interface BrokerEffectiveIdentity {
  source: BrokerEffectiveSource;
  /** block, passthrough or assign; '' for none. */
  mode: string;
  /** The assigned account's ID, for assign. */
  serviceAccountId: string;
}

/**
 * The identity a new agent on flat broker `brokerId` gets when it chooses none,
 * following the hub's ladder (explicit > per-broker default > project default >
 * hub default > runtime default).
 */
export function effectiveIdentityForBroker(
  brokerId: string,
  map: Record<string, string>,
  project: { mode: string; serviceAccountId: string },
  hub: { mode: string; serviceAccountId: string }
): BrokerEffectiveIdentity {
  const own = map[brokerId];
  if (own) return { source: 'broker', mode: 'assign', serviceAccountId: own };
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
