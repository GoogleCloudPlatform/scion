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

package provisioner

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// recordHandler collects every slog record.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// walkWarnings counts the shared-tree walk's upper-directory warnings.
func (h *recordHandler) walkWarnings() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, "NFS directory walk: an existing upper directory is group/other-writable") {
			n++
		}
	}
	return n
}

// The provisioning state directory is created with the Layout's nfs uid
// and gid as the expected owner (projectfs.EnsureSharedLeafExpecting), like
// every other walk of the shared tree: an existing upper directory with the
// share's layout (setgid, the configured group, owned by the configured nfs
// uid) is not warned about, and the state directory is still never given an
// owner. The workspace leaf alone is chowned (a root creator is simulated).
// With another configured gid the same tree is warned about, so the check
// sees the warning.
func TestProvisioner_ProvisionSharedWorkspace_ExpectedLayoutNotWarned(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gidDelta int
		wantWarn bool
	}{
		{name: "configured gid", gidDelta: 0, wantWarn: false},
		{name: "another gid", gidDelta: 1, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			projects := filepath.Join(base, "projects")
			require.NoError(t, os.Mkdir(projects, 0o755))
			require.NoError(t, unix.Chmod(projects, 0o2775))
			var st unix.Stat_t
			require.NoError(t, unix.Stat(projects, &st))
			if st.Uid == 0 || st.Gid == 0 || uint32(st.Mode)&0o7777 != 0o2775 {
				t.Skip("needs a setgid directory with a non-zero owner and group")
			}
			l := nfsLayout(base)
			l.UID, l.GID = int(st.Uid), int(st.Gid)+tc.gidDelta

			type chown struct {
				ino      uint64
				uid, gid int
			}
			var chowns []chown
			t.Cleanup(shareddirs.SetOwnerHooksForTest(func() int { return 0 }, func(fd, uid, gid int) error {
				var fst unix.Stat_t
				if err := unix.Fstat(fd, &fst); err != nil {
					return err
				}
				chowns = append(chowns, chown{ino: uint64(fst.Ino), uid: uid, gid: gid})
				return nil
			}))
			shareddirs.ResetWalkWarningsForTest()
			t.Cleanup(shareddirs.ResetWalkWarningsForTest)
			h := &recordHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(h))
			t.Cleanup(func() { slog.SetDefault(prev) })

			p := Provisioner{Layout: l, ProvisionShared: func(provision.ProvisionInput) error { return nil }}
			_, err := p.ProvisionSharedWorkspace(SharedWorkspace{Ctx: context.Background(), ProjectID: testPID})
			require.NoError(t, err)

			if tc.wantWarn {
				assert.Equal(t, 1, h.walkWarnings(), "another gid keeps the warning")
			} else {
				assert.Zero(t, h.walkWarnings(), "the expected shared layout is not warned about")
			}
			var stateSt, wsSt unix.Stat_t
			require.NoError(t, unix.Stat(filepath.Join(projects, testPID, "provision"), &stateSt))
			require.NoError(t, unix.Stat(filepath.Join(projects, testPID, "workspace"), &wsSt))
			for _, c := range chowns {
				assert.NotEqual(t, uint64(stateSt.Ino), c.ino, "the provisioning state directory was chowned to %d:%d", c.uid, c.gid)
			}
			assert.Contains(t, chowns, chown{ino: uint64(wsSt.Ino), uid: l.UID, gid: l.GID}, "the workspace leaf gets the Layout's owner")
		})
	}
}
