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
  GIT_REMOTE_TLS_PORT,
  displayGitRemote,
  normalizeGitRemote,
  sanitizeGitRemote,
  stripGitURLCredentials,
  stripQueryAndFragment,
  trimRemote,
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
    'git@gitserver:org/repo',
    'https://gitserver/org/repo.git',
    'https://u:p@ss@github.com/org/repo.git',
    'https://[::1]/org/repo',
    'https://[2001:db8::1]/org/repo',
    'https://[::ffff:192.0.2.1]/org/repo',
    // Dotted names are not dot segments; one trailing '/' is fine; punycode.
    'https://xn--bcher-kva.example/org/.github',
    'git@git.example.com:team/my..repo/',
    'http://git.example.com:80/team/repo',
    'git.example.com:443/team/repo',
    'https://dev.azure.com/org/My%20Project/_git/repo',
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
    'github.com:org/repo',
    'https://u:SECRET_P/w@github.com/org/repo',
    'https://u:SECRET_P/w@x@github.com/org/repo.git',
    'ssh://git:SECRET_P/w@github.com/org/repo.git',
    // Port then '@' in the path, and port-like passwords (GCP#2368 review).
    'https://git.example.com:8443/org/repo@v1',
    'https://github.com:443/org/repo@v1',
    'https://[::1]:8443/org/repo@v1',
    'https://u:SECRET_P@git.example.com:8443/org/repo@v1',
    'https://u:8443/SECRET_P@github.com/org/repo',
    'https://u:0123/SECRET_P@github.com/org/repo',
    'https://u:/SECRET_P@github.com/org/repo',
    'https://github.com/org/x@evil.example/repo',
    'https://bad_host/org/repo',
    'https://[::1/org/repo',
    // '@' in the path must not swap the repository.
    'https://github.com/org/repo@github.com/x',
    'https://u:SECRET_P@github.com/org/x@github.com/repo',
    'https://a/b@github.com/x',
    'git@github.com:org/repo@github.com/x',
    'github.com/org/repo@github.com/x',
    // Whitespace and control characters, in every form.
    'github.com/org/repo\nX',
    'git@github.com:org/repo\nX',
    'https://github.com/org/my repo',
    'https://github.com/org/repo\tx',
    'ssh://git@github.com/org/re\u0000po',
    'https://github.com/org/r\u00a0epo',
    // Loose ports and hosts.
    'https://u:SECRET_P@github.com:/org/repo',
    'https://github.com:0443/org/repo',
    'https://github.com:0/org/repo',
    'https://github.com:65536/org/repo',
    'git.example.com:0443/team/repo',
    'https://-x.com/org/repo',
    'https://x-.example.com/org/repo',
    'git@-gitserver:org/repo',
    '-x.example.com/org/repo',
    // Dot and empty path segments, escaped or not, in every form (r5).
    'https://github.com/org/../evil/repo',
    'https://github.com/org/%2e%2e/evil/repo',
    'https://github.com/org/%2E%2E/evil/repo',
    'https://github.com/./org/repo',
    'https://github.com//org/repo',
    'https://github.com/org//repo',
    'https://github.com/org/repo//',
    'ssh://git@github.com/org/../evil/repo.git',
    'git://github.com/org/./repo',
    'github.com/org/../evil/repo',
    'github.com/org/%2e%2e/evil/repo',
    'github.com//org/repo',
    'github.com/org//repo',
    'git@github.com:org/../evil/repo',
    'git@github.com:org/%2e%2e/evil/repo',
    'git@github.com:org//repo',
    'git@github.com:./org/repo',
    // Printable ASCII only (r5); escaped controls.
    'https://github.com/org/\u202erepo',
    'github.com/org/\u202erepo',
    'git@github.com:org/\u202erepo',
    'https://github.com/\u043erg/repo',
    'https://b\u00fccher.example/org/repo',
    'https://github.com/org/re%0Apo',
    'https://github.com/org/re%00po',
    'github.com/org/re%7Fpo',
    'git@github.com:org/re%1Fpo',
    // Hub parity (#2713 r4 F1): %40 in the path, malformed escapes,
    // invalid IPv6, userinfo characters net/url rejects.
    'https://github.com/org/%40evil/repo',
    'github.com/org/re%40po/x',
    'git@github.com:org/%40x/repo',
    'https://github.com/org/re%zzpo',
    'https://github.com/org/repo%',
    'github.com/org/re%zpo',
    'git@github.com:org/repo%',
    'https://u:SECRET_%zz@github.com/org/repo',
    // %2F inside a segment, in every form (r6 R1).
    'https://github.com/a/o%2Fr',
    'https://github.com/org/o%2fr',
    'github.com/a/o%2Fr',
    'git@github.com:a/o%2Fr',
    // Characters outside the RFC 3986 path set, in every form (r6 R2).
    'https://github.com/org/r\\x',
    'github.com/org/r\\x',
    'git@github.com:org/r\\x',
    'https://github.com/org/r%5Cx',
    'https://github.com/org/re"po',
    'https://github.com/org/re|po',
    'https://github.com/org/re^po',
    'https://github.com/org/re`po',
    'https://github.com/org/[repo]',
    'git@github.com:org/re{po}',
    'github.com/org/re<po>',
    'https://[1:2]/org/repo',
    'https://[:::]/org/repo',
    'https://[v1.x]/org/repo',
    'https://[1:2:3:4:5:6:7::8]/org/repo',
    'https://us"er@h.example/o/r',
    'https://h.com\\@evil.com/o/r',
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

  it.each([
    'git://git.example.com:9418/group/repo.git',
    'GIT://git.example.com:9419/group/repo',
    'http://git.example.com:8080/group/repo',
    'http://u:SECRET_P@git.example.com:443/group/repo',
    // The scheme-less form's clone-url is https (r6 R3).
    'git.example.com:80/group/repo',
  ])('rejects git:// with a port and http:// with a non-80 port: %s', (remote) => {
    expect(validateGitRemote(remote)).toBe(GIT_REMOTE_TLS_PORT);
  });

  // The form trims ASCII whitespace only, like the hub (#2713 r4 F2/F3):
  // U+0085 and U+FEFF are kept and then rejected as non-ASCII.
  const prepared = (raw: string) => validateGitRemote(stripQueryAndFragment(trimRemote(raw)));
  it('accepts a remote padded with ASCII whitespace', () => {
    expect(trimRemote(' \t github.com/org/repo \r\n')).toBe('github.com/org/repo');
    expect(prepared(' \t github.com/org/repo \r\n')).toBeNull();
  });
  it.each([
    'https://github.com/org/repo\u0085',
    '\ufeffhttps://github.com/org/repo',
    'https://github.com/org/repo\ufeff',
    '\u00a0github.com/org/repo',
  ])('rejects a remote with Unicode space at the edge: %j', (raw) => {
    expect(prepared(raw)).toBe(GIT_REMOTE_INVALID);
  });
});

describe('stripQueryAndFragment', () => {
  it.each([
    ['https://github.com/org/repo?access_token=x', 'https://github.com/org/repo'],
    ['https://github.com/org/repo#frag', 'https://github.com/org/repo'],
    ['https://github.com/org/repo', 'https://github.com/org/repo'],
  ])('strips %s', (raw, want) => {
    expect(stripQueryAndFragment(raw)).toBe(want);
  });
  it.each(['https://user:PSECRET?W@github.com/org/repo', 'https://user:PSECRET#W@github.com/org/repo'])(
    'never returns a password prefix for %s',
    (raw) => {
      expect(stripQueryAndFragment(raw)).toBe('');
      expect(sanitizeGitRemote(raw)).not.toContain('PSECRET');
      expect(displayGitRemote(raw)).not.toContain('PSECRET');
    },
  );
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
    ['https://u:p/w@github.com/org/repo', 'https://github.com/org/repo'],
    ['https://u:p/w@x@github.com/org/repo.git', 'https://github.com/org/repo.git'],
    ['ssh://git:p/w@github.com/org/repo.git', 'ssh://git@github.com/org/repo.git'],
    ['https://tok@github.com/org/repo@v1', 'https://github.com/org/repo@v1'],
    ['https://u:t@github.com', 'https://github.com'],
    ['https://u:t@github.com/org/repo?x=1', 'https://github.com/org/repo?x=1'],
    ['https://github.com/org/repo?u=a@b', 'https://github.com/org/repo?u=a@b'],
    ['https://github.com/org/repo@github.com/x', 'https://github.com/org/repo@github.com/x'],
    ['https://u:p@github.com/org/x@github.com/repo', 'https://github.com/org/x@github.com/repo'],
    ['https://a/b@github.com/x', 'https://a/b@github.com/x'],
    // A port then '@' in the path is not userinfo (GCP#2368 review).
    ['https://host:8443/org/repo@v1', 'https://host:8443/org/repo@v1'],
    ['https://github.com:443/org/repo@v1', 'https://github.com:443/org/repo@v1'],
    ['https://[::1]:8443/org/repo@v1', 'https://[::1]:8443/org/repo@v1'],
    ['https://u:t@host:8443/org/repo@v1', 'https://host:8443/org/repo@v1'],
    // A password with '/' that cannot be a port is still stripped.
    ['https://u:/pw@github.com/org/repo', 'https://github.com/org/repo'],
    ['https://u:0123/w@github.com/org/repo', 'https://github.com/org/repo'],
    ['https://u:65536/w@github.com/org/repo', 'https://github.com/org/repo'],
    ['https://u:12ab/w@github.com/org/repo', 'https://github.com/org/repo'],
    // Port-like password: read as a port; validation rejects the '@' path.
    ['https://u:8443/w@github.com/org/repo', 'https://u:8443/w@github.com/org/repo'],
  ])('%s -> %s', (input, want) => {
    expect(stripGitURLCredentials(input)).toBe(want);
  });

  it('drops a default port', () => {
    expect(sanitizeGitRemote('https://github.com:443/acme/repo.git')).toBe(
      'https://github.com/acme/repo.git'
    );
    expect(sanitizeGitRemote('http://u:SECRET@git.example.com:80/team/repo')).toBe(
      'http://git.example.com/team/repo'
    );
    expect(sanitizeGitRemote('https://git.example.com:8443/team/repo')).toBe(
      'https://git.example.com:8443/team/repo'
    );
    expect(sanitizeGitRemote('ssh://alice:SECRET@git.example.com/team/repo')).toBe(
      'ssh://alice@git.example.com/team/repo'
    ); // Scheme-less :443 is the https default too (r6 R3); SCP has no port.
    expect(sanitizeGitRemote('git.example.com:443/team/repo')).toBe('git.example.com/team/repo');
    expect(normalizeGitRemote('github.com:443/test/repo')).toBe('github.com/test/repo');
    expect(sanitizeGitRemote('git@gitserver:443/repo')).toBe('git@gitserver:443/repo');
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
    'https://github.com:443/acme/repo',
    'http://github.com:80/acme/repo.git',
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
    ['https://u:SECRET/w@github.com/acme/repo.git', 'github.com/acme/repo'],
    ['git@gitserver:org/repo', 'gitserver/org/repo'],
  ])('%s -> %s', (input, want) => {
    expect(displayGitRemote(input)).toBe(want);
  });
});
