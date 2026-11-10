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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Field ownership on registration of an existing Runtime Broker: values set
// through the update API (GCP host service account and project, labels)
// persist across registrations; registration fills the GCP host fields only
// while they are unset.

const (
	ownedEmailA   = "host-a@proj-a.iam.gserviceaccount.com"
	ownedEmailB   = "host-b@proj-b.iam.gserviceaccount.com"
	ownedProjectA = "proj-a"
	ownedProjectB = "proj-b"
)

// ownedBrokerFixture is an existing Runtime Broker row, flat or legacy,
// created by owner, plus a request template that re-registers it.
type ownedBrokerFixture struct {
	srv   *Server
	s     store.Store
	owner *store.User
	id    string
	flat  bool
	base  CreateBrokerRegistrationRequest
}

func newOwnedBrokerFixture(t *testing.T, flat bool, name string) *ownedBrokerFixture {
	t.Helper()
	return newOwnedBrokerFixtureWithStore(t, flat, name, nil)
}

// newOwnedBrokerFixtureWithStore is newOwnedBrokerFixture with install, when
// not nil, called right after the server is built and before any setup, so a
// store wrapper can be installed with installStoreFault (and armed later).
func newOwnedBrokerFixtureWithStore(t *testing.T, flat bool, name string, install func(srv *Server)) *ownedBrokerFixture {
	t.Helper()
	if flat {
		srv, s := testServer(t)
		if install != nil {
			install(srv)
		}
		setFlatExperiment(t, srv, true)
		f := &flatRegFixture{
			srv: srv, s: s,
			operator: newHubMemberUser(t, s, "flat-reg-operator"),
			other:    newHubMemberUser(t, s, "flat-reg-other"),
			target:   &api.RuntimeTargetDescriptor{ID: tid("flat-reg-target-" + t.Name()), Type: "docker", DisplayName: "Local Docker"},
		}
		id := tid("owned-" + name)
		base := CreateBrokerRegistrationRequest{BrokerID: id, Name: name, RuntimeTarget: f.targetFor(id)}
		rec := f.register(t, f.operator, base)
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		return &ownedBrokerFixture{srv: f.srv, s: f.s, owner: f.operator, id: id, flat: true, base: base}
	}
	srv, s := testServer(t)
	if install != nil {
		install(srv)
	}
	owner := newHubMemberUser(t, s, "owned-owner-"+name)
	b := createReregistrationTestBroker(t, s, name, owner.ID)
	return &ownedBrokerFixture{srv: srv, s: s, owner: owner, id: b.ID, base: CreateBrokerRegistrationRequest{Name: name}}
}

// req returns the re-registration request with mutate applied.
func (f *ownedBrokerFixture) req(mutate func(*CreateBrokerRegistrationRequest)) CreateBrokerRegistrationRequest {
	r := f.base
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func (f *ownedBrokerFixture) reregister(t *testing.T, mutate func(*CreateBrokerRegistrationRequest)) CreateBrokerRegistrationResponse {
	t.Helper()
	rec := doRequestAsUserLoopback(t, f.srv, f.owner, http.MethodPost, "/api/v1/brokers", f.req(mutate))
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	require.Equal(t, f.id, resp.BrokerID)
	return resp
}

func (f *ownedBrokerFixture) update(t *testing.T, body map[string]interface{}) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/runtime-brokers/"+f.id, body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func (f *ownedBrokerFixture) row(t *testing.T) *store.RuntimeBroker {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.id)
	require.NoError(t, err)
	return b
}

func (f *ownedBrokerFixture) requireGCPHost(t *testing.T, email, project string) {
	t.Helper()
	b := f.row(t)
	assert.Equal(t, email, b.GCPHostServiceAccountEmail, "GCP host service account")
	assert.Equal(t, project, b.GCPHostProjectID, "GCP host project")
}

func forFlatAndLegacy(t *testing.T, fn func(t *testing.T, flat bool)) {
	t.Helper()
	for _, flat := range []bool{true, false} {
		name := "legacy"
		if flat {
			name = "flat"
		}
		t.Run(name, func(t *testing.T) { fn(t, flat) })
	}
}

func withGCPHost(email, project string) func(*CreateBrokerRegistrationRequest) {
	return func(r *CreateBrokerRegistrationRequest) {
		r.GCPHostServiceAccountEmail = email
		r.GCPHostProjectID = project
	}
}

func gcpHostBody(email, project string) map[string]interface{} {
	return map[string]interface{}{"gcpHostServiceAccountEmail": email, "gcpHostProjectId": project}
}

func keptByField(kept []KeptSetting) map[string]KeptSetting {
	out := make(map[string]KeptSetting, len(kept))
	for _, k := range kept {
		out[k.Field] = k
	}
	return out
}

func TestReregistration_KeepsGCPHostFieldsSetByUpdate(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "keeps-update-value")
		audit := installBrokerAuditCapture(f.srv)
		f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))

		resp := f.reregister(t, withGCPHost(ownedEmailB, ownedProjectB))

		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Equal(t, store.GCPHostSourceAdmin, f.row(t).GCPHostSource)
		kept := keptByField(resp.KeptSettings)
		assert.Equal(t, KeptSetting{Field: "gcpHostServiceAccountEmail", StoredValue: ownedEmailA, RequestedValue: ownedEmailB, Reason: KeptReasonSet}, kept["gcpHostServiceAccountEmail"])
		assert.Equal(t, KeptSetting{Field: "gcpHostProjectId", StoredValue: ownedProjectA, RequestedValue: ownedProjectB, Reason: KeptReasonSet}, kept["gcpHostProjectId"])
		events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
		require.Len(t, events, 1)
		assert.Equal(t, "true", events[0].Details["gcp_host_kept"])
		assert.NotContains(t, events[0].Details, "gcp_host_set")
	})
}

func TestReregistration_OmittedGCPHostFieldsKeepStoredValues(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "omitted-keeps")
		f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))

		resp := f.reregister(t, nil)

		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Empty(t, resp.KeptSettings)
	})
}

func TestReregistration_FillsUnsetGCPHostFields(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "fills-unset")
		audit := installBrokerAuditCapture(f.srv)

		resp := f.reregister(t, withGCPHost("Host-A@Proj-A.iam.gserviceaccount.com", ownedProjectA))

		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Equal(t, store.GCPHostSourceRegistration, f.row(t).GCPHostSource)
		assert.Empty(t, resp.KeptSettings)
		events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
		require.Len(t, events, 1)
		assert.Equal(t, "true", events[0].Details["gcp_host_set"])
	})
}

func TestReregistration_GCPHostFieldsClearedByUpdateStayCleared(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "cleared-stays")
		f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
		f.update(t, gcpHostBody("", ""))

		f.reregister(t, withGCPHost(ownedEmailB, ownedProjectB))

		f.requireGCPHost(t, "", "")
		assert.Equal(t, store.GCPHostSourceAdmin, f.row(t).GCPHostSource)
	})
}

func TestReregistration_GCPHostFieldsWrittenTogether(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "written-together")
		// A stored email without a project (not from the update API).
		applied, err := f.s.SetRuntimeBrokerGCPHostIfUnset(context.Background(), f.id, ownedEmailA, "", store.GCPHostSourceRegistration)
		require.NoError(t, err)
		require.True(t, applied)

		f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))

		f.requireGCPHost(t, ownedEmailA, "")
	})
}

func TestReregistration_KeepsGCPHostFieldsForEveryCaller(t *testing.T) {
	t.Parallel()
	callers := []struct {
		name string
		do   func(t *testing.T, f *ownedBrokerFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder
	}{
		{"owner session", func(t *testing.T, f *ownedBrokerFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			return doRequestAsUserLoopback(t, f.srv, f.owner, http.MethodPost, "/api/v1/brokers", req)
		}},
		{"owner token", func(t *testing.T, f *ownedBrokerFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			key := mintHubBrokerUAT(t, f.srv, f.owner.ID, "broker:create")
			return doRequestWithTokenLoopback(t, f.srv, key, http.MethodPost, "/api/v1/brokers", req)
		}},
		{"super-admin", func(t *testing.T, f *ownedBrokerFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			return doRequestLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers", req)
		}},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			forFlatAndLegacy(t, func(t *testing.T, flat bool) {
				f := newOwnedBrokerFixture(t, flat, "every-caller")
				f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))

				rec := c.do(t, f, f.req(withGCPHost(ownedEmailB, ownedProjectB)))

				require.Equal(t, http.StatusCreated, rec.Code, "re-registration is admitted: %s", rec.Body.String())
				assert.NotEmpty(t, decodeBrokerRegistration(t, rec.Body).JoinToken)
				f.requireGCPHost(t, ownedEmailA, ownedProjectA)
			})
		})
	}
}

func TestReregistration_KeepsGCPHostFieldsOnEveryPath(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		for _, preserve := range []bool{false, true} {
			name := "preserve off"
			if preserve {
				name = "preserve on"
			}
			t.Run(name, func(t *testing.T) {
				f := newOwnedBrokerFixture(t, flat, "every-path")
				f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))

				f.reregister(t, func(r *CreateBrokerRegistrationRequest) {
					r.PreserveSettings = preserve
					r.GCPHostServiceAccountEmail = ownedEmailB
					r.GCPHostProjectID = ownedProjectB
				})
				f.requireGCPHost(t, ownedEmailA, ownedProjectA)

				f.reregister(t, func(r *CreateBrokerRegistrationRequest) { r.PreserveSettings = preserve })
				f.requireGCPHost(t, ownedEmailA, ownedProjectA)
			})
		}
	})
}

func TestReregistration_AddsAbsentLabelsOnly(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "absent-labels")
		f.update(t, map[string]interface{}{"labels": map[string]string{"team": "a", "keep": "me"}})

		resp := f.reregister(t, func(r *CreateBrokerRegistrationRequest) {
			r.Labels = map[string]string{"team": "b", "zone": "x"}
		})

		labels := f.row(t).Labels
		assert.Equal(t, "a", labels["team"], "an existing key keeps its value")
		assert.Equal(t, "x", labels["zone"], "an absent key is added")
		assert.Equal(t, "me", labels["keep"], "no key is removed")
		assert.Equal(t, []KeptSetting{{Field: "labels.team", StoredValue: "a", RequestedValue: "b", Reason: KeptReasonSet}}, resp.KeptSettings)
	})
}

func TestReregistration_ReportsKeptSettings(t *testing.T) {
	t.Parallel()
	t.Run("values", func(t *testing.T) {
		forFlatAndLegacy(t, func(t *testing.T, flat bool) {
			f := newOwnedBrokerFixture(t, flat, "reports-values")
			f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
			f.update(t, map[string]interface{}{"labels": map[string]string{"team": "a"}})

			resp := f.reregister(t, func(r *CreateBrokerRegistrationRequest) {
				withGCPHost(ownedEmailB, ownedProjectB)(r)
				r.Labels = map[string]string{"team": "b"}
			})

			assert.ElementsMatch(t, []KeptSetting{
				{Field: "gcpHostServiceAccountEmail", StoredValue: ownedEmailA, RequestedValue: ownedEmailB, Reason: KeptReasonSet},
				{Field: "gcpHostProjectId", StoredValue: ownedProjectA, RequestedValue: ownedProjectB, Reason: KeptReasonSet},
				{Field: "labels.team", StoredValue: "a", RequestedValue: "b", Reason: KeptReasonSet},
			}, resp.KeptSettings)
		})
	})
	t.Run("omitted", func(t *testing.T) {
		forFlatAndLegacy(t, func(t *testing.T, flat bool) {
			f := newOwnedBrokerFixture(t, flat, "reports-omitted")
			f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
			resp := f.reregister(t, nil)
			assert.Empty(t, resp.KeptSettings)
			resp = f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))
			assert.Empty(t, resp.KeptSettings, "an equal value is not reported")
		})
	})
	t.Run("audited", func(t *testing.T) {
		forFlatAndLegacy(t, func(t *testing.T, flat bool) {
			f := newOwnedBrokerFixture(t, flat, "reports-audited")
			audit := installBrokerAuditCapture(f.srv)
			f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
			f.update(t, map[string]interface{}{"labels": map[string]string{"team": "a"}})

			f.reregister(t, func(r *CreateBrokerRegistrationRequest) {
				withGCPHost(ownedEmailB, ownedProjectB)(r)
				r.Labels = map[string]string{"team": "b"}
			})

			events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
			require.Len(t, events, 1)
			d := events[0].Details
			assert.Equal(t, ownedEmailA, d["kept.gcpHostServiceAccountEmail.stored"])
			assert.Equal(t, ownedEmailB, d["kept.gcpHostServiceAccountEmail.requested"])
			assert.Equal(t, ownedProjectA, d["kept.gcpHostProjectId.stored"])
			assert.Equal(t, ownedProjectB, d["kept.gcpHostProjectId.requested"])
			assert.Equal(t, "a", d["kept.labels.team.stored"])
			assert.Equal(t, "b", d["kept.labels.team.requested"])
		})
	})
	t.Run("project without service account", func(t *testing.T) {
		forFlatAndLegacy(t, func(t *testing.T, flat bool) {
			f := newOwnedBrokerFixture(t, flat, "reports-project-only")
			resp := f.reregister(t, withGCPHost("", ownedProjectA))
			f.requireGCPHost(t, "", "")
			assert.Equal(t, []KeptSetting{{Field: "gcpHostProjectId", StoredValue: "", RequestedValue: ownedProjectA, Reason: KeptReasonEmailRequired}}, resp.KeptSettings)
		})
	})
	t.Run("cleared", func(t *testing.T) {
		forFlatAndLegacy(t, func(t *testing.T, flat bool) {
			f := newOwnedBrokerFixture(t, flat, "reports-cleared")
			f.update(t, gcpHostBody("", ""))

			resp := f.reregister(t, withGCPHost(ownedEmailB, ownedProjectB))

			assert.ElementsMatch(t, []KeptSetting{
				{Field: "gcpHostServiceAccountEmail", StoredValue: "", RequestedValue: ownedEmailB, Reason: KeptReasonPinned},
				{Field: "gcpHostProjectId", StoredValue: "", RequestedValue: ownedProjectB, Reason: KeptReasonPinned},
			}, resp.KeptSettings)
		})
	})
}

// TestReregistration_KeptSettingsInResponseJSON pins the wire shape of the
// response field.
func TestReregistration_KeptSettingsInResponseJSON(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "kept-json")
	f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
	rec := doRequestAsUserLoopback(t, f.srv, f.owner, http.MethodPost, "/api/v1/brokers", f.req(withGCPHost(ownedEmailB, "")))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, []interface{}{map[string]interface{}{
		"field": "gcpHostServiceAccountEmail", "storedValue": ownedEmailA, "requestedValue": ownedEmailB, "reason": "set",
	}}, raw["keptSettings"])
}

func TestRegisterProjectBroker_KeepsNameAndGCPHostFields(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()
	brokerID := tid("register-project-keeps")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "register-project-keeps-1",
		GitRemote: "github.com/org/register-project-keeps-1",
		Broker:    &RegisterProjectBrokerInfo{ID: brokerID, Name: "original-name", Version: "1.0.0"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+brokerID, map[string]interface{}{
		"name": "renamed-by-update", "gcpHostServiceAccountEmail": ownedEmailA, "gcpHostProjectId": ownedProjectA,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "register-project-keeps-2",
		GitRemote: "github.com/org/register-project-keeps-2",
		Broker:    &RegisterProjectBrokerInfo{ID: brokerID, Name: "original-name", Version: "1.1.0"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	b, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, "renamed-by-update", b.Name)
	assert.Equal(t, slugify("original-name"), b.Slug, "the stored slug is kept (the update API renamed without changing it)")
	assert.Equal(t, ownedEmailA, b.GCPHostServiceAccountEmail)
	assert.Equal(t, ownedProjectA, b.GCPHostProjectID)
	assert.Equal(t, "1.1.0", b.Version, "reported facts still apply")
}

func TestJoin_LeavesUpdateManagedFields(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "join-leaves")
		f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
		f.update(t, map[string]interface{}{"labels": map[string]string{"team": "a"}})
		resp := f.reregister(t, func(r *CreateBrokerRegistrationRequest) { r.PreserveSettings = true })

		join := BrokerJoinRequest{BrokerID: f.id, JoinToken: resp.JoinToken, Hostname: "join-leaves", Version: "1.0.0"}
		if flat {
			join.RuntimeTarget = f.base.RuntimeTarget
		}
		rec := doRequestNoAuthLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers/join", join)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Equal(t, "a", f.row(t).Labels["team"])
	})
}

// TestHeartbeat_LeavesUpdateManagedFields: a writer holding a copy read
// before an update (the heartbeat refresh writes such a copy) leaves the
// GCP host fields set by the update.
func TestHeartbeat_LeavesUpdateManagedFields(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "heartbeat-leaves")
		ctx := context.Background()
		stale := f.row(t)

		f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
		stale.Status = store.BrokerStatusOnline
		stale.LastHeartbeat = time.Now()
		require.NoError(t, f.s.UpdateRuntimeBroker(ctx, stale))

		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Equal(t, store.BrokerStatusOnline, f.row(t).Status)
	})
}

// TestReregistration_NeverClearsGCPHostFields: no registration, join or
// heartbeat path clears a stored GCP host pair; only the update API does.
func TestReregistration_NeverClearsGCPHostFields(t *testing.T) {
	t.Parallel()
	paths := []struct {
		name string
		run  func(t *testing.T, f *ownedBrokerFixture)
	}{
		{"registration without fields", func(t *testing.T, f *ownedBrokerFixture) { f.reregister(t, nil) }},
		{"registration with empty fields", func(t *testing.T, f *ownedBrokerFixture) { f.reregister(t, withGCPHost("", "")) }},
		{"registration with preserve", func(t *testing.T, f *ownedBrokerFixture) {
			f.reregister(t, func(r *CreateBrokerRegistrationRequest) { r.PreserveSettings = true })
		}},
		{"join", func(t *testing.T, f *ownedBrokerFixture) {
			resp := f.reregister(t, nil)
			join := BrokerJoinRequest{BrokerID: f.id, JoinToken: resp.JoinToken, Hostname: "never-clears", Version: "1.0.0", RuntimeTarget: f.base.RuntimeTarget}
			rec := doRequestNoAuthLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers/join", join)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}},
		{"heartbeat copy", func(t *testing.T, f *ownedBrokerFixture) {
			b := f.row(t)
			b.GCPHostServiceAccountEmail, b.GCPHostProjectID = "", ""
			require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			forFlatAndLegacy(t, func(t *testing.T, flat bool) {
				for _, source := range []string{store.GCPHostSourceRegistration, store.GCPHostSourceAdmin} {
					f := newOwnedBrokerFixture(t, flat, "never-clears")
					if source == store.GCPHostSourceAdmin {
						f.update(t, gcpHostBody(ownedEmailA, ownedProjectA))
					} else {
						f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))
					}
					p.run(t, f)
					f.requireGCPHost(t, ownedEmailA, ownedProjectA)
				}
			})
		})
	}
	t.Run("co-located flat registration", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		row := registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
		_, err := f.s.SetRuntimeBrokerGCPHost(context.Background(), row.ID, ownedEmailA, ownedProjectA)
		require.NoError(t, err)
		registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
		b, err := f.s.GetRuntimeBroker(context.Background(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, ownedEmailA, b.GCPHostServiceAccountEmail)
		assert.Equal(t, ownedProjectA, b.GCPHostProjectID)
	})
}

// registerColocatedFlatRow runs the co-located flat registration for the
// fixture's target.
func registerColocatedFlatRow(t *testing.T, f *flatRegFixture, opts EmbeddedFlatRegistrationOptions) *store.RuntimeBroker {
	t.Helper()
	id := embeddedFlatTestIdentity(f)
	inst := embeddedFlatTestInstance(f)
	if opts.Endpoint == "" {
		opts.Endpoint = "http://localhost:9800"
	}
	row, err := f.srv.RegisterEmbeddedFlatRuntimeBroker(context.Background(), id, inst, opts)
	require.NoError(t, err)
	return row
}

func TestEmbeddedFlat_LeavesGCPHostFields(t *testing.T) {
	t.Parallel()
	f := newFlatRegFixture(t, true)
	ctx := context.Background()
	row := registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
	assert.Empty(t, row.GCPHostServiceAccountEmail, "the co-located flat registration never writes the GCP host pair")

	_, err := f.s.SetRuntimeBrokerGCPHost(ctx, row.ID, ownedEmailA, ownedProjectA)
	require.NoError(t, err)
	registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})

	b, err := f.s.GetRuntimeBroker(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, ownedEmailA, b.GCPHostServiceAccountEmail)
	assert.Equal(t, ownedProjectA, b.GCPHostProjectID)
	assert.Equal(t, store.GCPHostSourceAdmin, b.GCPHostSource)
}

func TestBrokerUpdate_EmitsAuditEvent(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "update-audit")
	audit := installBrokerAuditCapture(f.srv)

	f.update(t, map[string]interface{}{
		"name":                       "update-audit-renamed",
		"labels":                     map[string]string{"team": "a"},
		"gcpHostServiceAccountEmail": ownedEmailA,
		"gcpHostProjectId":           ownedProjectA,
	})

	events := brokerAuditEventsOfType(audit, BrokerAuthEventUpdateSettings)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, f.id, e.BrokerID)
	assert.Equal(t, f.owner.ID, e.ActorID)
	assert.Equal(t, string(CredentialKindInteractive), e.Details["credential_kind"])
	assert.Equal(t, "update-audit", e.Details["name.old"])
	assert.Equal(t, "update-audit-renamed", e.Details["name.new"])
	assert.Equal(t, "team", e.Details["labels.added"])
	assert.Equal(t, "env", e.Details["labels.removed"])
	assert.Equal(t, "", e.Details["gcp_host_service_account_email.old"])
	assert.Equal(t, ownedEmailA, e.Details["gcp_host_service_account_email.new"])
	assert.Equal(t, ownedProjectA, e.Details["gcp_host_project_id.new"])
	assert.Equal(t, store.GCPHostSourceAdmin, e.Details["gcp_host_source.new"])

	// An update that changes nothing records no event.
	f.update(t, map[string]interface{}{"name": "update-audit-renamed", "gcpHostServiceAccountEmail": ownedEmailA})
	assert.Len(t, brokerAuditEventsOfType(audit, BrokerAuthEventUpdateSettings), 1)
}

func TestBrokerUpdate_ReleaseGCPHostFieldsToRegistration(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "release")
		f.update(t, gcpHostBody("", ""))
		f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))
		f.requireGCPHost(t, "", "")

		f.update(t, map[string]interface{}{"gcpHostPinned": false})
		assert.Empty(t, f.row(t).GCPHostSource)
		f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))
		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		assert.Equal(t, store.GCPHostSourceRegistration, f.row(t).GCPHostSource)
	})
}

// fillBeforeUpdateStore fills the GCP host pair as a registration would,
// right before the update API's write lands.
// It is transparent until its switch is armed.
type fillBeforeUpdateStore struct {
	store.Store
	fault      *storeFaultSwitch
	failUpdate bool
}

func (s *fillBeforeUpdateStore) SetRuntimeBrokerGCPHostIfPrior(ctx context.Context, id, priorEmail, priorProject, email, project string) (store.GCPHostSetting, bool, error) {
	if !s.fault.Active() {
		return s.Store.SetRuntimeBrokerGCPHostIfPrior(ctx, id, priorEmail, priorProject, email, project)
	}
	if s.failUpdate {
		return store.GCPHostSetting{}, false, errors.New("injected GCP host write failure")
	}
	if _, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, id, ownedEmailA, ownedProjectA, store.GCPHostSourceRegistration); err != nil {
		return store.GCPHostSetting{}, false, err
	}
	return s.Store.SetRuntimeBrokerGCPHostIfPrior(ctx, id, priorEmail, priorProject, email, project)
}

// TestBrokerUpdate_AuditRecordsReplacedValues: the update audit event's old
// GCP host values are the ones the write replaced: the pair a registration
// filled, with its source.
func TestBrokerUpdate_AuditRecordsReplacedValues(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "update-audit-replaced")
	applied, err := f.s.SetRuntimeBrokerGCPHostIfUnset(context.Background(), f.id, ownedEmailA, ownedProjectA, store.GCPHostSourceRegistration)
	require.NoError(t, err)
	require.True(t, applied)
	audit := installBrokerAuditCapture(f.srv)

	f.update(t, gcpHostBody(ownedEmailB, ownedProjectB))

	f.requireGCPHost(t, ownedEmailB, ownedProjectB)
	events := brokerAuditEventsOfType(audit, BrokerAuthEventUpdateSettings)
	require.Len(t, events, 1)
	d := events[0].Details
	assert.Equal(t, ownedEmailA, d["gcp_host_service_account_email.old"])
	assert.Equal(t, ownedProjectA, d["gcp_host_project_id.old"])
	assert.Equal(t, store.GCPHostSourceRegistration, d["gcp_host_source.old"])
	assert.Equal(t, ownedEmailB, d["gcp_host_service_account_email.new"])
	assert.Equal(t, ownedProjectB, d["gcp_host_project_id.new"])
	assert.Equal(t, store.GCPHostSourceAdmin, d["gcp_host_source.new"])
}

// TestBrokerUpdate_StoredPairChangedAfterDecision_Conflict: when a
// registration fills the GCP host pair after the update's actAs decision and
// before its write, the update writes nothing and returns 409. The pair is
// written only over the stored value the decision was made against.
func TestBrokerUpdate_StoredPairChangedAfterDecision_Conflict(t *testing.T) {
	t.Parallel()
	var fault *storeFaultSwitch
	f := newOwnedBrokerFixtureWithStore(t, false, "update-stale-prior", func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *fillBeforeUpdateStore {
			return &fillBeforeUpdateStore{Store: inner, fault: sw}
		})
	})
	audit := installBrokerAuditCapture(f.srv)
	fault.Arm()

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/runtime-brokers/"+f.id, gcpHostBody(ownedEmailB, ownedProjectB))

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	f.requireGCPHost(t, ownedEmailA, ownedProjectA)
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventUpdateSettings),
		"no update event: the update applied nothing")
}

// TestBrokerUpdate_AuditCoversAppliedPartOfFailedUpdate: when a later write
// of an update fails, the event still records what was applied.
func TestBrokerUpdate_AuditCoversAppliedPartOfFailedUpdate(t *testing.T) {
	t.Parallel()
	var fault *storeFaultSwitch
	f := newOwnedBrokerFixtureWithStore(t, false, "update-audit-partial", func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *fillBeforeUpdateStore {
			return &fillBeforeUpdateStore{Store: inner, fault: sw, failUpdate: true}
		})
	})
	audit := installBrokerAuditCapture(f.srv)
	fault.Arm()

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/runtime-brokers/"+f.id, map[string]interface{}{
		"labels":                     map[string]string{"team": "a"},
		"gcpHostServiceAccountEmail": ownedEmailB,
	})

	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	events := brokerAuditEventsOfType(audit, BrokerAuthEventUpdateSettings)
	require.Len(t, events, 1)
	assert.Equal(t, "team", events[0].Details["labels.added"])
	assert.NotContains(t, events[0].Details, "gcp_host_service_account_email.new")
}

// gcpHostWriteSpyStore records calls to the GCP host writers once its switch
// is armed.
type gcpHostWriteSpyStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	calls []string
}

func (s *gcpHostWriteSpyStore) record(name string) {
	if !s.fault.Active() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}

func (s *gcpHostWriteSpyStore) SetRuntimeBrokerGCPHostIfUnset(ctx context.Context, id, email, project, source string) (bool, error) {
	s.record("SetRuntimeBrokerGCPHostIfUnset")
	return s.Store.SetRuntimeBrokerGCPHostIfUnset(ctx, id, email, project, source)
}

func (s *gcpHostWriteSpyStore) SetRuntimeBrokerGCPHostDetected(ctx context.Context, id, email, project string) (bool, error) {
	s.record("SetRuntimeBrokerGCPHostDetected")
	return s.Store.SetRuntimeBrokerGCPHostDetected(ctx, id, email, project)
}

func (s *gcpHostWriteSpyStore) SetRuntimeBrokerGCPHost(ctx context.Context, id, email, project string) (store.GCPHostSetting, error) {
	s.record("SetRuntimeBrokerGCPHost")
	return s.Store.SetRuntimeBrokerGCPHost(ctx, id, email, project)
}

func (s *gcpHostWriteSpyStore) SetRuntimeBrokerGCPHostIfPrior(ctx context.Context, id, priorEmail, priorProject, email, project string) (store.GCPHostSetting, bool, error) {
	s.record("SetRuntimeBrokerGCPHostIfPrior")
	return s.Store.SetRuntimeBrokerGCPHostIfPrior(ctx, id, priorEmail, priorProject, email, project)
}

func (s *gcpHostWriteSpyStore) SetRuntimeBrokerGCPHostSource(ctx context.Context, id, source string) (store.GCPHostSetting, error) {
	s.record("SetRuntimeBrokerGCPHostSource")
	return s.Store.SetRuntimeBrokerGCPHostSource(ctx, id, source)
}

// TestBrokerReregistration_DeniedCallerWritesNoGCPHostFields: a
// re-registration that fails authorization reaches no GCP host writer.
func TestBrokerReregistration_DeniedCallerWritesNoGCPHostFields(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		var spy *gcpHostWriteSpyStore
		var fault *storeFaultSwitch
		f := newOwnedBrokerFixtureWithStore(t, flat, "denied-no-write", func(srv *Server) {
			spy, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *gcpHostWriteSpyStore {
				return &gcpHostWriteSpyStore{Store: inner, fault: sw}
			})
			// The broker auth service writes legacy registrations through
			// its own store field.
			srv.brokerAuthService.store = spy
		})
		other := newHubMemberUser(t, f.s, "denied-no-write-other")
		fault.Arm()

		rec := doRequestAsUserLoopback(t, f.srv, other, http.MethodPost, "/api/v1/brokers", f.req(withGCPHost(ownedEmailA, ownedProjectA)))

		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Empty(t, spy.calls)
		f.requireGCPHost(t, "", "")
	})
}

// TestFlatReregistration_StalePinWritesNothing: a flat re-registration
// pinned to a row that no longer matches is refused before any write.
func TestFlatReregistration_StalePinWritesNothing(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, true, "stale-pin")
	other := newOwnedBrokerFixture(t, true, "stale-pin-other")
	otherRow := other.row(t)

	_, err := f.srv.createFlatBrokerRegistration(context.Background(), f.req(func(r *CreateBrokerRegistrationRequest) {
		withGCPHost(ownedEmailA, ownedProjectA)(r)
		r.Labels = map[string]string{"zone": "x"}
	}), f.owner.ID, otherRow, false, hostSAClaimVerdict{})

	require.True(t, errors.Is(err, ErrBrokerRegistrationAuthorizationStale), "got %v", err)
	f.requireGCPHost(t, "", "")
	assert.NotContains(t, f.row(t).Labels, "zone")
}

// TestBrokerReregistration_FlatAuthorizationMatrix: the re-registration
// authorization applies to flat rows with PreserveSettings on and off.
func TestBrokerReregistration_FlatAuthorizationMatrix(t *testing.T) {
	t.Parallel()
	type caller struct {
		name    string
		allowed bool
		do      func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder
	}
	callers := []caller{
		{"non-owner member", false, func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			return f.register(t, f.other, req)
		}},
		{"broker read-only member", false, func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			reader := newPlainUser(t, f.s, "flat-matrix-reader")
			grantSystemBrokerReadPermission(t, f.s, reader.ID)
			return f.register(t, reader, req)
		}},
		{"owner token", true, func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			key := mintHubBrokerUAT(t, f.srv, f.operator.ID, "broker:create")
			return doRequestWithTokenLoopback(t, f.srv, key, http.MethodPost, "/api/v1/brokers", req)
		}},
		{"super-admin token on another user's broker", false, func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			admin := newSuperAdminUser(t, f.s, "flat-matrix-admin")
			key := mintHubBrokerUAT(t, f.srv, admin.ID, "broker:create")
			return doRequestWithTokenLoopback(t, f.srv, key, http.MethodPost, "/api/v1/brokers", req)
		}},
		{"super-admin dev credential", true, func(t *testing.T, f *flatRegFixture, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
			return doRequestLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers", req)
		}},
	}
	for _, preserve := range []bool{false, true} {
		for _, c := range callers {
			name := c.name + "/preserve off"
			if preserve {
				name = c.name + "/preserve on"
			}
			t.Run(name, func(t *testing.T) {
				f := newFlatRegFixture(t, true)
				id := tid("flat-matrix")
				base := CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-matrix", RuntimeTarget: f.targetFor(id)}
				rec := f.register(t, f.operator, base)
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				before, err := f.s.GetJoinTokenByBrokerID(context.Background(), id)
				require.NoError(t, err)

				req := base
				req.PreserveSettings = preserve
				req.Labels = map[string]string{"zone": "x"}
				rec = c.do(t, f, req)

				if c.allowed {
					require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
					return
				}
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), "joinToken")
				row := f.flatRow(t, id)
				assert.NotContains(t, row.Labels, "zone", "a denied request changes nothing")
				after, err := f.s.GetJoinTokenByBrokerID(context.Background(), id)
				require.NoError(t, err)
				assert.Equal(t, before.TokenHash, after.TokenHash, "the join token is not re-minted")
			})
		}
	}
	t.Run("ownerless row", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		row := registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
		require.Empty(t, row.CreatedBy)
		target := row.RuntimeTarget
		adminKey := mintHubBrokerUAT(t, f.srv, newSuperAdminUser(t, f.s, "flat-matrix-ownerless-admin").ID, "broker:create")
		for _, preserve := range []bool{false, true} {
			req := CreateBrokerRegistrationRequest{BrokerID: row.ID, Name: row.Name, RuntimeTarget: &api.RuntimeTargetDescriptor{ID: target.ID, Type: target.Type, DisplayName: target.DisplayName}, PreserveSettings: preserve}
			rec := f.register(t, f.operator, req)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			rec = doRequestWithTokenLoopback(t, f.srv, adminKey, http.MethodPost, "/api/v1/brokers", req)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		}
	})
}

func embeddedFlatTestIdentity(f *flatRegFixture) *brokeridentity.Identity {
	return &brokeridentity.Identity{
		SchemaVersion:   brokeridentity.SchemaVersion,
		InstanceKey:     "local-docker",
		RuntimeBrokerID: tid("embedded-flat-row"),
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: f.target.ID, Type: f.target.Type},
		ExecutionScope:  brokeridentity.ExecutionScope{Type: f.target.Type, Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}},
		CreatedAt:       time.Now(),
	}
}

func embeddedFlatTestInstance(f *flatRegFixture) config.V1RuntimeBrokerInstanceConfig {
	return config.V1RuntimeBrokerInstanceConfig{Key: "local-docker", Name: "embedded-flat-row",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: f.target.Type, DisplayName: f.target.DisplayName}}
}

// TestBrokerRoleLabel_HubManaged: scion.io/broker-role is set by the Hub.
// A value in a registration or update request is ignored; HTTP registration
// creates rows with "remote".
func TestBrokerRoleLabel_HubManaged(t *testing.T) {
	t.Parallel()
	embeddedLabel := map[string]string{store.BrokerRoleLabel: store.BrokerRoleEmbedded, "team": "a"}

	t.Run("new flat row", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		id := tid("role-new-flat")
		rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "role-new-flat", RuntimeTarget: f.targetFor(id), Labels: embeddedLabel})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		row := f.flatRow(t, id)
		assert.Equal(t, store.BrokerRoleRemote, row.Labels[store.BrokerRoleLabel])
		assert.Equal(t, "a", row.Labels["team"])
	})

	t.Run("new legacy row", func(t *testing.T) {
		srv, s := testServer(t)
		member := newHubMemberUser(t, s, "role-new-legacy")
		rec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "role-new-legacy", Labels: embeddedLabel})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		row, err := s.GetRuntimeBroker(context.Background(), decodeBrokerRegistration(t, rec.Body).BrokerID)
		require.NoError(t, err)
		assert.Equal(t, store.BrokerRoleRemote, row.Labels[store.BrokerRoleLabel])
	})

	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "role-existing")
		before := f.row(t).Labels[store.BrokerRoleLabel]

		resp := f.reregister(t, func(r *CreateBrokerRegistrationRequest) { r.Labels = embeddedLabel })
		assert.Equal(t, before, f.row(t).Labels[store.BrokerRoleLabel], "re-registration")
		assert.NotContains(t, keptByField(resp.KeptSettings), "labels."+store.BrokerRoleLabel)

		f.update(t, map[string]interface{}{"labels": embeddedLabel})
		assert.Equal(t, before, f.row(t).Labels[store.BrokerRoleLabel], "update")
		assert.Equal(t, "a", f.row(t).Labels["team"])
	})
}
