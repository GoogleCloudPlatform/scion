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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const (
	hsTestAgentID   = "0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10"
	hsTestProjectID = "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7"
)

// hsSettings returns global settings with a gke profile on a Kubernetes
// runtime and a docker profile, a complete shared_dir_storage nfs block
// and server.home_storage with the development gate set.
func hsSettings() *config.VersionedSettings {
	return &config.VersionedSettings{
		ActiveProfile: "local",
		Server: &config.V1ServerConfig{
			SharedDirStorage: &config.V1SharedDirStorageConfig{
				Backend: "local",
				NFS: &config.V1NFSConfig{
					MountRoot:   "/srv/nfs",
					SubPathRoot: "trees",
					Shares:      []config.V1NFSShare{{ID: "share-1", PVName: "pv-1"}},
				},
			},
			HomeStorage: &config.V1HomeStorageConfig{AllowIncompletePhases: true},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"docker": {Type: "docker"},
			"k8s":    {Type: "kubernetes"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"local": {Runtime: "docker", HomeStorageBackend: "nfs", SharedDirStorageBackend: "nfs"},
			"gke":   {Runtime: "k8s", HomeStorageBackend: "nfs", SharedDirStorageBackend: "nfs"},
		},
	}
}

type hsHarness struct {
	t        *testing.T
	agentDir string
	gs       *config.VersionedSettings
	loadErr  error
	loads    int
}

func newHSHarness(t *testing.T) *hsHarness {
	t.Helper()
	old := homeStorageNFSAvailable
	homeStorageNFSAvailable = true
	t.Cleanup(func() { homeStorageNFSAvailable = old })
	h := &hsHarness{t: t, agentDir: t.TempDir(), gs: hsSettings()}
	require.NoError(t, markHomeStoragePending(h.agentDir))
	return h
}

func (h *hsHarness) input(runtimeName, profile string) homeStorageInput {
	return homeStorageInput{
		AgentDir:     h.agentDir,
		AgentName:    "agent-a",
		Slug:         "agent-a",
		RuntimeName:  runtimeName,
		Profile:      profile,
		AgentID:      hsTestAgentID,
		ProjectID:    hsTestProjectID,
		ExperimentOn: true,
		LoadSettings: func() (*config.VersionedSettings, error) {
			h.loads++
			return h.gs, h.loadErr
		},
	}
}

func (h *hsHarness) record() *homeStorageRecord {
	h.t.Helper()
	rec, err := readHomeStorageRecord(h.agentDir)
	require.NoError(h.t, err)
	return rec
}

func TestResolveHomeStorage_FirstStartNFS(t *testing.T) {
	h := newHSHarness(t)
	plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
	require.NoError(t, err)
	assert.Equal(t, &homeStoragePlan{
		Backend:     "nfs",
		Leaf:        "pod",
		ShareID:     "share-1",
		PVClaimName: "pv-1",
		SubPathRoot: "trees",
		MountRoot:   "/srv/nfs",
		AgentID:     hsTestAgentID,
		Settings:    h.gs.Server.HomeStorage,
	}, plan)
	assert.Equal(t, &homeStorageRecord{Backend: "nfs", Leaf: "pod", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees"}, h.record())
}

func TestResolveHomeStorage_LeafFromSettings(t *testing.T) {
	h := newHSHarness(t)
	p := h.gs.Profiles["gke"]
	p.HomeStorageLeaf = "broker"
	h.gs.Profiles["gke"] = p
	plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
	require.NoError(t, err)
	assert.Equal(t, "broker", plan.Leaf)
	assert.Equal(t, "broker", h.record().Leaf)
}

// A dispatch to any runtime other than Kubernetes gets a local home whatever
// the global, runtime or profile settings and the record say. It reads no
// settings, writes no record and resolves no share.
func TestResolveHomeStorage_NonKubernetesIsAlwaysLocal(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container", "cloudrun", ""} {
		t.Run(rt, func(t *testing.T) {
			h := newHSHarness(t)
			h.gs.Server.HomeStorage.Backend = "nfs"
			r := h.gs.Runtimes["docker"]
			r.HomeStorageBackend = "nfs"
			h.gs.Runtimes["docker"] = r
			before, err := os.ReadFile(filepath.Join(h.agentDir, homeStorageRecordFile))
			require.NoError(t, err)

			plan, err := resolveHomeStorage(h.input(rt, "local"))
			require.NoError(t, err)
			assert.Equal(t, localHomeStoragePlan(), plan)
			assert.Equal(t, 0, h.loads, "a non-Kubernetes start must not read settings")
			after, err := os.ReadFile(filepath.Join(h.agentDir, homeStorageRecordFile))
			require.NoError(t, err)
			assert.Equal(t, before, after, "a non-Kubernetes start must not change the record")

			// Even an agent recorded with an NFS home.
			require.NoError(t, writeHomeStorageRecord(h.agentDir, homeStorageRecord{Backend: "nfs", Leaf: "pod", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees"}))
			plan, err = resolveHomeStorage(h.input(rt, "local"))
			require.NoError(t, err)
			assert.Equal(t, "local", plan.Backend)
			assert.Equal(t, 0, h.loads)
		})
	}
}

func TestResolveHomeStorage_FirstStartLocalCases(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(h *hsHarness, in *homeStorageInput)
	}{
		{"settings off", func(h *hsHarness, in *homeStorageInput) {
			p := h.gs.Profiles["gke"]
			p.HomeStorageBackend = ""
			h.gs.Profiles["gke"] = p
		}},
		{"profile local over global nfs", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.HomeStorage.Backend = "nfs"
			p := h.gs.Profiles["gke"]
			p.HomeStorageBackend = "local"
			h.gs.Profiles["gke"] = p
		}},
		{"experiment off", func(h *hsHarness, in *homeStorageInput) { in.ExperimentOn = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHSHarness(t)
			in := h.input("kubernetes", "gke")
			tt.mutate(h, &in)
			plan, err := resolveHomeStorage(in)
			require.NoError(t, err)
			assert.Equal(t, "local", plan.Backend)
			assert.Equal(t, &homeStorageRecord{Backend: "local"}, h.record(), "the local choice is recorded")
		})
	}
}

// Every first-start failure leaves the record pending, so nothing is
// decided until a start succeeds.
func TestResolveHomeStorage_FirstStartFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(h *hsHarness, in *homeStorageInput)
		wantErr string
	}{
		{"shared-dir storage not nfs", func(h *hsHarness, in *homeStorageInput) {
			p := h.gs.Profiles["gke"]
			p.SharedDirStorageBackend = "local"
			h.gs.Profiles["gke"] = p
		}, "needs shared_dir_storage nfs for profile \"gke\""},
		{"shared-dir storage unset", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage = nil
		}, "shared_dir_storage nfs block for profile \"gke\""},
		{"shared-dir nfs block incomplete", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage.NFS.MountRoot = ""
		}, "complete shared_dir_storage nfs block"},
		{"no claim", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage.NFS.Shares[0].PVName = ""
		}, "needs a claim"},
		{"no agent ID", func(h *hsHarness, in *homeStorageInput) { in.AgentID = "" }, "home_storage_unavailable: no agent ID"},
		{"agent ID is the name", func(h *hsHarness, in *homeStorageInput) { in.AgentID = "agent-a" }, "no agent ID"},
		{"agent ID equals slug", func(h *hsHarness, in *homeStorageInput) { in.Slug = hsTestAgentID }, "no agent ID"},
		{"no project ID", func(h *hsHarness, in *homeStorageInput) { in.ProjectID = "" }, "no valid hub project ID"},
		{"bad project ID", func(h *hsHarness, in *homeStorageInput) { in.ProjectID = "../x" }, "no valid hub project ID"},
		{"development gate unset", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.HomeStorage.AllowIncompletePhases = false
		}, "allow_incomplete_phases"},
		{"no home_storage block", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.HomeStorage = nil
		}, "allow_incomplete_phases"},
		{"invalid home_storage block", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.HomeStorage.StopGraceSeconds = -1
		}, "stop_grace_seconds"},
		{"not available in this build", func(h *hsHarness, in *homeStorageInput) {
			homeStorageNFSAvailable = false
		}, "not available in this version"},
		{"settings mention home storage but do not load", func(h *hsHarness, in *homeStorageInput) {
			h.loadErr = errors.New("bad yaml")
		}, "bad yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHSHarness(t)
			in := h.input("kubernetes", "gke")
			tt.mutate(h, &in)
			_, err := resolveHomeStorage(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, &homeStorageRecord{Backend: homeStoragePending}, h.record())
		})
	}
}

// Settings that cannot be loaded and never mention home storage give, and
// record, a local home, so the choice is made once.
func TestResolveHomeStorage_UnloadableSettingsWithoutHomeStorage(t *testing.T) {
	h := newHSHarness(t)
	h.loadErr = errHomeStorageNotConfigured
	plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
	require.NoError(t, err)
	assert.Equal(t, "local", plan.Backend)
	assert.Equal(t, &homeStorageRecord{Backend: "local"}, h.record())
}

// Settings values are checked where they are used: an unknown backend or
// leaf value fails the start, names its key, and is never recorded.
func TestResolveHomeStorage_UnknownValuesFailAndAreNotRecorded(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(h *hsHarness, in *homeStorageInput)
		wantErr string
	}{
		{"backend wrong case", func(h *hsHarness, in *homeStorageInput) {
			p := h.gs.Profiles["gke"]
			p.HomeStorageBackend = "NFS"
			h.gs.Profiles["gke"] = p
		}, "profiles.gke.home_storage_backend"},
		{"backend wrong case, experiment off", func(h *hsHarness, in *homeStorageInput) {
			p := h.gs.Profiles["gke"]
			p.HomeStorageBackend = "NFS"
			h.gs.Profiles["gke"] = p
			in.ExperimentOn = false
		}, "profiles.gke.home_storage_backend"},
		{"global backend typo", func(h *hsHarness, in *homeStorageInput) {
			p := h.gs.Profiles["gke"]
			p.HomeStorageBackend = ""
			h.gs.Profiles["gke"] = p
			h.gs.Server.HomeStorage.Backend = "nfs "
		}, "server.home_storage.backend"},
		{"leaf wrong case", func(h *hsHarness, in *homeStorageInput) {
			r := h.gs.Runtimes["k8s"]
			r.HomeStorageLeaf = "Broker"
			h.gs.Runtimes["k8s"] = r
		}, "runtimes.k8s.home_storage_leaf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHSHarness(t)
			in := h.input("kubernetes", "gke")
			tt.mutate(h, &in)
			_, err := resolveHomeStorage(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, &homeStorageRecord{Backend: homeStoragePending}, h.record())
		})
	}
}

// Every path component is checked: the slug and the subpath root, on the
// first start and when read back from the record.
func TestResolveHomeStorage_PathComponents(t *testing.T) {
	for _, slug := range []string{"", "My Agent", "a/b", ".."} {
		h := newHSHarness(t)
		in := h.input("kubernetes", "gke")
		in.Slug = slug
		_, err := resolveHomeStorage(in)
		require.Error(t, err, "slug %q", slug)
		assert.Equal(t, homeStoragePending, h.record().Backend)
	}
	for _, root := range []string{"../x", "/abs", "a/../b", "a//b", ".", "./a"} {
		h := newHSHarness(t)
		h.gs.Server.SharedDirStorage.NFS.SubPathRoot = root
		_, err := resolveHomeStorage(h.input("kubernetes", "gke"))
		require.Error(t, err, "subpath root %q", root)
		assert.Contains(t, err.Error(), "subpath")
		assert.Equal(t, homeStoragePending, h.record().Backend)

		// Read back from a record.
		h2 := newHSHarness(t)
		require.NoError(t, os.WriteFile(filepath.Join(h2.agentDir, homeStorageRecordFile),
			[]byte(`{"backend":"nfs","leaf":"pod","share_id":"share-1","pv_claim_name":"pv-1","subpath_root":"`+root+`"}`), 0o644))
		_, err = resolveHomeStorage(h2.input("kubernetes", "gke"))
		require.Error(t, err, "recorded subpath root %q", root)
	}
	// A recorded slug is the agent's own; a start whose slug is not a slug
	// fails even with a valid record.
	h := newHSHarness(t)
	require.NoError(t, writeHomeStorageRecord(h.agentDir, homeStorageRecord{Backend: "nfs", Leaf: "pod", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees"}))
	in := h.input("kubernetes", "gke")
	in.Slug = "Not A Slug"
	_, err := resolveHomeStorage(in)
	require.Error(t, err)
}

// A non-Kubernetes start does not read the record, so even a damaged one
// gives a local home without an error.
func TestResolveHomeStorage_NonKubernetesIgnoresDamagedRecord(t *testing.T) {
	h := newHSHarness(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.agentDir, homeStorageRecordFile), []byte("not json"), 0o644))
	plan, err := resolveHomeStorage(h.input("docker", "local"))
	require.NoError(t, err)
	assert.Equal(t, "local", plan.Backend)
	_, err = resolveHomeStorage(h.input("kubernetes", "gke"))
	assert.Error(t, err, "a Kubernetes start does read, and refuse, the damaged record")
}

// This version starts agents with an NFS home (behind the experiment and
// the development gate).
func TestHomeStorageNFSAvailable_On(t *testing.T) {
	assert.True(t, homeStorageNFSAvailable)
}

// After the first start the record decides, whatever the settings say now.
func TestResolveHomeStorage_RecordReadBack(t *testing.T) {
	t.Run("nfs record survives settings changes", func(t *testing.T) {
		h := newHSHarness(t)
		_, err := resolveHomeStorage(h.input("kubernetes", "gke"))
		require.NoError(t, err)

		// Settings now select a local home, another leaf mode, another
		// first share and another subpath root; the recorded share is
		// still configured.
		p := h.gs.Profiles["gke"]
		p.HomeStorageBackend = "local"
		p.HomeStorageLeaf = "broker"
		p.SharedDirStorageBackend = "local"
		h.gs.Profiles["gke"] = p
		h.gs.Server.SharedDirStorage.NFS.SubPathRoot = "other"
		h.gs.Server.SharedDirStorage.NFS.MountRoot = "/mnt/new"
		h.gs.Server.SharedDirStorage.NFS.Shares = []config.V1NFSShare{{ID: "share-2", PVName: "pv-2"}, {ID: "share-1", PVName: "pv-1"}}

		plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
		require.NoError(t, err)
		assert.Equal(t, "nfs", plan.Backend)
		assert.Equal(t, "pod", plan.Leaf)
		assert.Equal(t, "share-1", plan.ShareID)
		assert.Equal(t, "pv-1", plan.PVClaimName)
		assert.Equal(t, "trees", plan.SubPathRoot)
		assert.Equal(t, "/mnt/new", plan.MountRoot, "the host mount root follows the current settings")
		assert.Equal(t, "nfs", h.record().Backend)
	})
	t.Run("local record survives settings changes", func(t *testing.T) {
		h := newHSHarness(t)
		require.NoError(t, writeHomeStorageRecord(h.agentDir, homeStorageRecord{Backend: "local"}))
		plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
		require.NoError(t, err)
		assert.Equal(t, "local", plan.Backend)
		assert.Equal(t, 0, h.loads)
	})
	t.Run("no record means an agent created before the record: local", func(t *testing.T) {
		h := newHSHarness(t)
		require.NoError(t, os.Remove(filepath.Join(h.agentDir, homeStorageRecordFile)))
		plan, err := resolveHomeStorage(h.input("kubernetes", "gke"))
		require.NoError(t, err)
		assert.Equal(t, "local", plan.Backend)
		assert.Equal(t, 0, h.loads)
		assert.Equal(t, &homeStorageRecord{Backend: "local"}, h.record())
	})
}

func TestResolveHomeStorage_RecordedNFSFailures(t *testing.T) {
	nfsRecord := homeStorageRecord{Backend: "nfs", Leaf: "pod", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees"}
	tests := []struct {
		name    string
		mutate  func(h *hsHarness, in *homeStorageInput)
		wantErr string
	}{
		{"share removed", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage.NFS.Shares = []config.V1NFSShare{{ID: "share-2", PVName: "pv-2"}}
		}, "recorded home share share-1 not configured"},
		{"share has another claim", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage.NFS.Shares[0].PVName = "pv-other"
		}, "recorded home share share-1 not configured"},
		{"no shared_dir_storage", func(h *hsHarness, in *homeStorageInput) {
			h.gs.Server.SharedDirStorage = nil
		}, "not configured"},
		{"experiment off", func(h *hsHarness, in *homeStorageInput) { in.ExperimentOn = false }, "experiment is off"},
		{"no agent ID", func(h *hsHarness, in *homeStorageInput) { in.AgentID = "" }, "no agent ID"},
		{"unloadable settings", func(h *hsHarness, in *homeStorageInput) {
			h.loadErr = errHomeStorageNotConfigured
		}, "loading global settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHSHarness(t)
			require.NoError(t, writeHomeStorageRecord(h.agentDir, nfsRecord))
			in := h.input("kubernetes", "gke")
			tt.mutate(h, &in)
			plan, err := resolveHomeStorage(in)
			require.Error(t, err, "got plan %+v", plan)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, &nfsRecord, h.record(), "a failed start never changes the record")
		})
	}
}

func TestReadHomeStorageRecord_Damaged(t *testing.T) {
	for _, content := range []string{
		"not json",
		`{}`,
		`{"backend":"disk"}`,
		`{"backend":"nfs","leaf":"pod","share_id":"s"}`,
		`{"backend":"nfs","leaf":"node","share_id":"s","pv_claim_name":"c","subpath_root":"r"}`,
	} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, homeStorageRecordFile), []byte(content), 0o644))
		_, err := readHomeStorageRecord(dir)
		assert.Error(t, err, content)
	}
	rec, err := readHomeStorageRecord(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestMarkHomeStoragePending_KeepsExistingRecord(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeHomeStorageRecord(dir, homeStorageRecord{Backend: "local"}))
	require.NoError(t, markHomeStoragePending(dir))
	rec, err := readHomeStorageRecord(dir)
	require.NoError(t, err)
	assert.Equal(t, "local", rec.Backend)
}

func TestValidHomeAgentID(t *testing.T) {
	tests := []struct {
		id, slug string
		want     bool
	}{
		{hsTestAgentID, "agent-a", true},
		{"", "agent-a", false},
		{"agent-a", "agent-a", false},
		{strings.ToUpper(hsTestAgentID), "agent-a", false},
		{"0b9f6a523c1e4f439d8e2a6f1c7b5e10", "agent-a", false},
		{"{" + hsTestAgentID + "}", "agent-a", false},
		{hsTestAgentID, hsTestAgentID, false},
		{"0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e1/", "agent-a", false},
		{"../9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10", "agent-a", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, validHomeAgentID(tt.id, tt.slug), "%q", tt.id)
	}
}

// ProvisionAgent marks a new agent pending, and provisioning an existing
// agent again keeps its record.
func TestProvisionAgent_HomeStorageRecord(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	require.NoError(t, config.InitMachine(getTestHarnesses()))
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	require.NoError(t, config.InitProject(projectScionDir, getTestHarnesses()))
	require.NoError(t, os.Chdir(projectDir))

	_, _, _, err := ProvisionAgent(context.Background(), "hs-agent", "default", "", "claude", projectScionDir, "", "", "", "")
	require.NoError(t, err)
	agentDir := filepath.Join(projectScionDir, "agents", "hs-agent")
	rec, err := readHomeStorageRecord(agentDir)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, homeStoragePending, rec.Backend)

	require.NoError(t, writeHomeStorageRecord(agentDir, homeStorageRecord{Backend: "local"}))
	_, _, _, err = ProvisionAgent(context.Background(), "hs-agent", "default", "", "claude", projectScionDir, "", "", "", "")
	require.NoError(t, err)
	rec, err = readHomeStorageRecord(agentDir)
	require.NoError(t, err)
	assert.Equal(t, "local", rec.Backend, "provisioning an existing agent keeps its record")

	// An agent created before the record existed has none; provisioning it
	// again must not mark it pending, so it keeps a local home.
	require.NoError(t, os.Remove(filepath.Join(agentDir, homeStorageRecordFile)))
	_, _, _, err = ProvisionAgent(context.Background(), "hs-agent", "default", "", "claude", projectScionDir, "", "", "", "")
	require.NoError(t, err)
	rec, err = readHomeStorageRecord(agentDir)
	require.NoError(t, err)
	assert.Nil(t, rec, "re-provisioning a pre-feature agent writes no record")
}

// On a later start the current shared_dir_storage nfs block must be valid
// before its mount root is used.
func TestResolveHomeStorage_RecordedNFSNeedsValidNFSBlock(t *testing.T) {
	h := newHSHarness(t)
	require.NoError(t, writeHomeStorageRecord(h.agentDir, homeStorageRecord{Backend: "nfs", Leaf: "broker", ShareID: "share-1", PVClaimName: "pv-1", SubPathRoot: "trees"}))
	h.gs.Server.SharedDirStorage.NFS.MountRoot = ""
	_, err := resolveHomeStorage(h.input("kubernetes", "gke"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.shared_dir_storage.nfs is not valid")
	h.gs.Server.SharedDirStorage.NFS.MountRoot = "/srv/nfs"
	h.gs.Server.SharedDirStorage.NFS.SubPathRoot = "../x"
	_, err = resolveHomeStorage(h.input("kubernetes", "gke"))
	require.Error(t, err)
}

func TestFirstNFSShare(t *testing.T) {
	for _, sd := range []*config.V1SharedDirStorageConfig{
		nil,
		{Backend: "nfs"},
		{Backend: "nfs", NFS: &config.V1NFSConfig{}},
	} {
		_, err := firstNFSShare(sd)
		assert.Error(t, err, "%+v", sd)
	}
	share, err := firstNFSShare(&config.V1SharedDirStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{Shares: []config.V1NFSShare{{ID: "s", PVName: "pv"}}}})
	require.NoError(t, err)
	assert.Equal(t, "s", share.ID)
}
