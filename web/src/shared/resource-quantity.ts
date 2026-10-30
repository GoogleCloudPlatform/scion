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
 * Resource quantity checks for the Server Config page (ptone/scion#6581).
 *
 * Mirrors config.ValidateResourceSpec in pkg/config/resource_quantities.go,
 * which the hub applies on save: errors block Save, warnings are shown but
 * allow it. A Kubernetes quantity without a unit is a byte count, so a
 * memory limit of "16" is 16 bytes.
 */

/** A resource spec as the settings carry it. */
export interface ResourceSpecInput {
  requests?: { cpu?: string; memory?: string };
  limits?: { cpu?: string; memory?: string };
  disk?: string;
}

/** One finding, at a settings path such as profiles.gke.resources.limits.memory. */
export interface QuantityIssue {
  path: string;
  message: string;
}

export interface QuantityIssues {
  errors: QuantityIssue[];
  warnings: QuantityIssue[];
}

const BINARY: Record<string, number> = {
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
};
const DECIMAL: Record<string, number> = {
  n: 1e-9,
  u: 1e-6,
  m: 1e-3,
  '': 1,
  k: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
};

// The Kubernetes quantity grammar: a signed decimal number, then a binary
// suffix, a decimal suffix or a decimal exponent.
const QUANTITY =
  /^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$/;
const UNITLESS = /^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)$/;

const MIN_BYTES = 2 ** 20; // 1Mi
const LOW_MEMORY_BYTES = 64 * 2 ** 20; // 64Mi

/**
 * Parses a Kubernetes quantity to its value (bytes for memory and disk,
 * cores for cpu), or null when it is not a quantity.
 */
export function parseQuantity(value: string): number | null {
  const m = QUANTITY.exec(value);
  if (!m) return null;
  const num = Number(m[1]);
  const suffix = m[2] ?? '';
  if (suffix in BINARY) return num * BINARY[suffix];
  if (suffix in DECIMAL) return num * DECIMAL[suffix];
  return num * 10 ** Number(suffix.slice(1));
}

function example(kind: 'cpu' | 'memory' | 'disk'): string {
  switch (kind) {
    case 'cpu':
      return 'use a number of cores or millicores, such as 2 or 500m';
    case 'disk':
      return 'use a quantity with a unit, such as 20Gi';
    default:
      return 'use a quantity with a unit, such as 16Gi or 512Mi';
  }
}

/** Writes a byte count with the largest binary unit that divides it, else rounded to Mi. */
export function suggestBinaryUnit(bytes: number): string {
  for (const [suffix, size] of [
    ['Ti', 2 ** 40],
    ['Gi', 2 ** 30],
    ['Mi', 2 ** 20],
    ['Ki', 2 ** 10],
  ] as const) {
    if (bytes >= size && bytes % size === 0) return `${bytes / size}${suffix}`;
  }
  return `${Math.round(bytes / 2 ** 20)}Mi`;
}

/** Checks one resource spec at path; see config.ValidateResourceSpec for the rules. */
export function validateResourceSpec(
  path: string,
  spec: ResourceSpecInput | undefined | null
): QuantityIssues {
  const errors: QuantityIssue[] = [];
  const warnings: QuantityIssue[] = [];
  if (!spec) return { errors, warnings };
  const fields: Array<[string, string | undefined, 'cpu' | 'memory' | 'disk']> = [
    ['requests.cpu', spec.requests?.cpu, 'cpu'],
    ['requests.memory', spec.requests?.memory, 'memory'],
    ['limits.cpu', spec.limits?.cpu, 'cpu'],
    ['limits.memory', spec.limits?.memory, 'memory'],
    ['disk', spec.disk, 'disk'],
  ];
  const parsed: Record<string, number> = {};
  for (const [key, value, kind] of fields) {
    if (!value) continue;
    const p = `${path}.${key}`;
    const q = parseQuantity(value);
    if (q === null) {
      errors.push({
        path: p,
        message: `"${value}" is not a Kubernetes quantity; ${example(kind)}`,
      });
      continue;
    }
    if (q <= 0) {
      errors.push({ path: p, message: `"${value}" must be greater than zero; ${example(kind)}` });
      continue;
    }
    if (kind === 'cpu') {
      parsed[key] = q;
      continue;
    }
    const unitless = UNITLESS.test(value);
    if (q < MIN_BYTES) {
      const msg = unitless
        ? `"${value}" has no unit, so it means ${value} bytes, below the minimum of 1Mi`
        : `"${value}" is below the minimum of 1Mi`;
      errors.push({ path: p, message: `${msg}; ${example(kind)}` });
      continue;
    }
    parsed[key] = q;
    if (unitless) {
      warnings.push({
        path: p,
        message: `"${value}" has no unit, so it is a number of bytes; write it with a unit, for example ${suggestBinaryUnit(q)}`,
      });
    }
    if (kind === 'memory' && q < LOW_MEMORY_BYTES) {
      warnings.push({
        path: p,
        message: `"${value}" is below 64Mi, too little memory for an agent; did you mean a larger unit, such as 16Gi?`,
      });
    }
  }
  for (const r of ['cpu', 'memory']) {
    const req = parsed[`requests.${r}`];
    const lim = parsed[`limits.${r}`];
    if (req !== undefined && lim !== undefined && req > lim) {
      errors.push({
        path: `${path}.requests.${r}`,
        message: `request "${(r === 'cpu' ? spec.requests?.cpu : spec.requests?.memory) ?? ''}" is above the limit "${(r === 'cpu' ? spec.limits?.cpu : spec.limits?.memory) ?? ''}"; a request must not exceed its limit`,
      });
    }
  }
  return { errors, warnings };
}

/** Messages for one field path, errors first. */
export function issuesAt(
  issues: QuantityIssues,
  path: string
): { errors: string[]; warnings: string[] } {
  return {
    errors: issues.errors.filter((i) => i.path === path).map((i) => i.message),
    warnings: issues.warnings.filter((i) => i.path === path).map((i) => i.message),
  };
}
