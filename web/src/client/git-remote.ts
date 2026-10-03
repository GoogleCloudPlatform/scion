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
 * Client-side mirror of the hub's handling of a project-clone `gitRemote`
 * override (pkg/hub/project_clone.go: validateCloneGitRemote,
 * canonicalCloneRemote; pkg/util: StripGitURLCredentials, NormalizeGitRemote).
 *
 * The hub stays authoritative — it re-validates and returns a 400 with
 * `details.field = "gitRemote"` — but mirroring the rules lets the create form
 * flag obvious mistakes before submitting, tell whether an override actually
 * names a different repository, and display a remote without leaking a
 * pasted token. Keep the two in step.
 */

/** Same text as the hub's errCloneRemoteInvalid. */
export const GIT_REMOTE_INVALID =
  'gitRemote must be a remote git URL (https://, ssh://, git://, user@host:org/repo or host[:port]/org/repo)';

/** Same text as the hub's errCloneRemoteSSHPort. */
export const GIT_REMOTE_SSH_PORT =
  'gitRemote: ssh URLs with a port are not supported yet; use the https URL';

const SCHEMES = ['https://', 'http://', 'ssh://', 'git://'];
const SCP_LOGIN = /^[A-Za-z0-9._-]+$/;
const HOSTNAME = /^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$/;
const HOST_LABELS = /^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*$/;
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

/** Drop everything from the first `?` or `#`; git remotes need neither and a query can carry tokens. */
export function stripQueryAndFragment(remote: string): string {
  const i = remote.search(/[?#]/);
  return i >= 0 ? remote.slice(0, i) : remote;
}

/**
 * Index of the '@' ending the userinfo of `s` (a URL after "scheme://", with
 * the query and fragment removed), or -1. Picks the last '@' followed by
 * "host/..." with a non-empty host, or by a bare host when nothing before the
 * '@' contains '/'. Mirrors util.userinfoEnd.
 */
function userinfoEnd(s: string): number {
  let end = s.length;
  for (;;) {
    const at = s.lastIndexOf('@', end - 1);
    if (at < 0) return -1;
    const slash = s.indexOf('/', at + 1);
    if (slash > at + 1 || (slash < 0 && !s.slice(0, at).includes('/'))) return at;
    end = at;
  }
}

/**
 * Remove credentials: http(s):// and git:// lose their userinfo, ssh:// keeps
 * the login but loses a password, SCP shorthand is unchanged. A password with
 * an unencoded '/' or '@' is still removed. Mirrors util.StripGitURLCredentials.
 */
export function stripGitURLCredentials(remote: string): string {
  const schemeEnd = remote.indexOf('://');
  if (schemeEnd < 0) return remote;
  const scheme = remote.slice(0, schemeEnd).toLowerCase();
  const authorityStart = schemeEnd + 3;
  const rest = remote.slice(authorityStart);
  let limit = rest.search(/[?#]/);
  if (limit < 0) limit = rest.length;
  const at = userinfoEnd(rest.slice(0, limit));
  if (at < 0) return remote;
  let userinfo = rest.slice(0, at);
  const hostAndPath = rest.slice(at + 1);
  if (scheme === 'ssh') {
    const colon = userinfo.indexOf(':');
    if (colon >= 0) userinfo = userinfo.slice(0, colon);
    if (userinfo && !/[/@]/.test(userinfo)) {
      return remote.slice(0, authorityStart) + userinfo + '@' + hostAndPath;
    }
  }
  return remote.slice(0, authorityStart) + hostAndPath;
}

/** The safe form of an override: trimmed, without query/fragment or credentials. */
export function sanitizeGitRemote(remote: string): string {
  return stripGitURLCredentials(stripQueryAndFragment(remote.trim()));
}

function splitSCP(remote: string): { login: string; host: string; path: string } | null {
  if (remote.includes('://')) return null;
  const at = remote.indexOf('@');
  if (at < 0) return null;
  const rest = remote.slice(at + 1);
  const colon = rest.indexOf(':');
  if (colon < 0) return null;
  const host = rest.slice(0, colon);
  if (host.includes('/')) return null;
  return { login: remote.slice(0, at), host, path: rest.slice(colon + 1) };
}

function hasOrgAndRepo(path: string): boolean {
  return path.replace(/^\/+|\/+$/g, '').includes('/');
}

function isPort(s: string): boolean {
  if (!/^[0-9]+$/.test(s) || s.startsWith('0')) return false;
  const n = Number(s);
  return n > 0 && n <= 65535;
}

/**
 * Host and port of a scheme URL as Go's net/url splits them (the authority
 * ends at the first '/'; userinfo ends at its last '@'), or null when Go's
 * url.Parse would fail: an unterminated IPv6 literal or a non-numeric port.
 */
function parseURLHost(remote: string): { host: string; port: string } | null {
  const rest = remote.slice(remote.indexOf('://') + 3);
  let authority = rest.split('/')[0];
  const at = authority.lastIndexOf('@');
  if (at >= 0) authority = authority.slice(at + 1);
  let host: string;
  let portPart: string;
  if (authority.startsWith('[')) {
    const close = authority.indexOf(']');
    if (close < 0) return null;
    host = authority.slice(1, close);
    portPart = authority.slice(close + 1);
    if (portPart && !portPart.startsWith(':')) return null;
  } else {
    const colon = authority.lastIndexOf(':');
    host = colon >= 0 ? authority.slice(0, colon) : authority;
    portPart = colon >= 0 ? authority.slice(colon) : '';
  }
  const port = portPart.slice(1);
  if (port && !/^[0-9]+$/.test(port)) return null;
  return { host, port };
}

/** A DNS name (single label allowed) or an IP address, as url.Hostname() returns it. */
function isURLHost(host: string): boolean {
  return (
    HOST_LABELS.test(host) ||
    IPV4.test(host) ||
    (host.includes(':') && /^[0-9A-Fa-f:.]+$/.test(host))
  );
}

/** Validate a scheme URL override as the hub's validateCloneSchemeRemote does. */
function validateSchemeRemote(remote: string): string | null {
  const scheme = remote.slice(0, remote.indexOf('://')).toLowerCase();
  const isSSH = scheme === 'ssh';
  const parsed = parseURLHost(remote);
  if (!parsed) {
    if (isSSH) {
      let authority = remote.slice(remote.indexOf('://') + 3).split('/')[0];
      const at = authority.lastIndexOf('@');
      if (at >= 0) authority = authority.slice(at + 1);
      const colon = authority.indexOf(':');
      if (colon >= 0 && isPort(authority.slice(colon + 1))) return GIT_REMOTE_SSH_PORT;
    }
    return GIT_REMOTE_INVALID;
  }
  if (!['https', 'http', 'ssh', 'git'].includes(scheme)) return GIT_REMOTE_INVALID;
  if (!isURLHost(parsed.host)) return GIT_REMOTE_INVALID;
  if (isSSH && parsed.port) return GIT_REMOTE_SSH_PORT;
  if (!isSchemeGitURL(remote)) return GIT_REMOTE_INVALID;
  // Removing credentials must not change the host (catches a password with
  // an unencoded '/' and an '@' in the path).
  const stripped = parseURLHost(stripGitURLCredentials(remote));
  if (!stripped || stripped.host.toLowerCase() !== parsed.host.toLowerCase()) {
    return GIT_REMOTE_INVALID;
  }
  return null;
}

/** Mirror of util.IsGitURL for scheme URLs. */
function isSchemeGitURL(remote: string): boolean {
  const lower = remote.toLowerCase();
  const scheme = SCHEMES.find((s) => lower.startsWith(s));
  if (!scheme) return false;
  let rest = remote.slice(scheme.length);
  const at = rest.indexOf('@');
  if (at >= 0) rest = rest.slice(at + 1);
  const slash = rest.indexOf('/');
  return slash >= 1 && slash !== rest.length - 1;
}

/**
 * Validate an override as the hub does: scheme URLs with a valid host (no ssh
 * port, and credential stripping must keep the host), SCP user@host:org/repo
 * with any login and any host (single-label allowed), or scheme-less
 * host[:port]/org/repo with a dotted host. SCP without a login
 * (github.com:org/repo) is rejected. Returns null when it is acceptable,
 * otherwise the hub's 400 message. Input should be trimmed and have its query
 * and fragment removed (see {@link stripQueryAndFragment}).
 */
export function validateGitRemote(remote: string): string | null {
  if (remote.includes('://')) return validateSchemeRemote(remote);

  const scp = splitSCP(remote);
  if (scp) {
    const ok =
      SCP_LOGIN.test(scp.login) &&
      HOST_LABELS.test(scp.host) &&
      scp.path !== '' &&
      !scp.path.startsWith('/') &&
      hasOrgAndRepo(scp.path);
    return ok ? null : GIT_REMOTE_INVALID;
  }

  const slash = remote.indexOf('/');
  const hostPort = slash >= 0 ? remote.slice(0, slash) : remote;
  const path = slash >= 0 ? remote.slice(slash + 1) : '';
  const colon = hostPort.indexOf(':');
  const host = colon >= 0 ? hostPort.slice(0, colon) : hostPort;
  if (!HOSTNAME.test(host) || (colon >= 0 && !isPort(hostPort.slice(colon + 1)))) {
    return GIT_REMOTE_INVALID;
  }
  return hasOrgAndRepo(path) ? null : GIT_REMOTE_INVALID;
}

/**
 * Mirror of util.NormalizeGitRemote (after the hub's canonicalCloneRemote):
 * lowercase, no scheme, no login/credentials, SCP ':' as '/', no trailing
 * slash or `.git`. Two remotes naming the same repository normalize equal.
 */
export function normalizeGitRemote(remote: string): string {
  let r = stripQueryAndFragment(remote.trim());
  if (!r) return '';
  const scp = splitSCP(r);
  if (scp) r = `git@${scp.host}:${scp.path}`;
  r = r.toLowerCase();
  for (const s of SCHEMES) {
    if (r.startsWith(s)) {
      r = r.slice(s.length);
      break;
    }
  }
  if (r.startsWith('git@')) {
    r = r.slice(4).replace(':', '/');
  }
  const at = r.indexOf('@');
  if (at >= 0) {
    const slash = r.indexOf('/');
    if (slash < 0 || at < slash) r = r.slice(at + 1);
  }
  return r.replace(/\/+$/, '').replace(/\.git$/, '');
}

/**
 * Display form of a remote: credentials, query and fragment removed, then no
 * scheme and no `.git` (like the hub's normalized form, but keeping case).
 */
export function displayGitRemote(remote: string): string {
  let r = sanitizeGitRemote(remote);
  const scp = splitSCP(r);
  if (scp) r = `${scp.host}/${scp.path}`;
  r = r.replace(/^(https?:\/\/|ssh:\/\/|git:\/\/)/i, '');
  // ssh:// keeps its login after sanitizing; it is not part of the repo name.
  const at = r.indexOf('@');
  const slash = r.indexOf('/');
  if (at >= 0 && (slash < 0 || at < slash)) r = r.slice(at + 1);
  return r.replace(/\/+$/, '').replace(/\.git$/, '');
}
