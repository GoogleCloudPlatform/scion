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

//go:build !windows

package provision

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusedCloneURLs returns clone URLs every ProvisionShared call refuses: a
// local repository path (not registered as a fixture), the same repository
// as file://, plain http, git's <transport>::<address> form and a value
// starting with "-".
func refusedCloneURLs(t *testing.T) map[string]string {
	t.Helper()
	bare := newBareGitRepo(t)
	return map[string]string{
		"local path":   bare,
		"file":         "file://" + bare,
		"http":         "http://git.example.com/org/repo.git",
		"transport":    "ext::sh -c touch% /tmp/x",
		"leading dash": "--upload-pack=touch /tmp/x",
	}
}

// ProvisionShared refuses a clone URL CheckHostCloneURL refuses, in every
// mode, before it creates or clones anything, and also when the workspace
// is already provisioned. The inputs here are not hardened, so the refusal
// comes from the URL check alone.
func TestProvisionShared_RefusesHostCloneURLs(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	for name, cloneURL := range refusedCloneURLs(t) {
		for _, mode := range []store.WorkspaceSharingMode{store.SharingModeSharedPlain, store.SharingModeWorktreePerAgent} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				root := t.TempDir()
				hostPath := filepath.Join(root, "workspace")
				in := sharedPlainInput(hostPath, cloneURL)
				in.SentinelDir = root
				in.Mode = mode
				in.AgentID, in.AgentName = "agent-1", "agent-1"

				err := ProvisionShared(in)
				require.ErrorIs(t, err, ErrHostCloneURLRefused)
				assert.NoDirExists(t, hostPath, "nothing is created for a refused URL")
				assert.NoFileExists(t, filepath.Join(root, ProvisionSentinelFile))
			})
		}
	}
}

func TestProvisionShared_RefusesHostCloneURLWhenAlreadyProvisioned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	hostPath := t.TempDir()
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, origin)))
	require.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))

	for name, cloneURL := range refusedCloneURLs(t) {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, ProvisionShared(sharedPlainInput(hostPath, cloneURL)), ErrHostCloneURLRefused)
		})
	}
}

func TestCheckProvisionCloneURL(t *testing.T) {
	for _, u := range []string{
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"git://git.example.com/org/repo.git",
		"git@github.com:org/repo.git",
	} {
		assert.NoError(t, checkProvisionCloneURL(sharedPlainInput("/unused", u)), u)
	}
	// Not git-backed: nothing to check.
	assert.NoError(t, checkProvisionCloneURL(ProvisionInput{}))
	assert.NoError(t, checkProvisionCloneURL(ProvisionInput{GitClone: &api.GitCloneConfig{}}))
	for name, u := range refusedCloneURLs(t) {
		assert.ErrorIs(t, checkProvisionCloneURL(sharedPlainInput("/unused", u)), ErrHostCloneURLRefused, name)
	}
}

// A registration covers exactly the registered string, only while its test
// runs.
func TestAllowLocalCloneFixture_ExactURLForTheTestOnly(t *testing.T) {
	bare := newBareGitRepo(t)
	t.Run("registered", func(t *testing.T) {
		AllowLocalCloneFixture(t, bare)
		assert.NoError(t, checkProvisionCloneURL(sharedPlainInput("/unused", bare)))
		assert.ErrorIs(t, checkProvisionCloneURL(sharedPlainInput("/unused", "file://"+bare)), ErrHostCloneURLRefused)
		assert.ErrorIs(t, checkProvisionCloneURL(sharedPlainInput("/unused", bare+"/")), ErrHostCloneURLRefused)
	})
	assert.ErrorIs(t, checkProvisionCloneURL(sharedPlainInput("/unused", bare)), ErrHostCloneURLRefused,
		"the registration ends with the test that made it")
}

// A registered fixture is cloned even when the input asks for a hardened
// clone, so a caller that always hardens can still clone a test fixture.
func TestRunGitClone_RegisteredFixtureClonesWhenHardened(t *testing.T) {
	isolateGitConfig(t)
	bare := initBareGitRepo(t)
	depth := 0
	dest := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, runGitClone(context.Background(), ProvisionInput{
		GitClone:        &api.GitCloneConfig{URL: bare, Depth: &depth},
		Resolved:        ResolvedWorkspace{HostPath: dest},
		HostGitHardened: true,
	}))
	assert.FileExists(t, filepath.Join(dest, "README.md"))
}

// An ordinary binary (not a test binary) never honours a fixture
// registration: it builds testdata/prodprobe, which registers a local
// repository and asks ProvisionShared to clone it, and checks that the
// clone URL is refused, that the binary is not a test binary, and that
// importing testing did not run testing.Init (no test flags registered).
func TestAllowLocalCloneFixture_NotHonouredOutsideTestBinary(t *testing.T) {
	bare := newBareGitRepo(t)
	dir := t.TempDir()
	probe := filepath.Join(dir, "prodprobe")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", probe, "./testdata/prodprobe")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build prodprobe: %s", out)

	workspace := filepath.Join(dir, "workspace")
	run := exec.Command(probe, bare, workspace)
	run.Env = append(os.Environ(), "SCION_HOST_UID=")
	out, err = run.CombinedOutput()
	var exitErr *exec.ExitError
	require.False(t, errors.As(err, &exitErr) && exitErr.ExitCode() == 2, "prodprobe usage: %s", out)
	require.NoError(t, err, "prodprobe: %s", out)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	assert.Equal(t, "testing=false init=false refused=true", lines[len(lines)-1])
	assert.NoFileExists(t, filepath.Join(workspace, "README.md"))
}

// A hardened clone never follows an HTTP redirect: git, as run by
// ProvisionShared for a hardened input, receives -c http.followRedirects=false
// ahead of "clone", and no later setting of that key. (A behavioural redirect
// test is not possible here: a hardened clone refuses plain http, and the
// clone environment drops GIT_SSL_* settings, so a test TLS server cannot be
// trusted.) Removing the setting from HostCloneGitArgs makes this fail.
func TestProvisionShared_HardenedCloneNeverFollowsRedirects(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	record := fakeGit(t)
	hostPath := t.TempDir()
	in := sharedPlainInput(hostPath, "https://git.example.com/org/repo.git")
	in.HostGitHardened = true

	require.Error(t, ProvisionShared(in), "the fake git fails")
	argv, allow := readArgv(t, record)
	cloneAt := -1
	for i, a := range argv {
		if a == "clone" {
			cloneAt = i
			break
		}
	}
	require.Positive(t, cloneAt, "git was run with clone: %q", argv)
	last := ""
	for i := 0; i+1 < cloneAt; i++ {
		if argv[i] == "-c" && strings.HasPrefix(argv[i+1], "http.followRedirects=") {
			last = argv[i+1]
		}
	}
	assert.Equal(t, "http.followRedirects=false", last, "the last http.followRedirects setting ahead of clone")
	assert.Equal(t, HostCloneProtocols, allow)
}
