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
  hasOtherProfileEnvOverrides,
  imageRegistryBadgeLines,
  profileImageRegistryEnvLine,
  profileInheritedHint,
} from './image-registry-provenance.js';

describe('image_registry badge lines', () => {
  it('flags nothing for a file value with no env', () => {
    expect(imageRegistryBadgeLines({ source: 'file', value: 'f.example' }, [])).toEqual([]);
    expect(imageRegistryBadgeLines(null, [])).toEqual([]);
  });

  it('names the startup env when it sets the value', () => {
    const lines = imageRegistryBadgeLines({ source: 'file', value: 's.example' }, [
      'image_registry',
    ]);
    expect(lines).toHaveLength(1);
    expect(lines[0]).toContain('SCION_SERVER_IMAGEREGISTRY at startup');
  });

  it('says a saved value supersedes the startup env', () => {
    const lines = imageRegistryBadgeLines({ source: 'hub_db', value: 'db.example' }, [
      'image_registry',
    ]);
    expect(lines).toEqual([
      'A startup environment value (SCION_SERVER_IMAGEREGISTRY) is superseded by the saved value.',
    ]);
  });

  it('names the hub node env, which is never saved', () => {
    const lines = imageRegistryBadgeLines({ source: 'node_env', value: 'n.example' }, []);
    expect(lines).toHaveLength(1);
    expect(lines[0]).toContain('SCION_IMAGE_REGISTRY on the hub');
    expect(lines[0]).toContain('never saved');
  });

  it('also explains the startup value the field shows when the node env wins', () => {
    const lines = imageRegistryBadgeLines({ source: 'node_env', value: 'n.example' }, [
      'image_registry',
    ]);
    expect(lines).toHaveLength(2);
    expect(lines[1]).toContain('SCION_SERVER_IMAGEREGISTRY');
  });

  it('lists every broker node env, marking the ones that differ', () => {
    const lines = imageRegistryBadgeLines(
      {
        source: 'hub_db',
        value: 'db.example',
        node_env_overrides: [
          { broker_id: 'b1', broker_name: 'gpu-pool', value: 'n.example', differs: true },
          { broker_id: 'b2', value: 'db.example', differs: false },
        ],
      },
      []
    );
    expect(lines).toEqual([
      'Runtime Broker gpu-pool sets SCION_IMAGE_REGISTRY=n.example for its agents (differs from the hub value).',
      'Runtime Broker b2 sets SCION_IMAGE_REGISTRY=db.example for its agents.',
    ]);
  });
});

describe('profiles section env line', () => {
  it('describes profile image_registry env values as startup values', () => {
    const env = ['profiles.k8s.image_registry', 'profiles.gke.image_registry'];
    expect(profileImageRegistryEnvLine(env)).toContain('k8s, gke');
    expect(profileImageRegistryEnvLine(env)).toContain('a saved profile value replaces it');
    expect(hasOtherProfileEnvOverrides(env)).toBe(false);
  });

  it('keeps the generic badge for other profile env keys', () => {
    const env = ['profiles.k8s.runtime', 'profiles.k8s.image_registry'];
    expect(hasOtherProfileEnvOverrides(env)).toBe(true);
    expect(profileImageRegistryEnvLine(['profiles.k8s.runtime'])).toBeNull();
  });
});

describe('profile inherited hint', () => {
  const prov = {
    source: 'hub_db',
    value: 'db.example',
    profiles: {
      k8s: { source: 'hub_db', value: 'db.example', file_value: 'k8s-file.example' },
      bare: { source: 'default' },
    },
  };

  it('shows what an unsaved profile inherits and from where', () => {
    expect(profileInheritedHint(prov, 'k8s', '')).toBe('Inherited: db.example (saved hub value).');
    expect(profileInheritedHint(prov, 'bare', undefined)).toBe(
      'Inherited: no registry (images are not rewritten).'
    );
  });

  it("uses the inherited value, which leaves out the profile's own saved value", () => {
    const withSaved = {
      source: 'hub_db',
      profiles: {
        k8s: {
          source: 'hub_db_profile',
          value: 'saved-k8s.example',
          inherited_source: 'file_profile',
          inherited_value: 'k8s-file.example',
        },
      },
    };
    // The admin cleared the saved value in the editor: the hint shows what
    // the profile falls back to, not the value about to be removed.
    expect(profileInheritedHint(withSaved, 'k8s', '')).toBe(
      'Inherited: k8s-file.example (settings-file profile value).'
    );
  });

  it('shows nothing for a saved value or an unknown profile', () => {
    expect(profileInheritedHint(prov, 'k8s', 'mine.example')).toBeNull();
    expect(profileInheritedHint(prov, 'missing', '')).toBeNull();
    expect(profileInheritedHint(null, 'k8s', '')).toBeNull();
  });
});
