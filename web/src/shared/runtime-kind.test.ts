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

import { describe, it, expect } from 'vitest';
import {
  isKubernetesRuntimeType,
  isBrokerKubernetesOnly,
  isTargetKubernetesOnly,
} from './runtime-kind.js';

describe('isKubernetesRuntimeType', () => {
  it('accepts every spelling the runtime factory treats as Kubernetes', () => {
    expect(isKubernetesRuntimeType('kubernetes')).toBe(true);
    expect(isKubernetesRuntimeType('k8s')).toBe(true);
    expect(isKubernetesRuntimeType('remote')).toBe(true);
  });

  it('rejects other runtime types and empty input', () => {
    expect(isKubernetesRuntimeType('docker')).toBe(false);
    expect(isKubernetesRuntimeType('podman')).toBe(false);
    expect(isKubernetesRuntimeType('apple')).toBe(false);
    expect(isKubernetesRuntimeType(undefined)).toBe(false);
    expect(isKubernetesRuntimeType('')).toBe(false);
  });
});

describe('isBrokerKubernetesOnly', () => {
  it('is true only when every profile is Kubernetes, under any accepted spelling', () => {
    expect(
      isBrokerKubernetesOnly({ profiles: [{ name: 'a', type: 'kubernetes', available: true }] })
    ).toBe(true);
    expect(
      isBrokerKubernetesOnly({ profiles: [{ name: 'a', type: 'k8s', available: true }] })
    ).toBe(true);
    expect(
      isBrokerKubernetesOnly({ profiles: [{ name: 'a', type: 'remote', available: true }] })
    ).toBe(true);
  });

  it('is false for a mix of runtime types', () => {
    expect(
      isBrokerKubernetesOnly({
        profiles: [
          { name: 'a', type: 'kubernetes', available: true },
          { name: 'b', type: 'docker', available: true },
        ],
      })
    ).toBe(false);
  });

  it('is false for a broker with no profiles, rather than guessing', () => {
    expect(isBrokerKubernetesOnly({ profiles: [] })).toBe(false);
    expect(isBrokerKubernetesOnly({})).toBe(false);
    expect(isBrokerKubernetesOnly(undefined)).toBe(false);
  });
});

describe('isTargetKubernetesOnly', () => {
  const mixedBroker = {
    profiles: [
      { name: 'k8s-profile', type: 'kubernetes', available: true },
      { name: 'docker-profile', type: 'docker', available: true },
    ],
  };

  it('is false with no broker', () => {
    expect(isTargetKubernetesOnly(undefined, '')).toBe(false);
  });

  it('decides by the chosen profile when one is picked, even on a mixed broker', () => {
    expect(isTargetKubernetesOnly(mixedBroker, 'k8s-profile')).toBe(true);
    expect(isTargetKubernetesOnly(mixedBroker, 'docker-profile')).toBe(false);
  });

  it('is false for an unknown profile name', () => {
    expect(isTargetKubernetesOnly(mixedBroker, 'no-such-profile')).toBe(false);
  });

  it('does not guess a mixed broker with no profile chosen', () => {
    expect(isTargetKubernetesOnly(mixedBroker, '')).toBe(false);
  });

  it('is true for a single kubernetes-only available profile with none chosen', () => {
    expect(
      isTargetKubernetesOnly(
        { profiles: [{ name: 'default', type: 'kubernetes', available: true }] },
        ''
      )
    ).toBe(true);
  });

  it('ignores unavailable profiles when none is chosen', () => {
    expect(
      isTargetKubernetesOnly(
        {
          profiles: [{ name: 'default', type: 'kubernetes', available: false }],
        },
        ''
      )
    ).toBe(false);
  });
});
