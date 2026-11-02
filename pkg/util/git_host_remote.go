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

package util

import (
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact"
)

// Remote authentication for host-side git.
//
// A host-side git command that talks to a remote on a project's behalf (a
// clone, a fetch, an ls-remote) offers either no credential at all (the
// project has no git secret) or exactly one: the project's https token,
// through the credential helper scoped to the remote's host
// (ScopedCredentialHelperEnv). It never offers the host's own credentials:
// no credential helper of any scope, askpass, extra HTTP header, ~/.netrc,
// TLS client certificate, Kerberos ticket, ssh agent, ssh key or ssh config.
//
// HostGitRemoteEnv builds that environment. Local host-side commands do not
// use it: they keep the host user's global and system config (the git-lfs
// filter, safe.directory).
//
// Ordering: git reads GIT_CONFIG_COUNT entries before -c settings
// (GIT_CONFIG_PARAMETERS). A credential.helper reset passed with -c (for
// example in HostGitArgs) would therefore also clear the scoped token
// helper. Credential resets live in this environment layer only.

// hostGitAnonSSHCommand is the GIT_SSH_COMMAND every remote host-side git
// command runs with. It reads no ssh config file (-F, which replaces both the
// user's and the system's), uses no agent, key, certificate, GSSAPI, host
// based, password or keyboard-interactive authentication, does not reuse a
// multiplexed connection, runs no proxy command and never prompts. Only the
// "none" authentication method remains, so no host identity can be offered.
//
// The host key is still checked against the host user's and the system's
// known_hosts files, read as trust data: StrictHostKeyChecking=yes refuses an
// unknown or changed key and UpdateHostKeys=no never writes those files.
// OpenSSH finds them from the passwd entry, not $HOME.
var hostGitAnonSSHCommand = "ssh -F " + shellQuote(os.DevNull) +
	" -o BatchMode=yes" +
	" -o IdentityAgent=none" +
	" -o IdentitiesOnly=yes" +
	" -o IdentityFile=none" +
	" -o PubkeyAuthentication=no" +
	" -o GSSAPIAuthentication=no" +
	" -o HostbasedAuthentication=no" +
	" -o PasswordAuthentication=no" +
	" -o KbdInteractiveAuthentication=no" +
	" -o ControlMaster=no" +
	" -o ControlPath=none" +
	" -o ProxyCommand=none" +
	" -o ProxyJump=none" +
	" -o StrictHostKeyChecking=yes" +
	" -o UpdateHostKeys=no" +
	" -o ClearAllForwardings=yes" +
	" -o PermitLocalCommand=no"

// shellQuote quotes s for the POSIX shell git runs GIT_SSH_COMMAND with.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hostGitRemoteLayerEnv records, in an environment HostGitRemoteEnv built,
// the GIT_CONFIG_COUNT the caller's environment had before the remote layer
// was added. A second HostGitRemoteEnv call removes the earlier layer from
// that index on before adding its own, so applying it again is a no-op.
const hostGitRemoteLayerEnv = "SCION_HOST_GIT_REMOTE_LAYER"

// hostGitRemoteSet are the variables HostGitRemoteEnv sets, in the order
// they are appended. Any inherited entry for one of them is removed first.
var hostGitRemoteSet = []struct{ key, value string }{
	// No host global or system config: no credential helper, extra header,
	// client certificate, url.*.insteadOf rewrite, core.sshCommand or include
	// from the host user's files.
	{"GIT_CONFIG_GLOBAL", os.DevNull},
	{"GIT_CONFIG_NOSYSTEM", "1"},
	// No home directory: libcurl finds no ~/.netrc, git no ~/.git-credentials
	// or XDG config, and the credential-cache socket cannot be reached.
	{"HOME", os.DevNull},
	{"XDG_CONFIG_HOME", os.DevNull},
	// Never prompt. Askpass is disabled by NoCredentialHelperEnv's empty
	// GIT_ASKPASS, which stops the lookup of core.askPass and SSH_ASKPASS.
	{"GIT_TERMINAL_PROMPT", "0"},
	// ssh without any host identity.
	{"GIT_SSH_COMMAND", hostGitAnonSSHCommand},
	{"GIT_SSH_VARIANT", "ssh"},
	// No Kerberos ticket and no client keytab: an unset KRB5CCNAME falls back
	// to the default credential cache, so it is pointed at nothing instead.
	{"KRB5CCNAME", "FILE:" + os.DevNull},
	{"KRB5_CLIENT_KTNAME", "FILE:" + os.DevNull},
}

// hostGitRemoteDropped are the variables HostGitRemoteEnv removes.
// GITHUB_TOKEN is the variable Scion itself carries a project's git token in
// (provision.GitTokenEnv); host-side code reads it before git runs, and git
// never needs it. Other credentials in the host environment are not read by
// anything git runs under this layer: no credential helper, askpass, git-lfs
// or ssh configuration of the host's applies.
var hostGitRemoteDropped = []string{
	"GIT_CONFIG_SYSTEM",
	"SSH_ASKPASS",
	"GIT_SSH",
	"SSH_AUTH_SOCK",
	"GSS_USE_PROXY",
	"GITHUB_TOKEN",
}

// HostGitRemoteEnv returns base, the environment of a host-side git command
// that talks to remoteURL (already scrubbed by HostGitEnv, plus the caller's
// own variables), with the remote authentication layer added.
//
// token is the project credential offered on this call, "" for none. With no
// token git offers no credential of any kind. With a token and an http(s)
// remoteURL, the token, through the credential helper scoped to the URL's
// host, is the only credential git can offer. A token with any other URL is
// ignored and the command runs anonymously.
//
// The layer sets an empty host global and system config, HOME and
// XDG_CONFIG_HOME pointing at nothing, no prompt, the anonymous ssh command
// (hostGitAnonSSHCommand) and no Kerberos credential; it removes
// GIT_CONFIG_SYSTEM, SSH_ASKPASS, GIT_SSH, SSH_AUTH_SOCK, GSS_USE_PROXY and
// GITHUB_TOKEN;
// then it resets every credential helper and extra HTTP header and disables
// askpass (NoCredentialHelperEnv), and for a token adds the scoped helper
// after that reset (ScopedCredentialHelperEnv).
//
// It leaves trust and transport settings alone: LC_ALL, GIT_ALLOW_PROTOCOL,
// GIT_SSL_CAINFO, GIT_SSL_CAPATH, GIT_HTTP_USER_AGENT, GIT_LFS_SKIP_SMUDGE,
// GIT_CEILING_DIRECTORIES and the proxy variables. The caller's own
// GIT_CONFIG_* entries are kept.
//
// Applying it to its own result with the same arguments returns that result
// unchanged; with other arguments the earlier layer is replaced.
func HostGitRemoteEnv(base []string, remoteURL, token string) []string {
	env, start := withoutHostGitRemoteLayer(base)
	for _, s := range hostGitRemoteSet {
		env = append(env, s.key+"="+s.value)
	}
	env = append(env, hostGitRemoteLayerEnv+"="+strconv.Itoa(start))
	if start > 0 {
		env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(start))
	}
	env = NoCredentialHelperEnv(env, remoteURL)
	if token != "" {
		if scoped, ok := ScopedCredentialHelperEnv(env, remoteURL, token); ok {
			env = scoped
		}
	}
	return lastConfigCountOnly(env)
}

// withoutHostGitRemoteLayer returns base without any variable
// HostGitRemoteEnv sets or removes, without GIT_CONFIG_COUNT, and without an
// earlier remote layer's GIT_CONFIG_KEY_<i>/VALUE_<i> entries. start is the
// caller's own config count: the one recorded by an earlier layer, or else
// base's GIT_CONFIG_COUNT.
func withoutHostGitRemoteLayer(base []string) (env []string, start int) {
	start = existingConfigCount(base)
	if v := gitEnvValue(base, hostGitRemoteLayerEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= start {
			start = n
		}
	}
	owned := map[string]bool{
		hostGitRemoteLayerEnv:  true,
		scopedGitCredentialEnv: true,
		"GIT_CONFIG_COUNT":     true,
		"GIT_ASKPASS":          true,
	}
	for _, s := range hostGitRemoteSet {
		owned[s.key] = true
	}
	for _, k := range hostGitRemoteDropped {
		owned[k] = true
	}
	env = make([]string, 0, len(base)+len(hostGitRemoteSet)+12)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if owned[k] || configIndexAtLeast(k, start) {
			continue
		}
		env = append(env, kv)
	}
	return env, start
}

// configIndexAtLeast reports whether k is GIT_CONFIG_KEY_<i> or
// GIT_CONFIG_VALUE_<i> with i >= start.
func configIndexAtLeast(k string, start int) bool {
	idx, ok := strings.CutPrefix(k, "GIT_CONFIG_KEY_")
	if !ok {
		idx, ok = strings.CutPrefix(k, "GIT_CONFIG_VALUE_")
	}
	if !ok {
		return false
	}
	n, err := strconv.Atoi(idx)
	return err == nil && n >= start
}

// lastConfigCountOnly returns env with only the last GIT_CONFIG_COUNT entry,
// the one git and os/exec use.
func lastConfigCountOnly(env []string) []string {
	last := -1
	for i, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			last = i
		}
	}
	out := make([]string, 0, len(env))
	for i, kv := range env {
		if i != last && strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// appendConfigEnv returns env with key=value added as the next
// GIT_CONFIG_COUNT entry (command scope, which git honors for protected
// settings such as safe.directory).
func appendConfigEnv(env []string, key, value string) []string {
	n := existingConfigCount(env)
	return lastConfigCountOnly(append(env,
		"GIT_CONFIG_COUNT="+strconv.Itoa(n+1),
		"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+key,
		"GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"="+value,
	))
}

// HostGitRemote is the remote a network host-side git command talks to and
// the project credential offered to it ("" for none). HostGitCommand builds
// the command's remote authentication from it (HostGitRemoteEnv). Its
// formatted and logged forms never contain the token.
type HostGitRemote struct {
	url   string
	token string
}

// NewHostGitRemote returns the remote for a network host-side git command:
// the URL it talks to and the project token offered, "" for none.
func NewHostGitRemote(remoteURL, token string) *HostGitRemote {
	return &HostGitRemote{url: remoteURL, token: token}
}

// String describes r without the token or any URL credentials.
func (r *HostGitRemote) String() string {
	if r == nil {
		return "HostGitRemote(nil)"
	}
	state := "unset"
	if r.token != "" {
		state = "set"
	}
	return "HostGitRemote{url:" + redactRemoteURL(r.url) + ", token:" + state + "}"
}

// GoString is String, so %#v never prints the token.
func (r *HostGitRemote) GoString() string { return r.String() }

// LogValue is String, so a logged HostGitRemote never carries the token.
func (r *HostGitRemote) LogValue() slog.Value { return slog.StringValue(r.String()) }

// redactRemoteURL returns raw by its scheme, host and path only
// (urlredact.HostAndPath), or a fixed marker when that rule shows nothing of
// it (for example the scp-style ssh form).
func redactRemoteURL(raw string) string {
	if u, ok := urlredact.HostAndPath(strings.TrimSpace(raw)); ok && u.Host != "" {
		return u.String()
	}
	return "[redacted]"
}

// errHostGitRemoteRequired refuses a network command built without its
// remote. It names no path.
var errHostGitRemoteRequired = errors.New("host-side git: a network command needs its remote; refusing")

// errHostGitRemoteWithoutNetwork refuses a remote on a command that is not
// marked as a network command. It names no path.
var errHostGitRemoteWithoutNetwork = errors.New("host-side git: a remote was given for a command that is not a network command; refusing")
