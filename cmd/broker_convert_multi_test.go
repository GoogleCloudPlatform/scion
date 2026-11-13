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

//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const cmdMultiInstanceSettings = `schema_version: "1"
active_profile: build
server:
  broker:
    broker_id: ` + convertTestBrokerID + `
runtimes:
  docker:
    type: docker
  cluster:
    type: kubernetes
    context: prod
    namespace: agents
profiles:
  build:
    runtime: docker
    image_registry: reg.example/build
  k8s:
    runtime: cluster
    image_registry: reg.example/k8s
`

type cmdMultiHub struct {
	*httptest.Server
	mu        sync.Mutex
	instances map[string]map[string]any
	instMax   map[string]*int64
	instRev   map[string]int64
	projects  map[string]*cmdMultiProject
}

type cmdMultiProject struct {
	name          string
	defaultID     string
	activeProfile string
	providers     map[string]string
}

func newCmdMultiHub(t *testing.T) *cmdMultiHub {
	t.Helper()
	h := &cmdMultiHub{
		instances: map[string]map[string]any{},
		instMax:   map[string]*int64{},
		instRev:   map[string]int64{},
		projects: map[string]*cmdMultiProject{
			"p-1": {name: "web", defaultID: convertTestBrokerID, activeProfile: "build", providers: map[string]string{convertTestBrokerID: ""}},
			"p-2": {name: "k8s-app", defaultID: convertTestBrokerID, activeProfile: "k8s", providers: map[string]string{convertTestBrokerID: ""}},
		},
	}
	const rb = "/api/v1/runtime-brokers"
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()

		reply := func(body any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}
		p := r.URL.Path
		switch {
		case p == rb && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			var out []map[string]any
			for _, b := range h.instances {
				if name == "" || strings.EqualFold(b["name"].(string), name) {
					out = append(out, b)
				}
			}
			reply(map[string]any{"brokers": out})
		case p == rb+"/"+convertTestBrokerID && r.Method == http.MethodGet:
			reply(map[string]any{
				"id":     convertTestBrokerID,
				"name":   "legacy-host",
				"status": "offline",
				"profiles": []map[string]any{
					{"name": "build", "type": "docker"},
					{"name": "k8s", "type": "kubernetes"},
				},
			})
		case p == rb+"/"+convertTestBrokerID+"/settings" && r.Method == http.MethodGet:
			reply(map[string]any{
				"brokerId":      convertTestBrokerID,
				"settings":      map[string]any{"maxAgents": 10},
				"effective":     map[string]any{"maxAgents": map[string]any{"value": 10, "source": "broker", "count": 0, "inherited": map[string]any{"value": 0, "source": "unlimited"}}},
				"_capabilities": map[string]any{"update": true},
			})
		case p == rb+"/"+convertTestBrokerID+"/projects" && r.Method == http.MethodGet:
			reply(map[string]any{
				"projects": []map[string]any{
					{"projectId": "p-1", "projectName": "web"},
					{"projectId": "p-2", "projectName": "k8s-app"},
				},
			})
		case strings.HasPrefix(p, rb+"/"):
			parts := strings.Split(strings.TrimPrefix(p, rb+"/"), "/")
			bid := parts[0]
			switch {
			case len(parts) == 1 && r.Method == http.MethodGet:
				if h.instances[bid] == nil {
					http.NotFound(w, r)
					return
				}
				reply(h.instances[bid])
			case len(parts) == 1 && r.Method == http.MethodDelete:
				delete(h.instances, bid)
				delete(h.instMax, bid)
				delete(h.instRev, bid)
				w.WriteHeader(http.StatusNoContent)
			case len(parts) == 2 && parts[1] == "settings" && r.Method == http.MethodGet:
				s := map[string]any{}
				if v := h.instMax[bid]; v != nil {
					s["maxAgents"] = *v
				}
				reply(map[string]any{"brokerId": bid, "settings": s, "revision": h.instRev[bid], "_capabilities": map[string]any{"update": true}})
			case len(parts) == 2 && parts[1] == "settings" && r.Method == http.MethodPut:
				var body struct {
					Settings struct {
						MaxAgents *int64 `json:"maxAgents"`
					} `json:"settings"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				h.instMax[bid] = body.Settings.MaxAgents
				h.instRev[bid]++
				reply(map[string]any{"brokerId": bid, "settings": map[string]any{"maxAgents": h.instMax[bid]}, "revision": h.instRev[bid]})
			default:
				http.NotFound(w, r)
			}
		case p == "/api/v1/agents" && r.Method == http.MethodGet:
			reply(map[string]any{"agents": []any{}})
		case strings.HasPrefix(p, "/api/v1/projects/"):
			parts := strings.Split(strings.TrimPrefix(p, "/api/v1/projects/"), "/")
			pr := h.projects[parts[0]]
			if pr == nil {
				http.NotFound(w, r)
				return
			}
			switch {
			case len(parts) == 1 && r.Method == http.MethodGet:
				reply(map[string]any{"id": parts[0], "name": pr.name, "defaultRuntimeBrokerId": pr.defaultID})
			case len(parts) == 1:
				var body struct {
					DefaultRuntimeBrokerID string `json:"defaultRuntimeBrokerId"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				pr.defaultID = body.DefaultRuntimeBrokerID
				reply(map[string]any{"id": parts[0], "defaultRuntimeBrokerId": pr.defaultID})
			case len(parts) == 2 && parts[1] == "settings":
				reply(map[string]any{"activeProfile": pr.activeProfile})
			case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodGet:
				ids := make([]string, 0, len(pr.providers))
				for id := range pr.providers {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				var out []map[string]any
				for _, id := range ids {
					out = append(out, map[string]any{"brokerId": id, "localPath": pr.providers[id]})
				}
				reply(map[string]any{"providers": out})
			case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodPost:
				var body struct {
					BrokerID string `json:"brokerId"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				pr.providers[body.BrokerID] = ""
				reply(map[string]any{"provider": map[string]any{"brokerId": body.BrokerID}})
			case len(parts) == 3 && parts[1] == "providers" && r.Method == http.MethodDelete:
				delete(pr.providers, parts[2])
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// End-to-end CLI rehearsal of multi-instance Docker + Kubernetes:
// --apply -> repeat --apply (no-op via resumeConvertInput) -> --status -> --rollback.
func TestBrokerConvertCLI_MultiInstanceApplyStatusAndRollback(t *testing.T) {
	home, settingsPath := convertTestHome(t, cmdMultiInstanceSettings)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, []string{"--foreground"}))

	hub := newCmdMultiHub(t)
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) {
		return hubclient.New(hub.URL)
	})

	oldConfirm, oldHook, oldRunning := autoConfirm, convertEnvHook, convertServerRunning
	t.Cleanup(func() {
		autoConfirm, convertEnvHook, convertServerRunning = oldConfirm, oldHook, oldRunning
	})
	autoConfirm = true
	convertServerRunning = func(context.Context) bool { return false }
	convertEnvHook = func(env *brokerconvert.Env) {
		env.Register = func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
			tType := inst.RuntimeTarget.Type
			var scope brokeridentity.ExecutionScope
			if tType == brokeridentity.TargetTypeKubernetes {
				scope = brokeridentity.ExecutionScope{
					Type:       brokeridentity.TargetTypeKubernetes,
					Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-1", Namespace: inst.RuntimeTarget.Namespace},
				}
			} else {
				scope = brokeridentity.ExecutionScope{
					Type:   brokeridentity.TargetTypeDocker,
					Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"},
				}
			}
			id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(env.GlobalDir, inst.Key), inst.Key, tType, scope, []string{convertTestBrokerID})
			if err != nil {
				return "", "", err
			}
			hub.mu.Lock()
			hub.instances[id.RuntimeBrokerID] = map[string]any{"id": id.RuntimeBrokerID, "name": inst.Name, "status": "online"}
			hub.mu.Unlock()
			return id.RuntimeBrokerID, id.RuntimeTarget.ID, nil
		}
	}

	beforeBytes, err := os.ReadFile(settingsPath)
	require.NoError(t, err)

	// 1. --apply
	setConvertFlags(t, false, false, "json", "build", "k8s")
	brokerConvertApply = true
	out, err := runConvertForTest(t)
	require.NoError(t, err)
	var applyRep brokerconvert.Report
	require.NoError(t, json.Unmarshal([]byte(out), &applyRep), out)
	assert.Equal(t, brokerconvert.JournalApplied, applyRep.State)
	require.Len(t, applyRep.Instances, 2)

	byType := map[string]string{}
	for _, in := range applyRep.Instances {
		if in.Key == "docker" {
			byType["docker"] = in.BrokerID
		} else {
			byType["k8s"] = in.BrokerID
		}
		require.NotNil(t, hub.instMax[in.BrokerID])
		assert.Equal(t, int64(5), *hub.instMax[in.BrokerID])
	}
	assert.Equal(t, byType["docker"], hub.projects["p-1"].defaultID)
	assert.Equal(t, byType["k8s"], hub.projects["p-2"].defaultID)

	// 2. Repeat --apply via CLI (exercises resumeConvertInput on multi-instance settings.yaml)
	setConvertFlags(t, false, false, "json", "build", "k8s")
	brokerConvertApply = true
	out2, err := runConvertForTest(t)
	require.NoError(t, err)
	var noopRep brokerconvert.Report
	require.NoError(t, json.Unmarshal([]byte(out2), &noopRep), out2)
	assert.Contains(t, noopRep.Notes, brokerconvert.NoteNoop)

	// 3. --status
	setConvertFlags(t, false, false, "json")
	brokerConvertStatus = true
	stOut, err := runConvertForTest(t)
	require.NoError(t, err)
	var stRep brokerconvert.Report
	require.NoError(t, json.Unmarshal([]byte(stOut), &stRep), stOut)
	assert.True(t, stRep.Switched, "both instances online -> switched")

	// 4. --rollback
	setConvertFlags(t, false, false, "json")
	brokerConvertRollback = true
	rbOut, err := runConvertForTest(t)
	require.NoError(t, err)
	var rbRep brokerconvert.Report
	require.NoError(t, json.Unmarshal([]byte(rbOut), &rbRep), rbOut)
	assert.Equal(t, brokerconvert.JournalRolledBack, rbRep.State)
	assert.Empty(t, hub.instances)
	assert.Equal(t, convertTestBrokerID, hub.projects["p-1"].defaultID)
	assert.Equal(t, convertTestBrokerID, hub.projects["p-2"].defaultID)

	afterBytes, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(beforeBytes), string(afterBytes))
}

// CLI stopped-phase co-located apply with multiple instances (Docker + Kubernetes)
// plans locally with no Hub reachable and writes both instances to settings.yaml.
func TestRunConvertApply_ColocatedMultiInstanceStoppedPhaseNeedsNoHub(t *testing.T) {
	convertTestHome(t, cmdMultiInstanceSettings)
	setConvertFlags(t, false, false, "", "build", "k8s")
	brokerConvertApply = true
	oldConfirm, oldHook := autoConfirm, convertColocatedEnvHook
	t.Cleanup(func() { autoConfirm, convertColocatedEnvHook = oldConfirm, oldHook })
	autoConfirm = true
	convertColocatedEnvHook = func(ce *brokerconvert.ColocatedEnv) {
		ce.ServerUp = func(context.Context) bool { return false }
		ce.Q7 = func(context.Context, *brokerconvert.Journal) brokerconvert.Q7Input {
			return brokerconvert.Q7Input{
				Topology: brokerconvert.TopologySingleSQLite,
				LegacyStore: func() (brokerconvert.LegacyStoreFacts, error) {
					return brokerconvert.LegacyStoreFacts{RowPresent: true}, nil
				},
			}
		}
	}
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) { return nil, nil })
	env, settings, err := newConvertEnv()
	require.NoError(t, err)
	require.Nil(t, env.Client)

	c := &cobra.Command{}
	c.SetErr(io.Discard)
	c.SetOut(&bytes.Buffer{})
	c.SetContext(context.Background())
	in, _, err := loadConvertInput(c)
	require.NoError(t, err)
	plan, err := brokerconvert.BuildLocal(in)
	require.NoError(t, err)
	require.Len(t, plan.SettingsInstances(), 2)
	digest, err := brokerconvert.PlanDigest(plan, in.Choices)
	require.NoError(t, err)

	now := time.Now().UTC()
	j, err := brokerconvert.NewJournal(digest, convertTestBrokerID, now)
	require.NoError(t, err)
	j.State = brokerconvert.JournalAwaitingStop
	j.StopRecheckAt = &now
	require.NoError(t, brokerconvert.SaveJournal(env.GlobalDir, j, now))

	rep, err := runConvertApply(context.Background(), c, env, settings, colocatedHost{Colocated: true}, brokerconvert.HostModeColocated, brokerconvert.HostModeDetected)
	require.NoError(t, err)
	assert.Equal(t, brokerconvert.JournalApplied, rep.State)
	insts, err := env.LoadInstances()
	require.NoError(t, err)
	assert.Len(t, insts, 2, "both Docker and Kubernetes instances written in stopped phase")
}
