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

/** Presentation fixtures for the Runtime target card tests. */

import type { RuntimeTargetPresentation } from '../types.js';

/** A presentation with every FC4-a field set. */
export function fullPresentation(): RuntimeTargetPresentation {
  return {
    schemaVersion: 1,
    key: 'k8s-nfs-1',
    name: 'k8s nfs one',
    target: {
      type: 'kubernetes',
      displayName: 'Cluster A',
      namespace: 'agents',
      gke: true,
      priorityClassName: 'agents-high',
      sharedDirStorageClass: 'standard-rwx',
      sharedDirSize: '40Gi',
      safeToEvict: false,
      sharedDirStorageBackend: 'nfs',
      sharedDirStorageBackends: { data: 'local', cache: 'nfs' },
      homeStorageBackend: 'nfs',
    },
    agentDefaults: {
      defaultHarnessConfig: 'claude',
      imageRegistry: 'registry.example/team',
      cloneDepth: 'full',
      resources: {
        requests: { cpu: '1', memory: '2Gi' },
        limits: { cpu: '2', memory: '4Gi' },
        disk: '20Gi',
      },
      volumes: [{ target: '/mnt/a', readOnly: true, type: 'nfs' }],
      secrets: [{ key: 'API_KEY', type: 'file', description: 'the API key' }],
      harnessOverrides: {
        gemini: { envKeys: ['OTHER'] },
        claude: {
          image: 'img:1',
          imagePullPolicy: 'Never',
          authSelectedType: 'vertex-ai',
          user: 'agent',
          envKeys: ['EMPTY', 'TOKEN'],
          volumes: [{ target: '/mnt/b', readOnly: true, type: 'nfs' }],
          resources: { limits: { memory: '8Gi' } },
        },
      },
    },
  };
}

/**
 * fullPresentation plus every field FC4-a never publishes, with "NEVER-"
 * values, as a misbehaving or newer Hub might send them. TypeScript does not
 * strip unknown keys from JSON, so a renderer must ignore them by design.
 */
export function presentationWithNeverFields(): RuntimeTargetPresentation {
  const p = fullPresentation() as unknown as Record<string, Record<string, unknown>>;
  Object.assign(p.target, {
    context: 'NEVER-context',
    kubeconfig: '/NEVER-kubeconfig',
    kubernetesServiceAccountMappings: { 'NEVER-gsa': 'NEVER-ksa' },
    kubernetesBlockServiceAccount: 'NEVER-block-sa',
    homeStorageLeaf: 'NEVER-leaf',
  });
  const d = p.agentDefaults;
  const vols = d.volumes as Record<string, unknown>[];
  Object.assign(vols[0], {
    source: 'NEVER-source',
    bucket: 'NEVER-bucket',
    prefix: 'NEVER-prefix',
    server: 'NEVER-server',
    mode: 'NEVER-mode',
    volumeName: 'NEVER-volume-name',
  });
  const secrets = d.secrets as Record<string, unknown>[];
  Object.assign(secrets[0], { target: 'NEVER-target', alternativeEnvKeys: ['NEVER-ALT'] });
  const ho = d.harnessOverrides as Record<string, Record<string, unknown>>;
  ho.claude.env = { TOKEN: 'NEVER-env-value' };
  return p as unknown as RuntimeTargetPresentation;
}
