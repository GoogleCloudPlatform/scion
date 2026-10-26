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
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// fakeClaimReleaser records ReleaseProjectSharedDirClaims calls.
type fakeClaimReleaser struct {
	res      scionrt.SharedDirClaimRelease
	err      error
	calls    int
	gotID    string
	gotName  string
	gotKeep  []string
	ctxAlive bool
}

func (f *fakeClaimReleaser) ReleaseProjectSharedDirClaims(ctx context.Context, projectID, projectName string, keep []string) (scionrt.SharedDirClaimRelease, error) {
	f.calls++
	f.gotID, f.gotName, f.gotKeep = projectID, projectName, keep
	f.ctxAlive = ctx.Err() == nil
	return f.res, f.err
}

func withClaimReleasers(t *testing.T, rs ...sharedDirClaimReleaser) {
	t.Helper()
	claimReleasersHook = func(*Server) []sharedDirClaimReleaser { return rs }
	t.Cleanup(func() { claimReleasersHook = nil })
}

func captureBrokerLog(srv *Server) *syncBuffer {
	buf := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

func auditLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `msg="`+projectfs.AuditMessage+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// A project delete asks every Kubernetes runtime to release the project's
// shared-dir claims by project ID and slug, keeping the layout's shared
// claims; each deleted claim, each failed one and each project-named claim
// without the project-id label gets one audit line naming the broker as the
// deleter. The project directory's removal is audited too.
func TestDeleteProject_ReleasesSharedDirClaimsWithAudit(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	sd := projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: "/mnt/nfs/sd", Share: projectfs.Share{ID: "sd", PVName: "sd-nfs"}}
	srv.config.StorageLayout = projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: "/mnt/nfs/share1", Share: projectfs.Share{ID: "share1", PVName: "hub-nfs"}, SharedDirs: &sd}
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	r1 := &fakeClaimReleaser{res: scionrt.SharedDirClaimRelease{
		Namespace:  "agents",
		Deleted:    []string{"scion-shared-proj-a-scratchpad"},
		Failed:     map[string]error{"scion-shared-proj-a-data": errors.New("forbidden")},
		Unlabelled: []string{"scion-shared-proj-a-cache"},
		InUse:      []string{"scion-shared-proj-a-live"},
	}}
	r2 := &fakeClaimReleaser{err: errors.New("cluster unreachable"), res: scionrt.SharedDirClaimRelease{Namespace: "other"}}
	withClaimReleasers(t, r1, r2)
	logs := captureBrokerLog(srv)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a claim failure must not fail the delete: %d %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	for _, r := range []*fakeClaimReleaser{r1, r2} {
		if r.calls != 1 || r.gotID != scopeProjA || r.gotName != "proj-a" || !r.ctxAlive {
			t.Fatalf("release call: %+v", r)
		}
	}
	if len(r1.gotKeep) != 2 || r1.gotKeep[0] != "hub-nfs" || r1.gotKeep[1] != "sd-nfs" {
		t.Errorf("both layout claims (workspace and shared-dir share) must be kept: %v", r1.gotKeep)
	}

	lines := auditLines(logs.String())
	want := map[string]string{
		"path=agents/scion-shared-proj-a-scratchpad": "outcome=deleted",
		"path=agents/scion-shared-proj-a-data":       "outcome=failed",
		"path=agents/scion-shared-proj-a-cache":      "outcome=kept",
		"path=agents/scion-shared-proj-a-live":       "outcome=kept",
		"path=other/*":                               "outcome=failed",
	}
	for path, outcome := range want {
		found := false
		for _, l := range lines {
			if strings.Contains(l, path) {
				found = true
				for _, s := range []string{outcome, "tree=pvc", "deleter=broker", "project_id=" + scopeProjA} {
					if !strings.Contains(l, s) {
						t.Errorf("audit line for %s lacks %s: %s", path, s, l)
					}
				}
			}
		}
		if !found {
			t.Errorf("no audit line for %s in:\n%s", path, strings.Join(lines, "\n"))
		}
	}
	dirAudited := false
	for _, l := range lines {
		if strings.Contains(l, "tree=project-dir") && strings.Contains(l, "outcome=deleted") && strings.Contains(l, "deleter=broker") {
			dirAudited = true
		}
	}
	if !dirAudited {
		t.Errorf("the project directory's removal must be audited:\n%s", strings.Join(lines, "\n"))
	}
}

// Without a project ID no claim is touched: a claim is only ever selected
// by the project-id label.
func TestDeleteProject_NoProjectIDReleasesNoClaims(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	r := &fakeClaimReleaser{}
	withClaimReleasers(t, r)

	rec := doDeleteProject(t, srv, "proj-a", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if r.calls != 0 {
		t.Fatalf("no claim release without a project ID: %d calls", r.calls)
	}
}

// The broker's Kubernetes runtimes are the releasers; a broker without one
// releases nothing.
func TestClaimReleasers_OnlyKubernetesRuntimes(t *testing.T) {
	srv, _, _ := newNFSDeleteTestServer(t)
	if got := srv.claimReleasers(); len(got) != 0 {
		t.Fatalf("a broker without a Kubernetes runtime has no releasers: %d", len(got))
	}
}
