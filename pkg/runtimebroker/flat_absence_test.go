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
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A delete request is not confirmed absence (ptone/scion#3274): a flat
// instance finishes a deleted agent's record, and releases its slug, only
// once every recorded object is confirmed gone.

func assertDeletingAndReserved(t *testing.T, f *partitionFixture, why string) {
	t.Helper()
	rec, ok, err := f.a.srv.ownership.Get("proj-1", "agent-a1")
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	if rec.State != OwnershipStateDeleting || !rec.OwnsUIDAny("cid-a1") {
		t.Fatalf("%s: record %s with cid-a1 recorded=%v, want deleting with the object still recorded", why, rec.State, rec.OwnsUIDAny("cid-a1"))
	}
	if holder, _ := f.a.srv.ownership.SlugHolder("proj-1", "worker"); holder != "agent-a1" {
		t.Fatalf("%s: slug holder %q, want still reserved", why, holder)
	}
}

func assertDeletedAndReleased(t *testing.T, f *partitionFixture) {
	t.Helper()
	rec, _, err := f.a.srv.ownership.Get("proj-1", "agent-a1")
	if err != nil || rec.State != OwnershipStateDeleted {
		t.Fatalf("record = %+v (%v), want deleted", rec, err)
	}
	if holder, _ := f.a.srv.ownership.SlugHolder("proj-1", "worker"); holder != "" {
		t.Fatalf("slug still held by %s", holder)
	}
}

const absenceDeleteURL = "/api/v1/agents/worker?projectId=proj-1&deleteFiles=true"

// TestFlatAbsence_CheckErrorStaysDeletingThenRetryFinishes: when the
// absence check fails, the record stays deleting with its object recorded
// and its slug reserved; a later delete retry that confirms absence
// finishes the record and releases the slug.
func TestFlatAbsence_CheckErrorStaysDeletingThenRetryFinishes(t *testing.T) {
	f := newPartitionFixture(t)
	f.d.absentErr = errors.New("permission denied")
	if w := serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, ""); w.Code >= 300 {
		t.Fatalf("delete: %d %s (the HTTP answer is unchanged)", w.Code, w.Body.String())
	}
	assertDeletingAndReserved(t, f, "check error")

	f.d.mu.Lock()
	f.d.absentErr = nil
	f.d.mu.Unlock()
	serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, "")
	assertDeletedAndReleased(t, f)
}

// TestFlatAbsence_TerminatingStaysDeletingThenStartupFinishes: a deletion
// still pending (the object exists) keeps the record deleting; once the
// object is gone, the start-up reconciliation finishes the record and
// releases the slug.
func TestFlatAbsence_TerminatingStaysDeletingThenStartupFinishes(t *testing.T) {
	f := newPartitionFixture(t)
	f.d.keepOnDelete = true
	serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, "")
	assertDeletingAndReserved(t, f, "terminating object")

	f.d.mu.Lock()
	f.d.keepOnDelete = false
	f.d.mu.Unlock()
	f.d.remove("cid-a1")
	present := f.d.list(map[string]string{"scion.agent": "true"})
	if _, err := f.a.srv.ownership.ReconcileAbsent(present, func(h api.ResourceHandle) (bool, error) {
		return f.a.rt.ResourceAbsent(context.Background(), h)
	}, nil); err != nil {
		t.Fatal(err)
	}
	assertDeletedAndReleased(t, f)
}

// TestFlatAbsence_StartupKeepsRecordWithoutRemovedFiles: the start-up
// reconciliation never finishes a deleting record whose files were not
// removed (its delete failed before that), even with every object gone.
func TestFlatAbsence_StartupKeepsRecordWithoutRemovedFiles(t *testing.T) {
	f := newPartitionFixture(t)
	if err := f.a.srv.ownership.SetRecordState("proj-1", "agent-a1", OwnershipStateDeleting); err != nil {
		t.Fatal(err)
	}
	f.d.remove("cid-a1")
	if _, err := f.a.srv.ownership.ReconcileAbsent(nil, func(api.ResourceHandle) (bool, error) { return true, nil }, nil); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := f.a.srv.ownership.Get("proj-1", "agent-a1")
	if rec.State != OwnershipStateDeleting {
		t.Fatalf("record = %s, want deleting (files not removed)", rec.State)
	}
	if holder, _ := f.a.srv.ownership.SlugHolder("proj-1", "worker"); holder != "agent-a1" {
		t.Fatal("slug released although the files were not removed")
	}
}

// TestFlatAbsence_RuntimeWithoutCheckRefusedAtStartup: a flat instance
// whose runtime cannot confirm absence refuses to start.
func TestFlatAbsence_RuntimeWithoutCheckRefusedAtStartup(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	srv := New(f.srv.config, f.mgr, noAbsenceRuntime{f.srv.currentRuntime()})
	err := srv.StartServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot confirm that a deleted object is gone") {
		t.Fatalf("StartServices = %v, want the refusal", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// noAbsenceRuntime is a runtime without the exact absence check.
type noAbsenceRuntime struct{ runtime.Runtime }

// TestFlatAbsence_SlugReservedByUnconfirmedDeleteNamesIt: a create of a
// slug still reserved by an unconfirmed delete is a conflict naming the
// slug, the instance and the unconfirmed cleanup.
func TestFlatAbsence_SlugReservedByUnconfirmedDeleteNamesIt(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	st := f.srv.ownership
	if err := st.BeginRun(flatTestProjectID, "agent-old", "flat-agent", "run-old"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRecordState(flatTestProjectID, "agent-old", OwnershipStateDeleting); err != nil {
		t.Fatal(err)
	}
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-reuse", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"flat-agent", f.identity.RuntimeBrokerID, "not yet confirmed"} {
		if !strings.Contains(body, want) {
			t.Errorf("conflict message %q lacks %q", body, want)
		}
	}
}

// failOnceDeleteRuntime fails the first UID-conditioned delete (a transient
// API error), then behaves like the shared daemon.
type failOnceDeleteRuntime struct {
	*daemonRuntime
	failed bool
}

func (r *failOnceDeleteRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	if !r.failed {
		r.failed = true
		return errors.New("transient: the API server returned 500")
	}
	return r.daemonRuntime.DeleteResource(ctx, h)
}

// TestFlatAbsence_TransientCleanupFailureDoesNotWedgeTheSlug: when the
// runtime delete succeeded but the UID-conditioned cleanup failed once,
// the files are still recorded as removed, and the retry (which finds
// nothing left) finishes the record and releases the slug.
func TestFlatAbsence_TransientCleanupFailureDoesNotWedgeTheSlug(t *testing.T) {
	f := newPartitionFixture(t)
	f.a.mgr.Runtime = &failOnceDeleteRuntime{daemonRuntime: f.a.rt}
	serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, "")
	rec, _, _ := f.a.srv.ownership.Get("proj-1", "agent-a1")
	if rec.State != OwnershipStateDeleting || !rec.FilesRemoved {
		t.Fatalf("after the failed cleanup: state=%s filesRemoved=%v, want deleting with the files recorded as removed", rec.State, rec.FilesRemoved)
	}
	if w := serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, ""); w.Code != http.StatusNotFound {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	assertDeletedAndReleased(t, f)
}

// TestFlatAbsence_CrashBeforeMarkerFinishedByNotFoundRetry: a record left
// deleting without the files marker (a crash right after the runtime
// delete) is finished by a retry that finds neither a runtime entry nor
// files, once its objects are confirmed gone.
func TestFlatAbsence_CrashBeforeMarkerFinishedByNotFoundRetry(t *testing.T) {
	f := newPartitionFixture(t)
	if err := f.a.srv.ownership.SetRecordState("proj-1", "agent-a1", OwnershipStateDeleting); err != nil {
		t.Fatal(err)
	}
	f.d.remove("cid-a1")
	if w := serveFlat(f.a.srv, http.MethodDelete, absenceDeleteURL, ""); w.Code != http.StatusNotFound {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	assertDeletedAndReleased(t, f)
}

// TestFlatAbsence_FilesMarkerOnlyWhenFilesRemoved: a whole-agent delete
// that removed no files (local only, without file deletion) does not
// record the files as removed.
func TestFlatAbsence_FilesMarkerOnlyWhenFilesRemoved(t *testing.T) {
	f := newPartitionFixture(t)
	dir := hubProjectDir(t, "marker-proj", "proj-1")
	f.d.mu.Lock()
	for i := range f.d.objects {
		if f.d.objects[i].ContainerID == "cid-a1" {
			f.d.objects[i].ProjectPath = filepath.Join(dir, ".scion")
			f.d.objects[i].Labels["scion.project_path"] = filepath.Join(dir, ".scion")
		}
	}
	f.d.mu.Unlock()
	f.d.keepOnDelete = true // stays deleting, so the marker is observable
	serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1&localOnly=true", "")
	rec, _, _ := f.a.srv.ownership.Get("proj-1", "agent-a1")
	if rec.State != OwnershipStateDeleting {
		t.Fatalf("state = %s", rec.State)
	}
	if rec.FilesRemoved {
		t.Fatal("the files were recorded as removed although none were removed")
	}
}
