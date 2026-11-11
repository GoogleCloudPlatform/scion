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

//go:build unix

package util

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// remoteFakeSSH puts an ssh on PATH that records, per run, its argv and
// whether SSH_AUTH_SOCK reached it, and fails at once, so a test can see
// whether and how git reached the ssh transport without network access. It
// also returns a second recorder, hostSSH, for the host's own GIT_SSH /
// GIT_SSH_COMMAND / core.sshCommand to point at.
func remoteFakeSSH(t *testing.T) (record, hostSSH, hostRecord string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "ssh.log")
	hostRecord = filepath.Join(dir, "host-ssh.log")
	script := func(log string) string {
		return "#!/bin/sh\nprintf 'ARGV %s\\nSOCK %s\\n' \"$*\" \"${SSH_AUTH_SOCK-<unset>}\" >> '" + log + "'\nexit 255\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script(record)), 0o700); err != nil {
		t.Fatal(err)
	}
	hostSSH = filepath.Join(dir, "host-ssh")
	if err := os.WriteFile(hostSSH, []byte(script(hostRecord)), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record, hostSSH, hostRecord
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkAnonSSHRuns fails t unless every ssh run in log carried the
// anonymous ssh options and no SSH_AUTH_SOCK, and at least one run happened.
func checkAnonSSHRuns(t *testing.T, log string) {
	t.Helper()
	if !strings.Contains(log, "ARGV ") {
		t.Fatalf("git did not reach ssh; want an anonymous ssh run")
	}
	opts := strings.TrimPrefix(hostGitAnonSSHCommand, "ssh ")
	opts = strings.ReplaceAll(opts, shellQuote(os.DevNull), os.DevNull)
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if a, ok := strings.CutPrefix(line, "ARGV "); ok && !strings.HasPrefix(a, opts+" ") {
			t.Errorf("ssh argv %q, want it to start with the anonymous options %q", a, opts)
		}
		if s, ok := strings.CutPrefix(line, "SOCK "); ok && s != "<unset>" {
			t.Errorf("SSH_AUTH_SOCK reached ssh: %q", s)
		}
	}
}

// hostSSHSetup points every host ssh setting at hostSSH: GIT_SSH,
// GIT_SSH_COMMAND, core.sshCommand in the host global config, plus an
// SSH_AUTH_SOCK, so a test can tell whether any of them is used.
func hostSSHSetup(t *testing.T, hostSSH string) {
	t.Helper()
	global := hostGitTestEnv(t)
	hostRunGit(t, t.TempDir(), "config", "--file", global, "core.sshCommand", hostSSH+" -v")
	t.Setenv("GIT_SSH_COMMAND", hostSSH+" -i /host/id")
	t.Setenv("GIT_SSH", hostSSH)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/host-agent.sock")
}

// Control: with the host ssh settings in place, plain host git uses the
// host's ssh command, so the assertions below are not vacuous.
func TestRemoteSSH_ControlHostSSHIsUsedByPlainGit(t *testing.T) {
	_, hostSSH, hostRecord := remoteFakeSSH(t)
	hostSSHSetup(t, hostSSH)
	cmd := exec.Command("git", "ls-remote", "ssh://git@git.example.invalid/org/repo.git")
	_ = cmd.Run()
	if !strings.Contains(readLog(t, hostRecord), "-i /host/id") {
		t.Fatalf("plain git must run the host GIT_SSH_COMMAND (harness); log:\n%s", readLog(t, hostRecord))
	}
}

// Over ssh, a clone and a pull's fetch run only the anonymous ssh command,
// with or without a token: the host GIT_SSH_COMMAND, GIT_SSH,
// core.sshCommand and SSH_AUTH_SOCK are not used (precedence: the layer's
// GIT_SSH_COMMAND outranks core.sshCommand, and GIT_SSH is removed).
// Mutants this catches: dropping GIT_SSH_COMMAND from the layer (the host
// command runs), dropping SSH_AUTH_SOCK from the removed list (it reaches
// ssh), and any option removed from the command. GIT_SSH and the
// GIT_CONFIG_GLOBAL entry are outranked here by GIT_SSH_COMMAND; their
// mutants fail the env-capture tests and the insteadOf test.
func TestRemoteSSH_CloneAndPullUseAnonymousSSHOnly(t *testing.T) {
	for _, token := range []string{"", "project-token"} {
		t.Run("token="+token, func(t *testing.T) {
			record, hostSSH, hostRecord := remoteFakeSSH(t)
			_, workspace, _ := sharedWorkspaceFixture(t)
			hostSSHSetup(t, hostSSH)
			const url = "ssh://git@git.example.invalid/org/repo.git"
			_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), url, "", token)
			_, _ = PullSharedWorkspace(workspace, url, token)
			if got := readLog(t, hostRecord); got != "" {
				t.Errorf("a host ssh setting was used:\n%s", got)
			}
			log := readLog(t, record)
			checkAnonSSHRuns(t, log)
			if n := strings.Count(log, "ARGV "); n < 2 {
				t.Errorf("want ssh runs from the clone and the fetch, got %d:\n%s", n, log)
			}
		})
	}
}

// A host url.insteadOf rewriting the https URL to ssh does not apply: git
// stays on https, so no ssh runs at all, with or without a token. Control:
// plain host git follows the rewrite.
// Mutant this catches: dropping GIT_CONFIG_GLOBAL from the layer.
func TestRemoteInsteadOf_HostRewriteToSSHDoesNotApply(t *testing.T) {
	record, _, _ := remoteFakeSSH(t)
	_, workspace, _ := sharedWorkspaceFixture(t)
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	const url = "https://127.0.0.1:1/org/repo.git"
	hostRunGit(t, t.TempDir(), "config", "--file", global, "url.ssh://git@127.0.0.1/.insteadOf", "https://127.0.0.1:1/")
	for _, k := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	_ = exec.Command("git", "ls-remote", url).Run()
	if !strings.Contains(readLog(t, record), "ARGV ") {
		t.Fatalf("plain git must follow the host rewrite to ssh (harness)")
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "project-token"} {
		_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), url, "", token)
		_, _ = PullSharedWorkspace(workspace, url, token)
	}
	if got := readLog(t, record); got != "" {
		t.Errorf("the host rewrite took the remote command to ssh:\n%s", got)
	}
}

// A ~/.netrc entry for the server is not offered on a clone or a pull, with
// or without a token; with a token only the token is offered. Control:
// plain host git offers the netrc credential.
// Mutant this catches: dropping HOME from the layer.
func TestRemoteNetrc_NotOffered(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	cs.useAmbientConfig(t, "")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine 127.0.0.1 login netrc password netrc-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("NETRC", filepath.Join(home, ".netrc"))

	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "")
	_, _ = PullSharedWorkspace(workspace, cs.repoURL(), "")
	assertNoCredentialOffered(t, cs)
	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "project-token")
	_, _ = PullSharedWorkspace(workspace, cs.repoURL(), "project-token")
	assertOnlyTokenOffered(t, cs, "project-token")

	n := len(cs.seen())
	hostPlainClone(t, cs.repoURL())
	if got := cs.seen()[n:]; !slices.Contains(got, "netrc:netrc-secret") {
		t.Fatalf("plain host git must offer the netrc credential (harness), got %q", got)
	}
}

// The host global config, which can hold a TLS client certificate
// (http.sslCert/sslKey, also URL-scoped), is not read under the remote
// layer. Control: under HostGitEnv alone git lists it.
// Mutant this catches: dropping GIT_CONFIG_GLOBAL from the layer.
func TestRemoteClientCert_HostGlobalConfigNotRead(t *testing.T) {
	global := hostGitTestEnv(t)
	dir := t.TempDir()
	hostRunGit(t, dir, "config", "--file", global, "http.sslCert", "/host/client.pem")
	hostRunGit(t, dir, "config", "--file", global, "http.https://git.example.test.sslKey", "/host/client.key")
	list := func(env []string) string {
		cmd := exec.Command("git", append(HostGitArgs(), "config", "--list", "--show-scope")...)
		cmd.Dir = dir
		cmd.Env = env
		out, _ := cmd.Output()
		return string(out)
	}
	if got := list(HostGitEnv(os.Environ())); !strings.Contains(got, "http.sslcert=/host/client.pem") {
		t.Fatal("HostGitEnv must still read the host global config (harness); the config list is not printed")
	}
	for _, token := range []string{"", "tok-1"} {
		got := list(HostGitRemoteEnv(HostGitEnv(os.Environ()), "https://git.example.test/r.git", token))
		if strings.Contains(got, "global") || strings.Contains(got, "sslcert") || strings.Contains(got, "sslkey") {
			t.Errorf("token set=%v: the remote layer read the host global config (the config list is not printed)", token != "")
		}
	}
}

// The scoped token helper survives HostGitArgs: git reads GIT_CONFIG_COUNT
// entries before -c settings, so a credential reset passed with -c would
// clear the token helper. Real git answers the token through the remote
// layer with HostGitArgs in front, and answers nothing without a token. The
// control shows the hazard is real: a -c credential.helper= reset after the
// layer clears the token.
// Mutant this catches: adding a credential.helper reset to HostGitArgs.
func TestRemoteLayer_ScopedHelperSurvivesHostGitArgs(t *testing.T) {
	global := hostGitTestEnv(t)
	hostRunGit(t, t.TempDir(), "config", "--file", global, "credential.helper", "!f() { echo username=host; echo password=host-secret; }; f")
	fill := func(env []string, extra ...string) (string, error) {
		args := append(append(HostGitArgs(), extra...), "credential", "fill")
		cmd := exec.Command("git", args...)
		cmd.Dir = t.TempDir()
		cmd.Env = env
		cmd.Stdin = strings.NewReader("protocol=https\nhost=git.example.test\npath=org/repo.git\n\n")
		out, err := cmd.Output()
		return string(out), err
	}
	base := HostGitEnv(os.Environ())
	out, err := fill(HostGitRemoteEnv(base, "https://git.example.test/org/repo.git", "tok-1"))
	if err != nil || !strings.Contains(out, "password=tok-1\n") || strings.Contains(out, "host-secret") {
		t.Errorf("with the token: credential fill = %q, %v; want the token only", out, err)
	}
	out, err = fill(HostGitRemoteEnv(base, "https://git.example.test/org/repo.git", ""))
	if err == nil || strings.Contains(out, "password=") {
		t.Errorf("without a token: credential fill = %q, %v; want no credential", out, err)
	}
	out, _ = fill(HostGitRemoteEnv(base, "https://git.example.test/org/repo.git", "tok-1"), "-c", "credential.helper=")
	if strings.Contains(out, "password=tok-1") {
		t.Fatalf("a -c credential.helper= reset must clear the token helper (hazard control), got %q", out)
	}
	if out, _ := fill(base); !strings.Contains(out, "password=host-secret") {
		t.Fatalf("plain host git must use the host helper (harness), got %q", out)
	}
}

// remoteEnvFakeGit puts a fake git first on PATH that, for a clone or
// fetch, appends its whole environment to the returned log (one block per
// run, ended by "--ENVEND--") and then runs the real git.
func remoteEnvFakeGit(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in clone|fetch) env >> '" + log + "'; echo --ENVEND-- >> '" + log + "'; break;; esac; done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// capturedEnvs returns every environment block logged by remoteEnvFakeGit.
func capturedEnvs(t *testing.T, log string) [][]string {
	t.Helper()
	var out [][]string
	for _, block := range strings.Split(readLog(t, log), "--ENVEND--\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		out = append(out, strings.Split(strings.TrimSuffix(block, "\n"), "\n"))
	}
	return out
}

// The environment git runs with on a hardened clone and a hardened
// pull's fetch, captured at exec: the remote layer, with no helper
// without a token (and with NoCredentialHelpers even when a token is given)
// and exactly the scoped helper with one. Host authentication variables set
// in the test process do not reach git.
func TestRemoteEnvCapture_CloneAndPull(t *testing.T) {
	const url = "https://127.0.0.1:1/org/repo.git"
	const scoped = "credential.https://127.0.0.1:1.helper"
	for _, tc := range []struct {
		name, token, want string
		h                 HostGitHardening
	}{
		{"anonymous", "", "", HostGitHardening{}},
		{"NoCredentialHelpers", "project-token", "", HostGitHardening{NoCredentialHelpers: true}},
		{"token", "project-token", "project-token", HostGitHardening{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, workspace, _ := sharedWorkspaceFixture(t)
			log := remoteEnvFakeGit(t)
			for _, kv := range hostAuthEnv() {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "PATH", "GIT_LFS_SKIP_SMUDGE", "https_proxy", "no_proxy":
				default:
					t.Setenv(k, v)
				}
			}
			t.Setenv("NETRC", "/home/host/.netrc")
			t.Setenv("GIT_SSL_CERT", "/home/host/client.pem")
			_ = CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), url, "", tc.token, tc.h)
			_, _ = PullSharedWorkspaceHardened(workspace, url, tc.token, tc.h)
			envs := capturedEnvs(t, log)
			if len(envs) != 2 {
				t.Fatalf("want the clone's and the fetch's environment, got %d", len(envs))
			}
			for _, env := range envs {
				checkHostGitRemoteEnv(t, env, tc.want, scoped)
			}
		})
	}
}

// A network door command runs with the remote layer; a local door command
// keeps the host config (no layer).
// Mutant this catches: deleting the door's HostGitRemoteEnv line.
func TestHostGitCommand_NetworkAppliesRemoteLayer(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{
		Network: true, Remote: NewHostGitRemote("https://git.example.test/r.git", "tok-1"),
	}, "fetch")
	if err != nil {
		t.Fatal(err)
	}
	checkHostGitRemoteEnv(t, cmd.Env, "tok-1", "credential.https://git.example.test.helper")
	local, err := HostGitCommand(context.Background(), repo, HostGitOptions{}, "status")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(local.Env, "GIT_CONFIG_GLOBAL="+os.DevNull) || slices.Contains(local.Env, "HOME="+os.DevNull) {
		// Report a fixed message only: local.Env is the real process
		// environment, which can hold credentials.
		t.Errorf("a local door command got the remote layer (GIT_CONFIG_GLOBAL or HOME is %s)", os.DevNull)
	}
}

// The pull's fetch reads no host global config, so the host safe.directory
// list does not reach it; the fetch and the door's checks trust exactly the
// workspace path (never "*"). With git treating every repository as
// foreign-owned, the host list accepting the workspace is enough for the
// whole pull.
// Mutant this catches: dropping the fetch's safe.directory entry (the fetch
// is refused as dubious ownership).
func TestPullSharedWorkspace_FetchTrustsExactlyTheWorkspace(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	commitFile(t, source, "new.txt", "new\n", "add new")
	url := newAuthedDumbHTTPRepo(t, source, "")
	hostRunGit(t, t.TempDir(), "config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "--add", "safe.directory", workspace)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	log := remoteEnvFakeGit(t)
	res, err := PullSharedWorkspace(workspace, url, "")
	if err != nil {
		t.Fatalf("PullSharedWorkspace: %v", err)
	}
	if !res.Updated {
		t.Errorf("result = %+v, want the new commit pulled", res)
	}
	envs := capturedEnvs(t, log)
	if len(envs) != 1 {
		t.Fatalf("want one fetch, got %d", len(envs))
	}
	var trusted []string
	for _, kv := range envs[0] {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && v == "safe.directory" {
			trusted = append(trusted, envValues(envs[0], "GIT_CONFIG_VALUE_"+strings.TrimPrefix(k, "GIT_CONFIG_KEY_"))...)
		}
	}
	if !reflect.DeepEqual(trusted, []string{workspace}) {
		t.Errorf("the fetch trusts %q, want exactly [%q]", trusted, workspace)
	}
}

// The door re-applying the remote layer over an environment that already
// carries it, for the same remote, changes nothing: a caller that builds the
// layer itself and also passes Remote gets the same command environment.
// Mutant this catches: the door's HostGitRemoteEnv stacking a second layer.
func TestHostGitCommand_ReapplyingRemoteLayerIsNoOp(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	for _, token := range []string{"", "tok-1"} {
		const url = "https://git.example.test/r.git"
		pre := HostGitRemoteEnv(HostGitEnv(os.Environ()), url, token)
		cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{
			Env: pre, Network: true, Remote: NewHostGitRemote(url, token),
		}, "fetch")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cmd.Env, pre) {
			// Report only the keys that differ, never values: pre and cmd.Env
			// hold the real process environment, which can hold credentials.
			t.Errorf("token set=%v: the door changed an environment that already had the layer; differing keys: %q", token != "", diffEnvSliceKeys(pre, cmd.Env))
		}
	}
}

// A git token in the host process's environment is not offered on a clone
// or a pull, with or without a project token: the layer removes
// GITHUB_TOKEN (Scion's own variable), and nothing git runs under it reads
// any other (GH_TOKEN here), even with a host credential helper that would
// answer from them. Control: plain host git offers the GITHUB_TOKEN value
// through that helper.
// This is the behavioural proof; deleting GITHUB_TOKEN from the removed list
// is caught by TestHostGitRemoteEnv_NoToken and the env-capture tests, and
// deleting GIT_CONFIG_GLOBAL from the layer is caught here (the helper runs).
func TestRemoteHostTokenEnv_NotOffered(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	cs.useAmbientConfig(t, "[credential]\n\thelper = \"!f() { echo username=env; echo password=${GITHUB_TOKEN}${GH_TOKEN}; }; f\"\n")
	t.Setenv("GITHUB_TOKEN", "host-github-secret")
	t.Setenv("GH_TOKEN", "host-gh-secret")

	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "")
	_, _ = PullSharedWorkspace(workspace, cs.repoURL(), "")
	assertNoCredentialOffered(t, cs)
	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "project-token")
	_, _ = PullSharedWorkspace(workspace, cs.repoURL(), "project-token")
	assertOnlyTokenOffered(t, cs, "project-token")

	n := len(cs.seen())
	hostPlainClone(t, cs.repoURL())
	got := cs.seen()[n:]
	if len(got) == 0 || !strings.Contains(got[0], "host-github-secret") {
		t.Fatalf("plain host git must offer the host GITHUB_TOKEN through the helper (harness), got %q", got)
	}
}
