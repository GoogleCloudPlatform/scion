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
 * Client-side validation and serialization for create-time agent secrets
 * (behind hub.agent_secrets, ptone/scion#8745).
 *
 * Mirrors validateCreateAgentSecrets in pkg/hub/agent_create_secrets.go and
 * ReservedEnvPrefixes in pkg/api/member_env.go so obvious input mistakes are
 * caught before submit without storing or logging secret values.
 */

import type { AgentSecretInput, InjectionMode, SecretType } from './types.js';

/** Maximum number of secrets accepted in a single CreateAgentRequest. */
export const MAX_CREATE_AGENT_SECRETS = 50;

/** Maximum byte length of a single secret value (64 KiB). */
export const MAX_CREATE_AGENT_SECRET_VALUE_BYTES = 64 * 1024;

/** Control-plane environment variable prefixes reserved by Scion. */
export const RESERVED_ENV_PREFIXES: readonly string[] = ['SCION_', 'GCE_METADATA_'];

const SYSTEMONE_RESERVED_PREFIX = 'systemone.instance.';
const WEBHOOK_ENDPOINT_RESERVED_PREFIX = 'webhook_endpoint.';
const WEBHOOK_ENDPOINT_SANITIZED_PREFIX = 'webhook_endpoint-';

/** One editable row in the create-agent Secrets tab. Held in memory only. */
export interface AgentSecretDraftRow {
  key: string;
  value: string;
  type: SecretType;
  target: string;
  injectionMode: InjectionMode;
  revealed: boolean;
  /** Transient UI-only flag: true while the file textarea has focus. */
  editing?: boolean;
}

/** Creates a blank secret row with the hub's defaults (environment, as_needed). */
export function emptyAgentSecretRow(): AgentSecretDraftRow {
  return {
    key: '',
    value: '',
    type: 'environment',
    target: '',
    injectionMode: 'as_needed',
    revealed: false,
    editing: false,
  };
}

/** True when a row has no key, value, or target entered. */
export function isAgentSecretRowEmpty(row: AgentSecretDraftRow): boolean {
  return row.key.trim() === '' && row.value === '' && row.target.trim() === '';
}

/** Reports whether an environment-type target starts with a reserved prefix. */
export function isReservedEnvTarget(target: string): boolean {
  return RESERVED_ENV_PREFIXES.some((prefix) => target.startsWith(prefix));
}

/**
 * Reports a human-readable error if key falls under a hub-reserved secret
 * prefix (System One or webhook endpoint signing secrets), or null otherwise.
 */
export function reservedSecretKeyError(key: string): string | null {
  const sanitized = key.replace(/[^a-zA-Z0-9_-]/g, '-').toLowerCase();
  const systemOneSanitized = sanitized.replace(/_/g, '-');
  if (
    key.toLowerCase().startsWith(SYSTEMONE_RESERVED_PREFIX) ||
    systemOneSanitized.startsWith('systemone-instance-')
  ) {
    return `Secret key "${key}" uses reserved prefix "${SYSTEMONE_RESERVED_PREFIX}".`;
  }
  if (
    key.toLowerCase().startsWith(WEBHOOK_ENDPOINT_RESERVED_PREFIX) ||
    sanitized.startsWith(WEBHOOK_ENDPOINT_SANITIZED_PREFIX)
  ) {
    return `Secret key "${key}" uses reserved prefix "${WEBHOOK_ENDPOINT_RESERVED_PREFIX}".`;
  }
  return null;
}

const VALID_TYPES: ReadonlySet<string> = new Set(['environment', 'file', 'variable']);
const VALID_INJECTION_MODES: ReadonlySet<string> = new Set(['as_needed', 'always']);

/**
 * Validates the create-time secret rows against the hub's rules.
 * Completely blank rows are ignored so an unused empty row does not block submit.
 */
export function validateAgentSecretRows(rows: readonly AgentSecretDraftRow[]): string[] {
  const active = rows.filter((r) => !isAgentSecretRowEmpty(r));
  if (active.length === 0) return [];

  const errors: string[] = [];
  if (active.length > MAX_CREATE_AGENT_SECRETS) {
    errors.push(`At most ${MAX_CREATE_AGENT_SECRETS} secrets may be provided at agent creation.`);
  }

  const seen = new Set<string>();
  const encoder = new TextEncoder();

  for (const row of active) {
    const key = row.key.trim();
    if (!key) {
      errors.push('Secret key is required.');
      continue;
    }
    if (/[= \t\n\r]/.test(key)) {
      errors.push(`Secret key "${key}" cannot contain whitespace or '='.`);
      continue;
    }
    if (seen.has(key)) {
      errors.push(`Duplicate secret key: ${key}.`);
      continue;
    }
    seen.add(key);

    const reservedKeyErr = reservedSecretKeyError(key);
    if (reservedKeyErr) {
      errors.push(reservedKeyErr);
      continue;
    }

    if (row.value === '') {
      errors.push(`Secret value is required for key ${key}.`);
    } else if (encoder.encode(row.value).length > MAX_CREATE_AGENT_SECRET_VALUE_BYTES) {
      errors.push(`Secret value for key ${key} exceeds 64KB limit.`);
    }

    const secretType = row.type || 'environment';
    if (!VALID_TYPES.has(secretType)) {
      errors.push(`Secret type for key ${key} must be environment, variable, or file.`);
    }

    const target = row.target.trim();
    const effectiveTarget = target || key;

    if (secretType === 'environment' && isReservedEnvTarget(effectiveTarget)) {
      errors.push(
        `Secret target "${effectiveTarget}" is reserved for scion's own control-plane environment variables.`
      );
    }

    if (secretType === 'file') {
      if (!target) {
        errors.push(
          `File secret "${key}" requires a target path (absolute path or starting with ~/).`
        );
      } else if (target.includes('..')) {
        errors.push(`File secret "${key}" target path must not contain '..'.`);
      } else if (!target.startsWith('/') && !target.startsWith('~/')) {
        errors.push(`File secret "${key}" target must be an absolute path (or start with ~/).`);
      }
    }

    if (row.injectionMode && !VALID_INJECTION_MODES.has(row.injectionMode)) {
      errors.push(`Injection mode for key ${key} must be "always" or "as_needed".`);
    }
  }

  return errors;
}

/**
 * Converts non-empty draft rows into CreateAgentRequest.secrets payloads.
 * Returns an empty array when no secrets were entered so callers omit `secrets`.
 */
export function toCreateAgentSecrets(rows: readonly AgentSecretDraftRow[]): AgentSecretInput[] {
  const out: AgentSecretInput[] = [];
  for (const row of rows) {
    if (isAgentSecretRowEmpty(row)) continue;
    const key = row.key.trim();
    if (!key) continue;
    const item: AgentSecretInput = {
      key,
      value: row.value,
      type: row.type || 'environment',
      injectionMode: row.injectionMode || 'as_needed',
    };
    const target = row.target.trim();
    if (target) {
      item.target = target;
    }
    out.push(item);
  }
  return out;
}
