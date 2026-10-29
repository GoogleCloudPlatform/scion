// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provision

import (
	"errors"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// HostCloneProtocols is the GIT_ALLOW_PROTOCOL value of a hardened host-side
// clone (ProvisionInput.HostGitHardened). git applies it to every transport
// it uses, including redirects and submodules, ahead of any config.
const HostCloneProtocols = "https:ssh:git"

// HostCloneGitArgs returns the -c settings a hardened host-side clone
// carries ahead of the subcommand: hostGitArgs, then HostCloneGitConfig.
func HostCloneGitArgs() []string {
	args := hostGitArgs()
	for _, kv := range HostCloneGitConfig() {
		args = append(args, "-c", kv)
	}
	return args
}

// HostCloneGitConfig returns the key=value settings a hardened host-side
// clone or fetch adds after the base host pins (util.HostGitArgs): a
// transport allow-list that denies every protocol except https, ssh and
// git. protocol.file.allow=never overrides the base
// protocol.file.allow=user, so a local path or file:// URL is refused even
// on the command line. http.followRedirects=false: git never follows an
// HTTP redirect to another URL.
func HostCloneGitConfig() []string {
	return []string{
		"http.followRedirects=false",
		"protocol.allow=never",
		"protocol.file.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=always",
		"protocol.git.allow=always",
	}
}

// hostGitArgs returns the base -c settings every host-side git invocation in a
// repository that agents can write carries, ahead of the subcommand. It
// delegates to util.HostGitArgs so there is a single source of truth for the
// base pins (no hooks, no fsmonitor, no pager, restricted ext/file transports,
// no submodule recursion, no signature verification or signing); see
// util.HostGitArgs for the per-setting rationale.
func hostGitArgs() []string {
	return util.HostGitArgs()
}

// ErrHostCloneURLRefused is returned by CheckHostCloneURL. Its text names no
// part of the refused URL.
var ErrHostCloneURLRefused = errors.New("the repository URL must use https, ssh or git; local paths and other URL schemes are not cloned on the hub")

// CheckHostCloneURL accepts only a URL a hardened host-side clone may use:
// https://, ssh:// or git:// with a host, or an scp-style ssh address
// ([user@]host:path). It refuses, and never rewrites, everything else: local
// paths, file://, http:// and other schemes, git's "<transport>::<address>"
// form (ext::, fd::, ...), a host or user starting with "-", and a value
// with surrounding space, control characters or a leading "-".
func CheckHostCloneURL(raw string) error {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.HasPrefix(raw, "-") || remoteHelperForm(raw) {
		return ErrHostCloneURLRefused
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return ErrHostCloneURLRefused
		}
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Opaque != "" || strings.HasPrefix(u.Hostname(), "-") ||
			(u.User != nil && strings.HasPrefix(u.User.Username(), "-")) {
			return ErrHostCloneURLRefused
		}
		// url.Parse lower-cases the scheme; git does not, so it must be
		// written in lower case.
		if !strings.HasPrefix(raw, u.Scheme+"://") {
			return ErrHostCloneURLRefused
		}
		switch u.Scheme {
		case "https", "ssh", "git":
			return nil
		}
		return ErrHostCloneURLRefused
	}
	// scp-style: git reads "[user@]host:path" as ssh when the first ':'
	// comes before any '/'. Anything else without a scheme is a local path.
	colon := strings.Index(raw, ":")
	if colon <= 0 || strings.ContainsAny(raw[:colon], `/\`) || colon == len(raw)-1 {
		return ErrHostCloneURLRefused
	}
	host := raw[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	// A one-letter host is a Windows drive letter ("C:/repos/x").
	if host == "" || strings.HasPrefix(host, "-") || len(host) == 1 {
		return ErrHostCloneURLRefused
	}
	return nil
}

// remoteHelperForm reports whether raw is git's "<transport>::<address>"
// form, which runs the git-remote-<transport> helper (ext:: runs a
// command): a scheme-like name directly followed by "::", as git itself
// reads it. A "::" elsewhere (an IPv6 address) does not count.
func remoteHelperForm(raw string) bool {
	i := 0
	for i < len(raw) {
		c := raw[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !letter && (i == 0 || ((c < '0' || c > '9') && c != '+' && c != '-' && c != '.')) {
			break
		}
		i++
	}
	return i > 0 && strings.HasPrefix(raw[i:], "::")
}

// CloneTokenBound reports whether a project's git token may be offered to
// a host-side clone or fetch of cloneURL: cloneURL is https without user
// info and resolves to the host and owner/repo of the project's recorded
// git remote (cloneTokenHost). Any other URL gets no token.
func CloneTokenBound(projectRemote, cloneURL string) bool {
	_, ok := cloneTokenHost(projectRemote, cloneURL)
	return ok
}

// cloneTokenHost returns the host a project's git token may be offered to
// when cloning cloneURL: the URL's host, when cloneURL is https without user
// info and its normalized host and owner/repo path equal the project's
// recorded git remote (util.NormalizeGitRemote: case-insensitive, port kept,
// a trailing .git or / ignored). Otherwise it reports false and the clone
// gets no token. The host always comes from a URL that matches the
// project's own remote, never from another source.
func cloneTokenHost(projectRemote, cloneURL string) (string, bool) {
	remote := util.NormalizeGitRemote(projectRemote)
	if remote == "" || !strings.Contains(remote, "/") {
		return "", false
	}
	u, err := url.Parse(cloneURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if util.NormalizeGitRemote(cloneURL) != remote {
		return "", false
	}
	return strings.ToLower(u.Host), true
}
