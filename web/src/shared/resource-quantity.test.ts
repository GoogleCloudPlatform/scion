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

import { describe, expect, it } from 'vitest';
import { parseQuantity, suggestBinaryUnit, validateResourceSpec } from './resource-quantity.js';

// Mirrors TestValidateResourceSpec in pkg/config/resource_quantities_test.go.
describe('validateResourceSpec', () => {
  const p = 'profiles.gke.resources';
  const cases: Array<[string, Parameters<typeof validateResourceSpec>[1], string, string]> = [
    ['empty', {}, '', ''],
    [
      'typical',
      { requests: { cpu: '7', memory: '8Gi' }, limits: { cpu: '8', memory: '16Gi' }, disk: '20Gi' },
      '',
      '',
    ],
    ['millicores', { requests: { cpu: '500m' } }, '', ''],
    ['unitless memory 16', { limits: { memory: '16' } }, `${p}.limits.memory`, ''],
    ['memory below 1Mi with unit', { requests: { memory: '512Ki' } }, `${p}.requests.memory`, ''],
    ['disk below 1Mi', { disk: '100' }, `${p}.disk`, ''],
    ['not a quantity', { limits: { memory: '16GB' } }, `${p}.limits.memory`, ''],
    ['cpu not a quantity', { requests: { cpu: 'two' } }, `${p}.requests.cpu`, ''],
    ['zero cpu', { limits: { cpu: '0' } }, `${p}.limits.cpu`, ''],
    ['negative memory', { limits: { memory: '-1Gi' } }, `${p}.limits.memory`, ''],
    [
      'cpu request above limit',
      { requests: { cpu: '8' }, limits: { cpu: '7' } },
      `${p}.requests.cpu`,
      '',
    ],
    [
      'memory request above limit',
      { requests: { memory: '2Gi' }, limits: { memory: '1Gi' } },
      `${p}.requests.memory`,
      '',
    ],
    [
      'request above limit across units',
      { requests: { memory: '1G' }, limits: { memory: '900Mi' } },
      `${p}.requests.memory`,
      '',
    ],
    ['unitless byte count warns', { limits: { memory: '2147483648' } }, '', '2Gi'],
    ['unitless disk warns', { disk: '10737418240' }, '', '10Gi'],
    ['exactly 1Mi warns low', { limits: { memory: '1Mi' } }, '', 'below 64Mi'],
    ['low memory warns', { requests: { memory: '32Mi' } }, '', 'below 64Mi'],
    ['64Mi no warning', { requests: { memory: '64Mi' } }, '', ''],
    ['low disk no low-memory warning', { disk: '2Mi' }, '', ''],
    ['exponent is a unit', { limits: { memory: '2e9' } }, '', ''],
    ['unitless cpu fine', { limits: { cpu: '16' } }, '', ''],
  ];
  for (const [name, spec, wantErr, wantWarn] of cases) {
    it(name, () => {
      const { errors, warnings } = validateResourceSpec(p, spec);
      if (wantErr) {
        expect(errors.map((e) => e.path)).toEqual([wantErr]);
      } else {
        expect(errors).toEqual([]);
      }
      if (wantWarn) {
        expect(warnings.some((w) => w.message.includes(wantWarn))).toBe(true);
      } else {
        expect(warnings).toEqual([]);
      }
    });
  }

  it('names the byte meaning and an example for "16"', () => {
    const { errors } = validateResourceSpec(p, { limits: { memory: '16' } });
    expect(errors[0].message).toContain('"16"');
    expect(errors[0].message).toContain('16 bytes');
    expect(errors[0].message).toContain('16Gi');
  });
});

describe('parseQuantity', () => {
  it('parses the Kubernetes suffixes', () => {
    expect(parseQuantity('1Ki')).toBe(1024);
    expect(parseQuantity('1.5Gi')).toBe(1.5 * 2 ** 30);
    expect(parseQuantity('500m')).toBe(0.5);
    expect(parseQuantity('1k')).toBe(1000);
    expect(parseQuantity('2e3')).toBe(2000);
    expect(parseQuantity('16')).toBe(16);
  });
  it('rejects what Kubernetes rejects', () => {
    for (const v of ['16GB', 'Gi', '1 Gi', '', 'abc', '1.2.3']) {
      expect(parseQuantity(v)).toBeNull();
    }
  });
});

describe('suggestBinaryUnit', () => {
  it('uses the largest exact unit, else rounds to Mi', () => {
    expect(suggestBinaryUnit(2 ** 31)).toBe('2Gi');
    expect(suggestBinaryUnit(3 * 2 ** 20)).toBe('3Mi');
    expect(suggestBinaryUnit(1500000)).toBe('1Mi');
  });
});
