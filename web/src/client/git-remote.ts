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

/** Same text as the hub's errCloneRemoteTLSPort. */
export const GIT_REMOTE_TLS_PORT =
  'gitRemote: git:// URLs with a port, http:// URLs with a port other than 80 and host:80/... remotes are not supported; use the https URL';

const SCHEMES = ['https://', 'http://', 'ssh://', 'git://'];
const SCP_LOGIN = /^[A-Za-z0-9._-]+$/;
// DNS labels may not start or end with '-'.
const LABEL = '[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?';
const HOSTNAME = new RegExp(`^${LABEL}(\\.${LABEL})+$`);
const HOST_LABELS = new RegExp(`^${LABEL}(\\.${LABEL})*$`);
// Anything outside printable, non-space ASCII (0x21-0x7E): the hub's isPrintableASCII.
const NOT_PRINTABLE_ASCII = /[^\x21-\x7e]/;
// A '%' not followed by two hex digits (Go's url.Parse / url.PathUnescape fail).
const BAD_ESCAPE = /%(?![0-9A-Fa-f]{2})/;
// Characters Go's net/url accepts in userinfo (validUserinfo).
const USERINFO = /^[A-Za-z0-9\-._:~!$&'()*+,;=%@]*$/;
// ASCII whitespace trimmed from both ends, as the hub's trimRemote does.
const ASCII_SPACE_EDGES = /^[ \t\n\v\f\r]+|[ \t\n\v\f\r]+$/g;
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

/** Trim ASCII whitespace only (not U+0085 or U+FEFF), like the hub's trimRemote. */
export function trimRemote(remote: string): string {
  return remote.replace(ASCII_SPACE_EDGES, '');
}

/**
 * Drop everything from the first `?` or `#`; git remotes need neither. If the
 * removed span contains '@', the `?`/`#` may fall inside userinfo rather than
 * the path, so return '' rather than a partial value. Mirror of
 * util.CutQueryAndFragment.
 */
export function stripQueryAndFragment(remote: string): string {
  const i = remote.search(/[?#]/);
  if (i < 0) return remote;
  return remote.slice(i).includes('@') ? '' : remote.slice(0, i);
}

/**
 * Index of the '@' ending the userinfo of `s` (a URL after "scheme://", with
 * the query and fragment removed), or -1. Picks the first '@' such that the
 * login before it (up to the first ':') contains no '/', and the host after
 * it (up to the next '/') is non-empty and contains no '@'. Once a '/' is in
 * the login position the path has started, so no later '@' ends the userinfo.
 * A '/' after the first ':' is ambiguous: when the text before it is a valid
 * host:port ("host:8443/org/repo@v1") there is no userinfo, as in RFC 3986;
 * otherwise ("user:pa/ss@host/repo") the '@' ends a password, which is
 * stripped (fail closed). Mirrors util.userinfoEnd.
 */
function userinfoEnd(s: string): number {
  for (let from = 0; ; ) {
    const at = s.indexOf('@', from);
    if (at < 0) return -1;
    const login = s.slice(0, at).split(':')[0];
    if (login.includes('/')) return -1;
    const slash = s.indexOf('/');
    if (slash >= 0 && slash < at && isHostAndPort(s.slice(0, slash))) return -1;
    const host = s.slice(at + 1).split('/')[0];
    if (host !== '' && !host.includes('@')) return at;
    from = at + 1;
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

/**
 * Drop an explicit default port (:443 for https, :80 for http) from a
 * credential-free scheme URL. Mirrors the hub's dropDefaultPort.
 */
export function dropDefaultPort(remote: string): string {
  const schemeEnd = remote.indexOf('://');
  if (schemeEnd < 0) {
    // Scheme-less host:443/org/repo: the clone-url is https. SCP has no port.
    if (splitSCP(remote)) return remote;
    const slash = remote.indexOf('/');
    if (slash >= 0 && remote.slice(0, slash).endsWith(':443')) {
      return remote.slice(0, slash - 4) + remote.slice(slash);
    }
    return remote;
  }
  const scheme = remote.slice(0, schemeEnd).toLowerCase();
  const port = scheme === 'https' ? ':443' : scheme === 'http' ? ':80' : '';
  if (!port) return remote;
  const rest = remote.slice(schemeEnd + 3);
  const slash = rest.indexOf('/');
  const authority = slash >= 0 ? rest.slice(0, slash) : rest;
  if (authority.includes('@') || !authority.endsWith(port)) return remote;
  return (
    remote.slice(0, schemeEnd + 3) +
    authority.slice(0, -port.length) +
    (slash >= 0 ? rest.slice(slash) : '')
  );
}

/**
 * The safe form of an override, as the hub stores it in source-url: trimmed,
 * without query/fragment, credentials or a default port.
 */
export function sanitizeGitRemote(remote: string): string {
  return dropDefaultPort(stripGitURLCredentials(stripQueryAndFragment(trimRemote(remote))));
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

/**
 * Mirror of the hub's validRemotePath: a decoded path (no leading '/') whose
 * segments are non-empty, not "." or "..", and free of '@', '\\' and control
 * characters. One trailing '/' is allowed.
 */
function validRemotePath(path: string): boolean {
  return path
    .replace(/\/$/, '')
    .split('/')
    .every((seg) => seg !== '' && seg !== '.' && seg !== '..' && !hasBadSegmentChar(seg));
}

/** True when seg holds '@', '\\' or a control character (charCode loop, no control-char regex). */
function hasBadSegmentChar(seg: string): boolean {
  for (let i = 0; i < seg.length; i++) {
    const c = seg.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x40 || c === 0x5c) return true;
  }
  return false;
}

// RFC 3986 path characters the hub accepts in a raw path (isRemotePathChar):
// unreserved, sub-delims, ':', '/' and '%' (escapes are checked separately).
const RAW_PATH = /^[A-Za-z0-9\-._~!$&'()*+,;=:/%]*$/;

/**
 * Mirror of the hub's validRawRemotePath: only RFC 3986 path characters and
 * no %2F, which some servers decode to '/'.
 */
function validRawRemotePath(path: string): boolean {
  return RAW_PATH.test(path) && !/%2f/i.test(path);
}

/** Decode %-escapes (UTF-8 or not, byte-wise like Go), or null on a malformed escape. */
function unescapePath(path: string): string | null {
  if (BAD_ESCAPE.test(path)) return null;
  return path.replace(/%([0-9A-Fa-f]{2})/g, (_, h: string) => String.fromCharCode(parseInt(h, 16)));
}

/** Mirror of the hub's validEscapedRemotePath: checks the raw and the decoded path. */
function validEscapedRemotePath(path: string): boolean {
  const decoded = unescapePath(path);
  return (
    decoded !== null &&
    validRawRemotePath(path) &&
    validRemotePath(path) &&
    validRemotePath(decoded)
  );
}

/** A non-empty host, ':' and a port (e.g. "host:8443"). Mirrors util.isHostAndPort. */
function isHostAndPort(s: string): boolean {
  const colon = s.lastIndexOf(':');
  return colon > 0 && isPort(s.slice(colon + 1));
}

function isPort(s: string): boolean {
  if (!/^[0-9]+$/.test(s) || s.startsWith('0')) return false;
  const n = Number(s);
  return n > 0 && n <= 65535;
}

/**
 * Host, port and path of a scheme URL as Go's net/url splits them (the
 * authority ends at the first '/'; userinfo ends at its last '@'), or null
 * when Go's url.Parse would fail: a malformed %-escape, a userinfo character
 * net/url rejects (e.g. '\\' or '"'), an unterminated IPv6 literal or a
 * non-numeric port. `hasColon` is true when a ':' introduces the port.
 */
function parseURLHost(
  remote: string
): { host: string; port: string; hasColon: boolean; path: string } | null {
  if (BAD_ESCAPE.test(remote)) return null;
  const rest = remote.slice(remote.indexOf('://') + 3);
  const slash = rest.indexOf('/');
  const path = slash >= 0 ? rest.slice(slash) : '';
  let authority = slash >= 0 ? rest.slice(0, slash) : rest;
  const at = authority.lastIndexOf('@');
  if (at >= 0) {
    if (!USERINFO.test(authority.slice(0, at))) return null;
    authority = authority.slice(at + 1);
  }
  let host: string;
  let portPart: string;
  if (authority.startsWith('[')) {
    const close = authority.indexOf(']');
    if (close < 0) return null;
    host = authority.slice(1, close);
    if (!isIPv6(host)) return null; // brackets hold only an IPv6 literal
    portPart = authority.slice(close + 1);
    if (portPart && !portPart.startsWith(':')) return null;
  } else {
    const colon = authority.lastIndexOf(':');
    host = colon >= 0 ? authority.slice(0, colon) : authority;
    portPart = colon >= 0 ? authority.slice(colon) : '';
  }
  const port = portPart.slice(1);
  if (port && !/^[0-9]+$/.test(port)) return null;
  return { host, port, hasColon: portPart.startsWith(':'), path };
}

/** A DNS name (single label allowed) or an IP address, as url.Hostname() returns it. */
function isURLHost(host: string): boolean {
  return HOST_LABELS.test(host) || IPV4.test(host) || isIPv6(host);
}

/** Strict IPv6 literal check, matching Go's net.ParseIP (an IPv4 tail is allowed). */
function isIPv6(host: string): boolean {
  if (!host.includes(':')) return false;
  const halves = host.split('::');
  if (halves.length > 2) return false;
  const groups = (part: string): number | null => {
    if (part === '') return 0;
    const parts = part.split(':');
    let n = 0;
    for (let i = 0; i < parts.length; i++) {
      if (i === parts.length - 1 && parts[i].includes('.')) {
        if (!IPV4.test(parts[i])) return null;
        n += 2;
      } else if (/^[0-9A-Fa-f]{1,4}$/.test(parts[i])) {
        n += 1;
      } else {
        return null;
      }
    }
    return n;
  };
  const head = groups(halves[0]);
  if (head === null) return false;
  if (halves.length === 1) return head === 8;
  if (halves[0].includes('.')) return false; // an IPv4 tail must come last
  const tail = groups(halves[1]);
  return tail !== null && head + tail < 8;
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
  if (isSSH && parsed.hasColon) return GIT_REMOTE_SSH_PORT;
  // A port must be 1-65535 without leading zeros; a bare ':' is rejected.
  if (parsed.hasColon && !isPort(parsed.port)) return GIT_REMOTE_INVALID;
  // git:// with any port or http:// with a non-80 port would become an
  // https clone-url to a plain-text port.
  if (
    (scheme === 'git' && parsed.port) ||
    (scheme === 'http' && parsed.port && parsed.port !== '80')
  ) {
    return GIT_REMOTE_TLS_PORT;
  }
  // Dot, empty and '@' segments, raw and decoded ('@' is ambiguous with userinfo).
  if (!validEscapedRemotePath(parsed.path.replace(/^\//, ''))) return GIT_REMOTE_INVALID;
  if (!isSchemeGitURL(remote)) return GIT_REMOTE_INVALID;
  // Removing credentials must change nothing but the userinfo.
  const stripped = parseURLHost(stripGitURLCredentials(remote));
  if (
    !stripped ||
    stripped.host.toLowerCase() !== parsed.host.toLowerCase() ||
    stripped.port !== parsed.port ||
    stripped.path !== parsed.path
  ) {
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
 * Validate an override as the hub does: scheme URLs with a valid host and port
 * (no ssh port, and credential stripping must change only the userinfo), SCP
 * user@host:org/repo with any login and any host (single-label allowed), or
 * scheme-less host[:port]/org/repo with a dotted host. SCP without a login
 * (github.com:org/repo), anything but printable ASCII, malformed %-escapes,
 * '@' (raw or %40), "." / ".." or empty segments in the path, git:// with a
 * port, http:// with a port other than 80, and DNS labels starting or ending
 * with '-' are rejected. Returns null when it is acceptable,
 * otherwise the hub's 400 message. Input should be trimmed and have its query
 * and fragment removed (see {@link stripQueryAndFragment}).
 */
export function validateGitRemote(remote: string): string | null {
  if (NOT_PRINTABLE_ASCII.test(remote)) return GIT_REMOTE_INVALID;
  if (remote.includes('://')) return validateSchemeRemote(remote);

  const scp = splitSCP(remote);
  if (scp) {
    const ok =
      SCP_LOGIN.test(scp.login) &&
      HOST_LABELS.test(scp.host) &&
      !scp.path.includes('@') &&
      scp.path !== '' &&
      !scp.path.startsWith('/') &&
      hasOrgAndRepo(scp.path) &&
      validEscapedRemotePath(scp.path);
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
  // The clone-url is https, so :80 would be TLS to a plain-text port.
  if (colon >= 0 && hostPort.slice(colon + 1) === '80') return GIT_REMOTE_TLS_PORT;
  return hasOrgAndRepo(path) && !path.includes('@') && validEscapedRemotePath(path)
    ? null
    : GIT_REMOTE_INVALID;
}

/**
 * Mirror of util.NormalizeGitRemote (after the hub's canonicalCloneRemote):
 * lowercase, no scheme, no login/credentials, no default port, SCP ':' as '/', no trailing
 * slash or `.git`. Two remotes naming the same repository normalize equal.
 */
export function normalizeGitRemote(remote: string): string {
  let r = sanitizeGitRemote(remote);
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
