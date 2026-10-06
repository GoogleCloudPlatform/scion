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

package runtime

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func envValue(envs []corev1.EnvVar, name string) (string, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// The best-effort chown variable appears only on the provisioning
// (lock-winner) init container, and only when the broker created the
// workspace directory. It never reaches the agent container.
func TestBuildPod_ChownBestEffortEnv_OnlyWhenPreCreatedOnWinner(t *testing.T) {
	cases := []struct {
		name       string
		preCreated bool
		lockLost   bool
		nonGit     bool
		want       bool
	}{
		{name: "pre-created winner", preCreated: true, want: true},
		{name: "pre-created winner, non-git", preCreated: true, nonGit: true, want: true},
		{name: "not pre-created winner", preCreated: false, want: false},
		{name: "pre-created loser", preCreated: true, lockLost: true, want: false},
		{name: "not pre-created loser", lockLost: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := nfsBaseConfig("chown-best-effort")
			cfg.NFSWorkspacePreCreated = tc.preCreated
			cfg.nfsProvisionLockLost = tc.lockLost
			if tc.nonGit {
				cfg.GitCloneForInit = nil
			}

			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			require.NoError(t, err)
			require.Len(t, pod.Spec.InitContainers, 1)

			value, ok := envValue(pod.Spec.InitContainers[0].Env, provision.ChownBestEffortEnv)
			assert.Equal(t, tc.want, ok, "init container env presence")
			if tc.want {
				assert.Equal(t, "1", value)
			}
			for _, c := range pod.Spec.Containers {
				_, onMain := envValue(c.Env, provision.ChownBestEffortEnv)
				assert.False(t, onMain, "must never be set on the agent container")
			}
		})
	}
}

// Local-backend pods have no init container and never get the variable,
// even if the field were somehow set.
func TestBuildPod_ChownBestEffortEnv_NeverOnLocalBackend(t *testing.T) {
	cfg := RunConfig{
		Name:                   "local-chown",
		Image:                  "test-image",
		UnixUsername:           "scion",
		NFSWorkspacePreCreated: true,
		GitCloneForInit:        &api.GitCloneConfig{URL: "https://github.com/example/repo.git"},
	}
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	assert.Empty(t, pod.Spec.InitContainers)
	for _, c := range pod.Spec.Containers {
		_, ok := envValue(c.Env, provision.ChownBestEffortEnv)
		assert.False(t, ok)
	}
}

// The init container's command line is identical with and without the
// pre-created marker: the signal is an env var only, so an older sciontool
// in the agent image parses the same arguments as before, ignores the
// variable, and keeps a chown failure fatal exactly as it does today.
func TestBuildPod_ChownBestEffort_CommandUnchanged(t *testing.T) {
	for _, nonGit := range []bool{false, true} {
		base := nfsBaseConfig("chown-cmd")
		if nonGit {
			base.GitCloneForInit = nil
		}
		pre := base
		pre.NFSWorkspacePreCreated = true

		podBase, err := newNFSTestK8sRuntime().buildPod("default", base)
		require.NoError(t, err)
		podPre, err := newNFSTestK8sRuntime().buildPod("default", pre)
		require.NoError(t, err)

		icBase, icPre := podBase.Spec.InitContainers[0], podPre.Spec.InitContainers[0]
		assert.Equal(t, icBase.Command, icPre.Command, "command must not change (nonGit=%v)", nonGit)
		assert.Equal(t, icBase.Args, icPre.Args)
		assert.Equal(t, icBase.SecurityContext, icPre.SecurityContext)
		assert.Equal(t, icBase.VolumeMounts, icPre.VolumeMounts)
		assert.Equal(t, len(icBase.Env)+1, len(icPre.Env), "only the one variable is added")
		assert.Equal(t, podBase.Spec.Containers, podPre.Spec.Containers, "agent container must be unchanged")
	}
}
