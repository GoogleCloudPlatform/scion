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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The flat Cloud Run runtime target experiment (hub.flat_cloudrun_target,
// .design/flat-runtime-brokers/p5-cloudrun-amendment.md CR5): with it off, a
// NEW flat registration whose target type is cloudrun and an agent create on
// a flat Runtime Broker whose stored target type is cloudrun are refused
// with 412 experiment_disabled naming it. Docker and Kubernetes targets do
// not need it, and an existing cloudrun row still re-registers.

// setFlatExperiments stores explicit overrides for hub.flat_runtime_brokers
// and hub.flat_cloudrun_target.
func setFlatExperiments(t *testing.T, srv *Server, flatOn, cloudRunOn bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.FlatRuntimeBrokers: flatOn,
		experiments.FlatCloudRunTarget: cloudRunOn,
	})
}

// cloudRunTargetFor is a cloudrun runtime target of its own for name.
func cloudRunTargetFor(name string) *api.RuntimeTargetDescriptor {
	return &api.RuntimeTargetDescriptor{ID: tid("cloudrun-target-" + name), Type: "cloudrun", DisplayName: "Cloud Run"}
}

// nonCanonicalCloudRunTypes are spellings of cloudrun that differ from the
// canonical "cloudrun" only by case or surrounding whitespace, including
// control characters and a 4 KiB type string.
func nonCanonicalCloudRunTypes() []string {
	return []string{"CloudRun", " cloudrun ", "cloudrun\t", "cloudrun\n", "\tCLOUDRUN\r\n", "cloudrun" + strings.Repeat(" ", 4096)}
}

// requireTypeUnsupported asserts a 422 runtime_target_type_unsupported whose
// echoed type is the refused type cut to at most 64 bytes, carried only as a
// JSON-encoded string: the raw body holds no raw control character and the
// message does not contain the value.
func requireTypeUnsupported(t *testing.T, rec *httptest.ResponseRecorder, typ string) {
	t.Helper()
	body := strings.TrimRight(rec.Body.String(), "\n")
	d := requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeRuntimeTargetTypeUnsupported)
	echoed, ok := d["runtimeTargetType"].(string)
	require.True(t, ok, "the refused type is echoed as a JSON string: %s", body)
	assert.LessOrEqual(t, len(echoed), 64, "the echoed type is bounded")
	assert.Equal(t, truncateUTF8(typ, 64), echoed)
	assert.False(t, strings.ContainsAny(body, "\n\r\t"), "control characters are JSON-encoded in the response")
	assert.Less(t, len(body), 1024, "the response stays bounded")
	assert.NotContains(t, decodeFlatAPIError(t, rec).Message, typ, "the message never contains the refused type")
}

// requireCloudRunExperimentDisabled asserts a 412 experiment_disabled that
// names hub.flat_cloudrun_target.
func requireCloudRunExperimentDisabled(t *testing.T, d map[string]interface{}) {
	t.Helper()
	assert.Equal(t, experiments.FlatCloudRunTarget, d["experiment"], "the refusal names the Cloud Run target experiment")
}

func TestRegisterFlat_CloudRunExperimentOff(t *testing.T) {
	ctx := context.Background()

	t.Run("new cloudrun row refused, no row created", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, false)
		id := tid("cr-new-exact")
		rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "cr-new-exact", RuntimeTarget: cloudRunTargetFor("new-exact")})
		d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		requireCloudRunExperimentDisabled(t, d)
		_, err := f.s.GetRuntimeBroker(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "no row created")
	})

	t.Run("experiment off: non-canonical cloudrun spellings refused with 422, not 412, no row", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, false)
		for i, typ := range nonCanonicalCloudRunTypes() {
			key := fmt.Sprintf("cr-noncanon-off-%d", i)
			id := tid(key)
			target := &api.RuntimeTargetDescriptor{ID: tid(key + "-target"), Type: typ, DisplayName: "Cloud Run"}
			rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: key, RuntimeTarget: target})
			requireTypeUnsupported(t, rec, typ)
			_, err := f.s.GetRuntimeBroker(ctx, id)
			assert.ErrorIs(t, err, store.ErrNotFound, "spelling %d: no row created", i)
		}
	})

	t.Run("parent experiment off is reported first", func(t *testing.T) {
		f := newFlatRegFixture(t, false)
		setFlatExperiments(t, f.srv, false, false)
		rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("cr-parent-off"), Name: "cr-parent-off", RuntimeTarget: cloudRunTargetFor("parent-off")})
		d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])

		// The Cloud Run experiment alone does not enable flat registration.
		setFlatExperiments(t, f.srv, false, true)
		rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("cr-parent-off-2"), Name: "cr-parent-off-2", RuntimeTarget: cloudRunTargetFor("parent-off-2")})
		d = requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])

		// A non-canonical spelling gets the parent 412 first too, not the 422.
		target := cloudRunTargetFor("parent-off-3")
		target.Type = "CloudRun"
		rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("cr-parent-off-3"), Name: "cr-parent-off-3", RuntimeTarget: target})
		d = requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])
	})

	t.Run("docker and kubernetes register with it off", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, false)
		for _, typ := range []string{"docker", "kubernetes"} {
			id := tid("cr-other-" + typ)
			target := &api.RuntimeTargetDescriptor{ID: tid("cr-other-target-" + typ), Type: typ, DisplayName: typ}
			rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "cr-other-" + typ, RuntimeTarget: target})
			require.Equal(t, http.StatusCreated, rec.Code, "type %q registers with the Cloud Run experiment off: %s", typ, rec.Body.String())
			row := f.flatRow(t, id)
			require.NotNil(t, row.RuntimeTarget)
			assert.Equal(t, typ, row.RuntimeTarget.Type)
		}
	})

	t.Run("existing cloudrun row re-registers and joins", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, true)
		f.target = cloudRunTargetFor("existing")
		id := f.registerFlat(t, tid("cr-existing"), "cr-existing")
		before := f.flatRow(t, id)
		require.NotNil(t, before.RuntimeTarget)
		require.Equal(t, "cloudrun", before.RuntimeTarget.Type)

		for _, flatOn := range []bool{true, false} {
			setFlatExperiments(t, f.srv, flatOn, false)
			rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "cr-existing", RuntimeTarget: f.target})
			require.Equal(t, http.StatusCreated, rec.Code, "flat experiment %t: an existing cloudrun row re-registers: %s", flatOn, rec.Body.String())
			var resp CreateBrokerRegistrationResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, id, resp.BrokerID, "same id")
			require.NotNil(t, resp.RuntimeTarget)
			assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID, "same target")

			rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "cr-existing", Version: "0.1.0", RuntimeTarget: f.target})
			require.Equal(t, http.StatusOK, rec.Code, "flat experiment %t: the join succeeds: %s", flatOn, rec.Body.String())
			after := f.flatRow(t, id)
			require.NotNil(t, after.RuntimeTarget)
			assert.Equal(t, before.RuntimeTarget.ID, after.RuntimeTarget.ID)
			assert.Equal(t, "cloudrun", after.RuntimeTarget.Type)
		}
	})

	t.Run("embedded registration of a new cloudrun row refused", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, false)
		id := tid("cr-embedded")
		_, err := registerEmbeddedFlatForTest(t, f.srv, id, "cr-embedded", cloudRunTargetFor("embedded"))
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(err, &refusal), "a refusal, got %v", err)
		assert.Equal(t, ErrCodeExperimentDisabled, refusal.Code)
		assert.Equal(t, http.StatusPreconditionFailed, refusal.Status)
		assert.Equal(t, experiments.FlatCloudRunTarget, refusal.Details["experiment"])
		_, err = f.s.GetRuntimeBroker(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "no row created")
	})

	t.Run("experiment on: non-canonical cloudrun spellings refused with 422, no row", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, true)
		for i, typ := range nonCanonicalCloudRunTypes() {
			key := fmt.Sprintf("cr-noncanon-%d", i)
			id := tid(key)
			target := &api.RuntimeTargetDescriptor{ID: tid(key + "-target"), Type: typ, DisplayName: "Cloud Run"}
			rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: key, RuntimeTarget: target})
			requireTypeUnsupported(t, rec, typ)
			_, err := f.s.GetRuntimeBroker(ctx, id)
			assert.ErrorIs(t, err, store.ErrNotFound, "spelling %d: no row created", i)
		}
	})

	t.Run("embedded registration of a non-canonical cloudrun row refused with 422, experiment off or on", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		for _, cloudRunOn := range []bool{false, true} {
			setFlatExperiments(t, f.srv, true, cloudRunOn)
			key := fmt.Sprintf("cr-embedded-noncanon-%t", cloudRunOn)
			id := tid(key)
			target := cloudRunTargetFor(key)
			target.Type = "CloudRun"
			_, err := registerEmbeddedFlatForTest(t, f.srv, id, key, target)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "experiment %t: a refusal, got %v", cloudRunOn, err)
			assert.Equal(t, ErrCodeRuntimeTargetTypeUnsupported, refusal.Code, "experiment %t", cloudRunOn)
			assert.Equal(t, http.StatusUnprocessableEntity, refusal.Status, "experiment %t", cloudRunOn)
			assert.Equal(t, "CloudRun", refusal.Details["runtimeTargetType"], "experiment %t", cloudRunOn)
			_, err = f.s.GetRuntimeBroker(ctx, id)
			assert.ErrorIs(t, err, store.ErrNotFound, "experiment %t: no row created", cloudRunOn)
		}

		// With flat registration off, the parent 412 comes before the 422.
		setFlatExperiments(t, f.srv, false, true)
		id := tid("cr-embedded-noncanon-parent-off")
		target := cloudRunTargetFor("embedded-noncanon-parent-off")
		target.Type = "CloudRun"
		_, err := registerEmbeddedFlatForTest(t, f.srv, id, "cr-embedded-noncanon-parent-off", target)
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(err, &refusal), "a refusal, got %v", err)
		assert.Equal(t, ErrCodeExperimentDisabled, refusal.Code)
		assert.Equal(t, http.StatusPreconditionFailed, refusal.Status)
		assert.Equal(t, experiments.FlatRuntimeBrokers, refusal.Details["experiment"])
		_, err = f.s.GetRuntimeBroker(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "no row created")
	})

	t.Run("experiment on: a new cloudrun row registers", func(t *testing.T) {
		f := newFlatRegFixture(t, true)
		setFlatExperiments(t, f.srv, true, true)
		f.target = cloudRunTargetFor("on")
		id := f.registerFlat(t, tid("cr-on"), "cr-on")
		row := f.flatRow(t, id)
		require.NotNil(t, row.RuntimeTarget)
		assert.Equal(t, "cloudrun", row.RuntimeTarget.Type)
	})
}

// addFlatCloudRunBroker stores a second flat row whose target type is
// targetType (a cloudrun spelling) and links it to the fixture's project.
func (f *flatHubFixture) addFlatCloudRunBroker(t *testing.T, targetType string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:            tid("flat-cloudrun-broker-" + t.Name()),
		Name:          "flat-cloudrun",
		Slug:          "flat-cloudrun",
		Status:        store.BrokerStatusOnline,
		Endpoint:      "http://flat-cloudrun.invalid",
		Capabilities:  &store.BrokerCapabilities{Reprovision: true, Sync: true},
		RuntimeTarget: cloudRunTargetFor(t.Name()),
	}
	b.RuntimeTarget.Type = targetType
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, b))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: f.project.ID, BrokerID: b.ID, BrokerName: b.Name, LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline}))
	got, err := f.s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	return got
}

func TestCreateAgent_FlatCloudRunExperimentOff(t *testing.T) {
	t.Run("create on a cloudrun row refused, nothing written", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		cr := f.addFlatCloudRunBroker(t, "cloudrun")
		setFlatExperiments(t, f.srv, true, false)
		rec := f.create(t, map[string]interface{}{"name": "cr-off", "runtimeBrokerId": cr.ID, "task": "t"})
		d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		requireCloudRunExperimentDisabled(t, d)
		f.noAgentWritten(t, "cr-off")
	})

	t.Run("experiment off: a row stored with a non-canonical spelling refused with 422, not 412", func(t *testing.T) {
		for i, typ := range nonCanonicalCloudRunTypes() {
			t.Run(fmt.Sprintf("spelling-%d", i), func(t *testing.T) {
				f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
				cr := f.addFlatCloudRunBroker(t, typ)
				require.Equal(t, typ, cr.RuntimeTarget.Type, "the stored type keeps its spelling")
				setFlatExperiments(t, f.srv, true, false)
				rec := f.create(t, map[string]interface{}{"name": "cr-spelling", "runtimeBrokerId": cr.ID, "task": "t"})
				requireTypeUnsupported(t, rec, typ)
				f.noAgentWritten(t, "cr-spelling")
			})
		}
	})

	t.Run("experiment on: a row stored with a non-canonical spelling refused with 422, nothing written", func(t *testing.T) {
		for i, typ := range nonCanonicalCloudRunTypes() {
			t.Run(fmt.Sprintf("spelling-%d", i), func(t *testing.T) {
				f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
				cr := f.addFlatCloudRunBroker(t, typ)
				setFlatExperiments(t, f.srv, true, true)
				rec := f.create(t, map[string]interface{}{"name": "cr-noncanon", "runtimeBrokerId": cr.ID, "task": "t"})
				requireTypeUnsupported(t, rec, typ)
				f.noAgentWritten(t, "cr-noncanon")
			})
		}
	})

	t.Run("412 comes before the expected target and the profile", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		cr := f.addFlatCloudRunBroker(t, "cloudrun")
		setFlatExperiments(t, f.srv, true, false)
		body := map[string]interface{}{"name": "cr-prec", "runtimeBrokerId": cr.ID, "task": "t",
			"profile": "local", "expectedRuntimeTargetId": "other"}
		d := requireAPIError(t, f.create(t, body), http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		assert.Equal(t, experiments.FlatCloudRunTarget, d["experiment"])
		setFlatExperiments(t, f.srv, true, true)
		requireAPIError(t, f.create(t, body), http.StatusConflict, ErrCodeRuntimeTargetMismatch)
		delete(body, "expectedRuntimeTargetId")
		requireAPIError(t, f.create(t, body), http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
		f.noAgentWritten(t, "cr-prec")
	})

	t.Run("parent experiment off is reported first", func(t *testing.T) {
		for i, typ := range []string{"cloudrun", "CloudRun"} {
			t.Run(fmt.Sprintf("spelling-%d", i), func(t *testing.T) {
				f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
				cr := f.addFlatCloudRunBroker(t, typ)
				setFlatExperiments(t, f.srv, false, true)
				rec := f.create(t, map[string]interface{}{"name": "cr-parent", "runtimeBrokerId": cr.ID, "task": "t"})
				d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
				assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])
				f.noAgentWritten(t, "cr-parent")
			})
		}
	})

	t.Run("docker flat create succeeds with it off", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		f.addFlatCloudRunBroker(t, "cloudrun")
		setFlatExperiments(t, f.srv, true, false)
		rec := f.create(t, map[string]interface{}{"name": "cr-docker", "runtimeBrokerId": f.flat.ID, "task": "t"})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		a := f.agentBySlug(t, "cr-docker")
		require.NotNil(t, a)
		assert.Equal(t, f.flat.ID, a.PinnedRuntimeBrokerID)
		assert.Equal(t, "docker", a.PinnedRuntimeTargetType)
	})

	t.Run("existing pinned cloudrun agent resumes with the experiment off", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		cr := f.addFlatCloudRunBroker(t, "cloudrun")
		f.pinnedAgentWith(t, "cr-resume", string(state.PhaseStopped), func(a *store.Agent) {
			a.RuntimeBrokerID = cr.ID
			a.PinnedRuntimeBrokerID = cr.ID
			a.PinnedRuntimeTargetID = cr.RuntimeTarget.ID
			a.PinnedRuntimeTargetType = cr.RuntimeTarget.Type
		})
		setFlatExperiments(t, f.srv, true, false)
		rec := f.create(t, map[string]interface{}{"name": "cr-resume", "task": "t", "resume": true})
		require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "an existing cloudrun agent resumes: %d %s", rec.Code, rec.Body.String())
		require.True(t, f.client.startCalled)
		assert.Equal(t, cr.ID, f.client.lastBrokerID, "dispatched to the agent's own Runtime Broker")
	})

	t.Run("experiment on: create on a cloudrun row is pinned", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		cr := f.addFlatCloudRunBroker(t, "cloudrun")
		setFlatExperiments(t, f.srv, true, true)
		rec := f.create(t, map[string]interface{}{"name": "cr-on", "runtimeBrokerId": cr.ID, "task": "t"})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		a := f.agentBySlug(t, "cr-on")
		require.NotNil(t, a)
		assert.Equal(t, cr.ID, a.PinnedRuntimeBrokerID)
		assert.Equal(t, cr.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
		assert.Equal(t, "cloudrun", a.PinnedRuntimeTargetType)
	})
}

// TestFlatTargetTypeCheck pins the one type check both gates use: every
// spelling of cloudrun that differs only by case or surrounding whitespace
// maps to hub.flat_cloudrun_target; other types map to no experiment of their
// own.
func TestFlatTargetTypeCheck(t *testing.T) {
	for _, typ := range []string{"cloudrun", "CloudRun", "CLOUDRUN", " cloudrun ", "cloudrun\t", "\ncloudrun"} {
		exp, _ := flatTargetTypeCheck(typ)
		assert.Equal(t, experiments.FlatCloudRunTarget, exp, "type %q", typ)
	}
	for _, typ := range []string{"docker", "kubernetes", "cloudrun-instances", "cloudrun-sandbox", "cloud run", ""} {
		exp, canonical := flatTargetTypeCheck(typ)
		assert.Equal(t, "", exp, "type %q", typ)
		assert.True(t, canonical, "type %q is not a cloudrun spelling", typ)
	}
	_, canonical := flatTargetTypeCheck("cloudrun")
	assert.True(t, canonical, "the exact canonical type")
	for _, typ := range nonCanonicalCloudRunTypes() {
		exp, canonical := flatTargetTypeCheck(typ)
		assert.Equal(t, experiments.FlatCloudRunTarget, exp, "type %q", typ)
		assert.False(t, canonical, "type %q is not the canonical spelling", typ)
	}
}

// TestFlatTargetTypeEchoBound pins the bound on an echoed refused value
// (truncateUTF8 at maxEchoedRuntimeTargetType): at most 64 bytes, never
// splitting a rune.
func TestFlatTargetTypeEchoBound(t *testing.T) {
	assert.Equal(t, 64, maxEchoedRuntimeTargetType)
	assert.Equal(t, "cloudrun", truncateUTF8("cloudrun", maxEchoedRuntimeTargetType))
	assert.Len(t, truncateUTF8(strings.Repeat("x", 4096), maxEchoedRuntimeTargetType), 64)
	cut := truncateUTF8(strings.Repeat("x", 63)+"é", maxEchoedRuntimeTargetType)
	assert.Equal(t, strings.Repeat("x", 63), cut, "a two-byte rune that would cross the bound is dropped whole")
	assert.True(t, utf8.ValidString(cut))
}
