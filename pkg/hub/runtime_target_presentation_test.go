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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// FC4-a presentation transport (ptone/scion#3282): the Hub stores a flat
// instance's presentation from its heartbeat only when the request identity
// is the broker itself, only on a flat row bound to the reported target,
// with a Hub-computed hash, and only when the epoch/seq fence advances. An
// omitted field keeps the stored value.

// presentationCall is one SetRuntimeBrokerPresentation call seen by
// presentationRecordingStore, with its result.
type presentationCall struct {
	brokerID string
	hash     string
	epoch    int64
	seq      int64
	outcome  store.PresentationOutcome
	err      error
}

// presentationRecordingStore records every SetRuntimeBrokerPresentation
// call and always delegates (installed through installStoreFault; it never
// arms a fault).
type presentationRecordingStore struct {
	store.Store
	mu    sync.Mutex
	calls []presentationCall
}

func (r *presentationRecordingStore) SetRuntimeBrokerPresentation(ctx context.Context, brokerID, targetID string, p *api.RuntimeTargetPresentation, hash string, epoch, seq int64, observedAt time.Time) (store.PresentationOutcome, error) {
	out, err := r.Store.SetRuntimeBrokerPresentation(ctx, brokerID, targetID, p, hash, epoch, seq, observedAt)
	r.mu.Lock()
	r.calls = append(r.calls, presentationCall{brokerID: brokerID, hash: hash, epoch: epoch, seq: seq, outcome: out, err: err})
	r.mu.Unlock()
	return out, err
}

func (r *presentationRecordingStore) snapshot() []presentationCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]presentationCall(nil), r.calls...)
}

func (r *presentationRecordingStore) outcomes() []store.PresentationOutcome {
	var out []store.PresentationOutcome
	for _, c := range r.snapshot() {
		out = append(out, c.outcome)
	}
	return out
}

// presentationFixture is a flat Hub fixture with the recording store and a
// captured lifecycle log.
type presentationFixture struct {
	*flatHubFixture
	rec  *presentationRecordingStore
	logs *bytes.Buffer
}

func newPresentationFixture(t *testing.T, experimentOn bool) *presentationFixture {
	t.Helper()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: experimentOn, linkFlat: true,
		storeWrap: func(inner store.Store, _ *storeFaultSwitch) store.Store {
			return &presentationRecordingStore{Store: inner}
		}})
	logs := &bytes.Buffer{}
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &presentationFixture{flatHubFixture: f, rec: f.storeWrapper.(*presentationRecordingStore), logs: logs}
}

// dockerPresentation is a docker-target presentation (the fixture's flat
// row is a docker target) whose tag tells snapshots apart.
func dockerPresentation(tag string) *api.RuntimeTargetPresentation {
	evict := false
	return &api.RuntimeTargetPresentation{
		SchemaVersion: 1, Key: "local-docker", Name: "example-docker",
		Target: &api.PresentedTarget{Type: "docker", DisplayName: "Local Docker " + tag, SafeToEvict: &evict},
		AgentDefaults: &api.PresentedAgentDefaults{ImageRegistry: "registry.example/" + tag,
			HarnessOverrides: map[string]api.PresentedHarnessOverride{"claude": {Image: "img:" + tag, EnvKeys: []string{"TOKEN"}}}},
	}
}

// presentationBeat is a heartbeat body carrying a presentation and its
// fence; extra keys are added verbatim (a misbehaving broker).
func presentationBeat(p *api.RuntimeTargetPresentation, targetID string, epoch, seq int64, extra map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{"status": store.BrokerStatusOnline}
	if p != nil {
		body["runtimeTargetPresentation"] = p
		body["presentationTargetId"] = targetID
		body["presentationEpoch"] = epoch
		body["presentationSeq"] = seq
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// sendAsBroker posts a heartbeat authenticated as Runtime Broker brokerID
// itself (write rule 1 holds).
func sendAsBroker(t *testing.T, srv *Server, brokerID string, body interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ident := NewBrokerIdentity(brokerID)
	req = req.WithContext(contextWithIdentity(contextWithBrokerIdentity(req.Context(), ident), ident))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func (f *presentationFixture) row(t *testing.T, id string) *store.RuntimeBroker {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	return b
}

func mustHash(t *testing.T, p *api.RuntimeTargetPresentation) string {
	t.Helper()
	h, err := RuntimeTargetPresentationHash(p)
	require.NoError(t, err)
	return h
}

// TestPresentationHeartbeat_WritesOnHashChangeOnly: the first beat writes,
// an unchanged presentation is confirmed without a rewrite, a changed one
// is written; the stored hash is the Hub's own, never a hash the broker
// sends. The GET returns the presentation and observedAt, and never the
// hash or the fence.
func TestPresentationHeartbeat_WritesOnHashChangeOnly(t *testing.T) {
	f := newPresentationFixture(t, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)
	target := f.flat.RuntimeTarget.ID
	p1, p2 := dockerPresentation("one"), dockerPresentation("two")

	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(p1, target, 100, 1, nil))
	// A broker-sent hash is never trusted (the wire has no hash field).
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(p1, target, 100, 2, map[string]interface{}{
		"presentationHash": "broker-sent-hash", "runtimeTargetPresentationHash": "broker-sent-hash"}))
	row := f.row(t, f.flat.ID)
	assert.Equal(t, mustHash(t, p1), row.RuntimeTargetPresentationHash, "the stored hash is the Hub's")
	assert.Equal(t, p1, row.RuntimeTargetPresentation)
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(p2, target, 100, 3, nil))

	assert.Equal(t, []store.PresentationOutcome{store.PresentationWritten, store.PresentationConfirmed, store.PresentationWritten},
		f.rec.outcomes(), "written only when the Hub-computed hash changes")
	for _, c := range f.rec.snapshot() {
		assert.NotEqual(t, "broker-sent-hash", c.hash)
	}
	row = f.row(t, f.flat.ID)
	assert.Equal(t, p2, row.RuntimeTargetPresentation)
	assert.Equal(t, mustHash(t, p2), row.RuntimeTargetPresentationHash)

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	var gotP api.RuntimeTargetPresentation
	require.NoError(t, json.Unmarshal(got["runtimeTargetPresentation"], &gotP))
	assert.Equal(t, *p2, gotP)
	var observed time.Time
	require.NoError(t, json.Unmarshal(got["runtimeTargetPresentationObservedAt"], &observed))
	assert.WithinDuration(t, time.Now(), observed, time.Minute)
	for _, k := range []string{"runtimeTargetPresentationHash", "presentationEpoch", "presentationSeq", "presentationFencedAt"} {
		_, present := got[k]
		assert.False(t, present, "GET must not return %s", k)
	}
	assert.NotContains(t, rec.Body.String(), mustHash(t, p2), "the hash is never serialized")
}

// TestPresentationHeartbeat_IgnoredWithoutAWrite: the field is ignored,
// leaving the stored value, on a legacy row, on a target-ID or target-type
// mismatch (refused by the store's bound read, with typed errors and
// nothing written), on an unknown schema version, with the experiment off,
// and on the user heartbeat path with a warning (these three never reach
// the store).
func TestPresentationHeartbeat_IgnoredWithoutAWrite(t *testing.T) {
	f := newPresentationFixture(t, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)
	target := f.flat.RuntimeTarget.ID
	stored := dockerPresentation("stored")
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(stored, target, 100, 1, nil))
	require.Len(t, f.rec.snapshot(), 1)

	other := dockerPresentation("other")
	k8s := dockerPresentation("k8s")
	k8s.Target.Type = "kubernetes"
	noTarget := dockerPresentation("none")
	noTarget.Target = nil
	v2 := dockerPresentation("v2")
	v2.SchemaVersion = 2
	cases := []struct {
		name, brokerID, targetID, logWant string
		p                                 *api.RuntimeTargetPresentation
		storeErr                          error // nil: the store is never called
	}{
		{"legacy row", f.legacy.ID, target, "reason=legacy_row", other, store.ErrRuntimeBrokerNotFlat},
		{"target id mismatch", f.flat.ID, "some-other-target", "reason=target_mismatch", other, store.ErrRuntimeTargetChanged},
		{"empty target id", f.flat.ID, "", "reason=target_mismatch", other, store.ErrRuntimeTargetChanged},
		{"target type mismatch", f.flat.ID, target, "reason=target_mismatch", k8s, store.ErrRuntimeTargetChanged},
		{"no target", f.flat.ID, target, "reason=target_mismatch", noTarget, store.ErrRuntimeTargetChanged},
		{"unknown schema version", f.flat.ID, target, "reason=unknown_schema_version", v2, nil},
	}
	seq := int64(10)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.logs.Reset()
			seq++
			before := len(f.rec.snapshot())
			sendAsBroker(t, f.srv, tc.brokerID, presentationBeat(tc.p, tc.targetID, 100, seq, nil))
			calls := f.rec.snapshot()
			if tc.storeErr == nil {
				assert.Len(t, calls, before, "the store is never called")
			} else {
				require.Len(t, calls, before+1)
				last := calls[len(calls)-1]
				assert.ErrorIs(t, last.err, tc.storeErr)
				assert.Empty(t, last.outcome, "nothing is written")
			}
			assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation, "the stored value is unchanged")
			assert.Nil(t, f.row(t, f.legacy.ID).RuntimeTargetPresentation, "the legacy row has none")
			assert.Contains(t, f.logs.String(), tc.logWant)
		})
	}

	t.Run("user path", func(t *testing.T) {
		f.logs.Reset()
		before := len(f.rec.snapshot())
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+f.flat.ID+"/heartbeat",
			presentationBeat(other, target, 100, 50, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Len(t, f.rec.snapshot(), before, "the user path never reaches the store")
		assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation)
		assert.Contains(t, f.logs.String(), "ignoring runtimeTargetPresentation sent with a request identity other than the broker itself")
	})

	t.Run("experiment off", func(t *testing.T) {
		setFlatExperiment(t, f.srv, false)
		t.Cleanup(func() { setFlatExperiment(t, f.srv, true) })
		f.logs.Reset()
		before := len(f.rec.snapshot())
		sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(other, target, 100, 60, nil))
		assert.Len(t, f.rec.snapshot(), before, "the store is never called")
		assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation)
		assert.Contains(t, f.logs.String(), "reason=experiment_off")
	})
}

// TestPresentationHeartbeat_OmissionKeepsAndRestoreRepopulates: a heartbeat
// without the field (an older broker) keeps the stored presentation; a row
// without one (a Hub restored from a backup taken before the instance
// reported) is populated by the next heartbeat that carries it.
func TestPresentationHeartbeat_OmissionKeepsAndRestoreRepopulates(t *testing.T) {
	f := newPresentationFixture(t, true)
	target := f.flat.RuntimeTarget.ID
	require.Nil(t, f.row(t, f.flat.ID).RuntimeTargetPresentation, "the restored row has no presentation")

	p := dockerPresentation("one")
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(p, target, 100, 7, nil))
	assert.Equal(t, p, f.row(t, f.flat.ID).RuntimeTargetPresentation, "repopulated by the next heartbeat")

	for i := 0; i < 2; i++ {
		sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(nil, "", 0, 0, nil))
	}
	row := f.row(t, f.flat.ID)
	assert.Equal(t, p, row.RuntimeTargetPresentation, "an omitted field keeps the stored value")
	assert.Equal(t, mustHash(t, p), row.RuntimeTargetPresentationHash)
	require.NotNil(t, row.PresentationSeq)
	assert.Equal(t, int64(7), *row.PresentationSeq, "an omitted field does not touch the fence")
	assert.Len(t, f.rec.snapshot(), 1, "omitted beats never reach the store")
}

// TestBrokerHeartbeat_BrokerSelfOnlyForBrokerIdentity: the presentation is
// stored only when the request identity is the broker itself; a request
// whose identity is not the broker stores nothing. Both heartbeats are
// signed by the flat broker and pass through BrokerAuthMiddleware: one
// request identity is a user and stores nothing, the other is the broker
// and stores the presentation.
func TestBrokerHeartbeat_BrokerSelfOnlyForBrokerIdentity(t *testing.T) {
	f := newPresentationFixture(t, true)
	ctx := context.Background()
	target := f.flat.RuntimeTarget.ID
	secretKey := []byte("presentation-test-secret-32-byte")
	require.NoError(t, f.s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: f.flat.ID, SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusActive,
	}))
	userID := tid("presentation-user")
	require.NoError(t, f.s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.s, userID)
	send := func(t *testing.T, p *api.RuntimeTargetPresentation, seq int64, headers map[string]string) {
		t.Helper()
		raw, err := json.Marshal(presentationBeat(p, target, 100, seq, nil))
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+f.flat.ID+"/heartbeat", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		require.NoError(t, f.srv.brokerAuthService.SignRequest(req, f.flat.ID, secretKey))
		rec := httptest.NewRecorder()
		BrokerAuthMiddleware(f.srv.brokerAuthService)(f.srv.mux).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	t.Run("user request identity", func(t *testing.T) {
		send(t, dockerPresentation("user"), 1, map[string]string{HeaderOnBehalfOf: "user:" + userID + "@test.com"})
		assert.Empty(t, f.rec.snapshot(), "the presentation is stored only for the broker's own request identity")
		assert.Nil(t, f.row(t, f.flat.ID).RuntimeTargetPresentation)
	})
	t.Run("broker request identity", func(t *testing.T) {
		before := len(f.rec.snapshot())
		p := dockerPresentation("broker")
		send(t, p, 2, nil)
		assert.Len(t, f.rec.snapshot(), before+1, "the broker's own request identity reaches the store")
		assert.Equal(t, p, f.row(t, f.flat.ID).RuntimeTargetPresentation)
	})
	t.Run("user request identity with a broker grant", func(t *testing.T) {
		rd, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name: "presentation-identity-broker-update", ScopeType: store.RoleScopeSystem, Permissions: []string{"broker.update"},
		})
		require.NoError(t, err)
		_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
			ScopeType: store.RoleScopeSystem, ScopeID: "", CreatedBy: "test",
		})
		require.NoError(t, err)
		before := len(f.rec.snapshot())
		stored := f.row(t, f.flat.ID).RuntimeTargetPresentation
		send(t, dockerPresentation("granted"), 3, map[string]string{HeaderOnBehalfOf: "user:" + userID + "@test.com"})
		assert.Len(t, f.rec.snapshot(), before, "a request identity other than the broker stores nothing, whatever it is granted")
		assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation)
	})
}

// TestPresentationHeartbeat_UserPathStoresNothing: a user heartbeat never
// stores the presentation. A non-admin user whose only grant is broker.update
// is admitted on a session (200) with the presentation ignored, and refused
// on an access token (403, HOST_OPERATIONS); neither reaches the store.
func TestPresentationHeartbeat_UserPathStoresNothing(t *testing.T) {
	f := newPresentationFixture(t, true)
	ctx := context.Background()
	target := f.flat.RuntimeTarget.ID
	stored := dockerPresentation("stored")
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(stored, target, 100, 1, nil))
	require.Len(t, f.rec.snapshot(), 1)

	user := newHubMemberUser(t, f.s, "presentation-broker-updater")
	rd, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "presentation-broker-update", ScopeType: store.RoleScopeSystem, Permissions: []string{"broker.update"},
	})
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: user.ID,
		ScopeType: store.RoleScopeSystem, ScopeID: "", CreatedBy: "test",
	})
	require.NoError(t, err)
	path := "/api/v1/runtime-brokers/" + f.flat.ID + "/heartbeat"

	t.Run("session", func(t *testing.T) {
		f.logs.Reset()
		rec := doRequestAsUser(t, f.srv, user, http.MethodPost, path, presentationBeat(dockerPresentation("session"), target, 100, 50, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Len(t, f.rec.snapshot(), 1, "a user session heartbeat never reaches the store")
		assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation)
		assert.Contains(t, f.logs.String(), "ignoring runtimeTargetPresentation sent with a request identity other than the broker itself")
	})
	t.Run("access token", func(t *testing.T) {
		key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(user.ID), CreateTokenParams{
			UserID: user.ID, Name: "presentation-" + tid("tok"), Boundary: hubBoundary(), Scopes: []string{"broker:read"},
		})
		require.NoError(t, err)
		rec := doRequestWithToken(t, f.srv, key, http.MethodPost, path, presentationBeat(dockerPresentation("token"), target, 100, 60, nil))
		reason, credential := sessionOnlyDetailsOf(rec)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, string(authzop.ReasonHostOperations), reason, rec.Body.String())
		assert.Equal(t, sessionRequiredCredential, credential)
		assert.Len(t, f.rec.snapshot(), 1, "a refused heartbeat never reaches the store")
		assert.Equal(t, stored, f.row(t, f.flat.ID).RuntimeTargetPresentation)
	})
}

// TestPresentationHeartbeat_TwoWritersEndOnNewerProcess: process A (epoch
// 1, far ahead in sequence) and process B (epoch 2) of one instance send
// different snapshots, interleaved in every order; the row ends on B's. A
// delayed lower-seq request from B is stale.
func TestPresentationHeartbeat_TwoWritersEndOnNewerProcess(t *testing.T) {
	type beat struct {
		proc       string
		epoch, seq int64
	}
	a1, a2, b1, b2 := beat{"A", 1, 101}, beat{"A", 1, 102}, beat{"B", 2, 1}, beat{"B", 2, 2}
	orders := [][]beat{
		{a1, a2, b1, b2}, {a1, b1, a2, b2}, {a1, b1, b2, a2},
		{b1, a1, a2, b2}, {b1, a1, b2, a2}, {b1, b2, a1, a2},
	}
	snap := map[string]*api.RuntimeTargetPresentation{"A": dockerPresentation("a"), "B": dockerPresentation("b")}
	for _, order := range orders {
		f := newPresentationFixture(t, true)
		target := f.flat.RuntimeTarget.ID
		for _, b := range order {
			sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(snap[b.proc], target, b.epoch, b.seq, nil))
		}
		row := f.row(t, f.flat.ID)
		assert.Equal(t, snap["B"], row.RuntimeTargetPresentation, "order %v", order)
		require.NotNil(t, row.PresentationEpoch)
		assert.Equal(t, int64(2), *row.PresentationEpoch, "order %v", order)

		// A delayed retry of B's first beat (lower seq) is stale.
		f.logs.Reset()
		before := len(f.rec.snapshot())
		sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(dockerPresentation("b-old"), target, 2, 1, nil))
		calls := f.rec.snapshot()
		require.Len(t, calls, before+1)
		assert.Equal(t, store.PresentationStale, calls[len(calls)-1].outcome, "order %v", order)
		assert.Equal(t, snap["B"], f.row(t, f.flat.ID).RuntimeTargetPresentation, "order %v", order)
		assert.Contains(t, f.logs.String(), "runtime target presentation stale")
	}
}

// TestPresentationHeartbeat_NeverKeysNotStored: a heartbeat whose
// presentation object also carries fields FC4-a never publishes (as a
// misbehaving broker might send them) stores and serves none of them.
func TestPresentationHeartbeat_NeverKeysNotStored(t *testing.T) {
	f := newPresentationFixture(t, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)
	raw := map[string]interface{}{
		"schemaVersion": 1, "key": "local-docker", "name": "example-docker",
		"target": map[string]interface{}{"type": "docker", "context": "NEVER-context", "kubeconfig": "/NEVER-kubeconfig",
			"kubernetesServiceAccountMappings": map[string]string{"NEVER-gsa": "NEVER-ksa"}, "homeStorageLeaf": "NEVER-leaf"},
		"agentDefaults": map[string]interface{}{
			"volumes": []map[string]interface{}{{"target": "/mnt/a", "source": "NEVER-source", "bucket": "NEVER-bucket"}},
			"secrets": []map[string]interface{}{{"key": "API_KEY", "target": "NEVER-target", "alternativeEnvKeys": []string{"NEVER-ALT"}}},
			"harnessOverrides": map[string]interface{}{"claude": map[string]interface{}{
				"envKeys": []string{"TOKEN"}, "env": map[string]string{"TOKEN": "NEVER-env-value"}}},
		},
	}
	sendAsBroker(t, f.srv, f.flat.ID, map[string]interface{}{
		"status": store.BrokerStatusOnline, "runtimeTargetPresentation": raw,
		"presentationTargetId": f.flat.RuntimeTarget.ID, "presentationEpoch": 1, "presentationSeq": 1,
	})
	row := f.row(t, f.flat.ID)
	require.NotNil(t, row.RuntimeTargetPresentation)
	assert.Equal(t, []string{"TOKEN"}, row.RuntimeTargetPresentation.AgentDefaults.HarnessOverrides["claude"].EnvKeys)
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"runtimeTargetPresentation"`)
	assert.NotContains(t, rec.Body.String(), "NEVER")
}

// TestPresentation_RoundTripFileToGet: settings file → strict instance load
// → PresentInstance(entry).API() (what a flat instance sends) → heartbeat →
// GET /runtime-brokers/{id} equals PresentInstance(file entry), with
// safe_to_evict unset, false and true kept distinct.
func TestPresentation_RoundTripFileToGet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	settings := `schema_version: "1"
server:
  broker:
    instances:
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
          display_name: Local Docker
        agent_defaults:
          default_harness_config: instance-hc
          image_registry: registry.example/instance
          volumes:
            - source: /inst
              target: /mnt/inst
              read_only: true
          resources:
            requests:
              cpu: 500m
              memory: 1Gi
          harness_overrides:
            claude:
              image: instance-image:v2
              env:
                SHARED: from-instance
          secrets:
            - key: INSTANCE_SECRET
              description: instance secret
              type: environment
              alternative_env_keys: [ALT]
          clone_depth: full
`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(settings), 0o644))
	instances, err := config.LoadRuntimeBrokerInstances("")
	require.NoError(t, err)
	require.Len(t, instances, 1)

	tr, fa := true, false
	for name, evict := range map[string]*bool{"unset": nil, "false": &fa, "true": &tr} {
		t.Run(name, func(t *testing.T) {
			f := newPresentationFixture(t, true)
			grantDevUserRuntimeBrokerAccess(t, f.s)
			inst := instances[0]
			rt := *inst.RuntimeTarget
			rt.SafeToEvict = evict
			inst.RuntimeTarget = &rt
			want := config.PresentInstance(inst).API()
			sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(&want, f.flat.RuntimeTarget.ID, 1, 1, nil))

			rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				RuntimeTargetPresentation *api.RuntimeTargetPresentation `json:"runtimeTargetPresentation"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.NotNil(t, got.RuntimeTargetPresentation)
			assert.Equal(t, want, *got.RuntimeTargetPresentation)
			gotEvict := got.RuntimeTargetPresentation.Target.SafeToEvict
			if evict == nil {
				assert.Nil(t, gotEvict)
				assert.NotContains(t, rec.Body.String(), "safeToEvict")
			} else {
				require.NotNil(t, gotEvict)
				assert.Equal(t, *evict, *gotEvict)
			}
			assert.False(t, strings.Contains(rec.Body.String(), "from-instance"), "env values are never published")
			assert.False(t, strings.Contains(rec.Body.String(), `"/inst"`), "volume sources are never published")
			assert.Contains(t, rec.Body.String(), `"/mnt/inst"`, "volume targets are published")
		})
	}
}

// TestPresentation_ImageRegistryRedactedOnBrokerRead: presented agent
// defaults use the same image-registry display redaction as the broker
// record, so a broker read never returns the userinfo of the configured
// image registry.
func TestPresentation_ImageRegistryRedactedOnBrokerRead(t *testing.T) {
	f := newPresentationFixture(t, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)
	inst := config.V1RuntimeBrokerInstanceConfig{
		Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"},
		AgentDefaults: &config.V1InstanceAgentDefaults{ImageRegistry: "https://user:s3cret@registry.example/team"},
	}
	p := config.PresentInstance(inst).API()
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(&p, f.flat.RuntimeTarget.ID, 100, 1, nil))

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "s3cret", "a broker read never returns the registry's userinfo")
	var got struct {
		RuntimeTargetPresentation *api.RuntimeTargetPresentation `json:"runtimeTargetPresentation"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.RuntimeTargetPresentation)
	require.NotNil(t, got.RuntimeTargetPresentation.AgentDefaults)
	assert.Equal(t, "https://***@registry.example/team", got.RuntimeTargetPresentation.AgentDefaults.ImageRegistry)
}

// TestPresentationHeartbeat_StoresImageRegistryRedacted: the hub stores
// presented image registries with the same display redaction as the broker
// record, whatever the reporting broker sent. A presentation whose image
// registry carries userinfo is stored, hashed and read back redacted; a
// plain image registry is stored unchanged.
func TestPresentationHeartbeat_StoresImageRegistryRedacted(t *testing.T) {
	f := newPresentationFixture(t, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)
	target := f.flat.RuntimeTarget.ID
	reported := dockerPresentation("raw")
	reported.AgentDefaults.ImageRegistry = "https://user:s3cret@registry.example/team"
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(reported, target, 100, 1, nil))

	want := dockerPresentation("raw")
	want.AgentDefaults.ImageRegistry = "https://***@registry.example/team"
	row := f.row(t, f.flat.ID)
	assert.Equal(t, want, row.RuntimeTargetPresentation, "the hub stores the presented image registry redacted")
	assert.Equal(t, mustHash(t, want), row.RuntimeTargetPresentationHash, "the stored hash is the stored form's")
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "s3cret", "a broker read never returns the registry's userinfo")
	assert.Contains(t, rec.Body.String(), "https://***@registry.example/team", "a broker read returns the redacted image registry")

	plain := dockerPresentation("plain")
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(plain, target, 100, 2, nil))
	assert.Equal(t, plain, f.row(t, f.flat.ID).RuntimeTargetPresentation, "a plain image registry is stored unchanged")
}

// TestPresentation_StoredFormMatchesPresentInstanceOutput: a presentation
// built by config.PresentInstance (whose image registry is already redacted)
// is stored by ApplyRuntimeTargetPresentation unchanged: the stored value and
// the stored hash equal the presentation sent and its hash.
func TestPresentation_StoredFormMatchesPresentInstanceOutput(t *testing.T) {
	f := newPresentationFixture(t, true)
	inst := config.V1RuntimeBrokerInstanceConfig{
		Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"},
		AgentDefaults: &config.V1InstanceAgentDefaults{ImageRegistry: "https://user:s3cret@registry.example/team"},
	}
	sent := config.PresentInstance(inst).API()
	require.NotNil(t, sent.AgentDefaults)
	require.Equal(t, "https://***@registry.example/team", sent.AgentDefaults.ImageRegistry, "PresentInstance redacts the image registry")
	want := config.PresentInstance(inst).API()

	res, err := f.srv.ApplyRuntimeTargetPresentation(context.Background(), f.flat.ID, f.flat.RuntimeTarget.ID, &sent, 100, 1, time.Now())
	require.NoError(t, err)
	require.Equal(t, store.PresentationWritten, res.Outcome, "ignored: %q", res.Ignored)
	assert.Equal(t, want, sent, "the presentation sent is not modified")

	row := f.row(t, f.flat.ID)
	assert.Equal(t, &want, row.RuntimeTargetPresentation, "the stored value equals the presentation sent")
	assert.Equal(t, mustHash(t, &want), row.RuntimeTargetPresentationHash, "the stored hash is the hash of the presentation sent")
	calls := f.rec.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, mustHash(t, &want), calls[0].hash, "the store receives the hash of the presentation sent")
}
