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
 * Environment values of env-overridden settings keys, as the server-config
 * GET reports them in env_values: the value a SCION_SERVER_* variable gives
 * the key on the node that served the page (masked like the rest of the
 * body), and whether the hub runs with it.
 */
export interface EnvKeyValue {
  /** The environment value, or the masked placeholder when masked. */
  value?: unknown;
  /** True when value is the masked placeholder (a secret, or not shown). */
  masked?: boolean;
  /** True when the hub on this node runs with the environment value. */
  in_effect: boolean;
}

/** The placeholder the server sends for a secret (maskedValue in pkg/hub). */
export const MASKED_ENV_VALUE = '********';

/**
 * The environment value as text: a list joined with ", " (as the
 * comma-separated fields show it), a scalar as is, an object as JSON. A
 * masked value is always the placeholder, whatever the server sent.
 */
export function formatEnvValue(ev: EnvKeyValue): string {
  if (ev.masked) return MASKED_ENV_VALUE;
  const v = ev.value;
  if (v === undefined || v === null) return '';
  if (Array.isArray(v)) return v.map((x) => (typeof x === 'string' ? x : JSON.stringify(x))).join(', ');
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

/** The first of keys that env_values reports, with its entry. */
export function envValueFor(
  envValues: Record<string, EnvKeyValue> | null | undefined,
  keys: readonly string[]
): { key: string; ev: EnvKeyValue } | null {
  if (!envValues) return null;
  for (const key of keys) {
    const ev = envValues[key];
    if (ev) return { key, ev };
  }
  return null;
}

/** The env_values entries at prefix or below it, sorted by key. */
export function envValuesUnder(
  envValues: Record<string, EnvKeyValue> | null | undefined,
  prefix: string
): Array<{ key: string; ev: EnvKeyValue }> {
  if (!envValues) return [];
  return Object.keys(envValues)
    .filter((k) => k === prefix || k.startsWith(`${prefix}.`))
    .sort()
    .map((key) => ({ key, ev: envValues[key] }));
}
