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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// A create whose provisioning failed leaves an active record whose only
// run its failure cleanup already finished. The Hub's rollback delete then
// finds nothing on the runtime or on disk (ptone/scion#3274).

const ghostAgentID, ghostSlug, ghostRun = "agent-id-ghost", "ghost", "run-ghost"

func seedFailedCreateRecord(t *testing.T, f *flatInstanceFixture, endRun bool) {
	t.Helper()
	st := f.srv.ownership
	if err := st.BeginRun(flatTestProjectID, ghostAgentID, ghostSlug, ghostRun); err != nil {
		t.Fatal(err)
	}
	if !endRun {
		return
	}
	for _, state := range []string{OwnershipStateDeleting, OwnershipStateDeleted} {
		if err := st.SetRunState(flatTestProjectID, ghostAgentID, ghostRun, state); err != nil {
			t.Fatal(err)
		}
	}
}

func ghostState(t *testing.T, f *flatInstanceFixture) (state, holder string) {
	t.Helper()
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, ghostAgentID)
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	holder, err = f.srv.ownership.SlugHolder(flatTestProjectID, ghostSlug)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State, holder
}

func deleteGhost(t *testing.T, f *flatInstanceFixture, query string) {
	t.Helper()
	w := serveFlat(f.srv, http.MethodDelete, "/api/v1/agents/"+ghostSlug+"?projectId="+flatTestProjectID+query, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete: status = %d, want 404 (nothing on the runtime or on disk): %s", w.Code, w.Body.String())
	}
}

// TestFlatDelete_FailedCreateGhostIsRetiredByHubRollbackDelete: a create
// that fails at the broker leaves an active record holding the slug, whose
// only run its failure cleanup finished. The Hub's whole-agent rollback
// delete then marks the record deleted (tombstone kept) and releases the
// slug, so a create of the same slug with a new agent ID succeeds.
func TestFlatDelete_FailedCreateGhostIsRetiredByHubRollbackDelete(t *testing.T) {
	for _, query := range []string{"&deleteFiles=true&runId=" + ghostRun, "&deleteFiles=true", "&localOnly=true"} {
		t.Run(query, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			target := map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
				"config": map[string]interface{}{"template": "claude"}}

			// The failed create (as when the workspace guard refuses the
			// workspace source).
			f.mgr.startErr = errors.New(`workspace source "/tmp" is not an allowed workspace path`)
			failed := map[string]interface{}{"id": ghostAgentID, "runId": ghostRun}
			for k, v := range target {
				failed[k] = v
			}
			if w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-ghost", ghostSlug, failed)); w.Code < 400 {
				t.Fatalf("create: status = %d, want a failure: %s", w.Code, w.Body.String())
			}
			state, holder := ghostState(t, f)
			if state != OwnershipStateActive || holder != ghostAgentID {
				t.Fatalf("after the failed create: state=%s holder=%q, want active holding the slug", state, holder)
			}
			rec, _, _ := f.srv.ownership.Get(flatTestProjectID, ghostAgentID)
			if liveRunsOtherThan(rec, "") {
				t.Fatalf("the failed create's run is still live: %+v", rec.Runs)
			}

			// The Hub's rollback delete finds nothing on the runtime or on
			// disk.
			deleteGhost(t, f, query)
			if state, holder := ghostState(t, f); state != OwnershipStateDeleted || holder != "" {
				t.Fatalf("after the rollback delete: state=%s holder=%q, want deleted with the slug released", state, holder)
			}
			if live, err := f.srv.ownership.HasLiveAgents(flatTestProjectID); err != nil || live {
				t.Fatalf("an active record remains in the project (%v)", err)
			}

			// The same slug, with a new agent ID, is created.
			f.mgr.startErr = nil
			again := map[string]interface{}{"id": "agent-id-ghost-2"}
			for k, v := range target {
				again[k] = v
			}
			if w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-ghost-2", ghostSlug, again)); w.Code != http.StatusCreated {
				t.Fatalf("same-slug re-create: status = %d, want 201: %s", w.Code, w.Body.String())
			}
			if holder, _ := f.srv.ownership.SlugHolder(flatTestProjectID, ghostSlug); holder != "agent-id-ghost-2" {
				t.Fatalf("slug holder = %q, want the new agent", holder)
			}
		})
	}
}

// TestFlatDelete_GhostRetirementControls: a record with a live run, or a
// delete that is not a whole-agent delete, is left active with its slug.
func TestFlatDelete_GhostRetirementControls(t *testing.T) {
	cases := []struct {
		name   string
		endRun bool
		query  string
	}{
		{"live provisioning run", false, "&deleteFiles=true&runId=other-run"},
		{"unfenced delete, live provisioning run", false, "&deleteFiles=true"},
		{"soft delete", true, "&deleteFiles=true&softDelete=true"},
		{"no file delete", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			seedFailedCreateRecord(t, f, tc.endRun)
			deleteGhost(t, f, tc.query)
			if state, holder := ghostState(t, f); state != OwnershipStateActive || holder != ghostAgentID {
				t.Fatalf("state=%s holder=%q, want active with the slug still reserved", state, holder)
			}
		})
	}
}

// TestFlatDelete_GhostWithUnconfirmedObjectStaysDeleting: an ended record
// whose run still records an object not confirmed absent (its failure
// cleanup could not confirm it) is retired to deleting by the rollback
// delete, but not finished: the slug stays reserved until absence is
// confirmed.
func TestFlatDelete_GhostWithUnconfirmedObjectStaysDeleting(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	st := f.srv.ownership
	if err := st.BeginRun(flatTestProjectID, ghostAgentID, ghostSlug, ghostRun); err != nil {
		t.Fatal(err)
	}
	if err := st.AddResource(flatTestProjectID, ghostAgentID, ghostRun,
		api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "c-ghost", UID: "uid-ghost"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunState(flatTestProjectID, ghostAgentID, ghostRun, OwnershipStateDeleting); err != nil {
		t.Fatal(err)
	}
	confirmAbsence(f, func(api.ResourceHandle) (bool, error) { return false, nil })

	deleteGhost(t, f, "&deleteFiles=true&runId="+ghostRun)
	if state, holder := ghostState(t, f); state != OwnershipStateDeleting || holder != ghostAgentID {
		t.Fatalf("state=%s holder=%q, want deleting with the slug still reserved", state, holder)
	}
	rec, _, _ := st.Get(flatTestProjectID, ghostAgentID)
	if res := rec.Runs[0].Resources; len(res) != 1 || res[0].State != OwnedResourceRecorded {
		t.Fatalf("the unconfirmed object is no longer recorded: %+v", res)
	}
}
