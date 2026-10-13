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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// Changes of a shared dir's backend from nfs back to local.

func TestChangeSharedDirBackends_ToLocal(t *testing.T) {
	notes := map[string]string{"notes": "local"}
	tests := []struct {
		name       string
		rec        sharedDirStorageRecord
		allowEmpty bool
		want       sharedDirStorageRecord
	}{
		{
			name: "nfs entry on a local default",
			rec:  sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}},
			want: sharedDirStorageRecord{Backend: "local", Previous: map[string]string{"notes": "nfs"}},
		},
		{
			name: "nfs default",
			rec:  sharedDirStorageRecord{Backend: "nfs"},
			want: sharedDirStorageRecord{Backend: "nfs", Dirs: notes, Previous: map[string]string{"notes": "nfs"}},
		},
		{
			name:       "nfs default, empty allowed",
			rec:        sharedDirStorageRecord{Backend: "nfs"},
			allowEmpty: true,
			want:       sharedDirStorageRecord{Backend: "nfs", Dirs: notes},
		},
		{
			name: "already local",
			rec:  sharedDirStorageRecord{Backend: "local"},
			want: sharedDirStorageRecord{Backend: "local"},
		},
		{
			name: "back before any start drops the pending check",
			rec:  sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}, Previous: map[string]string{"notes": "local", "other": "local"}},
			want: sharedDirStorageRecord{Backend: "local", Previous: map[string]string{"other": "local"}},
		},
		{
			name:       "empty allowed clears a pending check",
			rec:        sharedDirStorageRecord{Backend: "local", Previous: map[string]string{"notes": "nfs"}},
			allowEmpty: true,
			want:       sharedDirStorageRecord{Backend: "local"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.rec
			before := sharedDirStorageRecord{Backend: in.Backend, Dirs: cloneMap(in.Dirs), Previous: cloneMap(in.Previous)}
			got := changeSharedDirBackends(&in, notes, tt.allowEmpty)
			assert.Equal(t, tt.want, *got)
			assert.Equal(t, before, in, "the input record is not modified")
		})
	}
}

// fakeClaims is a SharedDirClaimChecker with fixed answers. A dir uses a
// claim unless noClaim is set.
type fakeClaims struct {
	noClaim bool
	exists  bool
	err     error
	calls   []string
	cfg     runtime.RunConfig
}

func (f *fakeClaims) SharedDirUsesClaim(_ runtime.RunConfig, _ string) bool {
	return !f.noClaim
}

func (f *fakeClaims) SharedDirClaimExists(_ context.Context, cfg runtime.RunConfig, dirName string) (bool, error) {
	f.calls = append(f.calls, dirName)
	f.cfg = cfg
	return f.exists, f.err
}

// localCheckFixture lays out a project dir with a local notes leaf and an
// nfs export holding the notes leaf of project pid-1, with notes changed
// back from nfs to local.
type localCheckFixture struct {
	projectDir string
	localLeaf  string
	nfsLeaf    string
	shareDir   string
	in         sharedDirCheckInput
	runCfg     runtime.RunConfig
}

func newLocalCheckFixture(t *testing.T, runtimeName string) *localCheckFixture {
	t.Helper()
	projectDir := newTestProjectDir(t)
	localLeaf, err := config.GetSharedDirPath(projectDir, "notes")
	require.NoError(t, err)
	mountRoot := t.TempDir()
	gs := perDirSettings(mountRoot, config.V1ProfileConfig{}, config.V1RuntimeConfig{})
	shareDir := filepath.Join(mountRoot, gs.Server.SharedDirStorage.NFS.Shares[0].ID)
	require.NoError(t, os.MkdirAll(shareDir, 0o775))
	return &localCheckFixture{
		projectDir: projectDir,
		localLeaf:  localLeaf,
		nfsLeaf:    filepath.Join(shareDir, "projects", "pid-1", "shared-dirs", "notes"),
		shareDir:   shareDir,
		in: sharedDirCheckInput{
			rec:         &sharedDirStorageRecord{Backend: "local", Previous: map[string]string{"notes": "nfs"}},
			dirs:        notesAndCache(),
			volumes:     map[string]api.VolumeMount{"notes": {Source: localLeaf}, "gocache": {Source: "/local/gocache"}},
			projectDir:  projectDir,
			runtimeName: runtimeName,
			gs:          gs,
			projectID:   "pid-1",
		},
		runCfg: runtime.RunConfig{Labels: projectkeys.ProjectNameLabels("proj")},
	}
}

func (c *localCheckFixture) check() ([]string, error) {
	passed, deferred, err := checkChangedSharedDirs(context.Background(), c.in)
	if err != nil || len(deferred) == 0 {
		return passed, err
	}
	more, err := checkSharedDirClaims(context.Background(), c.in, deferred, c.runCfg)
	if err != nil {
		return nil, err
	}
	return append(passed, more...), nil
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestCheckChangedSharedDirs_ToLocal_Docker(t *testing.T) {
	t.Run("empty local with nfs data is refused", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		writeFileIn(t, c.nfsLeaf, "note.md")
		require.NoError(t, os.MkdirAll(c.localLeaf, 0o755))
		_, err := c.check()
		require.Error(t, err)
		assert.Contains(t, err.Error(), c.localLeaf)
		assert.Contains(t, err.Error(), c.nfsLeaf)
		assert.Contains(t, err.Error(), "--shared-dir-backend notes=local --allow-empty-shared-dir")
		_, statErr := os.Stat(filepath.Join(c.nfsLeaf, "note.md"))
		assert.NoError(t, statErr, "the nfs directory is left in place")
	})
	t.Run("missing local with nfs data is refused", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		writeFileIn(t, c.nfsLeaf, "note.md")
		_, err := c.check()
		require.Error(t, err)
	})
	t.Run("local has data", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		writeFileIn(t, c.nfsLeaf, "note.md")
		writeFileIn(t, c.localLeaf, "note.md")
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("empty nfs directory", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		require.NoError(t, os.MkdirAll(c.nfsLeaf, 0o775))
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("no nfs directory", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
		_, statErr := os.Stat(c.nfsLeaf)
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "the check creates nothing")
	})
	t.Run("symlinked host base is followed", func(t *testing.T) {
		c := newLocalCheckFixture(t, "docker")
		writeFileIn(t, c.nfsLeaf, "note.md")
		writeFileIn(t, c.localLeaf, "note.md")
		link := filepath.Join(t.TempDir(), "mnt")
		require.NoError(t, os.Symlink(filepath.Dir(c.shareDir), link))
		c.in.gs.Server.SharedDirStorage.NFS.MountRoot = link
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
}

// The nfs directory is opened as an nfs start opens it: a symlink at any
// component below the host base is refused, so the start is refused as
// unable to check it rather than reading through the link.
func TestCheckChangedSharedDirs_ToLocal_SymlinkBelowHostBaseRefused(t *testing.T) {
	for name, linkAt := range map[string]func(c *localCheckFixture) string{
		"middle directory": func(c *localCheckFixture) string { return filepath.Join(c.shareDir, "projects", "pid-1") },
		"leaf":             func(c *localCheckFixture) string { return c.nfsLeaf },
	} {
		t.Run(name, func(t *testing.T) {
			c := newLocalCheckFixture(t, "docker")
			elsewhere := t.TempDir()
			writeFileIn(t, filepath.Join(elsewhere, "shared-dirs", "notes"), "note.md")
			writeFileIn(t, elsewhere, "note.md")
			at := linkAt(c)
			require.NoError(t, os.MkdirAll(filepath.Dir(at), 0o775))
			target := elsewhere
			if at == c.nfsLeaf {
				target = filepath.Join(elsewhere, "shared-dirs", "notes")
			}
			require.NoError(t, os.Symlink(target, at))
			_, err := c.check()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "previous nfs directory cannot be checked")
			assert.Contains(t, err.Error(), "--shared-dir-backend notes=local --allow-empty-shared-dir")
		})
	}
}

func TestCheckChangedSharedDirs_ToLocal_NFSUncheckable(t *testing.T) {
	for name, mutate := range map[string]func(c *localCheckFixture){
		"no nfs block":  func(c *localCheckFixture) { c.in.gs.Server = nil },
		"no settings":   func(c *localCheckFixture) { c.in.gs = nil },
		"no project id": func(c *localCheckFixture) { c.in.projectID = "" },
		"export missing": func(c *localCheckFixture) {
			c.in.gs.Server.SharedDirStorage.NFS.MountRoot = filepath.Join(t.TempDir(), "gone")
		},
		"invalid project": func(c *localCheckFixture) { c.in.projectID = "../x" },
	} {
		for _, rt := range []string{"docker", "kubernetes"} {
			t.Run(name+" on "+rt, func(t *testing.T) {
				c := newLocalCheckFixture(t, rt)
				c.in.claims = &fakeClaims{exists: true}
				writeFileIn(t, c.localLeaf, "note.md")
				mutate(c)
				_, err := c.check()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "previous nfs directory cannot be checked")
				assert.Contains(t, err.Error(), "--shared-dir-backend notes=local --allow-empty-shared-dir")
			})
		}
	}
}

// When the local storage cannot be checked anyway, the check is skipped
// before the nfs directory is read: an nfs directory that cannot be
// checked does not refuse the start there.
func TestCheckChangedSharedDirs_ToLocal_SkipBeforeNFSCheck(t *testing.T) {
	t.Run("runtime without claim lookup", func(t *testing.T) {
		logs := captureWarnings(t)
		c := newLocalCheckFixture(t, "kubernetes")
		c.in.gs = nil
		passed, deferred, err := checkChangedSharedDirs(context.Background(), c.in)
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
		assert.Empty(t, deferred)
		assert.Contains(t, logs.String(), "cannot look up its local storage")
	})
	t.Run("no claim of its own", func(t *testing.T) {
		logs := captureWarnings(t)
		c := newLocalCheckFixture(t, "kubernetes")
		c.in.gs = nil
		claims := &fakeClaims{noClaim: true}
		c.in.claims = claims
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
		assert.Empty(t, claims.calls)
		assert.Contains(t, logs.String(), "has no claim of its own")
	})
}

func TestCheckChangedSharedDirs_ToLocal_Kubernetes(t *testing.T) {
	t.Run("deferred to the run config", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		claims := &fakeClaims{}
		c.in.claims = claims
		passed, deferred, err := checkChangedSharedDirs(context.Background(), c.in)
		require.NoError(t, err)
		assert.Empty(t, passed)
		assert.Equal(t, []string{"notes"}, deferred)
		assert.Empty(t, claims.calls, "nothing is looked up before the run config is known")
	})
	t.Run("missing claim is refused", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		claims := &fakeClaims{}
		c.in.claims = claims
		_, err := c.check()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
		assert.Contains(t, err.Error(), "--shared-dir-backend notes=local --allow-empty-shared-dir")
		assert.Equal(t, []string{"notes"}, claims.calls)
		assert.Equal(t, "proj", projectkeys.ProjectNameFromLabels(claims.cfg.Labels))
	})
	t.Run("claim lookup error is refused", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		c.in.claims = &fakeClaims{err: errors.New("forbidden")}
		_, err := c.check()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forbidden")
		assert.Contains(t, err.Error(), "--allow-empty-shared-dir")
	})
	t.Run("existing claim passes with a warning", func(t *testing.T) {
		logs := captureWarnings(t)
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		c.in.claims = &fakeClaims{exists: true}
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
		assert.Contains(t, logs.String(), "it was not checked")
	})
	t.Run("empty nfs directory does not look up the claim", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		claims := &fakeClaims{}
		c.in.claims = claims
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
		assert.Empty(t, claims.calls)
	})
}

// With the Kubernetes runtime's own lookup: a workspace on nfs with a bound
// claim serves local shared dirs from it, so the check is skipped; without
// a pv_name the dir gets its own claim, and the claim is checked.
func TestCheckSharedDirClaims_KubernetesRuntime(t *testing.T) {
	newRT := func(t *testing.T) *runtime.KubernetesRuntime {
		t.Helper()
		rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), k8sfake.NewClientset()))
		rt.DefaultNamespace = "default"
		return rt
	}
	t.Run("nfs workspace with a claim skips", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		c.in.claims = newRT(t)
		c.runCfg.WorkspaceBackendName = "nfs"
		c.runCfg.NFSPVClaimName = "ws"
		passed, err := c.check()
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("nfs workspace without pv_name checks the claim", func(t *testing.T) {
		c := newLocalCheckFixture(t, "kubernetes")
		writeFileIn(t, c.nfsLeaf, "note.md")
		c.in.claims = newRT(t)
		c.runCfg.WorkspaceBackendName = "nfs"
		_, err := c.check()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})
}

// A previous nfs entry is checked only when the dir is now local.
func TestCheckChangedSharedDirs_ToLocal_Skips(t *testing.T) {
	c := newLocalCheckFixture(t, "docker")
	writeFileIn(t, c.nfsLeaf, "note.md")
	c.in.rec = &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}, Previous: map[string]string{"notes": "nfs"}}
	c.in.realization = &runtime.SharedDirRealization{Backend: "nfs", SubPaths: map[string]string{"notes": "x"}, LocalDirs: map[string]bool{"gocache": true}}
	passed, err := c.check()
	require.NoError(t, err)
	assert.Empty(t, passed, "the entry is kept")
}

// claimRuntime is a mock runtime that can look up shared dir claims.
type claimRuntime struct {
	*runtime.MockRuntime
	*fakeClaims
}

// Round trip on docker: local, then nfs, then back to local. Each start
// after a change refuses an empty directory on the new backend while the
// other one holds data, and passes once the operator has copied the data.
// No directory is ever removed.
func TestSharedDirBackendChange_DockerRoundTrip(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &first)).Start(context.Background(), opts)
	require.NoError(t, err)
	localNotes := volumeSource(first.cfg, "/scion-volumes/notes")
	require.NotEmpty(t, localNotes)
	writeFileIn(t, localNotes, "note.md")
	nfsNotes := filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes")

	reprov := func(backend string) {
		t.Helper()
		_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
			Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
			SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": backend},
		})
		require.NoError(t, err)
	}

	// local -> nfs
	reprov("nfs")
	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.Error(t, err)
	writeFileIn(t, nfsNotes, "note.md")
	var onNFS sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &onNFS)).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, nfsNotes, volumeSource(onNFS.cfg, "/scion-volumes/notes"))
	assert.Equal(t, &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}}, perDirRecord(t, f, "agent"))

	// The operator stops the agent and copies nfs back to an emptied local
	// directory; the start refuses until the copy is there.
	require.NoError(t, os.Remove(filepath.Join(localNotes, "note.md")))
	reprov("local")
	assert.Equal(t, &sharedDirStorageRecord{Backend: "local", Previous: map[string]string{"notes": "nfs"}}, perDirRecord(t, f, "agent"))
	var refused sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &refused)).Start(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--shared-dir-backend notes=local --allow-empty-shared-dir")
	assert.Equal(t, 0, refused.ran)
	assert.Equal(t, map[string]string{"notes": "nfs"}, perDirRecord(t, f, "agent").Previous, "a refused start keeps the check")

	writeFileIn(t, localNotes, "note.md")
	var onLocal sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &onLocal)).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, onLocal.ran)
	assert.Equal(t, localNotes, volumeSource(onLocal.cfg, "/scion-volumes/notes"))
	assert.Equal(t, &sharedDirStorageRecord{Backend: "local"}, perDirRecord(t, f, "agent"), "the check passed and is not repeated")

	_, statErr := os.Stat(filepath.Join(nfsNotes, "note.md"))
	assert.NoError(t, statErr, "the nfs directory is never removed")
}

// With AllowEmptySharedDir a dir changed back to local starts on an empty
// local directory even though the nfs directory holds data.
func TestSharedDirBackendChange_ToLocalAllowEmpty(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)
	writeFileIn(t, filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes"), "note.md")

	reprov := api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
		SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "local"},
	}
	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), reprov)
	require.NoError(t, err)
	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.Error(t, err)

	reprov.AllowEmptySharedDir = true
	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), reprov)
	require.NoError(t, err)
	assert.Equal(t, &sharedDirStorageRecord{Backend: "local"}, perDirRecord(t, f, "agent"))
	var started sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &started)).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, started.ran)
}

// Kubernetes through Manager.Start: after a change back to local the start
// looks up the dir's claim through the runtime. A missing claim refuses,
// an existing one starts with the dir on its local claim, and a runtime
// without the lookup starts with a warning.
func TestSharedDirBackendChange_ToLocalKubernetes(t *testing.T) {
	setup := func(t *testing.T) (sharedDirStorageRunFixture, api.StartOptions, api.StartOptions) {
		f, mountRoot := newPerDirFixture(t)
		f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))
		workspace := filepath.Join(f.tmpDir, "checkout")
		require.NoError(t, os.MkdirAll(workspace, 0o755))
		opts := perDirStartOpts(f, "agent", "gke")
		opts.Workspace = workspace
		_, err := NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Start(context.Background(), opts)
		require.NoError(t, err)
		writeFileIn(t, filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes"), "note.md")
		reprov := api.StartOptions{
			Name: "agent", ProjectPath: f.projectScionDir, Profile: "gke", Workspace: workspace,
			SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "local"},
		}
		_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), reprov)
		require.NoError(t, err)
		return f, opts, reprov
	}

	t.Run("missing claim refuses, flag overrides", func(t *testing.T) {
		f, opts, reprov := setup(t)
		var refused sdsCapture
		claims := &fakeClaims{}
		_, err := NewManager(claimRuntime{newSDSMockRuntime("kubernetes", &refused), claims}).Start(context.Background(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
		assert.Equal(t, 0, refused.ran)
		assert.Equal(t, []string{"notes"}, claims.calls)
		assert.NotEmpty(t, projectkeys.ProjectNameFromLabels(claims.cfg.Labels))
		assert.Equal(t, map[string]string{"notes": "nfs"}, perDirRecord(t, f, "agent").Previous, "a refused start keeps the check")

		reprov.AllowEmptySharedDir = true
		_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), reprov)
		require.NoError(t, err)
		var started sdsCapture
		_, err = NewManager(claimRuntime{newSDSMockRuntime("kubernetes", &started), &fakeClaims{}}).Start(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, 1, started.ran)
		assert.False(t, started.cfg.SharedDirStorage.Serves("notes"))
		assert.Nil(t, perDirRecord(t, f, "agent").Previous)
	})

	t.Run("lookup error refuses, flag overrides", func(t *testing.T) {
		f, opts, reprov := setup(t)
		_, err := NewManager(claimRuntime{newSDSMockRuntime("kubernetes", &sdsCapture{}), &fakeClaims{err: errors.New("forbidden")}}).Start(context.Background(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forbidden")
		assert.Equal(t, map[string]string{"notes": "nfs"}, perDirRecord(t, f, "agent").Previous, "a refused start keeps the check")

		reprov.AllowEmptySharedDir = true
		_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), reprov)
		require.NoError(t, err)
		var started sdsCapture
		_, err = NewManager(claimRuntime{newSDSMockRuntime("kubernetes", &started), &fakeClaims{err: errors.New("forbidden")}}).Start(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, 1, started.ran)
	})

	t.Run("existing claim starts with a warning", func(t *testing.T) {
		f, opts, _ := setup(t)
		logs := captureWarnings(t)
		var started sdsCapture
		_, err := NewManager(claimRuntime{newSDSMockRuntime("kubernetes", &started), &fakeClaims{exists: true}}).Start(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, 1, started.ran)
		assert.Contains(t, logs.String(), "it was not checked")
		assert.Nil(t, perDirRecord(t, f, "agent").Previous)
	})

	t.Run("runtime without the lookup starts with a warning", func(t *testing.T) {
		_, opts, _ := setup(t)
		logs := captureWarnings(t)
		var started sdsCapture
		_, err := NewManager(newSDSMockRuntime("kubernetes", &started)).Start(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, 1, started.ran)
		assert.Contains(t, logs.String(), "cannot look up its local storage")
	})
}
