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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type conduitNamedRuntime struct {
	*scionrt.MockRuntime
	name string
}

func (r *conduitNamedRuntime) Name() string { return r.name }

type countingDeliverer struct {
	inner agent.ConduitDeliverer
	calls atomic.Int32
}

func (c *countingDeliverer) DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	c.calls.Add(1)
	return c.inner.DeliverMessage(ctx, agentID, req)
}

func newTestHubClient(t *testing.T, hitCount *atomic.Int32) hubclient.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hitCount != nil {
			hitCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"outcome":"delivered"}`))
	}))
	t.Cleanup(ts.Close)
	c, err := hubclient.New(ts.URL)
	if err != nil {
		t.Fatalf("hubclient.New: %v", err)
	}
	return c
}

// TestHubConduitDeliverer_SeveralHubsUsesExecAtOnce verifies that a broker
// connected to several hubs returns ErrConduitUnavailable (wrapping
// errConduitSeveralHubs) so AgentManager falls back to exec immediately with
// zero retries and zero hub HTTP calls.
func TestHubConduitDeliverer_SeveralHubsUsesExecAtOnce(t *testing.T) {
	var hubHits atomic.Int32
	c1 := newTestHubClient(t, &hubHits)
	c2 := newTestHubClient(t, &hubHits)

	var mu sync.Mutex
	var execs []string
	rt := &conduitNamedRuntime{
		name: "docker",
		MockRuntime: &scionrt.MockRuntime{
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{
					ID:          "hub-agent-1",
					ContainerID: "c-1",
					Name:        "agent-1",
					Labels: map[string]string{
						"scion.name":              "agent-1",
						agent.LabelConduitDeliver: "true",
					},
				}}, nil
			},
			ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				execs = append(execs, strings.Join(cmd, " "))
				return "", nil
			},
			ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, _ io.Reader) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				execs = append(execs, strings.Join(cmd, " "))
				return "", nil
			},
		},
	}

	mgr := agent.NewManager(rt).(*agent.AgentManager)
	srv := New(ServerConfig{
		ConduitDeliverRuntimes: []string{"cloudrun", "kubernetes", "docker"},
	}, mgr, rt, config.RuntimeBuildRecord{})
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "b-1", HubClient: c1}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "b-2", HubClient: c2}

	// Direct check on hubConduitDeliverer error typing.
	_, _, derr := (hubConduitDeliverer{s: srv}).DeliverMessage(context.Background(), "hub-agent-1", &agentcontrol.DeliverRequest{
		V:          agentcontrol.DeliverVersion,
		DeliveryID: "d-1",
		MessageIDs: []string{"m-1"},
		Op:         agentcontrol.OpPaste,
	})
	if !errors.Is(derr, agent.ErrConduitUnavailable) || !errors.Is(derr, errConduitSeveralHubs) {
		t.Fatalf("DeliverMessage err = %v, want ErrConduitUnavailable + errConduitSeveralHubs", derr)
	}

	// End-to-end through AgentManager.Message (interrupt path exercises deliver immediately).
	cd := &countingDeliverer{inner: hubConduitDeliverer{s: srv}}
	mgr.SetConduitDeliverer(cd)

	ctx := agent.WithDeliveryMessageID(context.Background(), "m-1")
	if err := mgr.Message(ctx, "agent-1", "", "hello", true); err != nil {
		t.Fatalf("mgr.Message: %v", err)
	}
	if cd.calls.Load() != 1 {
		t.Fatalf("deliverer calls = %d, want 1 (no retries on ErrConduitUnavailable)", cd.calls.Load())
	}
	if hubHits.Load() != 0 {
		t.Fatalf("hub HTTP hits = %d, want 0", hubHits.Load())
	}
	foundPaste := false
	for _, e := range execs {
		if strings.Contains(e, "load-buffer") {
			foundPaste = true
			break
		}
	}
	if !foundPaste {
		t.Fatalf("expected exec delivery (load-buffer), got execs=%v", execs)
	}
}

// TestServer_ConduitDeliverRuntimesPropagated verifies that ServerConfig.ConduitDeliverRuntimes
// is passed to the primary manager in New and to any manager via enableConduitDelivery.
func TestServer_ConduitDeliverRuntimesPropagated(t *testing.T) {
	var hubHits atomic.Int32
	c1 := newTestHubClient(t, &hubHits)

	newMgr := func(rtName string, execs *[]string) (*agent.AgentManager, scionrt.Runtime) {
		var mu sync.Mutex
		rt := &conduitNamedRuntime{
			name: rtName,
			MockRuntime: &scionrt.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{
						ID:          "hub-agent-1",
						ContainerID: "c-1",
						Name:        "agent-1",
						Labels: map[string]string{
							"scion.name":              "agent-1",
							agent.LabelConduitDeliver: "true",
						},
					}}, nil
				},
				ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					*execs = append(*execs, strings.Join(cmd, " "))
					return "", nil
				},
				ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, _ io.Reader) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					*execs = append(*execs, strings.Join(cmd, " "))
					return "", nil
				},
			},
		}
		return agent.NewManager(rt).(*agent.AgentManager), rt
	}

	var dockerExecs []string
	dockerMgr, dockerRT := newMgr("docker", &dockerExecs)
	srv := New(ServerConfig{
		ConduitDeliverRuntimes: []string{"podman"},
	}, dockerMgr, dockerRT, config.RuntimeBuildRecord{})
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "b-1", HubClient: c1}

	// Docker is excluded by [podman], so it must take exec.
	srv.enableConduitDelivery(dockerMgr)
	if err := dockerMgr.Message(agent.WithDeliveryMessageID(context.Background(), "m-1"), "agent-1", "", "hi", true); err != nil {
		t.Fatalf("docker Message: %v", err)
	}
	if hubHits.Load() != 0 || len(dockerExecs) == 0 {
		t.Fatalf("docker with [podman]: hubHits=%d execs=%v; want exec", hubHits.Load(), dockerExecs)
	}

	// Auxiliary Podman manager is included by [podman], so it must take conduit.
	var podmanExecs []string
	podmanMgr, _ := newMgr("podman", &podmanExecs)
	srv.enableConduitDelivery(podmanMgr)
	if err := podmanMgr.Message(agent.WithDeliveryMessageID(context.Background(), "m-2"), "agent-1", "", "hi", true); err != nil {
		t.Fatalf("podman Message: %v", err)
	}
	if hubHits.Load() != 1 || len(podmanExecs) != 0 {
		t.Fatalf("podman with [podman]: hubHits=%d execs=%v; want conduit", hubHits.Load(), podmanExecs)
	}
}
