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
  GIT_REMOTE_INVALID,
  GIT_REMOTE_SSH_PORT,
  displayGitRemote,
  normalizeGitRemote,
  sanitizeGitRemote,
  stripGitURLCredentials,
  validateGitRemote,
} from './git-remote.js';

// Cases mirror pkg/hub/project_clone_test.go and pkg/util/git_test.go so the
// client and hub agree on what an override is.

describe('validateGitRemote', () => {
  it.each([
    'https://github.com/org/repo.git',
    'http://gitlab.example.com/g/r',
    'git://host.example/org/repo.git',
    'ssh://git@github.com/org/repo.git',
    'git@github.com:org/repo.git',
    'alice@git.example.com:team/repo.git',
    'github.com/org/repo',
    'git.example.com:8443/team/repo',
    'https://git.example.com:8443/team/repo.git',
  ])('accepts %s', (remote) => {
    expect(validateGitRemote(remote)).toBeNull();
  });

  it.each([
    '/home/user/code/repo',
    './repo',
    '../repo',
    '~/code/repo',
    'repo',
    'org/repo',
    'github.com',
    'https://github.com',
    'C:\\code\\repo',
    'file:///home/user/repo',
    'git@github.com',
    'alice:pw@github.com:org/repo',
    'alice@github.com:repo',
    'alice@github.com:/abs/path',
    'github.com:notaport/org/repo',
    'github.com:8443/repo',
    'localhost:8080/org/repo',
  ])('rejects %s', (remote) => {
    expect(validateGitRemote(remote)).toBe(GIT_REMOTE_INVALID);
  });

  it.each([
    'ssh://git@git.example.com:2222/group/repo.git',
    'ssh://review.example.com:29418/project/repo',
    'SSH://git:pw@git.example.com:2222/group/repo.git',
  ])('rejects ssh with a port: %s', (remote) => {
    expect(validateGitRemote(remote)).toBe(GIT_REMOTE_SSH_PORT);
  });
});

describe('stripGitURLCredentials / sanitizeGitRemote', () => {
  it.each([
    [
      'https://x-access-token:ghp_SECRET@github.com/org/repo.git',
      'https://github.com/org/repo.git',
    ],
    ['https://u:p@ss@github.com/org/repo', 'https://github.com/org/repo'],
    ['ssh://git:pw@github.com/org/repo.git', 'ssh://git@github.com/org/repo.git'],
    ['ssh://:pw@github.com/org/repo.git', 'ssh://github.com/org/repo.git'],
    ['https://github.com/org/repo@v1', 'https://github.com/org/repo@v1'],
    ['git@github.com:org/repo.git', 'git@github.com:org/repo.git'],
  ])('%s -> %s', (input, want) => {
    expect(stripGitURLCredentials(input)).toBe(want);
  });

  it('drops the query and fragment, then credentials', () => {
    expect(
      sanitizeGitRemote('  https://u:SECRET@github.com/acme/repo.git?access_token=SECRET#frag ')
    ).toBe('https://github.com/acme/repo.git');
  });
});

describe('normalizeGitRemote', () => {
  it.each([
    'https://github.com/Acme/Repo.git',
    'git@github.com:acme/repo.git',
    'ssh://git@github.com/acme/repo',
    'alice@github.com:acme/repo.git',
    'https://x-access-token:T@github.com/acme/repo.git?x=1',
    'github.com/acme/repo/',
  ])('%s names github.com/acme/repo', (remote) => {
    expect(normalizeGitRemote(remote)).toBe('github.com/acme/repo');
  });

  it('keeps a port in scheme-less and https forms', () => {
    expect(normalizeGitRemote('git.example.com:8443/team/repo')).toBe(
      'git.example.com:8443/team/repo'
    );
    expect(normalizeGitRemote('https://git.example.com:8443/team/repo.git')).toBe(
      'git.example.com:8443/team/repo'
    );
  });
});

describe('displayGitRemote', () => {
  it.each([
    ['https://x-access-token:ghp_SECRET@github.com/acme/repo.git', 'github.com/acme/repo'],
    ['https://github.com/acme/repo.git?access_token=SECRET#frag', 'github.com/acme/repo'],
    ['git@github.com:acme/repo.git', 'github.com/acme/repo'],
    ['alice@git.example.com:team/repo.git', 'git.example.com/team/repo'],
    ['ssh://git:pw@github.com/acme/repo.git', 'github.com/acme/repo'],
    ['github.com/Acme/Repo', 'github.com/Acme/Repo'],
  ])('%s -> %s', (input, want) => {
    expect(displayGitRemote(input)).toBe(want);
  });
});
