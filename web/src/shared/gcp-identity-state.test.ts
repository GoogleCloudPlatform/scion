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
 * The GcpIdentityState picker rules shared by the create and configure pages
 * (ptone/scion#3974), including Block on Kubernetes targets (ptone/scion#4077).
 */

import { describe, it, expect } from 'vitest';

import {
  GcpIdentityState,
  KUBERNETES_BLOCK_HINT,
  NO_IDENTITY_MODE_HINT,
} from './gcp-identity-state.js';

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

  it('offers and sends Block on a Kubernetes target, with the Kubernetes Block hint', () => {
    const g = new GcpIdentityState();
    g.setTarget(true, '');
    g.pickMode('block');
    expect(g.gcpMetadataMode).toBe('block');
    expect(g.validate()).toBeNull();
    expect(g.toRequest()).toEqual({ metadata_mode: 'block' });
    expect(g.hint).toContain(KUBERNETES_BLOCK_HINT);
    // A later target change does not touch the pick.
    g.setTarget(false, '');
    expect(g.toRequest()).toEqual({ metadata_mode: 'block' });
    expect(g.hint).not.toContain(KUBERNETES_BLOCK_HINT);
  });

  it('shows a Block project default as Block on a Kubernetes target, submittable untouched', () => {
    const g = new GcpIdentityState();
    g.setTarget(true, '');
    g.applyProjectDefaults({ defaultGCPIdentityMode: 'block' });
    expect(g.pickerBlank).toBe(false);
    expect(g.gcpMetadataMode).toBe('block');
    expect(g.validate()).toBeNull();
    expect(g.toRequest()).toBeUndefined();
  });

  it('describes Block on Kubernetes without naming the metadata endpoint', () => {
    expect(KUBERNETES_BLOCK_HINT).toContain('block service account');
    expect(KUBERNETES_BLOCK_HINT).toContain("namespace's default service account");
    expect(KUBERNETES_BLOCK_HINT).toContain('no GCP credentials');
    expect(KUBERNETES_BLOCK_HINT.toLowerCase()).not.toContain('metadata');
  });

  it('shows an untouched Assign default with its account', () => {
    const g = new GcpIdentityState();
    g.defaultGcpMetadataMode = 'assign';
    g.defaultGcpServiceAccountId = 'sa-1';
    expect(g.normalize()).toBe(true);
    expect(g).toMatchObject({ gcpMetadataMode: 'assign', gcpServiceAccountId: 'sa-1' });
    expect(g.normalize()).toBe(false);
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
