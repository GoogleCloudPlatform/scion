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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provisioning path ProvisionShared -> runGitClone (called host-side by the
// broker, and by `sciontool provision` in the k8s init container) clones the
// repository for a shared checkout. This guards that
// that provisioning clone goes through the hardened path, so a
// future change that bypasses it (dropping HostGitEnv, or the LFS skip-smudge) is
// caught:
//   - GIT_LFS_SKIP_SMUDGE=1 is set (git-lfs never contacts an endpoint named in a
//     cloned .lfsconfig with the host's credentials);
//   - GIT_* variables that choose a repository or add config are scrubbed
//     (util.HostGitEnv), e.g. a GIT_DIR in the broker environment does not reach
//     the clone;
//   - the remote authentication layer is applied (util.HostGitRemoteEnv): no
//     host credential is offered.
//
// The scoped credential helper (no unscoped credential.helper, no token in the
// URL) is covered by TestRunGitClone_UsesScopedHelperNotUnscoped; the no-endpoint
// contact behaviour by TestRunGitClone_SkipsLFSSmudge_NoEndpointContact.
func TestProvisioningClone_UsesHardenedEnv(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	// A fake git on PATH that records the environment of the `clone` invocation,
	// then runs the real git so provisioning actually succeeds.
	binDir := t.TempDir()
	envFile := filepath.Join(binDir, "clone-env.txt")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = clone ]; then env > '" + envFile + "'; break; fi; done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bareRepo := initBareGitRepo(t)
	in, _ := sharedPlainStateDirInput(t, bareRepo)

	// A GIT_DIR in the broker environment must be scrubbed by HostGitEnv and not
	// reach the clone. Set it only now, after the repo fixtures are built (which
	// run the real git), so it affects only the provisioning clone under test.
	t.Setenv("GIT_DIR", "/tmp/should-be-scrubbed")
	require.NoError(t, ProvisionShared(in))

	data, err := os.ReadFile(envFile)
	require.NoError(t, err, "the provisioning clone did not run through the fake git")
	recorded := string(data)

	assert.Contains(t, recorded, "GIT_LFS_SKIP_SMUDGE=1",
		"the provisioning clone must skip the LFS smudge")
	for _, line := range strings.Split(recorded, "\n") {
		assert.NotEqual(t, "GIT_DIR=/tmp/should-be-scrubbed", line,
			"HostGitEnv must scrub GIT_DIR from the provisioning clone env")
	}
	// The clone offers no host credential (the remote layer, no token here),
	// and still succeeds without the host global config.
	checkRemoteLayerEnv(t, strings.Split(strings.TrimSuffix(recorded, "\n"), "\n"), "", "")
}
