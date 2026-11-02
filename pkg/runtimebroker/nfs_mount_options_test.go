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

package runtimebroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endsWithNosuidNodev reports whether a comma-separated option string ends
// with nosuid,nodev and carries each of them exactly once.
func endsWithNosuidNodev(opts string) bool {
	toks := strings.Split(opts, ",")
	if len(toks) < 2 || toks[len(toks)-2] != "nosuid" || toks[len(toks)-1] != "nodev" {
		return false
	}
	return strings.Count(","+opts+",", ",nosuid,") == 1 && strings.Count(","+opts+",", ",nodev,") == 1
}

func TestHardenNFSMountOptions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "nosuid,nodev"},
		{defaultNFSMountOptions, "vers=3,hard,nconnect=4,_netdev,nosuid,nodev"},
		{"vers=4.1,hard,rsize=1048576", "vers=4.1,hard,rsize=1048576,nosuid,nodev"},
		{"nosuid,vers=3,nodev", "vers=3,nosuid,nodev"},
		{"vers=3,nosuid,nodev", "vers=3,nosuid,nodev"},
		{"nodev,nodev,nosuid,vers=3", "vers=3,nosuid,nodev"},
		{"suid,vers=3", "vers=3,nosuid,nodev"},
		{"vers=3,dev", "vers=3,nosuid,nodev"},
		{"defaults,exec,vers=3", "defaults,exec,vers=3,nosuid,nodev"},
		{" vers=3 , ,hard,", "vers=3,hard,nosuid,nodev"},
	}
	for _, tc := range cases {
		got := HardenNFSMountOptions(tc.in)
		assert.Equal(t, tc.want, got, "HardenNFSMountOptions(%q)", tc.in)
		assert.True(t, endsWithNosuidNodev(got), "%q does not end with nosuid,nodev exactly once each", got)
		assert.Equal(t, got, HardenNFSMountOptions(got), "not idempotent for %q", tc.in)
	}
}

func TestEffectiveNFSMountOptions_DefaultsAndHardens(t *testing.T) {
	assert.Equal(t, "vers=3,hard,nconnect=4,_netdev,nosuid,nodev", effectiveNFSMountOptions(nil))
	assert.Equal(t, "vers=3,hard,nconnect=4,_netdev,nosuid,nodev", effectiveNFSMountOptions(&config.V1NFSConfig{}))
	assert.Equal(t, "vers=3,hard,nconnect=4,_netdev,nosuid,nodev", effectiveNFSMountOptions(&config.V1NFSConfig{MountOptions: "  "}))
	assert.Equal(t, "vers=4.1,nosuid,nodev", effectiveNFSMountOptions(&config.V1NFSConfig{MountOptions: "vers=4.1"}))
}

// mountOptionOrigins are mount_options as they reach the broker's
// V1NFSConfig from either origin: the operator's settings file and a
// hub-managed block. Both are a V1NFSConfig.MountOptions string by the time
// the broker mounts, so they go through the same builder.
var mountOptionOrigins = []struct {
	origin, opts string
}{
	{"settings file, default", ""},
	{"settings file, suid", "vers=3,suid,hard"},
	{"settings file, dev", "dev,vers=3,hard"},
	{"settings file, defaults and exec", "defaults,exec,vers=3"},
	{"settings file, suid after nosuid", "nosuid,nodev,suid,dev"},
	{"hub-managed, default", ""},
	{"hub-managed, suid", "vers=4.1,suid"},
	{"hub-managed, dev", "vers=4.1,dev"},
	{"hub-managed, allowlisted", "vers=4.1,proto=tcp,hard,nconnect=8,rsize=1048576,noatime"},
}

// TestNFSMount_EveryOriginEndsWithNosuidNodev: for every origin and option
// string, the options the reconciler hands to the mount layer, and the argv
// of the real mount command, end with nosuid,nodev.
func TestNFSMount_EveryOriginEndsWithNosuidNodev(t *testing.T) {
	for _, tc := range mountOptionOrigins {
		t.Run(tc.origin, func(t *testing.T) {
			cfg := testNFSConfig()
			cfg.MountOptions = tc.opts

			// Reconciler: first mount.
			mc := newMockMountChecker()
			r := NewNFSMountReconciler(cfg, mc, nil)
			require.NoError(t, r.Reconcile(context.Background()))
			require.Len(t, mc.mountCalls, 1)
			assert.True(t, endsWithNosuidNodev(mc.mountCalls[0].Options), "mount options %q", mc.mountCalls[0].Options)

			// Reconciler: remount after a wrong source.
			mc = newMockMountChecker()
			mc.mountpoints["/mnt/nfs/ws1"] = "10.9.9.9:/other"
			r = NewNFSMountReconciler(cfg, mc, nil)
			require.NoError(t, r.Reconcile(context.Background()))
			require.Len(t, mc.mountCalls, 1)
			assert.True(t, endsWithNosuidNodev(mc.mountCalls[0].Options), "remount options %q", mc.mountCalls[0].Options)

			// Exec layer, given the raw (unhardened) options.
			ec := NewExecMountChecker(nil)
			ec.geteuid = func() int { return 0 }
			var argv []string
			ec.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
				argv = append([]string{name}, args...)
				return nil, nil
			}
			require.NoError(t, ec.Mount(context.Background(), "10.0.0.2", "/e", "/mnt/nfs/ws1", tc.opts))
			require.Len(t, argv, 7)
			assert.Equal(t, []string{"mount", "-t", "nfs", "-o"}, argv[:4])
			assert.True(t, endsWithNosuidNodev(argv[4]), "argv -o %q", argv[4])
			assert.NotContains(t, strings.Split(argv[4], ","), "suid")
			assert.NotContains(t, strings.Split(argv[4], ","), "dev")
		})
	}
}

func TestExecMountChecker_RemountRequiredFlags(t *testing.T) {
	ec := NewExecMountChecker(nil)
	ec.geteuid = func() int { return 0 }
	var argv []string
	ec.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		argv = append([]string{name}, args...)
		return nil, nil
	}
	require.NoError(t, ec.RemountRequiredFlags(context.Background(), "/mnt/nfs/ws1"))
	assert.Equal(t, []string{"mount", "-o", "remount,nosuid,nodev", "/mnt/nfs/ws1"}, argv)

	ec.geteuid = func() int { return 1000 }
	argv = nil
	require.Error(t, ec.RemountRequiredFlags(context.Background(), "/mnt/nfs/ws1"))
	assert.Nil(t, argv, "a non-root broker does not run mount")
}

func TestMissingNFSMountFlags(t *testing.T) {
	assert.Empty(t, MissingNFSMountFlags("rw,nosuid,nodev,relatime,vers=3"))
	assert.Equal(t, []string{"nodev"}, MissingNFSMountFlags("rw,nosuid,relatime"))
	assert.Equal(t, []string{"nosuid"}, MissingNFSMountFlags("rw,nodev"))
	assert.Equal(t, []string{"nosuid", "nodev"}, MissingNFSMountFlags("rw,relatime"))
	assert.Equal(t, []string{"nosuid", "nodev"}, MissingNFSMountFlags(""))
}

const flaglessOptions = "rw,relatime,vers=3,hard"

// TestReconcile_MountFlags covers a share mounted from the expected source
// whose mount table entry lacks nosuid or nodev.
func TestReconcile_MountFlags(t *testing.T) {
	const target = "/mnt/nfs/ws1"
	setup := func(opts string) *mockMountChecker {
		mc := newMockMountChecker()
		mc.mountpoints[target] = "10.0.0.2:/scion-workspaces"
		mc.mountOptions[target] = opts
		return mc
	}

	t.Run("both flags present: healthy, nothing run", func(t *testing.T) {
		mc := setup("rw,nosuid,nodev,relatime")
		r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assert.True(t, r.IsHealthy())
		assert.Empty(t, mc.remountCalls)
		assert.Empty(t, mc.mountCalls)
		assert.Empty(t, mc.unmountCalls)
	})

	for _, opts := range []string{flaglessOptions, "rw,nosuid,relatime", "rw,nodev,relatime"} {
		t.Run("auto_mount: remounted in place, "+opts, func(t *testing.T) {
			mc := setup(opts)
			r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
			require.NoError(t, r.Reconcile(context.Background()))
			assert.Equal(t, []string{target}, mc.remountCalls)
			assert.Empty(t, mc.unmountCalls, "the remount is in place")
			assert.Empty(t, mc.mountCalls)
			st, ok := r.ShareStatus("ws1")
			require.True(t, ok)
			assert.True(t, st.Healthy, st.Message)
			assert.Empty(t, st.Reason)
			assert.Empty(t, MissingNFSMountFlags(mc.mountOptions[target]))
		})
	}

	t.Run("auto_mount: remount without effect is mount_flags", func(t *testing.T) {
		mc := setup(flaglessOptions)
		mc.remountNoEffect = true
		r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assertMountFlagsUnhealthy(t, r)
	})

	t.Run("auto_mount: remount error is mount_flags", func(t *testing.T) {
		mc := setup(flaglessOptions)
		mc.remountErr = errors.New("mount: permission denied")
		r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assertMountFlagsUnhealthy(t, r)
	})

	t.Run("auto_mount off: mount_flags, nothing run, dispatch refused", func(t *testing.T) {
		mc := setup(flaglessOptions)
		cfg := testNFSConfig()
		cfg.AutoMount = false
		r := NewNFSMountReconciler(cfg, mc, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assertMountFlagsUnhealthy(t, r)
		assert.Empty(t, mc.remountCalls)
		assert.Empty(t, mc.mountCalls)
		assert.Empty(t, mc.unmountCalls)
		err := r.EnsureShareMounted(context.Background(), "ws1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "without nosuid,nodev")
		assert.Empty(t, mc.remountCalls)
	})

	t.Run("auto_mount without privilege: mount_flags, nothing run", func(t *testing.T) {
		mc := setup(flaglessOptions)
		r := NewNFSMountReconciler(testNFSConfig(), &privCheckingMock{mockMountChecker: mc}, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assertMountFlagsUnhealthy(t, r)
		assert.Empty(t, mc.remountCalls)
	})

	t.Run("verify-only (platform mount): healthy, nothing run", func(t *testing.T) {
		mc := setup(flaglessOptions)
		r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
		r.SetVerifyOnly("the broker's default runtime is kubernetes, so the broker does not mount it")
		require.NoError(t, r.Reconcile(context.Background()))
		require.NoError(t, r.Reconcile(context.Background()))
		assert.True(t, r.IsHealthy())
		require.NoError(t, r.EnsureShareMounted(context.Background(), "ws1"))
		assert.Empty(t, mc.remountCalls)
		assert.Empty(t, mc.mountCalls)
		assert.Empty(t, mc.unmountCalls)
	})
}

// privCheckingMock is a mockMountChecker that reports it cannot mount.
type privCheckingMock struct{ *mockMountChecker }

func (p *privCheckingMock) MountPrivilegeError() error {
	return errors.New("mounting NFS requires root")
}

func assertMountFlagsUnhealthy(t *testing.T, r *NFSMountReconciler) {
	t.Helper()
	st, ok := r.ShareStatus("ws1")
	require.True(t, ok)
	assert.False(t, st.Healthy)
	assert.Equal(t, ShareReasonMountFlags, st.Reason)
	assert.False(t, r.IsHealthy())
	assert.Equal(t, "unhealthy: ws1: unhealthy", r.HealthCheckCodes())
}

// TestReconcile_MountedShareShowsFlags: a share the reconciler mounted shows
// both flags on the next pass, so it is not remounted.
func TestReconcile_MountedShareShowsFlags(t *testing.T) {
	mc := newMockMountChecker()
	r := NewNFSMountReconciler(testNFSConfig(), mc, nil)
	require.NoError(t, r.Reconcile(context.Background()))
	require.NoError(t, r.Reconcile(context.Background()))
	assert.Len(t, mc.mountCalls, 1)
	assert.Empty(t, mc.remountCalls)
	assert.True(t, r.IsHealthy())
}

// TestHostNFSMounter_OptionsDifferingOnlyByFlagsCompatible: on a flat host,
// instances whose mount_options differ only by nosuid/nodev (always added)
// share one mounter; other differences are still refused.
func TestHostNFSMounter_OptionsDifferingOnlyByFlagsCompatible(t *testing.T) {
	m := NewHostNFSMounter(newSyncMountChecker(), nil)
	a := hostNFSConfig(true, shareWS1)
	a.MountOptions = "vers=3,hard"
	b := hostNFSConfig(true, shareWS2)
	b.MountOptions = "vers=3,hard,nosuid,nodev"
	c := hostNFSConfig(false, shareWS2)
	c.MountOptions = "nosuid,vers=3,nodev,hard"
	_, err := m.Register("docker-a", "docker", a)
	require.NoError(t, err)
	_, err = m.Register("docker-b", "docker", b)
	require.NoError(t, err)
	_, err = m.Register("docker-c", "docker", c)
	require.NoError(t, err)
	assert.Equal(t, []string{"ws1", "ws2"}, m.ShareIDs())

	d := hostNFSConfig(true, shareWS1)
	d.MountOptions = "vers=4.1,hard"
	_, err = m.Register("docker-d", "docker", d)
	require.ErrorIs(t, err, ErrNFSRequirementIncompatible)
}

// symlinkedMountRoot returns a mount_root that is a symlink to a real
// directory, and the resolved share target the mount table lists.
func symlinkedMountRoot(t *testing.T) (mountRoot, resolvedTarget string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))
	resolved, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	return link, filepath.Join(resolved, "ws1")
}

func TestNFSShareTarget_ResolvesMountRoot(t *testing.T) {
	root, want := symlinkedMountRoot(t)
	assert.Equal(t, want, NFSShareTarget(root, "ws1"))
	assert.Equal(t, want, NFSShareTarget(root+"/", "ws1"))
	// A mount_root that does not exist yet stays lexical (cleaned).
	assert.Equal(t, "/nonexistent-scion-root/ws1", NFSShareTarget("/nonexistent-scion-root/./", "ws1"))
}

// TestNFSMountFlags_SymlinkedMountRoot: with mount_root behind a symlink,
// the mount table lists the resolved path. The reconciler and the dispatch
// check match it there, so a share mounted without nosuid,nodev is seen
// (remounted, or refused) rather than taken for not mounted.
func TestNFSMountFlags_SymlinkedMountRoot(t *testing.T) {
	root, target := symlinkedMountRoot(t)
	cfg := testNFSConfig()
	cfg.MountRoot = root

	t.Run("dispatch check refuses a flagless mount at the resolved path", func(t *testing.T) {
		mc := newMockMountChecker()
		mc.mountpoints[target] = "10.0.0.2:/scion-workspaces"
		mc.mountOptions[target] = flaglessOptions
		r := NewNFSMountReconciler(cfg, mc, nil)
		err := r.CheckShareMountFlags(context.Background(), "ws1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "without nosuid,nodev")
	})
	t.Run("dispatch check refuses a flagless nested mount under the resolved path", func(t *testing.T) {
		mc := newMockMountChecker()
		mc.mountpoints[target+"/projects/p1"] = "10.0.0.7:/nested"
		mc.mountOptions[target+"/projects/p1"] = flaglessOptions
		r := NewNFSMountReconciler(cfg, mc, nil)
		require.Error(t, r.CheckShareMountFlags(context.Background(), "ws1"))
	})
	t.Run("reconciler remounts in place, no new mount over it", func(t *testing.T) {
		mc := newMockMountChecker()
		mc.mountpoints[target] = "10.0.0.2:/scion-workspaces"
		mc.mountOptions[target] = flaglessOptions
		r := NewNFSMountReconciler(cfg, mc, nil)
		require.NoError(t, r.Reconcile(context.Background()))
		assert.Equal(t, []string{target}, mc.remountCalls)
		assert.Empty(t, mc.mountCalls)
		st, ok := r.ShareStatus("ws1")
		require.True(t, ok)
		assert.True(t, st.Healthy, st.Message)
		assert.Equal(t, target, st.Target)
	})
}
