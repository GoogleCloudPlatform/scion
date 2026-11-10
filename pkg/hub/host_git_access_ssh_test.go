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
		{"git.example.test:org/repo.git", true},
		{"https://git.example.test/org/repo.git", false},
		{"git://git.example.test/org/repo.git", false},
	} {
		assert.Equal(t, tc.want, isSSHCloneURL(tc.url), tc.url)
	}
	// The recorded remote falls through to the clone URL: an explicit ssh
	// remote stays ssh after resolution (and is refused);
	// an scp address with another login resolves to https.
	assert.True(t, isSSHCloneURL(resolveCloneURL("", "ssh://git@git.example.test/org/repo.git")))
	assert.True(t, isSSHCloneURL(resolveCloneURL("", "git@git.example.test:org/repo.git")))
	assert.False(t, isSSHCloneURL(resolveCloneURL("", "git.example.test/org/repo")))
	// The git+ssh / ssh+git aliases are ssh whether or not they were
	// normalized to ssh:// on the way (NormalizeCloneURL does rewrite them).
	assert.True(t, isSSHCloneURL(resolveCloneURL("git+ssh://git@git.example.test/org/repo.git", "")))
	assert.True(t, isSSHCloneURL(resolveCloneURL("ssh+git://git@git.example.test/org/repo.git", "")))
}

// hubRefusesCloneURL reports whether the hub refuses raw (a clone-url label
// value) at one of its two layers: the host clone-URL check
// (provision.CheckHostCloneURL) or the ssh refusal, after
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
		{"upper-case GIT+SSH label", "git.example.test/org/repo", "GIT+SSH://git@git.example.test/org/repo.git"},
		{"ssh recorded remote", "ssh://git@git.example.test/org/repo.git", ""},
		{"scp recorded remote", "git@git.example.test:org/repo.git", ""},
	}
}

// sshEdgeCases are ssh-looking clone URLs the host clone-URL check refuses
// before the ssh refusal is consulted: an upper-case SSH:// scheme (git
// takes the scheme as written) and a scheme-less host:path with no login
// (resolution reads it as a host/path and makes it an https URL with a
// non-numeric port).
func sshEdgeCases() []struct{ name, gitRemote, label string } {
	return []struct{ name, gitRemote, label string }{
		{"upper-case SSH label", "git.example.test/org/repo", "SSH://git@git.example.test/org/repo.git"},
		{"user-less scp label", "git.example.test/org/repo", "git.example.test:org/repo.git"},
	}
}

// The two ssh refusal texts, written out so a change to either is a test
// change.
const (
	sshUnsupportedText     = "an ssh repository URL is not supported for hub-side git; use an https repository URL"
	sshSecretHTTPSOnlyText = "a project git secret authenticates https only; use an https repository URL"
)

// sshRefusalWant is the text a hub-host git path answers for an ssh clone
// URL: the host clone-URL check's for an edge case, otherwise the ssh
// refusal with or without a project git secret.
func sshRefusalWant(edge, withSecret bool) string {
	switch {
	case edge:
		return provision.ErrHostCloneURLRefused.Error()
	case withSecret:
		return sshSecretHTTPSOnlyText
	default:
		return sshUnsupportedText
	}
}

type sshCase struct {
	name, gitRemote, label string
	edge                   bool
}

// sshAllCases are sshRefusalCases and sshEdgeCases, the edge ones marked.
func sshAllCases() []sshCase {
	var out []sshCase
	for _, c := range sshRefusalCases() {
		out = append(out, sshCase{c.name, c.gitRemote, c.label, false})
	}
	for _, c := range sshEdgeCases() {
		out = append(out, sshCase{c.name, c.gitRemote, c.label, true})
	}
	return out
}

// A workspace pull of an ssh repository URL is refused (422, fixed text)
// before git runs, with or without a project git secret; the text says
// which.
func TestHostGitAccess_PullSSHRefused(t *testing.T) {
	for i, tc := range sshAllCases() {
		for _, withSecret := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/secret=%t", tc.name, withSecret), func(t *testing.T) {
				record := fakeSSH(t)
				gitLog := hostGitFakeGit(t)
				srv, st, setToken := hostGitTokenServer(t)
				p := hostGitProvisionedProject(t, srv, st, fmt.Sprintf("host-git-ssh-%d-%t", i, withSecret), tc.gitRemote, tc.label)
				if withSecret {
					setToken(p.ID)
				}
				gitRuns := len(hostGitInvocations(t, gitLog))

				code, body := hostGitPull(t, srv, p.ID)
				require.Equal(t, http.StatusUnprocessableEntity, code, body)
				assert.Equal(t, sshRefusalWant(tc.edge, withSecret), decodeErrorMessage(t, []byte(body)))
				assert.False(t, sshRan(t, record), "git must not reach ssh")
				assert.Len(t, hostGitInvocations(t, gitLog), gitRuns, "git must not run")
			})
		}
	}
}

// The local-backend clone of an ssh repository URL is refused before git
// runs or the workspace is made, with or without a project git secret.
func TestCloneSharedWorkspaceProject_LocalSSHRefused(t *testing.T) {
	for i, tc := range sshAllCases() {
		for _, withSecret := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/secret=%t", tc.name, withSecret), func(t *testing.T) {
				record := fakeSSH(t)
				gitLog := hostGitFakeGit(t)
				srv, st, setToken := hostGitTokenServer(t)
				p := hostGitProject(t, st, fmt.Sprintf("host-git-ssh-clone-%d-%t", i, withSecret), tc.gitRemote, tc.label)
				if withSecret {
					setToken(p.ID)
				}
				err := srv.cloneSharedWorkspaceProject(context.Background(), p)
				require.Error(t, err)
				assert.Equal(t, sshRefusalWant(tc.edge, withSecret), err.Error())
				assert.False(t, sshRan(t, record), "git must not reach ssh")
				assert.Empty(t, hostGitInvocations(t, gitLog), "git must not run")
				ws, pathErr := localProjectPath(p.Slug)
				require.NoError(t, pathErr)
				_, statErr := os.Stat(ws)
				assert.True(t, errors.Is(statErr, os.ErrNotExist), "no workspace is made")
			})
		}
	}
}
