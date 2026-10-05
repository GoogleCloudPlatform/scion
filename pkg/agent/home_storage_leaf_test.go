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

package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// spyHomeLeafHost records every host operation.
type spyHomeLeafHost struct {
	calls    []string
	mountErr error
}

func (s *spyHomeLeafHost) CheckMount(hostBase string) error {
	s.calls = append(s.calls, "check "+hostBase)
	return s.mountErr
}

func (s *spyHomeLeafHost) EnsureHome(hostBase, agentDirRel, homeName string, gid int) error {
	s.calls = append(s.calls, "ensure "+hostBase+" "+agentDirRel+" "+homeName)
	return nil
}

func withSpyHomeLeaf(t *testing.T) *spyHomeLeafHost {
	t.Helper()
	spy := &spyHomeLeafHost{}
	old := homeLeafOps
	homeLeafOps = spy
	t.Cleanup(func() { homeLeafOps = old })
	return spy
}

func nfsPlan(leaf string) *homeStoragePlan {
	return &homeStoragePlan{
		Backend: "nfs", Leaf: leaf, ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees",
		MountRoot: "/srv/nfs", AgentID: hsTestAgentID,
		Settings: &config.V1HomeStorageConfig{StopGraceSeconds: 40},
	}
}

// Pod leaf mode does nothing on the host: no mount check and no directory
// is created through the broker.
func TestPrepareHomeStorage_PodLeafTouchesNoHostPath(t *testing.T) {
	spy := withSpyHomeLeaf(t)
	got, err := prepareHomeStorage(nfsPlan("pod"), hsTestProjectID, "agent-a")
	require.NoError(t, err)
	assert.Empty(t, spy.calls, "pod leaf mode must not touch the host")
	assert.Equal(t, &runtime.HomeStorageRealization{
		PVClaimName: "pv-1", SubPathRoot: "trees", ProjectID: hsTestProjectID, AgentSlug: "agent-a",
		AgentID: hsTestAgentID, Leaf: "pod", GID: 1000,
		StopGraceSeconds: 40, TerminationWaitSeconds: 15, SkeletonMaxBytes: 256 << 20,
	}, got)

	// A local plan (every non-Kubernetes dispatch) yields no home and no
	// host operation either.
	got, err = prepareHomeStorage(localHomeStoragePlan(), hsTestProjectID, "agent-a")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, spy.calls)
}

func TestPrepareHomeStorage_BrokerLeaf(t *testing.T) {
	spy := withSpyHomeLeaf(t)
	got, err := prepareHomeStorage(nfsPlan("broker"), hsTestProjectID, "agent-a")
	require.NoError(t, err)
	assert.Equal(t, "broker", got.Leaf)
	assert.Equal(t, []string{
		"check /srv/nfs/share-1",
		"ensure /srv/nfs/share-1 trees/" + hsTestProjectID + "/agents/agent-a home-" + hsTestAgentID,
	}, spy.calls)
}

// A missing host mount fails only this dispatch, before any directory is
// created.
func TestPrepareHomeStorage_BrokerLeafMissingMount(t *testing.T) {
	spy := withSpyHomeLeaf(t)
	spy.mountErr = errors.New("not an NFS mount")
	_, err := prepareHomeStorage(nfsPlan("broker"), hsTestProjectID, "agent-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "home_storage_unavailable: host mount /srv/nfs/share-1 missing")
	assert.Equal(t, []string{"check /srv/nfs/share-1"}, spy.calls)

	p := nfsPlan("broker")
	p.MountRoot = ""
	_, err = prepareHomeStorage(p, hsTestProjectID, "agent-a")
	require.Error(t, err)
}

func TestPrepareHomeStorage_InvalidPathComponents(t *testing.T) {
	spy := withSpyHomeLeaf(t)
	for _, tc := range []struct{ project, slug string }{{"../x", "agent-a"}, {hsTestProjectID, "../a"}, {"", "agent-a"}} {
		_, err := prepareHomeStorage(nfsPlan("broker"), tc.project, tc.slug)
		assert.Error(t, err, "%+v", tc)
	}
	assert.Empty(t, spy.calls, "nothing is touched for an invalid path")
}
