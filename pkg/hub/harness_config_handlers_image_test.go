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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

type neverConnectedTunnel struct{}

func (n *neverConnectedTunnel) IsConnected(string) bool { return false }
func (n *neverConnectedTunnel) TunnelRequest(context.Context, string, *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	return nil, fmt.Errorf("not connected")
}

func TestIsNodeBoundBroker(t *testing.T) {
	tests := []struct {
		name     string
		profiles []store.BrokerProfile
		want     bool
	}{
		{"docker", []store.BrokerProfile{{Type: "docker"}}, true},
		{"podman", []store.BrokerProfile{{Type: "podman"}}, true},
		{"apple", []store.BrokerProfile{{Type: "apple"}}, true},
		{"kubernetes", []store.BrokerProfile{{Type: "kubernetes"}}, false},
		{"cloudrun", []store.BrokerProfile{{Type: "cloudrun"}}, false},
		{"mixed_with_docker", []store.BrokerProfile{{Type: "kubernetes"}, {Type: "docker"}}, true},
		{"empty", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &store.RuntimeBroker{Profiles: tc.profiles}
			got := isNodeBoundBroker(b)
			if got != tc.want {
				t.Errorf("isNodeBoundBroker(%v) = %v, want %v", tc.profiles, got, tc.want)
			}
		})
	}
}

// fakeBrokerServer creates an httptest server that responds to /api/v1/images/status.
// statusCode controls the response code; body is returned for 200.
func fakeBrokerServer(statusCode int, body *BrokerImageStatusResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusCode == http.StatusNotFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func setupImageStatusTest(t *testing.T) (*Server, store.Store) {
	t.Helper()
	db, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := &Server{
		store:        db,
		imageChecker: imagecheck.NewChecker(),
		authzService: NewAuthzService(db, nil),
	}
	return srv, db
}

func createTestBroker(t *testing.T, db store.Store, id, name, endpoint string, profiles []store.BrokerProfile, labels map[string]string) {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:       tid(id),
		Name:     name,
		Slug:     name,
		Endpoint: endpoint,
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
		Labels:   labels,
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create broker %s: %v", name, err)
	}
}

func createTestHarnessConfig(t *testing.T, db store.Store, id, image string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:      tid(id),
		Name:    "test-config",
		Slug:    "test-config",
		Harness: "test",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Image: image,
		},
	}
	if err := db.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}
	return hc
}

func TestImageStatusHandler_SingleReachableBroker(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	brokerTS := fakeBrokerServer(http.StatusOK, &BrokerImageStatusResponse{
		LocalShort: &BrokerImageEntityState{Exists: true, Hash: "sha256:abc123"},
	})
	defer brokerTS.Close()

	createTestBroker(t, db, "broker1", "test-broker", brokerTS.URL, []store.BrokerProfile{{Type: "docker"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(resp.Brokers))
	}
	if !resp.Brokers[0].Reachable {
		t.Error("expected broker to be reachable")
	}
	if resp.Brokers[0].LocalShort == nil || !resp.Brokers[0].LocalShort.Exists {
		t.Error("expected local_short to exist")
	}
}

func TestImageStatusHandler_UnreachableBroker(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	createTestBroker(t, db, "broker1", "dead-broker", "http://127.0.0.1:1", []store.BrokerProfile{{Type: "docker"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(resp.Brokers))
	}
	if resp.Brokers[0].Reachable {
		t.Error("expected broker to be unreachable")
	}
}

func TestImageStatusHandler_OldBroker404(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	brokerTS := fakeBrokerServer(http.StatusNotFound, nil)
	defer brokerTS.Close()

	createTestBroker(t, db, "broker1", "old-broker", brokerTS.URL, []store.BrokerProfile{{Type: "docker"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(resp.Brokers))
	}
	b := resp.Brokers[0]
	if !b.Reachable {
		t.Error("expected broker to be reachable (it responded, just doesn't support the endpoint)")
	}
	if !b.Unsupported {
		t.Error("expected broker to be marked unsupported")
	}
}

func TestImageStatusHandler_NoNodeBoundBrokers(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	createTestBroker(t, db, "broker1", "k8s-broker", "", []store.BrokerProfile{{Type: "kubernetes"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 0 {
		t.Errorf("expected 0 node-bound brokers, got %d", len(resp.Brokers))
	}
	if len(resp.ProxyBrokers) != 1 {
		t.Errorf("expected 1 proxy broker, got %d", len(resp.ProxyBrokers))
	}
}

type fakeImageManager struct {
	exists map[string]bool
}

func (f *fakeImageManager) ImageExists(_ context.Context, image string) (bool, error) {
	return f.exists[image], nil
}
func (f *fakeImageManager) PullImage(context.Context, string) error   { return nil }
func (f *fakeImageManager) RemoveImage(context.Context, string) error { return nil }
func (f *fakeImageManager) Name() string                              { return "Podman" }

func TestImageStatusHandler_ProxyBrokersWithLocalImageManager(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	createTestBroker(t, db, "broker1", "k8s-broker", "", []store.BrokerProfile{{Type: "kubernetes"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	mgr := &fakeImageManager{exists: map[string]bool{"my-image:latest": true}}
	srv.imageManager = mgr
	srv.imageChecker.SetLocal(mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker entry (local), got %d", len(resp.Brokers))
	}
	if !resp.Brokers[0].Reachable {
		t.Error("expected local entry to be reachable")
	}
	if resp.Brokers[0].BrokerName != "Podman" {
		t.Errorf("expected broker name %q, got %q", "Podman", resp.Brokers[0].BrokerName)
	}
	if len(resp.ProxyBrokers) != 1 {
		t.Errorf("expected 1 proxy broker, got %d", len(resp.ProxyBrokers))
	}
}

func TestImageStatusHandler_BareImageCheck(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	brokerTS := fakeBrokerServer(http.StatusOK, &BrokerImageStatusResponse{
		LocalShort: &BrokerImageEntityState{Exists: true, Hash: "sha256:bare123"},
	})
	defer brokerTS.Close()

	createTestBroker(t, db, "broker1", "docker-broker", brokerTS.URL, []store.BrokerProfile{{Type: "docker"}}, nil)
	hc := createTestHarnessConfig(t, db, "hc1", "my-custom-image")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/check-image", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigCheckImage(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	status, ok := resp["image_status"].(string)
	if !ok {
		t.Fatal("image_status not found in response")
	}
	if status != store.HarnessConfigImageStatusValid {
		t.Errorf("expected status %q, got %q", store.HarnessConfigImageStatusValid, status)
	}
}

func TestImageStatusHandler_MultipleBrokersMixed(t *testing.T) {
	srv, db := setupImageStatusTest(t)

	reachableTS := fakeBrokerServer(http.StatusOK, &BrokerImageStatusResponse{
		LocalShort: &BrokerImageEntityState{Exists: true, Hash: "sha256:abc"},
	})
	defer reachableTS.Close()

	createTestBroker(t, db, "broker1", "good-broker", reachableTS.URL, []store.BrokerProfile{{Type: "docker"}}, nil)
	createTestBroker(t, db, "broker2", "bad-broker", "http://127.0.0.1:1", []store.BrokerProfile{{Type: "podman"}}, nil)

	hc := createTestHarnessConfig(t, db, "hc1", "my-image:latest")

	transport := newBrokerHTTPTransport(false, nil)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     transport,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/image-status", nil)
	rr := httptest.NewRecorder()
	srv.handleHarnessConfigImageStatus(rr, req, hc.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AggregatedImageStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(resp.Brokers) != 2 {
		t.Fatalf("expected 2 brokers, got %d", len(resp.Brokers))
	}

	var reachableCount, unreachableCount int
	for _, b := range resp.Brokers {
		if b.Reachable {
			reachableCount++
		} else {
			unreachableCount++
		}
	}
	if reachableCount != 1 || unreachableCount != 1 {
		t.Errorf("expected 1 reachable and 1 unreachable, got %d reachable and %d unreachable", reachableCount, unreachableCount)
	}
}

func TestBrokerUnsupportedError(t *testing.T) {
	err := &BrokerUnsupportedError{StatusCode: 404}
	if err.Error() != "broker does not support this endpoint (HTTP 404)" {
		t.Errorf("unexpected error message: %s", err.Error())
	}
}
