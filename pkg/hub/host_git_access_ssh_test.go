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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSSH puts an ssh on PATH that records each invocation and fails at
// once, so a test can tell whether git reached the ssh transport without
// any network access. It returns the path of the record file.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + record + "'\nexit 255\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "")
	require.NoError(t, os.Unsetenv("GIT_SSH_COMMAND"))
	t.Setenv("GIT_SSH", "")
	require.NoError(t, os.Unsetenv("GIT_SSH"))
	return record
}

func sshRan(t *testing.T, record string) bool {
	t.Helper()
	b, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return len(strings.TrimSpace(string(b))) > 0
}

func TestIsSSHCloneURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"ssh://git@git.example.test/org/repo.git", true},
		{"SSH://git@git.example.test/org/repo.git", true},
		{"git+ssh://git@git.example.test/org/repo.git", true},
		{"ssh+git://git@git.example.test/org/repo.git", true},
		{"GIT+SSH://git@git.example.test/org/repo.git", true},
		{"git@git.example.test:org/repo.git", true},
		{"https://git.example.test/org/repo.git", false},
		{"git://git.example.test/org/repo.git", false},
	} {
		assert.Equal(t, tc.want, isSSHCloneURL(tc.url), tc.url)
	}
	// The recorded remote falls through to the clone URL: an explicit ssh
	// remote stays ssh after resolution (and is refused without a secret);
	// an scp address with another login resolves to https.
	assert.True(t, isSSHCloneURL(resolveCloneURL("", "ssh://git@git.example.test/org/repo.git")))
	assert.True(t, isSSHCloneURL(resolveCloneURL("", "git@git.example.test:org/repo.git")))
	assert.False(t, isSSHCloneURL(resolveCloneURL("", "git.example.test/org/repo")))
	// The git+ssh / ssh+git aliases are ssh whether or not they were
	// normalized to ssh:// on the way (NormalizeCloneURL does rewrite them).
	assert.True(t, isSSHCloneURL(resolveCloneURL("git+ssh://git@git.example.test/org/repo.git", "")))
	assert.True(t, isSSHCloneURL(resolveCloneURL("ssh+git://git@git.example.test/org/repo.git", "")))
	assert.ErrorIs(t, sshCloneURLRefused("git@git.example.test:org/repo.git", false), errSSHCloneURLNeedsProjectSecret)
	assert.NoError(t, sshCloneURLRefused("git@git.example.test:org/repo.git", true))
	assert.NoError(t, sshCloneURLRefused("https://git.example.test/org/repo.git", false))
}

// hubRefusesCloneURL reports whether the hub refuses raw (a clone-url label
// value) at one of its two layers: the host clone-URL check
// (provision.CheckHostCloneURL) or the ssh-without-a-secret refuse, after
// the hub's own resolution of the label (resolveCloneURL).
func hubRefusesCloneURL(raw string) bool {
	resolved := resolveCloneURL(raw, "")
	return provision.CheckHostCloneURL(resolved) != nil || sshCloneURLRefused(resolved, false) != nil
}

// Every form git could use to reach a remote over ssh (or with no transport
// check at all) is refused for a project with no git secret, by one of the
// hub's two layers, whichever catches it: ssh://, git+ssh:// and ssh+git://
// in any letter case, scp-style, rsync://, ext:: and file://. https:// and
// git:// (anonymous transports) are not refused.
func TestHubRefusesEverySSHFormWithoutProjectSecret(t *testing.T) {
	for _, raw := range []string{
		"ssh://git@git.example.test/org/repo.git",
		"SSH://git@git.example.test/org/repo.git",
		"git+ssh://git@git.example.test/org/repo.git",
		"ssh+git://git@git.example.test/org/repo.git",
		"GIT+SSH://git@git.example.test/org/repo.git",
		"git@git.example.test:org/repo.git",
		"rsync://git.example.test/org/repo.git",
		"ext::ssh -o x git.example.test",
		"file:///srv/org/repo.git",
	} {
		assert.True(t, hubRefusesCloneURL(raw), "%q must be refused without a project secret", raw)
	}
	for _, raw := range []string{"https://git.example.test/org/repo.git", "git://git.example.test/org/repo.git"} {
		assert.False(t, hubRefusesCloneURL(raw), "%q runs anonymously, not refused", raw)
	}
	// Which layer catches what, at this head: CheckHostCloneURL admits only
	// https://, ssh://, git:// (lower case) and scp-style; the aliases reach
	// the ssh refuse because resolution rewrites them to ssh://.
	for _, raw := range []string{"rsync://git.example.test/org/repo.git", "ext::ssh -o x git.example.test", "file:///srv/org/repo.git"} {
		assert.Error(t, provision.CheckHostCloneURL(resolveCloneURL(raw, "")), "%q is refused by the host clone-URL check", raw)
	}
}

// sshRefusalCases are ssh clone URLs reached through the clone-url label
// and, falling through, through the recorded remote itself.
func sshRefusalCases() []struct{ name, gitRemote, label string } {
	return []struct{ name, gitRemote, label string }{
		{"ssh label", "git.example.test/org/repo", "ssh://git@git.example.test/org/repo.git"},
		{"scp label", "git.example.test/org/repo", "git@git.example.test:org/repo.git"},
		{"git+ssh label", "git.example.test/org/repo", "git+ssh://git@git.example.test/org/repo.git"},
		{"ssh+git label", "git.example.test/org/repo", "ssh+git://git@git.example.test/org/repo.git"},
		{"ssh recorded remote", "ssh://git@git.example.test/org/repo.git", ""},
		{"scp recorded remote", "git@git.example.test:org/repo.git", ""},
	}
}

// A workspace pull of an ssh repository URL with no project git secret is
// refused (422, fixed text) before git runs; with a project secret it goes
// ahead to git.
func TestHostGitAccess_PullSSHWithoutProjectSecretRefused(t *testing.T) {
	for i, tc := range sshRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			record := fakeSSH(t)
			srv, st, setToken := hostGitTokenServer(t)
			p := hostGitProvisionedProject(t, srv, st, fmt.Sprintf("host-git-ssh-%d", i), tc.gitRemote, tc.label)

			code, body := hostGitPull(t, srv, p.ID)
			assert.Equal(t, http.StatusUnprocessableEntity, code, body)
			assert.True(t, strings.Contains(body, errSSHCloneURLNeedsProjectSecret.Error()) || strings.Contains(body, provision.ErrHostCloneURLRefused.Error()),
				"refused by one of the hub's layers: %s", body)
			assert.False(t, sshRan(t, record), "git must not reach ssh without a project secret")

			setToken(p.ID)
			code, body = hostGitPull(t, srv, p.ID)
			assert.NotContains(t, body, errSSHCloneURLNeedsProjectSecret.Error(), "with a project secret the pull is not refused")
			assert.NotEqual(t, http.StatusOK, code, "fixture check: the fake ssh fails the pull")
			assert.True(t, sshRan(t, record), "with a project secret git goes ahead over ssh")
		})
	}
}

// The local-backend clone of an ssh repository URL with no project git
// secret is refused before git runs.
func TestCloneSharedWorkspaceProject_LocalSSHWithoutProjectSecretRefused(t *testing.T) {
	for i, tc := range sshRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			record := fakeSSH(t)
			srv, st, _ := hostGitTokenServer(t)
			p := hostGitProject(t, st, fmt.Sprintf("host-git-ssh-clone-%d", i), tc.gitRemote, tc.label)
			err := srv.cloneSharedWorkspaceProject(context.Background(), p)
			require.True(t, errors.Is(err, errSSHCloneURLNeedsProjectSecret) || errors.Is(err, provision.ErrHostCloneURLRefused),
				"refused by one of the hub's layers: %v", err)
			assert.False(t, sshRan(t, record), "git must not reach ssh without a project secret")
		})
	}
}
