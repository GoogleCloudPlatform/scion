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

package config

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Template imports (the ls-remote probe and the sparse fetch) offer no host
// credential: the tests below hold each host authentication source on both
// paths, with a control showing plain host git would use it.

// templateHostAuth sets up, for the test, every host authentication source
// for host: a global git config with a credential helper scoped to host's
// URL, a url.insteadOf rewriting it to ssh, a TLS client certificate and a
// core.sshCommand; a ~/.netrc entry; an askpass program; and the ssh, GSS
// and Kerberos environment. ssh and the host's own ssh command are fake
// recorders. It returns the global config path and the two ssh logs.
func templateHostAuth(t *testing.T, rawURL, host string) (global, sshLog, hostSSHLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake programs require a POSIX shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	dir := t.TempDir()
	sshLog = filepath.Join(dir, "ssh.log")
	hostSSHLog = filepath.Join(dir, "host-ssh.log")
	rec := func(log string) string {
		return "#!/bin/sh\nprintf 'ARGV %s\\n' \"$*\" >> '" + log + "'\nexit 255\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(rec(sshLog)), 0o700))
	hostSSH := filepath.Join(dir, "host-ssh")
	require.NoError(t, os.WriteFile(hostSSH, []byte(rec(hostSSHLog)), 0o700))
	askpass := filepath.Join(dir, "askpass")
	require.NoError(t, os.WriteFile(askpass, []byte("#!/bin/sh\necho askpass-secret\n"), 0o700))
	store := filepath.Join(dir, "store")
	require.NoError(t, os.WriteFile(store, []byte(strings.Replace(rawURL, "https://", "https://scoped:scoped-secret@", 1)+"\n"), 0o600))
	global = filepath.Join(dir, "gitconfig")
	body := "[credential \"" + rawURL + "\"]\n\thelper = store --file=" + store + "\n" +
		"[credential]\n\thelper = \"!f() { echo username=env; echo password=${GITHUB_TOKEN}${GH_TOKEN}; }; f\"\n" +
		"[http]\n\tsslCert = " + filepath.Join(dir, "client.pem") + "\n" +
		"[core]\n\tsshCommand = " + hostSSH + " -v\n"
	require.NoError(t, os.WriteFile(global, []byte(body), 0o600))
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine "+host+" login netrc password netrc-secret\n"), 0o600))

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("GIT_SSH_COMMAND", hostSSH+" -i /host/id")
	t.Setenv("GIT_SSH", hostSSH)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/host-agent.sock")
	t.Setenv("KRB5CCNAME", "FILE:/tmp/krb5cc_host")
	t.Setenv("GSS_USE_PROXY", "yes")
	t.Setenv("GITHUB_TOKEN", "host-github-secret")
	t.Setenv("GH_TOKEN", "host-gh-secret")
	return global, sshLog, hostSSHLog
}

func readTestLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

// templateTraceServer is an https endpoint answering 401 (Basic) that
// records decoded Authorization credentials. GIT_SSL_CAINFO trusts it.
type templateTraceServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
	// reqs counts every request, with or without credentials, so a test can
	// show git reached the server before asserting what it did not send.
	reqs int
}

func newTemplateTraceServer(t *testing.T) *templateTraceServer {
	t.Helper()
	ts := &templateTraceServer{}
	ts.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.reqs++
		ts.mu.Unlock()
		if h := r.Header.Get("Authorization"); h != "" {
			decoded := h
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				decoded = string(b)
			}
			ts.mu.Lock()
			ts.auth = append(ts.auth, decoded)
			ts.mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(ts.srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.srv.Certificate().Raw}), 0o600))
	t.Setenv("GIT_SSL_CAINFO", ca)
	return ts
}

func (ts *templateTraceServer) requests() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.reqs
}

func (ts *templateTraceServer) seen() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.auth...)
}

// The ls-remote probe offers no host credential to an https remote: not the
// URL-scoped helper, ~/.netrc or askpass, and with a token only the token.
// The host global config is not read, so its client certificate does not
// apply (git would fail before the request trying to load it) and its
// insteadOf does not take the probe to ssh. Control: plain host git offers
// the netrc and helper credentials.
func TestExecGitLsRemote_OffersNoHostCredential(t *testing.T) {
	ts := newTemplateTraceServer(t)
	repo := ts.srv.URL + "/org/repo.git"
	global, sshLog, _ := templateHostAuth(t, repo, "127.0.0.1")
	run := exec.Command("git", "config", "--file", global, "url.ssh://git@127.0.0.1/.insteadOf", ts.srv.URL+"/")
	require.NoError(t, run.Run())

	_, err := execGitLsRemote(context.Background(), repo, "")
	require.Error(t, err)
	require.Positive(t, ts.requests(), "the anonymous probe must reach the server (otherwise the next assertion is vacuous)")
	assert.Empty(t, ts.seen(), "the anonymous probe offered a credential")
	_, err = execGitLsRemote(context.Background(), repo, "tok-1")
	require.Error(t, err)
	got := ts.seen()
	require.NotEmpty(t, got, "the token must reach the server: the probe went over https with no client certificate")
	for _, a := range got {
		assert.Equal(t, "oauth2:tok-1", a)
	}
	assert.Empty(t, readTestLog(t, sshLog), "the host insteadOf took the probe to ssh")

	// Control, without the rewrite (which would take plain git to ssh) and
	// without the client certificate (which plain git cannot load).
	require.NoError(t, os.WriteFile(global, []byte(strings.SplitN(readTestLog(t, global), "[http]", 2)[0]), 0o600))
	n := len(ts.seen())
	plain := exec.Command("git", "ls-remote", repo)
	plain.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = plain.Run()
	assert.Contains(t, ts.seen()[n:], "netrc:netrc-secret", "plain host git must offer the netrc credential (harness)")
}

// Over ssh the ls-remote probe runs only the anonymous ssh command, with or
// without a token: the host's GIT_SSH_COMMAND, GIT_SSH and core.sshCommand
// are not used.
func TestExecGitLsRemote_SSHIsAnonymous(t *testing.T) {
	_, sshLog, hostSSHLog := templateHostAuth(t, "https://git.example.invalid/org/repo.git", "git.example.invalid")
	for _, token := range []string{"", "tok-1"} {
		_, err := execGitLsRemote(context.Background(), "ssh://git@git.example.invalid/org/repo.git", token)
		require.Error(t, err)
	}
	assert.Empty(t, readTestLog(t, hostSSHLog), "a host ssh setting was used")
	log := readTestLog(t, sshLog)
	require.Equal(t, 2, strings.Count(log, "ARGV "), "one anonymous ssh run per probe:\n%s", log)
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		assert.True(t, strings.HasPrefix(line, "ARGV -F "+os.DevNull+" -o BatchMode=yes -o IdentityAgent=none -o IdentitiesOnly=yes -o IdentityFile=none"), line)
	}
}

// templateGitProbe puts a fake git first on PATH. For the network command
// (ls-remote or fetch) it records the environment it got and, under that
// same environment, what the real git does: the config list with scopes, a
// credential fill for https://github.com (empty when git finds no
// credential), and, when PROBE_TRACE_URL and PROBE_SSH_URL are set, an
// ls-remote of each, so a live https server and the ssh transport see what
// the template path's environment would offer them. Every command then
// fails.
func templateGitProbe(t *testing.T) (envLog, configLog, fillLog string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	dir := t.TempDir()
	envLog = filepath.Join(dir, "env.log")
	configLog = filepath.Join(dir, "config.log")
	fillLog = filepath.Join(dir, "fill.log")
	// The probes run in an empty directory, so no repository config (the
	// sparse checkout's, or the one around the test's working directory)
	// is listed.
	probeDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in ls-remote|fetch)\n" +
		"  env > '" + envLog + "'\n" +
		"  cd '" + probeDir + "'\n" +
		"  '" + realGit + "' config --list --show-scope > '" + configLog + "' 2>&1\n" +
		"  printf 'protocol=https\\nhost=github.com\\npath=org/repo.git\\n\\n' | '" + realGit + "' credential fill > '" + fillLog + "' 2>/dev/null\n" +
		"  [ -n \"${PROBE_TRACE_URL-}\" ] && '" + realGit + "' ls-remote \"$PROBE_TRACE_URL\" >/dev/null 2>&1\n" +
		"  [ -n \"${PROBE_SSH_URL-}\" ] && '" + realGit + "' ls-remote \"$PROBE_SSH_URL\" >/dev/null 2>&1\n" +
		"  echo 'fatal: refused by test' >&2; exit 128;;\n" +
		"esac; done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return envLog, configLog, fillLog
}

// Both template paths (the ls-remote probe and the sparse fetch) run git
// with the remote layer, captured at exec, and under it git reads no host
// global config (no URL-scoped helper, insteadOf, client certificate or
// core.sshCommand), finds no host credential for github.com (none without a
// token, the token with one), and has no home directory for ~/.netrc, the
// anonymous ssh command, and none of the host ssh, GSS or Kerberos settings.
// Control: under the test process's own environment git reads the global
// config and the helper answers.
func TestTemplateGitPaths_RemoteLayerAtExec(t *testing.T) {
	const repo = "https://github.com/org/repo.git"
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "main", Path: "templates"}
	paths := map[string]func(token string){
		"ls-remote": func(token string) { _, _ = execGitLsRemote(context.Background(), repo, token) },
		"sparse fetch": func(token string) {
			_ = execSparseGitCheckout(context.Background(), parts, t.TempDir(), token)
		},
	}
	for name, runPath := range paths {
		for _, token := range []string{"", "tok-1"} {
			t.Run(name+"/token="+token, func(t *testing.T) {
				ts := newTemplateTraceServer(t)
				global, sshLog, hostSSHLog := templateHostAuth(t, repo, "github.com")
				netrc := filepath.Join(os.Getenv("HOME"), ".netrc")
				require.NoError(t, os.WriteFile(netrc, []byte("machine github.com login netrc password netrc-secret\nmachine 127.0.0.1 login netrc password netrc-secret\n"), 0o600))
				t.Setenv("PROBE_TRACE_URL", ts.srv.URL+"/org/repo.git")
				t.Setenv("PROBE_SSH_URL", "ssh://git@git.example.invalid/org/repo.git")
				require.NoError(t, exec.Command("git", "config", "--file", global, "url.ssh://git@github.com/.insteadOf", "https://github.com/").Run())
				control, err := exec.Command("git", "config", "--list", "--show-scope").Output()
				require.NoError(t, err)
				require.Contains(t, string(control), "global\turl.ssh://git@github.com/.insteadof", "harness: the host global config is read by plain git")

				realGit, err := exec.LookPath("git")
				require.NoError(t, err)
				envLog, configLog, fillLog := templateGitProbe(t)
				reqs0 := ts.requests()
				runPath(token)
				env := strings.Split(readTestLog(t, envLog), "\n")
				require.NotEmpty(t, env, "the network command did not run")
				val := func(k string) []string {
					var out []string
					for _, kv := range env {
						if key, v, ok := strings.Cut(kv, "="); ok && key == k {
							out = append(out, v)
						}
					}
					return out
				}
				for k, v := range map[string]string{
					"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "HOME": os.DevNull,
					"XDG_CONFIG_HOME": os.DevNull, "GIT_ASKPASS": "", "GIT_TERMINAL_PROMPT": "0",
					"KRB5CCNAME": "FILE:" + os.DevNull, "KRB5_CLIENT_KTNAME": "FILE:" + os.DevNull, "LC_ALL": "C",
				} {
					// Values are never printed: env is captured from the real process environment.
					got := val(k)
					assert.True(t, len(got) == 1 && got[0] == v, "%s: %d value(s), want exactly the layer value (values not printed)", k, len(got))
				}
				if ssh := val("GIT_SSH_COMMAND"); assert.Len(t, ssh, 1) {
					assert.True(t, strings.HasPrefix(ssh[0], "ssh -F "), "GIT_SSH_COMMAND is not the anonymous ssh command (value not printed)")
				}
				for _, k := range []string{"GIT_SSH", "SSH_AUTH_SOCK", "GSS_USE_PROXY", "SSH_ASKPASS", "GITHUB_TOKEN"} {
					assert.True(t, len(val(k)) == 0, "%s must not reach git (value not printed)", k)
				}

				cfg := readTestLog(t, configLog)
				for _, bad := range []string{"global", "insteadof", "sslcert", "sshcommand", "store --file"} {
					assert.NotContains(t, cfg, bad, "git read the host global config")
				}
				fill := readTestLog(t, fillLog)
				assert.NotContains(t, fill, "scoped-secret")
				assert.NotContains(t, fill, "askpass-secret")
				assert.NotContains(t, fill, "host-github-secret")
				assert.NotContains(t, fill, "host-gh-secret")
				// A live https server (not github.com, so the scoped token does
				// not apply) gets nothing: no netrc, host helper or host token.
				// The probe must have reached it, so the check is not vacuous.
				require.Greater(t, ts.requests(), reqs0, "the probe under the template path's environment must reach the live server")
				assert.Empty(t, ts.seen(), "the template path's environment offered a credential to a live server")
				// The ssh transport runs exactly the anonymous ssh command.
				assert.Empty(t, readTestLog(t, hostSSHLog), "a host ssh setting was used")
				want := "ARGV -F " + os.DevNull + " -o BatchMode=yes -o IdentityAgent=none -o IdentitiesOnly=yes -o IdentityFile=none" +
					" -o PubkeyAuthentication=no -o GSSAPIAuthentication=no -o HostbasedAuthentication=no" +
					" -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no" +
					" -o ControlMaster=no -o ControlPath=none -o ProxyCommand=none -o ProxyJump=none" +
					" -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o ClearAllForwardings=yes -o PermitLocalCommand=no "
				sshRuns := strings.Split(strings.TrimSpace(readTestLog(t, sshLog)), "\n")
				require.Len(t, sshRuns, 1, "one ssh run from the probe")
				assert.True(t, strings.HasPrefix(sshRuns[0], want), "ssh argv %q, want the full anonymous ssh command", sshRuns[0])

				// Control: plain host git, with the same host setup, does offer
				// a credential to the same live server (netrc or the host
				// helper), so the empty-credential assertion above can fail.
				require.NoError(t, os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo username=env; echo password=${GITHUB_TOKEN}${GH_TOKEN}; }; f\"\n"), 0o600))
				plain := exec.Command(realGit, "ls-remote", ts.srv.URL+"/org/repo.git")
				plain.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "PROBE_TRACE_URL=", "PROBE_SSH_URL=")
				_ = plain.Run()
				got := ts.seen()
				require.NotEmpty(t, got, "plain host git must offer a credential to the live server (harness)")
				assert.True(t, slices.ContainsFunc(got, func(a string) bool {
					return a == "netrc:netrc-secret" || strings.Contains(a, "host-github-secret")
				}), "plain host git offered %q, want the netrc or host-helper credential", got)
				if token == "" {
					assert.NotContains(t, fill, "password=", "no credential without a token")
				} else {
					assert.Contains(t, fill, "password=tok-1\n", "the token is the credential")
				}
			})
		}
	}
}
