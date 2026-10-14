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
 * Display rows for a flat Runtime Broker's runtime target and the
 * presentation its instance reports (.design/flat-runtime-brokers/contract-appendix-p3.md
 * FC4-a; ptone/scion#3282, ptone/scion#3283).
 *
 * The rows name each FC4-a published field explicitly and read nothing
 * else. The payload is JSON from the Hub and TypeScript types are not
 * enforced at runtime, so a client must never walk the received object
 * generically: a field FC4-a never publishes (a kubeconfig context, a
 * volume source, an env value) that reached the browser anyway, from a
 * misbehaving or newer Hub, must still never be rendered.
 */

import type {
  PresentedResourceSpec,
  PresentedVolume,
  RuntimeTargetDescriptor,
  RuntimeTargetPresentation,
} from './types.js';

/**
 * The caption under the reported defaults: what they are, and what they are
 * not (ptone/scion#3283: no implication of verified Kubernetes permissions).
 */
export const PRESENTATION_CAPTION =
  'Defaults for new agents; not the configuration of existing agents and not a check of cluster permissions.';

/** One label/value line of the Runtime target card. */
export interface DisplayRow {
  label: string;
  value: string;
}

/** safe_to_evict as the card shows it: unset, true or false. */
export function safeToEvictLabel(value: boolean | null | undefined): 'unset' | 'true' | 'false' {
  if (value === true) return 'true';
  if (value === false) return 'false';
  return 'unset';
}

function push(rows: DisplayRow[], label: string, value: string | undefined): void {
  if (value) rows.push({ label, value });
}

function sortedPairs(m: Record<string, string> | undefined): string {
  if (!m) return '';
  return Object.keys(m)
    .sort()
    .map((k) => `${k}=${m[k]}`)
    .join(', ');
}

function volumeLabel(v: PresentedVolume): string {
  const attrs: string[] = [];
  if (v.type) attrs.push(v.type);
  if (v.readOnly) attrs.push('read-only');
  return attrs.length > 0 ? `${v.target} (${attrs.join(', ')})` : v.target;
}

function resourcesLabel(r: PresentedResourceSpec | undefined): string {
  if (!r) return '';
  const list = (name: string, l?: { cpu?: string; memory?: string }) => {
    const parts: string[] = [];
    if (l?.cpu) parts.push(`cpu=${l.cpu}`);
    if (l?.memory) parts.push(`memory=${l.memory}`);
    return parts.length > 0 ? `${name} ${parts.join(' ')}` : '';
  };
  const parts = [list('requests', r.requests), list('limits', r.limits)].filter((s) => s !== '');
  if (r.disk) parts.push(`disk ${r.disk}`);
  return parts.join('; ');
}

/** The stored target's rows: ID, type, display name, and the reported namespace. */
export function runtimeTargetRows(
  target: RuntimeTargetDescriptor,
  presentation?: RuntimeTargetPresentation
): DisplayRow[] {
  const rows: DisplayRow[] = [];
  push(rows, 'ID', target.id);
  push(rows, 'Type', target.type);
  push(rows, 'Display name', target.displayName);
  push(rows, 'Namespace', presentation?.target?.namespace);
  return rows;
}

/**
 * The reported defaults, one row per set FC4-a field (safe_to_evict always,
 * as unset/true/false). Environment variables appear by name only.
 */
export function presentationRows(p: RuntimeTargetPresentation): DisplayRow[] {
  const rows: DisplayRow[] = [];
  const t = p.target;
  if (t) {
    rows.push({ label: 'Safe to evict', value: safeToEvictLabel(t.safeToEvict) });
    if (t.gke === true) rows.push({ label: 'GKE', value: 'true' });
    push(rows, 'Priority class', t.priorityClassName);
    push(rows, 'Shared dir storage class', t.sharedDirStorageClass);
    push(rows, 'Shared dir size', t.sharedDirSize);
    push(rows, 'Shared dir storage backend', t.sharedDirStorageBackend);
    push(rows, 'Shared dir backends', sortedPairs(t.sharedDirStorageBackends));
    push(rows, 'Home storage backend', t.homeStorageBackend);
  }
  const d = p.agentDefaults;
  if (!d) return rows;
  push(rows, 'Default harness config', d.defaultHarnessConfig);
  push(rows, 'Image registry', d.imageRegistry);
  push(rows, 'Clone depth', d.cloneDepth);
  push(rows, 'Resources', resourcesLabel(d.resources));
  push(rows, 'Volumes', (d.volumes ?? []).map(volumeLabel).join(', '));
  push(
    rows,
    'Secrets',
    (d.secrets ?? [])
      .map((s) => {
        let text = s.key;
        if (s.type) text += ` (${s.type})`;
        if (s.description) text += `: ${s.description}`;
        return text;
      })
      .join('; ')
  );
  const overrides = d.harnessOverrides ?? {};
  for (const name of Object.keys(overrides).sort()) {
    const o = overrides[name];
    const parts: string[] = [];
    if (o.image) parts.push(`image ${o.image}`);
    if (o.imagePullPolicy) parts.push(`pull ${o.imagePullPolicy}`);
    if (o.authSelectedType) parts.push(`auth ${o.authSelectedType}`);
    if (o.user) parts.push(`user ${o.user}`);
    if (o.envKeys && o.envKeys.length > 0) parts.push(`env keys ${o.envKeys.join(', ')}`);
    if (o.volumes && o.volumes.length > 0) {
      parts.push(`volumes ${o.volumes.map(volumeLabel).join(', ')}`);
    }
    const res = resourcesLabel(o.resources);
    if (res) parts.push(`resources ${res}`);
    rows.push({
      label: `Harness override: ${name}`,
      value: parts.length > 0 ? parts.join(' · ') : '—',
    });
  }
  return rows;
}
