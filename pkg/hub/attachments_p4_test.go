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
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 4 of project storage (ptone/scion#5827).

// ---------------------------------------------------------------------------
// ptone/scion#5828: hub.attachment_artifacts defaults on with object storage
// ---------------------------------------------------------------------------

func TestAttachmentArtifacts_DefaultOnWithObjectStorage(t *testing.T) {
	srv, _ := testServer(t)
	name := experiments.AttachmentArtifacts
	exp, ok := srv.experimentRegistry().Lookup(name)
	require.True(t, ok)
	require.False(t, exp.Default, "the compiled default stays off")

	assert.False(t, srv.experimentEnabled(name), "no storage: compiled default")
	assert.False(t, srv.experimentDefault(exp))

	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "hub", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(local)
	assert.False(t, srv.experimentEnabled(name), "local resource storage (T1): off")

	srv.SetStorage(&mockStorage{provider: storage.ProviderGCS})
	assert.True(t, srv.experimentEnabled(name), "object storage: on by default")
	assert.True(t, srv.experimentDefault(exp), "the admin view reports the deployment default")

	// A stored override and a malformed document still win.
	assert.False(t, srv.experimentEnabledIn(ExperimentsSnapshot{Overrides: map[string]bool{name: false}}, name))
	assert.False(t, srv.experimentEnabledIn(ExperimentsSnapshot{Overrides: map[string]bool{}, Malformed: true}, name))
	assert.True(t, srv.experimentEnabledIn(ExperimentsSnapshot{Overrides: map[string]bool{name: true}}, name))

	// No other experiment changes default with the storage.
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == name {
			continue
		}
		assert.Equal(t, e.Default, srv.experimentDefault(e), e.Name)
	}
}

// ---------------------------------------------------------------------------
// The legacy store is hub-private and read through os.Root
// ---------------------------------------------------------------------------

func TestLocalDiskAttachmentStore_GetThroughRoot(t *testing.T) {
	base := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(base)
	require.NoError(t, err)
	ctx := context.Background()

	meta, err := as.Save(ctx, "proj-1", "a.txt", strings.NewReader("hello"), 5, "text/plain")
	require.NoError(t, err)
	rc, got, err := as.Get(ctx, "proj-1", meta.ID)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "hello", string(b))
	assert.Equal(t, "a.txt", got.Filename)

	// A project-less (DM) upload lives at <base>/<id>/<name>.
	dm, err := as.Save(ctx, "", "dm.txt", strings.NewReader("dm"), 2, "text/plain")
	require.NoError(t, err)
	rc, _, err = as.Get(ctx, "", dm.ID)
	require.NoError(t, err)
	_ = rc.Close()

	for _, bad := range [][2]string{{"proj-1", ".."}, {"..", meta.ID}, {"proj-1", "a/b"}, {"proj-1", ""}} {
		_, _, err := as.Get(ctx, bad[0], bad[1])
		assert.Error(t, err, "Get(%q, %q)", bad[0], bad[1])
	}
}

func TestLocalDiskAttachmentStore_GetRefusesSymlinks(t *testing.T) {
	base := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(base)
	require.NoError(t, err)
	outside := newOutsideTree(t)
	ctx := context.Background()

	// A linked attachment directory pointing out of the store.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "proj-1"), 0o755))
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(base, "proj-1", "linked-dir")))
	_, _, err = as.Get(ctx, "proj-1", "linked-dir")
	assert.Error(t, err, "a directory link out of the store is refused")

	// A linked file inside an attachment directory.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "proj-1", "linked-file"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "secret.txt"), filepath.Join(base, "proj-1", "linked-file", "secret.txt")))
	_, _, err = as.Get(ctx, "proj-1", "linked-file")
	assert.Error(t, err, "a file link is refused")
}

// ---------------------------------------------------------------------------
// ptone/scion#5832: retention
// ---------------------------------------------------------------------------

// ageDir sets dir's modification time to d in the past.
func ageDir(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(dir, old, old))
}

func TestSweepProjectionsIn(t *testing.T) {
	sp := t.TempDir()
	outside := newOutsideTree(t)
	mk := func(rel string) string {
		p := filepath.Join(sp, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(p, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(p, "f.txt"), []byte("x"), 0o644))
		return p
	}
	oldWeb := mk(".attachments/_webchat/old")
	newWeb := mk(".attachments/_webchat/new")
	oldCLI := mk(".attachments/some-agent/msg-old")
	// A flat plugin file from before per-id directories.
	flat := filepath.Join(sp, ".attachments", "_telegram", "tg_1_a.png")
	require.NoError(t, os.MkdirAll(filepath.Dir(flat), 0o755))
	require.NoError(t, os.WriteFile(flat, []byte("x"), 0o644))
	ageDir(t, oldWeb, 10*24*time.Hour)
	ageDir(t, oldCLI, 10*24*time.Hour)
	ageDir(t, flat, 10*24*time.Hour)
	// A channel that is a link out of the scratchpad is never walked.
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(sp, ".attachments", "_evil")))
	ageDir(t, outside.dir, 10*24*time.Hour)
	before := outside.snapshot(t)

	res := projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: sp}
	n, err := sweepProjectionsIn(res, time.Now().Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.NoDirExists(t, oldWeb)
	assert.NoDirExists(t, oldCLI)
	assert.NoFileExists(t, flat)
	assert.DirExists(t, newWeb)
	outside.assertIntact(t, before)

	// A scratchpad without .attachments, and one that does not exist.
	n, err = sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: t.TempDir()}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: filepath.Join(t.TempDir(), "gone")}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestSweepProjectionsIn_SymlinkedAttachmentsDir: an agent-planted `.attachments -> .` (or a link out of the scratchpad) must
// not turn the sweep on the rest of the scratchpad: nothing outside a real
// .attachments directory is removed, however old.
func TestSweepProjectionsIn_SymlinkedAttachmentsDir(t *testing.T) {
	outside := newOutsideTree(t)
	for _, target := range []string{".", "work", "OUTSIDE"} {
		t.Run(target, func(t *testing.T) {
			sp := t.TempDir()
			// Other agents' files, one level below a would-be channel.
			victim := filepath.Join(sp, "work", "notes")
			require.NoError(t, os.MkdirAll(victim, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644))
			ageDir(t, victim, 30*24*time.Hour)
			ageDir(t, filepath.Join(sp, "work"), 30*24*time.Hour)
			link := target
			if target == "OUTSIDE" {
				link = outside.dir
				ageDir(t, filepath.Join(outside.dir, "landing"), 30*24*time.Hour)
			}
			require.NoError(t, os.Symlink(link, filepath.Join(sp, projectfs.AttachmentsDir)))
			before := outside.snapshot(t)

			n, err := sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: sp}, time.Now())
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.FileExists(t, filepath.Join(victim, "keep.txt"))
			assert.DirExists(t, filepath.Join(sp, "work"))
			outside.assertIntact(t, before)
		})
	}
}

// TestSweepAttachmentProjections_InboxRetention: the server sweep resolves
// each project's scratchpad and removes projections older than
// messaging.attachment_inbox_retention_days (default 7), on the local
// backend too (the one intentional T1 change).
func TestSweepAttachmentProjections_InboxRetention(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	old := filepath.Join(sharedDir, ".attachments", "_webchat", "old")
	recent := filepath.Join(sharedDir, ".attachments", "_webchat", "recent")
	require.NoError(t, os.MkdirAll(old, 0o755))
	require.NoError(t, os.MkdirAll(recent, 0o755))
	ageDir(t, old, 8*24*time.Hour)
	ageDir(t, recent, 6*24*time.Hour)

	now := time.Now()
	cutoff := srv.projectionRetentionCutoff(now)
	assert.WithinDuration(t, now.Add(-time.Duration(DefaultAttachmentInboxRetentionDays)*24*time.Hour), cutoff, time.Second)
	n := srv.sweepAttachmentProjections(context.Background(), cutoff)
	assert.Equal(t, 1, n, "project %s", project.ID)
	assert.NoDirExists(t, old)
	assert.DirExists(t, recent)
}

func TestAttachmentRetention_DuePacing(t *testing.T) {
	var r attachmentRetention
	now := time.Now()
	assert.True(t, r.due(now))
	assert.False(t, r.due(now.Add(30*time.Minute)))
	assert.True(t, r.due(now.Add(61*time.Minute)))
}

func TestLocalDiskAttachmentStore_Sweep(t *testing.T) {
	base := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(base)
	require.NoError(t, err)
	ctx := context.Background()
	outside := newOutsideTree(t)

	oldP, err := as.Save(ctx, "proj-1", "a.txt", strings.NewReader("a"), 1, "text/plain")
	require.NoError(t, err)
	newP, err := as.Save(ctx, "proj-1", "b.txt", strings.NewReader("b"), 1, "text/plain")
	require.NoError(t, err)
	oldDM, err := as.Save(ctx, "", "c.txt", strings.NewReader("c"), 1, "text/plain")
	require.NoError(t, err)
	kept, err := as.Save(ctx, "proj-1", "k.txt", strings.NewReader("k"), 1, "text/plain")
	require.NoError(t, err)
	ageDir(t, filepath.Join(base, "proj-1", oldP.ID), 200*time.Hour)
	ageDir(t, filepath.Join(base, oldDM.ID), 200*time.Hour)
	ageDir(t, filepath.Join(base, "proj-1", kept.ID), 200*time.Hour)
	ageDir(t, filepath.Join(base, "proj-1"), 200*time.Hour)
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(base, "linked")))
	before := outside.snapshot(t)

	// Candidates come from the layout: (project, id) at depth 2, ("", id)
	// at depth 1; the callback sees exactly those pairs.
	var seen []string
	n, err := as.sweep(time.Now().Add(-168*time.Hour), func(projectID, id string) bool {
		seen = append(seen, projectID+"/"+id)
		return (projectID == "proj-1" && id == oldP.ID) || (projectID == "" && id == oldDM.ID)
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Contains(t, seen, "/proj-1", "the project dir is offered only as a depth-1 candidate, which the record check refuses")
	assert.Contains(t, seen, "proj-1/"+kept.ID)
	assert.NotContains(t, seen, "proj-1/"+newP.ID, "a recent blob is not even offered")
	assert.NoDirExists(t, filepath.Join(base, "proj-1", oldP.ID))
	assert.NoDirExists(t, filepath.Join(base, oldDM.ID))
	assert.DirExists(t, filepath.Join(base, "proj-1", newP.ID))
	assert.DirExists(t, filepath.Join(base, "proj-1", kept.ID), "sweepable answered no")
	outside.assertIntact(t, before)
}

// TestSweepLegacyAttachmentBlobs_Rules pins the retention rules for legacy
// blobs: an orphan upload is swept; a blob a sent message references is kept
// on a hub without object storage; with object storage it is swept once its
// messages are dispatched, never while one is pending. A directory is
// removed only at its attachment's recorded location:
// a blob with no metadata row, or under another project's directory than
// its row records, is kept, and a project directory is never removed as if
// it were an attachment.
func TestSweepLegacyAttachmentBlobs_Rules(t *testing.T) {
	srv, s := attachmentTestServer(t)
	ctx := context.Background()
	srv.mu.RLock()
	as := srv.attachmentStore.(*LocalDiskAttachmentStore)
	wcs := srv.webChatStore
	srv.mu.RUnlock()

	project := &store.Project{ID: api.NewUUID(), Name: "retention", Slug: "retention"}
	require.NoError(t, s.CreateProject(ctx, project))
	pid := project.ID
	age := func(rel string) { ageDir(t, filepath.Join(as.baseDir, rel), 400*time.Hour) }
	// save stores a blob under storedIn and records its row under recorded.
	save := func(name, storedIn, recorded string, withRow bool) AttachmentMeta {
		meta, err := as.Save(ctx, storedIn, name, strings.NewReader(name), int64(len(name)), "text/plain")
		require.NoError(t, err)
		if withRow {
			row := meta
			row.ProjectID = recorded
			row.UploadedBy = "u1"
			require.NoError(t, wcs.CreateAttachment(ctx, row))
		}
		age(filepath.Join(storedIn, meta.ID))
		return meta
	}
	message := func(state string) string {
		id := api.NewUUID()
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: pid, Sender: "user:u1", SenderID: "u1", Recipient: "agent:a", Msg: "m",
			Type: messages.TypeInstruction, Channel: "web", ThreadID: "dm:x", CreatedAt: time.Now().UTC(),
			DispatchState: state,
		}))
		return id
	}
	orphan := save("orphan.txt", pid, pid, true)
	dmOrphan := save("dm.txt", "", "", true)
	noRow := save("norow.txt", pid, "", false)
	misplaced := save("misplaced.txt", pid, api.NewUUID(), true)
	// A stray regular file directly under the project directory must not
	// make the project directory look like an attachment.
	require.NoError(t, os.WriteFile(filepath.Join(as.baseDir, pid, "stray.txt"), []byte("stray"), 0o644))
	// An empty attachment directory with a matching orphan row is an
	// attachment like any other.
	emptyID := api.NewUUID()
	require.NoError(t, os.MkdirAll(filepath.Join(as.baseDir, emptyID), 0o755))
	require.NoError(t, wcs.CreateAttachment(ctx, AttachmentMeta{ID: emptyID, Filename: "e.txt", MimeType: "text/plain", UploadedBy: "u1", CreatedAt: time.Now()}))
	age(emptyID)
	sent := save("sent.txt", pid, pid, true)
	require.NoError(t, wcs.LinkAttachmentToMessage(ctx, message(store.MessageDispatchDispatched), sent.ID))
	pending := save("pending.txt", pid, pid, true)
	require.NoError(t, wcs.LinkAttachmentToMessage(ctx, message(store.MessageDispatchPending), pending.ID))
	age(pid)

	cutoff := srv.legacyBlobRetentionCutoff(time.Now())
	dir := func(m AttachmentMeta, storedIn string) string { return filepath.Join(as.baseDir, storedIn, m.ID) }

	// No object storage (T1): only the orphans go.
	assert.Equal(t, 3, srv.sweepLegacyAttachmentBlobs(ctx, cutoff))
	assert.NoDirExists(t, dir(orphan, pid))
	assert.NoDirExists(t, dir(dmOrphan, ""))
	assert.NoDirExists(t, filepath.Join(as.baseDir, emptyID))
	assert.FileExists(t, filepath.Join(as.baseDir, pid, "stray.txt"), "a stray file never causes a project-wide delete")
	assert.DirExists(t, dir(noRow, pid), "a blob without a metadata row is never removed")
	assert.DirExists(t, dir(misplaced, pid), "a blob outside its recorded location is never removed")
	assert.DirExists(t, dir(sent, pid), "T1 keeps a blob a sent message references")
	assert.DirExists(t, dir(pending, pid))
	assert.DirExists(t, filepath.Join(as.baseDir, pid), "the project directory is never taken for an attachment")

	// Object storage: the dispatched message's blob goes, the pending one stays.
	srv.SetStorage(&mockStorage{provider: storage.ProviderGCS})
	assert.Equal(t, 1, srv.sweepLegacyAttachmentBlobs(ctx, cutoff))
	assert.NoDirExists(t, dir(sent, pid))
	assert.DirExists(t, dir(pending, pid), "never swept while a referencing message is undispatched")
	assert.DirExists(t, dir(noRow, pid))
	assert.DirExists(t, dir(misplaced, pid))
}

// ---------------------------------------------------------------------------
// ptone/scion#5830: container targets come from the authority
// ---------------------------------------------------------------------------

func TestSharedDirsList_TargetFromAuthority(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	ctx := context.Background()
	p, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	p.SharedDirs = append(p.SharedDirs, api.SharedDir{Name: "inws", InWorkspace: true})
	require.NoError(t, f.s.UpdateProject(ctx, p))

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/shared-dirs", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		SharedDirs []struct {
			Name        string `json:"name"`
			InWorkspace bool   `json:"in_workspace"`
			Target      string `json:"target"`
		} `json:"sharedDirs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got := map[string]string{}
	for _, d := range resp.SharedDirs {
		got[d.Name] = d.Target
	}
	assert.Equal(t, "/scion-volumes/scratchpad", got["scratchpad"])
	assert.Equal(t, "/scion-volumes/custom", got["custom"])
	assert.Equal(t, "/workspace/.scion-volumes/inws", got["inws"])

	// The stored declaration carries no target.
	p, err = f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	raw, err := json.Marshal(p.SharedDirs)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "target")
}

func TestAgentAttachmentRelPath_TargetsFromAuthority(t *testing.T) {
	for _, target := range projectfs.SharedDirTargets(attachmentSharedDirName, "") {
		rel, ok := agentAttachmentRelPath(target + "/.attachments/a/b.txt")
		assert.True(t, ok, target)
		assert.Equal(t, ".attachments/a/b.txt", rel)
	}
}

// ---------------------------------------------------------------------------
// Delivery plan: the projection is listed for every state but withheld
// ---------------------------------------------------------------------------

func TestAttachmentPlan_ProjectionCopiedUnlessWithheld(t *testing.T) {
	del := messages.AttachmentDelivery{
		ArtifactID: "a1", MessageID: "m1", Name: "x.txt",
		ProjectedPath: "/scion-volumes/scratchpad/.attachments/_webchat/a1/x.txt", ProjectedMount: "scratchpad",
	}
	for _, state := range []string{messages.AttachmentDeliverable, messages.AttachmentUndeliverableRuntime, messages.AttachmentWithheld} {
		e := messages.AttachmentPlanEntry{Delivery: del, State: state}
		applyProjection(&e)
		if state == messages.AttachmentWithheld {
			assert.Empty(t, e.ProjectedPath, state)
			assert.Empty(t, e.ProjectedMount, state)
			continue
		}
		assert.Equal(t, del.ProjectedPath, e.ProjectedPath, state)
		assert.Equal(t, "scratchpad", e.ProjectedMount, state)
	}
}

// TestProjection_OnlyChatV2SendPathProjects pins, at the source level, that
// the scratchpad projection of artifact attachments is reachable from one
// place only: stageChatAttachments is called only by
// sendAgentRouted (every chat v2 send, scheduled sends and promotions
// included, goes through it; mention copies reuse its result), and
// AttachmentRef.projectedPath/projectedMount are assigned only inside
// stageChatAttachments, behind its projectionAudienceIsProject check.
func TestProjection_OnlyChatV2SendPathProjects(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs; guards stageChatAttachments call sites and projectedPath/projectedMount writes.
	paths, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var callers, assigners []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "stageChatAttachments" {
						callers = append(callers, fn.Name.Name)
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "projectedPath" || sel.Sel.Name == "projectedMount") {
							assigners = append(assigners, fn.Name.Name+"."+sel.Sel.Name)
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && (id.Name == "projectedPath" || id.Name == "projectedMount") {
						assigners = append(assigners, fn.Name.Name+"."+id.Name+"(literal)")
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"sendAgentRouted"}, callers, "stageChatAttachments call sites")
	assert.ElementsMatch(t, []string{"stageChatAttachments.projectedPath", "stageChatAttachments.projectedMount"}, assigners,
		"projectedPath/projectedMount are set only by stageChatAttachments")
}

// failingLinksWCS fails every link lookup.
type failingLinksWCS struct{ WebChatStore }

func (failingLinksWCS) ListMessageIDsForAttachment(context.Context, string, int) ([]string, error) {
	return nil, errors.New("planted link lookup failure")
}

// TestSweepLegacyAttachmentBlobs_LinkLookupFailureKeeps: when the link
// lookup fails, no blob counts as an orphan:
// the sweep removes a blob only when the lookup succeeded and found no
// message.
func TestSweepLegacyAttachmentBlobs_LinkLookupFailureKeeps(t *testing.T) {
	srv, _ := attachmentTestServer(t)
	ctx := context.Background()
	srv.mu.RLock()
	as := srv.attachmentStore.(*LocalDiskAttachmentStore)
	wcs := srv.webChatStore
	srv.mu.RUnlock()

	pid := api.NewUUID()
	meta, err := as.Save(ctx, pid, "a.txt", strings.NewReader("a"), 1, "text/plain")
	require.NoError(t, err)
	row := meta
	row.UploadedBy = "u1"
	require.NoError(t, wcs.CreateAttachment(ctx, row))
	ageDir(t, filepath.Join(as.baseDir, pid, meta.ID), 400*time.Hour)

	srv.SetWebChatStore(failingLinksWCS{wcs})
	cutoff := srv.legacyBlobRetentionCutoff(time.Now())
	assert.Zero(t, srv.sweepLegacyAttachmentBlobs(ctx, cutoff))
	assert.DirExists(t, filepath.Join(as.baseDir, pid, meta.ID))

	// With the lookup working again the orphan goes.
	srv.SetWebChatStore(wcs)
	assert.Equal(t, 1, srv.sweepLegacyAttachmentBlobs(ctx, cutoff))
}
