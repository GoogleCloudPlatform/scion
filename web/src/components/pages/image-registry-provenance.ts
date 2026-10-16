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
 * Text for the admin settings image_registry badge and the profiles
 * "inherited" hint, from the server's image_registry_provenance
 * (ptone/scion#3916). Pure functions, so the wording is tested on its own.
 */

/** A Runtime Broker's reported SCION_IMAGE_REGISTRY. */
export interface ImageRegistryNodeEnvOverride {
  broker_id: string;
  broker_name?: string;
  value: string;
  differs: boolean;
}

/** A profile's image_registry in effect and its settings-file value. */
export interface ImageRegistryProfileProvenance {
  source: string;
  value?: string;
  file_value?: string;
}

/** Where the image_registry in effect comes from (read-only). */
export interface ImageRegistryProvenance {
  source: string;
  value?: string;
  node_env_overrides?: ImageRegistryNodeEnvOverride[];
  profiles?: Record<string, ImageRegistryProfileProvenance>;
}

/** Koanf key of the top-level image_registry in env_overrides. */
export const IMAGE_REGISTRY_KEY = 'image_registry';

/** Human-readable label of each image_registry source. */
export const IMAGE_REGISTRY_SOURCE_LABELS: Record<string, string> = {
  node_env: 'SCION_IMAGE_REGISTRY on the node',
  hub_db_profile: 'saved hub profile value',
  hub_db: 'saved hub value',
  file_profile: 'settings-file profile value',
  file: 'settings file',
  default: 'not set',
};

/** The label of source, or the source itself when unknown. */
export function imageRegistrySourceLabel(source: string): string {
  return IMAGE_REGISTRY_SOURCE_LABELS[source] ?? source;
}

/**
 * The badge lines for the top-level image_registry field: one line for how
 * the hub-wide value is set when an environment variable is involved, and
 * one line per Runtime Broker whose SCION_IMAGE_REGISTRY overrides every
 * settings value for its agents. Empty when there is nothing to flag.
 */
export function imageRegistryBadgeLines(
  prov: ImageRegistryProvenance | null | undefined,
  envOverrides: readonly string[]
): string[] {
  const lines: string[] = [];
  const startupEnv = envOverrides.includes(IMAGE_REGISTRY_KEY);
  if (prov?.source === 'node_env') {
    lines.push(
      'Set by SCION_IMAGE_REGISTRY on the hub for hub-wide reads (setup, image pull, maintenance builds); it is never saved.'
    );
  } else if (startupEnv && prov?.source === 'hub_db') {
    lines.push(
      'A startup environment value (SCION_SERVER_IMAGEREGISTRY) is superseded by the saved value.'
    );
  } else if (startupEnv) {
    lines.push(
      'Set by SCION_SERVER_IMAGEREGISTRY at startup on this node; saving a different value replaces it.'
    );
  }
  for (const o of prov?.node_env_overrides ?? []) {
    const name = o.broker_name || o.broker_id;
    lines.push(
      `Runtime Broker ${name} sets SCION_IMAGE_REGISTRY=${o.value} for its agents${o.differs ? ' (differs from the hub value)' : ''}.`
    );
  }
  return lines;
}

/**
 * The badge line for the profiles section's env overrides of
 * profiles.<p>.image_registry (SCION_SERVER_PROFILES_<P>_IMAGEREGISTRY):
 * startup values that a saved profile value replaces, not node overrides.
 * null when no such key is overridden.
 */
export function profileImageRegistryEnvLine(envOverrides: readonly string[]): string | null {
  const names = envOverrides
    .filter((k) => k.startsWith('profiles.') && k.endsWith(`.${IMAGE_REGISTRY_KEY}`))
    .map((k) => k.slice('profiles.'.length, -`.${IMAGE_REGISTRY_KEY}`.length));
  if (names.length === 0) return null;
  return `Image registry of ${names.join(', ')} set by SCION_SERVER_PROFILES_* at startup on this node; a saved profile value replaces it.`;
}

/** Whether profile env overrides other than image_registry exist. */
export function hasOtherProfileEnvOverrides(envOverrides: readonly string[]): boolean {
  return envOverrides.some(
    (k) => (k === 'profiles' || k.startsWith('profiles.')) && !k.endsWith(`.${IMAGE_REGISTRY_KEY}`)
  );
}

/**
 * The hint under a profile's image_registry field when no value is saved
 * for the profile: the value it inherits and where that comes from. null
 * when the profile has a saved value or nothing is known.
 */
export function profileInheritedHint(
  prov: ImageRegistryProvenance | null | undefined,
  profile: string,
  savedValue: string | undefined
): string | null {
  if (savedValue) return null;
  const p = prov?.profiles?.[profile];
  if (!p) return null;
  if (!p.value) return 'Inherited: no registry (images are not rewritten).';
  return `Inherited: ${p.value} (${imageRegistrySourceLabel(p.source)}).`;
}
