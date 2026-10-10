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
 * normalizeGcpModeForTarget, the single implementation of the Kubernetes
 * Block rule for the create and configure pages (ptone/scion#3974), and the
 * GcpIdentityState picker rules.
 */

import { describe, it, expect } from 'vitest';

import {
  GcpIdentityState,
  NO_IDENTITY_MODE_HINT,
  normalizeGcpModeForTarget,
  type GcpModeFields,
} from './gcp-identity-state.js';

function fields(over: Partial<GcpModeFields> = {}): GcpModeFields {
  return {
    gcpMetadataMode: 'block',
    gcpServiceAccountId: '',
    gcpIdentityUserSet: false,
    gcpUserBlockSuspended: false,
    defaultGcpMetadataMode: 'block',
    defaultGcpServiceAccountId: '',
    ...over,
  };
}

describe('normalizeGcpModeForTarget', () => {
  it('never rewrites a stored value (edit mode)', () => {
    const s = fields({ gcpMetadataMode: 'block' });
    expect(normalizeGcpModeForTarget(s, true, true)).toBe(false);
    expect(s).toEqual(fields({ gcpMetadataMode: 'block' }));
  });

  it('shows a Block default as Passthrough on a Kubernetes target, without making it a choice', () => {
    const s = fields();
    expect(normalizeGcpModeForTarget(s, true)).toBe(true);
    expect(s.gcpMetadataMode).toBe('passthrough');
    expect(s.gcpIdentityUserSet).toBe(false);
  });

  it('suspends an explicit Block on a Kubernetes target and reinstates it after', () => {
    const s = fields({ gcpIdentityUserSet: true });
    normalizeGcpModeForTarget(s, true);
    expect(s).toMatchObject({
      gcpMetadataMode: 'passthrough',
      gcpIdentityUserSet: false,
      gcpUserBlockSuspended: true,
    });
    normalizeGcpModeForTarget(s, false);
    expect(s).toMatchObject({
      gcpMetadataMode: 'block',
      gcpIdentityUserSet: true,
      gcpUserBlockSuspended: false,
    });
  });

  it('leaves an explicit non-Block choice alone', () => {
    const s = fields({ gcpMetadataMode: 'passthrough', gcpIdentityUserSet: true });
    expect(normalizeGcpModeForTarget(s, true)).toBe(false);
    expect(s.gcpMetadataMode).toBe('passthrough');
  });

  it('keeps the account in step with an Assign default', () => {
    const s = fields({
      gcpMetadataMode: 'passthrough',
      defaultGcpMetadataMode: 'assign',
      defaultGcpServiceAccountId: 'sa-1',
    });
    normalizeGcpModeForTarget(s, false);
    expect(s).toMatchObject({ gcpMetadataMode: 'assign', gcpServiceAccountId: 'sa-1' });
  });

  it('is idempotent', () => {
    const s = fields();
    normalizeGcpModeForTarget(s, true);
    expect(normalizeGcpModeForTarget(s, true)).toBe(false);
  });
});

describe('GcpIdentityState', () => {
  it('starts blank with the no-mode hint on every target, and nothing chosen is submittable', () => {
    for (const k8s of [false, true]) {
      const g = new GcpIdentityState();
      g.setTarget(k8s, '');
      expect(g.pickerBlank).toBe(true);
      expect(g.hint.startsWith(NO_IDENTITY_MODE_HINT)).toBe(true);
      expect(g.validate()).toBeNull();
      expect(g.toRequest()).toBeUndefined();
    }
  });

  it('counts any pick as a choice, including Block', () => {
    const g = new GcpIdentityState();
    g.pickMode('block');
    expect(g.gcpIdentityUserSet).toBe(true);
    expect(g.toRequest()).toEqual({ metadata_mode: 'block' });
  });

  it('never lets an arriving project default overwrite a pick made while it loaded', () => {
    const g = new GcpIdentityState();
    g.pickMode('block');
    g.applyProjectDefaults({ defaultGCPIdentityMode: 'passthrough' });
    expect(g.gcpMetadataMode).toBe('block');
    expect(g.defaultGcpMetadataMode).toBe('passthrough');
  });

  it('blocks submit for a Block project default on a Kubernetes target until a pick', () => {
    const g = new GcpIdentityState();
    g.setTarget(true, '');
    g.applyProjectDefaults({ defaultGCPIdentityMode: 'block' });
    expect(g.blockDefaultNeedsExplicitChoice).toBe(true);
    expect(g.validate()).toContain('Choose Passthrough');
    g.pickMode('passthrough');
    expect(g.validate()).toBeNull();
  });

  it('notifies subscribers on change, until unsubscribed', () => {
    const g = new GcpIdentityState();
    let n = 0;
    const off = g.subscribe(() => n++);
    g.pickMode('passthrough');
    expect(n).toBe(1);
    off();
    g.pickMode('block');
    expect(n).toBe(1);
  });
});
