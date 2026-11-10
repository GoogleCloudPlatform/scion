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
 * Pending user secrets: user-scope secrets an agent saved, which wait for
 * the owner's approval before they are delivered to any agent.
 *
 * This module holds the client side the profile secrets list, the profile
 * nav badge and the start-time hint share: the response shapes, the merge
 * of approved and pending rows into one entry per key, the text shown for
 * a pending row, and the pending count. It never handles a value; the
 * reveal request lives in the secret list, which owns the dialog that
 * shows it.
 */

import { apiFetch } from './api.js';
import { showToast } from '../utils/toast.js';
import type { Secret } from '../shared/types.js';

/** Owner endpoints for pending secrets (list, approve, reject, reveal). */
export const PENDING_SECRETS_API = '/api/v1/pending-secrets';

/** Error code of an approval refused because the existing secret changed. */
export const PENDING_EXISTING_CHANGED_CODE = 'secret_pending_existing_changed';

/**
 * Window event fired after an approval or rejection changes the number of
 * pending secrets. `detail.count` is the new count when it is known.
 */
export const PENDING_SECRETS_CHANGED_EVENT = 'scion:pending-secrets-changed';

/** A project whose project secret of the same key a pending secret overrides. */
export interface PendingOverride {
  projectId: string;
  projectName: string;
}

/** One pending secret as its owner sees it: metadata, never the value. */
export interface PendingSecret {
  id: string;
  key: string;
  type: Secret['type'];
  target?: string;
  scope: string;
  allowProgeny: boolean;
  injectionMode?: Secret['injectionMode'];
  description?: string;
  valueLen: number;
  agentId: string;
  agentSlug: string;
  projectId: string;
  projectName: string;
  created: string;
  updated: string;
  expiresAt: string;
  replacesExisting: boolean;
  replacesSecretId?: string;
  replacesVersion?: number;
  state: 'pending';
  overridesProjectSecrets?: PendingOverride[];
}

/** One item of GET /api/v1/secrets?scope=user&includePending=true. */
export type SecretListItem =
  | { state: 'approved'; key: string; secret: Secret }
  | { state: 'pending'; key: string; pending: PendingSecret };

/** The merged secret list response. */
export interface SecretListResponse {
  secrets?: Secret[];
  items?: SecretListItem[];
}

/** One key of the profile secret list: its approved secret, its pending one, or both. */
export interface SecretEntry {
  key: string;
  approved?: Secret;
  pending?: PendingSecret;
}

/**
 * The entries of a secret list response, one per key. A key with both an
 * approved and a pending secret is one entry. Entries with a pending secret
 * come first, then the rest; each group is sorted by key. A response
 * without `items` yields its approved `secrets` only.
 */
export function secretEntries(data: SecretListResponse | Secret[]): SecretEntry[] {
  const byKey = new Map<string, SecretEntry>();
  const entry = (key: string): SecretEntry => {
    let e = byKey.get(key);
    if (!e) {
      e = { key };
      byKey.set(key, e);
    }
    return e;
  };
  if (Array.isArray(data)) {
    for (const s of data) entry(s.key).approved = s;
  } else if (Array.isArray(data.items)) {
    for (const item of data.items) {
      if (item.state === 'approved' && item.secret) entry(item.key).approved = item.secret;
      else if (item.state === 'pending' && item.pending) entry(item.key).pending = item.pending;
    }
  } else {
    for (const s of data.secrets ?? []) entry(s.key).approved = s;
  }
  return [...byKey.values()].sort((a, b) => {
    if (!!a.pending !== !!b.pending) return a.pending ? -1 : 1;
    return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
  });
}

/** The number of entries with a pending secret. */
export function pendingCount(entries: SecretEntry[]): number {
  return entries.filter((e) => e.pending).length;
}

/** The state badge text of an entry. */
export function entryStateLabel(entry: SecretEntry): string {
  if (entry.pending && entry.approved) return 'Approved, replacement pending';
  if (entry.pending) return 'Pending';
  return 'Approved';
}

/** "Written by agent <slug> (<id>) in project <name> (<id>)". */
export function pendingWriterText(p: PendingSecret): string {
  return (
    `Written by agent ${p.agentSlug} (${p.agentId}) ` +
    `in project ${p.projectName} (${p.projectId})`
  );
}

/** Where an approved pending secret is delivered. */
export function pendingEffectText(p: PendingSecret, allowProgeny = p.allowProgeny): string {
  return allowProgeny
    ? 'Delivered to your agents in all projects and to agents they create'
    : 'Delivered to your agents in all projects';
}

/**
 * "Overrides project secret <KEY> in projects P, Q", or '' when the
 * pending secret overrides no project secret.
 */
export function pendingOverridesText(p: PendingSecret): string {
  const projects = p.overridesProjectSecrets ?? [];
  if (projects.length === 0) return '';
  const names = projects.map((o) => o.projectName || o.projectId).join(', ');
  return `Overrides project secret ${p.key} in ${projects.length === 1 ? 'project' : 'projects'} ${names}`;
}

/**
 * "Replaces your existing secret (last set by <who>, <date>)", or '' when
 * the pending secret replaces none. `existing` is the approved secret of the
 * same key, when the list carries it; `formatDate` renders its update time.
 */
export function pendingReplacesText(
  p: PendingSecret,
  existing: Secret | undefined,
  formatDate: (iso: string) => string
): string {
  if (!p.replacesExisting) return '';
  if (!existing) return 'Replaces your existing secret';
  const by = existing.updatedBy || existing.createdBy;
  const parts = [
    by ? `last set by ${by}` : '',
    existing.updated ? formatDate(existing.updated) : '',
  ]
    .filter(Boolean)
    .join(', ');
  return parts ? `Replaces your existing secret (${parts})` : 'Replaces your existing secret';
}

/** Whether the pending secret was replaced by its agent after it was first written. */
export function pendingWasReplaced(p: PendingSecret): boolean {
  return !!p.updated && p.updated !== p.created;
}

/** "1 byte" / "N bytes". */
export function valueLengthText(n: number): string {
  return n === 1 ? '1 byte' : `${n} bytes`;
}

/**
 * Decodes a base64 value to text. Returns null for a value that is not
 * valid base64 or not valid UTF-8, so the caller can show it as base64.
 */
export function decodeBase64Text(b64: string): string | null {
  try {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
}

/**
 * The number of the signed-in user's pending secrets, or null when it
 * cannot be read. Never redirects or toasts on an auth error: callers use
 * it for a badge or a hint only.
 */
export async function fetchPendingSecretCount(): Promise<number | null> {
  try {
    const res = await apiFetch(PENDING_SECRETS_API, { authErrors: 'caller' });
    if (!res.ok) return null;
    const data = (await res.json()) as { pendingSecrets?: unknown[] };
    return Array.isArray(data.pendingSecrets) ? data.pendingSecrets.length : 0;
  } catch {
    return null;
  }
}

/** The start-time hint text for `n` pending secrets. */
export function pendingSecretsHintText(n: number): string {
  return n === 1
    ? '1 secret awaits approval in Profile > Secrets.'
    : `${n} secrets await approval in Profile > Secrets.`;
}

/**
 * After the user starts an agent, shows "N secrets await approval" when
 * they have pending secrets. Shown to the user in the browser only; it
 * never fails or delays the start.
 */
export async function showPendingSecretsStartHint(): Promise<void> {
  const n = await fetchPendingSecretCount();
  if (n && n > 0) {
    showToast(pendingSecretsHintText(n), 'primary');
  }
}

/** Tells listeners (the profile nav badge) that the pending count changed. */
export function dispatchPendingSecretsChanged(count?: number): void {
  window.dispatchEvent(
    new CustomEvent(PENDING_SECRETS_CHANGED_EVENT, {
      detail: count === undefined ? {} : { count },
    })
  );
}
