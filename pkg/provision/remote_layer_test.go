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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureCloneEnv runs runGitClone(in) for url with a fake git first on
// PATH that records the environment of the clone invocation and fails it,
// and returns that environment: what git runs with at exec.
func captureCloneEnv(t *testing.T, in ProvisionInput, url string) []string {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "clone-env.txt")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = clone ]; then env > '" + envFile + "'; echo 'fatal: refused by test' >&2; exit 128; fi; done\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700))
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	in.GitClone = &api.GitCloneConfig{URL: url}
	in.Resolved = ResolvedWorkspace{HostPath: filepath.Join(t.TempDir(), "dest")}
	require.Error(t, runGitClone(context.Background(), in))
	data, err := os.ReadFile(envFile)
	require.NoError(t, err, "the clone did not run through the fake git")
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func envValuesOf(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			vals = append(vals, v)
		}
	}
	return vals
}

// checkRemoteLayerEnv fails t unless env carries the remote authentication
// layer: an empty host global and system config, no home directory, no
// askpass or prompt, the anonymous ssh command, no Kerberos credential, none
// of the host's ssh agent, GIT_SSH or GSS settings, and in effect no
// credential helper (token "") or exactly the one scoped to scopedKey with
// the token.
func checkRemoteLayerEnv(t *testing.T, env []string, token, scopedKey string) {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"HOME":                os.DevNull,
		"XDG_CONFIG_HOME":     os.DevNull,
		"GIT_ASKPASS":         "",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_SSH_VARIANT":     "ssh",
		"KRB5CCNAME":          "FILE:" + os.DevNull,
		"KRB5_CLIENT_KTNAME":  "FILE:" + os.DevNull,
		"LC_ALL":              "C",
		"GIT_LFS_SKIP_SMUDGE": "1",
	} {
		// Values are never printed: env is captured from the real process environment.
		got := envValuesOf(env, k)
		assert.True(t, len(got) == 1 && got[0] == v, "%s: %d value(s), want exactly the layer value (values not printed)", k, len(got))
	}
	sshCmd := envValuesOf(env, "GIT_SSH_COMMAND")
	if assert.Len(t, sshCmd, 1, "GIT_SSH_COMMAND") {
		assert.True(t, strings.HasPrefix(sshCmd[0], "ssh -F "), "GIT_SSH_COMMAND is not the anonymous ssh command (value not printed)")
		assert.True(t, strings.Contains(sshCmd[0], "-o IdentityAgent=none"), "GIT_SSH_COMMAND lacks IdentityAgent=none (value not printed)")
	}
	for _, k := range []string{"GIT_CONFIG_SYSTEM", "SSH_ASKPASS", "GIT_SSH", "SSH_AUTH_SOCK", "GSS_USE_PROXY", "NETRC", GitTokenEnv} {
		assert.True(t, len(envValuesOf(env, k)) == 0, "%s must be absent (value not printed)", k)
	}
	counts := envValuesOf(env, "GIT_CONFIG_COUNT")
	require.Len(t, counts, 1, "GIT_CONFIG_COUNT")
	n, err := strconv.Atoi(counts[0])
	require.NoError(t, err)
	var helpers []string
	for i := 0; i < n; i++ {
		keys := envValuesOf(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		vals := envValuesOf(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		require.Len(t, keys, 1)
		require.Len(t, vals, 1)
		k := strings.ToLower(keys[0])
		if !strings.HasPrefix(k, "credential.") || !strings.HasSuffix(k, ".helper") {
			continue
		}
		if vals[0] == "" {
			helpers = nil
			continue
		}
		helpers = append(helpers, keys[0])
	}
	cred := envValuesOf(env, "SCION_SCOPED_GIT_CREDENTIAL")
	if token == "" {
		assert.Empty(t, helpers, "no credential helper may be in effect")
		assert.Empty(t, cred, "no scoped credential without a token")
		return
	}
	assert.Equal(t, []string{scopedKey}, helpers, "exactly the scoped helper must be in effect")
	assert.Equal(t, []string{token}, cred, "the scoped credential must be the token")
}

// hostAuthSetenv sets, for the test, every host authentication variable the
// remote layer overrides or removes.
func hostAuthSetenv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_SSH":            "/usr/bin/host-ssh",
		"GIT_SSH_COMMAND":    "ssh -i /home/host/.ssh/id",
		"GIT_SSH_VARIANT":    "plink",
		"SSH_AUTH_SOCK":      "/tmp/agent.sock",
		"SSH_ASKPASS":        "/usr/bin/ssh-askpass",
		"KRB5CCNAME":         "FILE:/tmp/krb5cc_host",
		"KRB5_CLIENT_KTNAME": "/etc/krb5.keytab",
		"GSS_USE_PROXY":      "yes",
		"NETRC":              "/home/host/.netrc",
		"GIT_CONFIG_SYSTEM":  "/etc/gitconfig",
		"GH_TOKEN":           "host-gh-secret",
	} {
		t.Setenv(k, v)
	}
}

// runGitClone with a token runs git with the remote layer and exactly the
// credential helper scoped to the clone URL's host, captured at exec; an
// unscoped helper is never in effect, and the host's ssh, Kerberos and
// askpass settings do not reach git. A token with an ssh URL is ignored.
func TestRunGitClone_UsesScopedHelperNotUnscoped(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	hostAuthSetenv(t)
	const url = "https://github.com/org/repo.git"
	for _, in := range []ProvisionInput{
		{HostGitHardened: true, CloneToken: "tok", CloneTokenRemote: url},
		{CloneToken: "tok"},
	} {
		env := captureCloneEnv(t, in, url)
		checkRemoteLayerEnv(t, env, "tok", "credential.https://github.com.helper")
	}
	t.Setenv(GitTokenEnv, "env-tok")
	checkRemoteLayerEnv(t, captureCloneEnv(t, ProvisionInput{CloneWithToken: true}, url), "env-tok", "credential.https://github.com.helper")

	ssh := "ssh://git@github.com/org/repo.git"
	checkRemoteLayerEnv(t, captureCloneEnv(t, ProvisionInput{HostGitHardened: true, CloneToken: "tok", CloneTokenRemote: ssh}, ssh), "", "")
}

// A ~/.netrc entry for the server is not offered by a hardened or a
// broker-side clone, with or without a token. Control: plain host git
// offers it.
func TestRunGitClone_NetrcNotOffered(t *testing.T) {
	tr := newHostCloneTrace(t)
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine 127.0.0.1 login netrc password netrc-secret\n"), 0o600))
	t.Setenv("HOME", home)
	tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url()})
	tr.clone(t, ProvisionInput{})
	tr.assertNothingOffered(t)
	tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url(), CloneToken: "project-token"})
	auth, _, _ := tr.seen()
	require.NotEmpty(t, auth)
	for _, a := range auth {
		assert.Equal(t, "oauth2:project-token", a)
	}

	n := len(auth)
	cmd := exec.Command("git", "clone", "-q", "--", tr.url(), filepath.Join(t.TempDir(), "dest"))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = cmd.Run()
	auth, _, _ = tr.seen()
	assert.Contains(t, auth[n:], "netrc:netrc-secret", "plain host git must offer the netrc credential (harness)")
}

// An ssh clone, hardened (hub and broker shared clones) or not, runs only
// the anonymous ssh command, with or without a token: the host's
// GIT_SSH_COMMAND, GIT_SSH, core.sshCommand and SSH_AUTH_SOCK are not used.
// Control: plain host git runs the host's GIT_SSH_COMMAND.
func TestRunGitClone_SSHIsAnonymous(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "ssh.log")
	hostRecord := filepath.Join(dir, "host-ssh.log")
	script := func(log string) string {
		return "#!/bin/sh\nprintf 'ARGV %s\\nSOCK %s\\n' \"$*\" \"${SSH_AUTH_SOCK-<unset>}\" >> '" + log + "'\nexit 255\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(script(record)), 0o700))
	hostSSH := filepath.Join(dir, "host-ssh")
	require.NoError(t, os.WriteFile(hostSSH, []byte(script(hostRecord)), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run(t, "git", "config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "core.sshCommand", hostSSH+" -v")
	t.Setenv("GIT_SSH_COMMAND", hostSSH+" -i /host/id")
	t.Setenv("GIT_SSH", hostSSH)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/host-agent.sock")

	const url = "ssh://git@git.example.invalid/org/repo.git"
	_ = exec.Command("git", "ls-remote", url).Run()
	hostLog, _ := os.ReadFile(hostRecord)
	require.Contains(t, string(hostLog), "-i /host/id", "plain host git must run the host GIT_SSH_COMMAND (harness)")
	require.NoError(t, os.Remove(hostRecord))

	for _, in := range []ProvisionInput{
		{HostGitHardened: true, CloneTokenRemote: url},
		{HostGitHardened: true, CloneTokenRemote: url, CloneToken: "project-token"},
		{},
	} {
		in.GitClone = &api.GitCloneConfig{URL: url}
		in.Resolved = ResolvedWorkspace{HostPath: filepath.Join(t.TempDir(), "dest")}
		require.Error(t, runGitClone(context.Background(), in))
	}
	_, err := os.Stat(hostRecord)
	assert.True(t, os.IsNotExist(err), "a host ssh setting was used")
	data, err := os.ReadFile(record)
	require.NoError(t, err, "git did not reach ssh")
	var argvs []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if a, ok := strings.CutPrefix(line, "ARGV "); ok {
			argvs = append(argvs, a)
		}
		if s, ok := strings.CutPrefix(line, "SOCK "); ok {
			assert.Equal(t, "<unset>", s, "SSH_AUTH_SOCK reached ssh")
		}
	}
	assert.Len(t, argvs, 3, "one ssh run per clone")
	for _, a := range argvs {
		assert.True(t, strings.HasPrefix(a, "-F "+os.DevNull+" -o BatchMode=yes -o IdentityAgent=none"), "ssh argv %q", a)
		assert.False(t, slices.Contains(strings.Fields(a), "-i"), "ssh argv %q names an identity", a)
	}
}

// A git token in the host process's environment is not offered by a
// hardened or a broker-side clone with no project token, even with a host
// credential helper that answers from it: the layer removes GITHUB_TOKEN and
// nothing git runs under it reads GH_TOKEN. Control: plain host git offers
// the GITHUB_TOKEN value through that helper.
func TestRunGitClone_HostTokenEnvNotOffered(t *testing.T) {
	tr := newHostCloneTrace(t)
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(cfg, []byte("[credential]\n\thelper = \"!f() { echo username=env; echo password=${GITHUB_TOKEN}${GH_TOKEN}; }; f\"\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv(GitTokenEnv, "host-github-secret")
	t.Setenv("GH_TOKEN", "host-gh-secret")
	tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url()})
	tr.clone(t, ProvisionInput{})
	tr.assertNothingOffered(t)

	cmd := exec.Command("git", "clone", "-q", "--", tr.url(), filepath.Join(t.TempDir(), "dest"))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = cmd.Run()
	auth, _, _ := tr.seen()
	require.NotEmpty(t, auth, "plain host git must offer the helper's credential (harness)")
	assert.Contains(t, auth[0], "host-github-secret")
}
