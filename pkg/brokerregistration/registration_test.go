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

package brokerregistration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// P2.1 registration and activation tests, frozen by name, act function and
// assertion in .design/flat-runtime-brokers-contract.md section 15. The Hub
// replicas here are fakes so each can play a capable, unaware or name-adopting
// Hub; registration_hub_test.go runs the real Hub.

const testHubName = "test-hub"

var testSecret = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// fakeReplica answers the registration, join and self-read endpoints with
// the behaviour its handlers choose, and records every request.
type fakeReplica struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	register func(body map[string]any) any
	join     func(body map[string]any) any
	get      func(id string) (int, any)
}

type recorded struct {
	Method, Path string
	BrokerHeader string
	Auth         string
}

func newFakeReplica(t *testing.T) *fakeReplica {
	t.Helper()
	f := &fakeReplica{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeReplica) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Method: r.Method, Path: r.URL.Path,
		BrokerHeader: r.Header.Get(apiclient.HeaderBrokerID), Auth: r.Header.Get("Authorization")})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/brokers" && f.register != nil:
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.register(body))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/brokers/join" && f.join != nil:
		_ = json.NewEncoder(w).Encode(f.join(body))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers/") && f.get != nil:
		status, resp := f.get(strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-brokers/"))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReplica) recordedRequests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fakeReplica) paths() []string {
	var out []string
	for _, r := range f.recordedRequests() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// capableRegister acknowledges the requested ID and the sent descriptor.
func capableRegister(body map[string]any) any {
	return map[string]any{"brokerId": body["brokerId"], "joinToken": "scion_join_t", "expiresAt": "2026-10-09T05:00:00Z",
		"runtimeTarget": body["runtimeTarget"]}
}

// capableJoin echoes the stored descriptor (here: the one sent).
func capableJoin(body map[string]any) any {
	return map[string]any{"brokerId": body["brokerId"], "secretKey": testSecret, "hubEndpoint": "http://hub.example",
		"runtimeTarget": body["runtimeTarget"]}
}

// unawareJoin is a Hub replica that predates the contract: it rotates the
// secret and drops runtimeTarget.
func unawareJoin(body map[string]any) any {
	return map[string]any{"brokerId": body["brokerId"], "secretKey": testSecret, "hubEndpoint": "http://hub.example"}
}

type testInstance struct {
	globalDir string
	inst      config.V1RuntimeBrokerInstanceConfig
	id        *brokeridentity.Identity
	credDir   string
}

func newTestInstance(t *testing.T) *testInstance {
	t.Helper()
	globalDir := t.TempDir()
	inst := config.V1RuntimeBrokerInstanceConfig{Key: "remote-docker", Name: "example-remote",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker", DisplayName: "Remote Docker"}}
	scope := brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1", Endpoint: "unix:///var/run/docker.sock"}}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, inst.Key), inst.Key, "docker", scope, nil)
	require.NoError(t, err)
	return &testInstance{globalDir: globalDir, inst: inst, id: id, credDir: InstanceCredentialsDir(globalDir, inst.Key)}
}

func (ti *testInstance) identityBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(brokeridentity.InstanceDir(ti.globalDir, ti.inst.Key), brokeridentity.IdentityFileName))
	require.NoError(t, err)
	return data
}

func (ti *testInstance) register(t *testing.T, hubURL string) (*brokercredentials.BrokerCredentials, error) {
	t.Helper()
	client, err := hubclient.New(hubURL, hubclient.WithBearerToken("user-token"))
	require.NoError(t, err)
	return RegisterInstance(context.Background(), client, ti.inst, ti.id, testHubName, ti.credDir)
}

func ackCode(t *testing.T, err error) (code, phase string) {
	t.Helper()
	var ack *brokeridentity.AckError
	require.True(t, errors.As(err, &ack), "want an activation acknowledgement error, got %v", err)
	return ack.Code, ack.Phase
}

func assertNoCredentials(t *testing.T, ti *testInstance) {
	t.Helper()
	entries, err := os.ReadDir(ti.credDir)
	if err == nil {
		assert.Empty(t, entries, "no instance credentials may be saved")
	} else {
		assert.True(t, os.IsNotExist(err), "unexpected error reading the credentials dir: %v", err)
	}
	_, err = LoadInstanceCredentials(ti.globalDir, ti.id)
	var nr *NotRegisteredError
	assert.True(t, errors.As(err, &nr), "the instance stays unregistered")
}

func TestRegisterInstance_UnawareHubRefusedBeforeJoin(t *testing.T) {
	ti := newTestInstance(t)
	before := ti.identityBytes(t)
	hub := newFakeReplica(t)
	// An unaware Hub decodes the registration leniently: it keeps the ID
	// and drops runtimeTarget.
	hub.register = func(body map[string]any) any {
		return map[string]any{"brokerId": body["brokerId"], "joinToken": "scion_join_t", "expiresAt": "2026-10-09T05:00:00Z"}
	}
	hub.join = capableJoin

	creds, err := ti.register(t, hub.URL)
	require.Error(t, err)
	assert.Nil(t, creds)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetAckMissing, code)
	assert.Equal(t, brokeridentity.PhaseRegister, phase)
	assert.Equal(t, []string{"POST /api/v1/brokers"}, hub.paths(), "no join after a missing acknowledgement")
	assertNoCredentials(t, ti)
	assert.Equal(t, before, ti.identityBytes(t), "identity unchanged")
}

func TestRegisterInstance_NameAdoptionRefused(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	// An unaware Hub that matched an existing row by name returns that
	// row's ID and no runtimeTarget.
	hub.register = func(map[string]any) any {
		return map[string]any{"brokerId": "a-different-existing-row", "joinToken": "scion_join_t", "expiresAt": "2026-10-09T05:00:00Z", "reregistered": true}
	}
	hub.join = capableJoin

	_, err := ti.register(t, hub.URL)
	require.Error(t, err)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetBindingConflict, code)
	assert.Equal(t, brokeridentity.PhaseRegister, phase)
	assert.Equal(t, []string{"POST /api/v1/brokers"}, hub.paths(), "no join after a name adoption")
	assertNoCredentials(t, ti)
}

func TestRegisterInstance_TargetMismatchRefused(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.register = func(body map[string]any) any {
		return map[string]any{"brokerId": body["brokerId"], "joinToken": "scion_join_t", "expiresAt": "2026-10-09T05:00:00Z",
			"runtimeTarget": map[string]any{"id": "another-target", "type": "docker"}}
	}
	hub.join = capableJoin

	_, err := ti.register(t, hub.URL)
	require.Error(t, err)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetBindingConflict, code)
	assert.Equal(t, brokeridentity.PhaseRegister, phase)
	assert.Equal(t, []string{"POST /api/v1/brokers"}, hub.paths())
	assertNoCredentials(t, ti)
}

func TestRegisterInstance_JoinMissingAckDiscardsSecret(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.register = capableRegister
	hub.join = unawareJoin

	creds, err := ti.register(t, hub.URL)
	require.Error(t, err)
	assert.Nil(t, creds, "the returned secret is discarded")
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetAckMissing, code)
	assert.Equal(t, brokeridentity.PhaseJoin, phase)
	var refusal *RegistrationRefusal
	require.True(t, errors.As(err, &refusal))
	assert.True(t, refusal.SecretRotated)
	assert.Contains(t, err.Error(), "rotated the secret")
	assert.Contains(t, err.Error(), "may show online")
	assert.NotContains(t, err.Error(), testSecret, "the discarded secret is never reported")
	assertNoCredentials(t, ti)
}

func TestRegisterInstance_JoinEchoMismatchDiscardsSecret(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.register = capableRegister
	hub.join = func(body map[string]any) any {
		return map[string]any{"brokerId": body["brokerId"], "secretKey": testSecret, "hubEndpoint": "http://hub.example",
			"runtimeTarget": map[string]any{"id": "another-target", "type": "docker"}}
	}

	creds, err := ti.register(t, hub.URL)
	require.Error(t, err)
	assert.Nil(t, creds)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetBindingConflict, code)
	assert.Equal(t, brokeridentity.PhaseJoin, phase)
	assertNoCredentials(t, ti)
}

func TestRegisterInstance_MixedReplicasNotActivated(t *testing.T) {
	ti := newTestInstance(t)
	capable := newFakeReplica(t)
	capable.register = capableRegister
	capable.join = capableJoin
	unaware := newFakeReplica(t)
	unaware.join = unawareJoin
	// The load balancer sends the registration to the capable replica and
	// the join to the unaware one.
	lb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/brokers/join" {
			unaware.serve(w, r)
			return
		}
		capable.serve(w, r)
	}))
	t.Cleanup(lb.Close)

	_, err := ti.register(t, lb.URL)
	require.Error(t, err)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetAckMissing, code)
	assert.Equal(t, brokeridentity.PhaseJoin, phase)
	assert.Equal(t, []string{"POST /api/v1/brokers"}, capable.paths())
	assert.Equal(t, []string{"POST /api/v1/brokers/join"}, unaware.paths())
	assertNoCredentials(t, ti)

	// Not activated: activation finds no instance credentials.
	err = ValidateActivation(context.Background(), nil, ti.id, nil)
	var nr *NotRegisteredError
	require.True(t, errors.As(err, &nr))
	assert.Equal(t, api.ErrCodeFlatRuntimeBrokerNotRegistered, nr.Code())
}

func TestRegisterInstance_NoAutomaticCleanup(t *testing.T) {
	for name, tc := range map[string]struct {
		register func(map[string]any) any
		join     func(map[string]any) any
		leftover string
	}{
		"register phase (legacy row or name adoption)": {
			register: func(map[string]any) any {
				return map[string]any{"brokerId": "a-different-existing-row", "joinToken": "scion_join_t", "expiresAt": "2026-10-09T05:00:00Z"}
			},
			join:     capableJoin,
			leftover: "re-registered an existing row matched by the name",
		},
		"join phase (rotated secret)": {
			register: capableRegister,
			join:     unawareJoin,
			leftover: "rotated the secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ti := newTestInstance(t)
			hub := newFakeReplica(t)
			hub.register, hub.join = tc.register, tc.join

			_, err := ti.register(t, hub.URL)
			require.Error(t, err)
			for _, r := range hub.recordedRequests() {
				assert.Equal(t, http.MethodPost, r.Method, "no delete or rewrite of the pre-existing row: %s %s", r.Method, r.Path)
				assert.Contains(t, []string{"/api/v1/brokers", "/api/v1/brokers/join"}, r.Path)
			}
			assert.Contains(t, err.Error(), tc.leftover, "leftovers are reported")
			assert.Contains(t, err.Error(), ti.id.RuntimeBrokerID, "the refusal names the Runtime Broker ID")
			assert.Contains(t, err.Error(), ti.inst.Name, "the refusal names the Runtime Broker name")
			assert.Contains(t, err.Error(), "Nothing was deleted or rewritten automatically")
		})
	}
}

func TestRegisterInstance_IdentityPreservedAfterRefusal(t *testing.T) {
	ti := newTestInstance(t)
	before := ti.identityBytes(t)
	hub := newFakeReplica(t)
	hub.register = capableRegister
	hub.join = unawareJoin

	_, err := ti.register(t, hub.URL)
	require.Error(t, err)
	assert.Equal(t, before, ti.identityBytes(t), "the identity file is untouched")

	// Reloading yields the same identity: nothing was re-minted.
	scope := brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}}
	again, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(ti.globalDir, ti.inst.Key), ti.inst.Key, "docker", scope, nil)
	require.NoError(t, err)
	assert.Equal(t, ti.id.RuntimeBrokerID, again.RuntimeBrokerID)
	assert.Equal(t, ti.id.RuntimeTarget, again.RuntimeTarget)

	// The next registration, against a capable Hub, uses the same identity.
	hub.join = capableJoin
	creds, err := ti.register(t, hub.URL)
	require.NoError(t, err)
	assert.Equal(t, ti.id.RuntimeBrokerID, creds.BrokerID)
}

func TestRegisterInstance_CredentialsWrittenOnlyAfterBothAcks(t *testing.T) {
	ti := newTestInstance(t)
	credPath := filepath.Join(ti.credDir, testHubName+".json")
	hub := newFakeReplica(t)
	var sent []map[string]any
	hub.register = func(body map[string]any) any {
		sent = append(sent, body)
		if len(sent) == 1 {
			_, err := os.Stat(credPath)
			assert.True(t, os.IsNotExist(err), "nothing is saved before the registration acknowledgement")
		}
		return capableRegister(body)
	}
	hub.join = func(body map[string]any) any {
		sent = append(sent, body)
		if len(sent) == 2 {
			_, err := os.Stat(credPath)
			assert.True(t, os.IsNotExist(err), "nothing is saved before the join acknowledgement")
		}
		return capableJoin(body)
	}

	creds, err := ti.register(t, hub.URL)
	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.Equal(t, []string{"POST /api/v1/brokers", "POST /api/v1/brokers/join"}, hub.paths())

	// Both requests carry the identity's binding.
	sent = sent[:2]
	wantTarget := map[string]any{"id": ti.id.RuntimeTarget.ID, "type": "docker", "displayName": "Remote Docker"}
	require.Len(t, sent, 2)
	assert.Equal(t, ti.id.RuntimeBrokerID, sent[0]["brokerId"])
	assert.Equal(t, ti.inst.Name, sent[0]["name"])
	assert.Equal(t, wantTarget, sent[0]["runtimeTarget"])
	assert.Equal(t, wantTarget, sent[1]["runtimeTarget"])
	assert.NotContains(t, sent[0], "profiles")
	assert.NotContains(t, sent[1], "profiles")
	assert.NotContains(t, sent[1], "defaultProfile")

	// Saved after both acknowledgements, 0600, in the brokercredentials format.
	info, err := os.Stat(credPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(brokercredentials.FileMode), info.Mode().Perm())
	loaded, err := brokercredentials.NewMultiStore(ti.credDir).Load(testHubName)
	require.NoError(t, err)
	assert.Equal(t, ti.id.RuntimeBrokerID, loaded.BrokerID)
	assert.Equal(t, testSecret, loaded.SecretKey)
	assert.Equal(t, "http://hub.example", loaded.HubEndpoint)
	assert.Equal(t, brokercredentials.AuthModeHMAC, loaded.AuthMode)
	leftovers, _ := filepath.Glob(filepath.Join(ti.credDir, ".*tmp-*"))
	assert.Empty(t, leftovers, "no temp files remain")

	// Existing credentials never short-circuit: the next call re-registers
	// and re-joins, and saves the rotated secret.
	rotated := base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
	hub.join = func(body map[string]any) any {
		resp := capableJoin(body).(map[string]any)
		resp["secretKey"] = rotated
		return resp
	}
	_, err = ti.register(t, hub.URL)
	require.NoError(t, err)
	assert.Len(t, hub.paths(), 4)
	loaded, err = brokercredentials.NewMultiStore(ti.credDir).Load(testHubName)
	require.NoError(t, err)
	assert.Equal(t, rotated, loaded.SecretKey)
}

// savedInstanceCredentials writes instance credentials as RegisterInstance
// would after a successful registration against hubURL.
func savedInstanceCredentials(t *testing.T, ti *testInstance, hubURL string) *brokercredentials.BrokerCredentials {
	t.Helper()
	creds := &brokercredentials.BrokerCredentials{Name: testHubName, BrokerID: ti.id.RuntimeBrokerID, SecretKey: testSecret,
		HubEndpoint: hubURL, AuthMode: brokercredentials.AuthModeHMAC}
	require.NoError(t, saveCredentialsAtomic(ti.credDir, creds))
	list, err := LoadInstanceCredentials(ti.globalDir, ti.id)
	require.NoError(t, err)
	require.Len(t, list, 1)
	return &list[0]
}

func selfRow(ti *testInstance, target *api.RuntimeTargetDescriptor) map[string]any {
	row := map[string]any{"id": ti.id.RuntimeBrokerID, "name": ti.inst.Name, "status": "online"}
	if target != nil {
		row["runtimeTarget"] = target
	}
	return row
}

func TestValidateActivation_SavedCredentialsDowngradedHubRefused(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	// The Hub returns the row without runtimeTarget (an unaware replica, a
	// legacy row or an unbound row).
	hub.get = func(string) (int, any) { return http.StatusOK, selfRow(ti, nil) }
	creds := savedInstanceCredentials(t, ti, hub.URL)

	err := ValidateActivation(context.Background(), nil, ti.id, creds)
	require.Error(t, err)
	code, phase := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetAckMissing, code)
	assert.Equal(t, brokeridentity.PhaseActivate, phase)
	reqs := hub.recordedRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "GET /api/v1/runtime-brokers/"+ti.id.RuntimeBrokerID, reqs[0].Method+" "+reqs[0].Path)
	assert.Equal(t, ti.id.RuntimeBrokerID, reqs[0].BrokerHeader, "the self-read authenticates with the instance's HMAC credentials")
	assert.Empty(t, reqs[0].Auth, "no user credential is used")
}

func TestValidateActivation_BindingConflictRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		row   func(ti *testInstance) map[string]any
		creds func(ti *testInstance, c *brokercredentials.BrokerCredentials)
	}{
		"different target": {
			row: func(ti *testInstance) map[string]any {
				return selfRow(ti, &api.RuntimeTargetDescriptor{ID: "another-target", Type: "docker"})
			},
		},
		"different type": {
			row: func(ti *testInstance) map[string]any {
				return selfRow(ti, &api.RuntimeTargetDescriptor{ID: ti.id.RuntimeTarget.ID, Type: "kubernetes"})
			},
		},
		"different Runtime Broker ID": {
			row: func(ti *testInstance) map[string]any {
				row := selfRow(ti, &ti.id.RuntimeTarget)
				row["id"] = "another-row"
				return row
			},
		},
		"credentials of another Runtime Broker": {
			row: func(ti *testInstance) map[string]any { return selfRow(ti, &ti.id.RuntimeTarget) },
			creds: func(_ *testInstance, c *brokercredentials.BrokerCredentials) {
				c.BrokerID = "another-runtime-broker"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ti := newTestInstance(t)
			hub := newFakeReplica(t)
			hub.get = func(string) (int, any) { return http.StatusOK, tc.row(ti) }
			creds := savedInstanceCredentials(t, ti, hub.URL)
			if tc.creds != nil {
				tc.creds(ti, creds)
			}

			err := ValidateActivation(context.Background(), nil, ti.id, creds)
			require.Error(t, err)
			code, phase := ackCode(t, err)
			assert.Equal(t, api.ErrCodeRuntimeTargetBindingConflict, code)
			assert.Equal(t, brokeridentity.PhaseActivate, phase)
		})
	}
}

func TestValidateActivation_NoInstanceCredentials(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.get = func(string) (int, any) { return http.StatusOK, selfRow(ti, &ti.id.RuntimeTarget) }
	// Legacy credentials for this very Runtime Broker ID, in both legacy
	// locations, are present and must be ignored.
	legacy := &brokercredentials.BrokerCredentials{Name: testHubName, BrokerID: ti.id.RuntimeBrokerID, SecretKey: testSecret, HubEndpoint: hub.URL}
	require.NoError(t, brokercredentials.NewMultiStore(filepath.Join(ti.globalDir, "hub-credentials")).Save(legacy))
	require.NoError(t, brokercredentials.NewStore(filepath.Join(ti.globalDir, brokercredentials.DefaultFileName)).Save(legacy))

	list, err := LoadInstanceCredentials(ti.globalDir, ti.id)
	assert.Nil(t, list)
	var nr *NotRegisteredError
	require.True(t, errors.As(err, &nr))
	assert.Equal(t, api.ErrCodeFlatRuntimeBrokerNotRegistered, nr.Code())
	assert.Equal(t, ti.inst.Key, nr.InstanceKey)
	assert.Equal(t, ti.id.RuntimeBrokerID, nr.RuntimeBrokerID)

	err = ValidateActivation(context.Background(), nil, ti.id, nil)
	require.True(t, errors.As(err, &nr))
	assert.Contains(t, err.Error(), api.ErrCodeFlatRuntimeBrokerNotRegistered)
	assert.Empty(t, hub.recordedRequests(), "not activated: no Hub request at all")
}

func TestValidateActivation_EveryStart(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	var mu sync.Mutex
	target := &ti.id.RuntimeTarget
	hub.get = func(string) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, selfRow(ti, target)
	}
	creds := savedInstanceCredentials(t, ti, hub.URL)

	for i := 0; i < 2; i++ {
		require.NoError(t, ValidateActivation(context.Background(), nil, ti.id, creds), "start %d", i+1)
	}
	assert.Len(t, hub.recordedRequests(), 2, "each start reads the Hub's current binding")

	// The Hub is downgraded between starts: the next start is refused, so
	// an earlier success is never cached.
	mu.Lock()
	target = nil
	mu.Unlock()
	err := ValidateActivation(context.Background(), nil, ti.id, creds)
	require.Error(t, err)
	code, _ := ackCode(t, err)
	assert.Equal(t, api.ErrCodeRuntimeTargetAckMissing, code)
	assert.Len(t, hub.recordedRequests(), 3)
}

func TestValidateActivation_AuthenticationFailureNotActivated(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.get = func(string) (int, any) {
		return http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid signature"}}
	}
	creds := savedInstanceCredentials(t, ti, hub.URL)

	err := ValidateActivation(context.Background(), nil, ti.id, creds)
	require.Error(t, err)
	var ack *brokeridentity.AckError
	assert.False(t, errors.As(err, &ack), "an authentication failure is reported as the Hub's auth error")
	assert.Contains(t, err.Error(), "not activated")
}

// TestRegisterInstance_SavesTheEndpointTheClientUsed: a Hub without a public
// endpoint reports its own (for example http://localhost:<port>) in the join
// response. The saved credentials keep the endpoint the registration reached
// the Hub on, as the legacy join does, so a remote instance activates
// against a Hub it can reach.
func TestRegisterInstance_SavesTheEndpointTheClientUsed(t *testing.T) {
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.register = capableRegister
	hub.join = func(body map[string]any) any {
		resp := capableJoin(body).(map[string]any)
		resp["hubEndpoint"] = "http://localhost:19811"
		return resp
	}
	client, err := hubclient.New(hub.URL, hubclient.WithBearerToken("user-token"))
	require.NoError(t, err)
	creds, err := RegisterInstance(context.Background(), client, ti.inst, ti.id, testHubName, ti.credDir,
		WithOptions(Options{HubEndpoint: hub.URL}))
	require.NoError(t, err)
	assert.Equal(t, hub.URL, creds.HubEndpoint)
	loaded, err := brokercredentials.NewMultiStore(ti.credDir).Load(testHubName)
	require.NoError(t, err)
	assert.Equal(t, hub.URL, loaded.HubEndpoint, "the saved endpoint is the one the client used, not the Hub's self-report")
}
