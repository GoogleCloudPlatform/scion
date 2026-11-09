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
 * The conversion plan of a legacy Runtime Broker, as served by
 * GET /api/v1/runtime-brokers/{id}/conversion-plan (Go
 * api.ConversionPlanResponse), and the pure helpers the read-only plan
 * section renders it with.
 *
 * The plan carries only publishable data: each proposed instance is a
 * RuntimeTargetPresentation, and a finding is a code, a severity, a
 * settings key path and profile names, never a message. Findings are shown
 * with a fixed text per code, and a split group's differing fields come
 * only from comparing the published presentations of its instances.
 */

import type { RuntimeTargetPresentation } from './types.js';
import { placementRows, presentationRows, type DisplayRow } from './runtime-target.js';

/** One converter finding without its message. */
export interface ConversionPlanFinding {
  code: string;
  severity: string;
  path: string;
  profiles?: string[];
}

/** The snapshot a legacy Runtime Broker reports on its heartbeats. */
export interface ConversionPlanPresentation {
  schemaVersion: number;
  instances?: RuntimeTargetPresentation[];
  profileMap?: Record<string, string>;
  findings?: ConversionPlanFinding[];
  computedAt?: string;
}

/** A profile name and the proposed instance it maps to. */
export interface ConversionPlanProfileRef {
  profile?: string;
  /** "mapped", "no_mapping" or "profile_unknown". */
  mapping: string;
  instance?: string;
}

export interface ConversionPlanAgent extends ConversionPlanProfileRef {
  id: string;
  name: string;
  projectId?: string;
  phase?: string;
}

export interface ConversionPlanProject {
  id: string;
  name: string;
  activeProfile?: ConversionPlanProfileRef;
  serviceAccountProfiles?: ConversionPlanProfileRef[];
}

export interface ConversionPlanCapacity {
  outcome: string;
  maxAgents?: number;
  allocations?: { instance: string; maxAgents: number }[];
  reasons?: string[];
  flags?: string[];
}

export interface ConversionPlanResponse {
  brokerId: string;
  observedAt?: string;
  plan: ConversionPlanPresentation;
  agents: { items?: ConversionPlanAgent[]; hiddenCount?: number; terminalCount?: number };
  projects: { items?: ConversionPlanProject[]; hiddenCount?: number };
  capacity: ConversionPlanCapacity;
}

/** Error codes of the conversion plan endpoint. */
export const CONVERSION_PLAN_NOT_REPORTED = 'conversion_plan_not_reported';
export const RUNTIME_BROKER_NOT_LEGACY = 'runtime_broker_not_legacy';

/** The finding a Runtime Broker adds when it shortened the plan to fit the caps. */
export const PLAN_TRUNCATED_CODE = 'plan_truncated';

/** The path of the split notice (FC6 errata, preserve by splitting). */
export const SPLIT_NOTICE_PATH = 'profiles.*';

/** The pointer to the full, host-local plan. */
export const CLI_POINTER = 'scion runtime-broker convert --plan';

/**
 * The fixed text for each finding code. The converter's own message is
 * never sent to the Hub, because it can quote settings values.
 */
const FINDING_TEXTS: Record<string, string> = {
  unsupported_flat_target_type:
    'This runtime type has no flat Runtime Broker target, so these profiles cannot move as they are.',
  flat_target_scope_violation:
    'This runtime reaches beyond one namespace (list_all_namespaces), and a flat instance serves exactly one.',
  profile_conflict:
    'These profiles differ on a shared target. Each value is kept: the profiles are split onto separate proposed instances, or keep their own placement.',
  profile_conflict_resolved: 'These profiles differed here; an operator choice resolves it.',
  legacy_field_no_effect:
    'This legacy setting has no effect on a flat instance and is not carried over.',
  legacy_field_removed: 'This setting was removed and is not carried over.',
  unknown_settings_key:
    'This settings key is not recognized, or an operator choice cannot be applied. Review it on the host.',
  field_not_valid_for_target_type:
    'This setting does not apply to the runtime type and is not carried over.',
  [PLAN_TRUNCATED_CODE]:
    'The Runtime Broker shortened this plan to fit the reporting limits, so it may be incomplete.',
};

/** The fixed text for a finding code; a generic pointer for an unknown code. */
export function findingText(code: string): string {
  return FINDING_TEXTS[code] ?? `Run ${CLI_POINTER} on the host for details.`;
}

/** The fixed label of a capacity outcome; the code itself when unknown. */
export function capacityOutcomeLabel(outcome: string): string {
  switch (outcome) {
    case 'preserve_by_split':
      return 'The limit is split across the proposed instances';
    case 'operator_decision_required':
      return 'An operator decision is required';
    case 'no_limit':
      return 'No limit to split';
    default:
      return outcome;
  }
}

/** A profile reference as text: "instance key", "no mapping" or "profile unknown". */
export function profileRefLabel(ref: ConversionPlanProfileRef | undefined): string {
  if (!ref) return '—';
  if (ref.mapping === 'mapped' && ref.instance) return ref.instance;
  if (ref.mapping === 'profile_unknown') return 'profile unknown';
  return 'no mapping';
}

/** Whether the plan carries the plan_truncated finding. */
export function isTruncated(plan: ConversionPlanPresentation): boolean {
  return (plan.findings ?? []).some((f) => f.code === PLAN_TRUNCATED_CODE);
}

/** One row of the profile map: a mapped profile, or one a blocking finding left unmapped. */
export interface ProfileMapRow {
  profile: string;
  instance?: string;
  /** The code of the blocking finding that leaves the profile unmapped. */
  noMappingCode?: string;
}

/**
 * The profile map rows, sorted by profile: every mapped profile, plus each
 * profile named by a blocking finding that has no map entry.
 */
export function profileMapRows(plan: ConversionPlanPresentation): ProfileMapRow[] {
  const map = plan.profileMap ?? {};
  const rows = new Map<string, ProfileMapRow>();
  for (const profile of Object.keys(map)) {
    if (map[profile]) rows.set(profile, { profile, instance: map[profile] });
  }
  for (const f of plan.findings ?? []) {
    if (f.severity !== 'blocking') continue;
    for (const profile of f.profiles ?? []) {
      if (!rows.has(profile)) rows.set(profile, { profile, noMappingCode: f.code });
    }
  }
  return Array.from(rows.values()).sort((a, b) => a.profile.localeCompare(b.profile));
}

/**
 * The published rows of a proposed instance: the target's type, display
 * name and namespace, the reported defaults and, for Kubernetes, the
 * placement. Every row names a published field explicitly (shared/runtime-
 * target.ts); the object is never walked, so a key outside the published
 * set is never shown.
 */
export function instanceRows(p: RuntimeTargetPresentation): DisplayRow[] {
  const rows: DisplayRow[] = [];
  const t = p.target;
  if (t) {
    if (t.type) rows.push({ label: 'Type', value: t.type });
    if (t.displayName) rows.push({ label: 'Display name', value: t.displayName });
    if (t.namespace) rows.push({ label: 'Namespace', value: t.namespace });
  }
  rows.push(...presentationRows(p));
  if (t) rows.push(...placementRows({ id: '', type: t.type }, p));
  return rows;
}

/** A published field whose value differs between a split group's instances. */
export interface FieldDifference {
  label: string;
  /** One entry per instance, in instance key order; "unset" when absent. */
  values: { instance: string; value: string }[];
}

/** A split group: the profiles of one split notice and their instances. */
export interface SplitGroup {
  /** The split notice this group comes from. */
  finding: ConversionPlanFinding;
  profiles: string[];
  /** The proposed instance keys of the group, sorted. */
  instances: string[];
  /**
   * The published fields that differ. Empty when the instances could not
   * be resolved or their published fields are equal: the difference is then
   * in a setting the Hub never receives, and no field or value is shown.
   */
  differences: FieldDifference[];
}

/**
 * The split groups of the plan: for each split notice (profile_conflict at
 * profiles.*), the instances its profiles map to, compared on their
 * published rows.
 */
export function splitGroups(plan: ConversionPlanPresentation): SplitGroup[] {
  const map = plan.profileMap ?? {};
  const byKey = new Map<string, RuntimeTargetPresentation>();
  for (const inst of plan.instances ?? []) byKey.set(inst.key, inst);
  const groups: SplitGroup[] = [];
  for (const f of plan.findings ?? []) {
    if (f.code !== 'profile_conflict' || f.path !== SPLIT_NOTICE_PATH) continue;
    const profiles = [...(f.profiles ?? [])];
    const keys = Array.from(
      new Set(profiles.map((p) => map[p]).filter((k): k is string => !!k && byKey.has(k)))
    ).sort();
    groups.push({
      finding: f,
      profiles,
      instances: keys,
      differences: keys.length < 2 ? [] : compare(keys, byKey),
    });
  }
  return groups;
}

function compare(keys: string[], byKey: Map<string, RuntimeTargetPresentation>): FieldDifference[] {
  const rowsByKey = keys.map((k) => {
    const m = new Map<string, string>();
    for (const r of instanceRows(byKey.get(k)!)) m.set(r.label, r.value);
    return m;
  });
  const labels: string[] = [];
  for (const m of rowsByKey) {
    for (const label of m.keys()) if (!labels.includes(label)) labels.push(label);
  }
  const out: FieldDifference[] = [];
  for (const label of labels) {
    const values = rowsByKey.map((m, i) => ({ instance: keys[i], value: m.get(label) ?? 'unset' }));
    if (new Set(values.map((v) => v.value)).size > 1) out.push({ label, values });
  }
  return out;
}
