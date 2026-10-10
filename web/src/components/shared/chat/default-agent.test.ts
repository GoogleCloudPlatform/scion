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

/** Tests for thread default-agent resolution (findDefaultAgent). */

import { describe, it, expect } from 'vitest';
import { findDefaultAgent } from './default-agent.js';

const AGENTS = [
  { id: 'uuid-1', slug: 'coder', name: 'Code Writer' },
  { id: 'uuid-2', slug: 'reviewer', name: 'coder-bot' },
  { id: 'uuid-3', name: 'No Slug' },
];
const nameOf = (a: { name: string }): string => a.name;

describe('findDefaultAgent', () => {
  it('matches by ID', () => {
    expect(findDefaultAgent('uuid-2', AGENTS, nameOf)?.id).toBe('uuid-2');
  });

  it('matches by slug', () => {
    expect(findDefaultAgent('reviewer', AGENTS, nameOf)?.id).toBe('uuid-2');
  });

  it('matches by display name', () => {
    expect(findDefaultAgent('No Slug', AGENTS, nameOf)?.id).toBe('uuid-3');
  });

  it('prefers a slug match over another agent whose name equals the slug', () => {
    const agents = [
      { id: 'uuid-a', slug: 'other', name: 'coder' },
      { id: 'uuid-b', slug: 'coder', name: 'Code Writer' },
    ];
    expect(findDefaultAgent('coder', agents, nameOf)?.id).toBe('uuid-b');
  });

  it('returns undefined for an empty or unknown reference', () => {
    expect(findDefaultAgent('', AGENTS, nameOf)).toBeUndefined();
    expect(findDefaultAgent('missing', AGENTS, nameOf)).toBeUndefined();
  });
});
