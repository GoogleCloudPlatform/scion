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
 * Capacity text shared by the Brokers area Overview and Configure tabs
 * (broker UI Option A, slice A1): the effective max-agents value and the
 * precedence step that produced it, worded as on the existing broker page.
 */

/** The effective agent limit as text: "unlimited", a number or "unknown". */
export function effectiveLimitText(value: number | null | undefined): string {
  if (value === null || value === undefined) return 'unknown';
  return value === 0 ? 'unlimited' : String(value);
}

/** Where the effective limit comes from (EffectiveSetting.source). */
export function limitSourceLabel(source: string): string {
  switch (source) {
    case 'broker':
      return 'this broker’s own setting';
    case 'entitlement':
      return 'an entitlement binding';
    case 'hub_default':
      return 'the hub-wide default';
    case 'not_enforced':
      return 'not enforced — the quota switch is off';
    case 'unlimited':
      return 'unlimited (no quota configured)';
    case '':
      return 'unknown — resolution failed';
    default:
      return source;
  }
}
