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

package integration_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProcessTopologyStartsDistinctProcessesAndCancelsThem(t *testing.T) {
	topology := newProcessTopology(t, nil)
	first := topology.start(t, processSpec{Name: "hub", Mode: "backend", ReplicaID: "hub"})
	second := topology.start(t, processSpec{Name: "bridge-1", Mode: "backend", ReplicaID: "bridge-1"})

	if first.PID == second.PID {
		t.Fatalf("processes share PID %d", first.PID)
	}
	if first.Port == second.Port {
		t.Fatalf("processes share port %d", first.Port)
	}
	if first.Port == 0 || second.Port == 0 {
		t.Fatalf("ports were not allocated: %d, %d", first.Port, second.Port)
	}
	if !first.Ready || !second.Ready {
		t.Fatalf("readiness not captured: first=%t second=%t", first.Ready, second.Ready)
	}
	if got := topology.observations.String(); strings.Count(got, `"outcome":"ready"`) != 2 {
		t.Fatalf("structured readiness observations = %q", got)
	}

	topology.stop(t)
	for _, process := range []*testProcess{first, second} {
		if process.cmd.ProcessState == nil {
			t.Errorf("process %s (PID %d) was not reaped", process.Name, process.PID)
		}
	}
}

func TestLoadAlternatorUsesRealProcessesAndPinsSSE(t *testing.T) {
	topology := newProcessTopology(t, nil)
	backend1 := topology.start(t, processSpec{Name: "bridge-1", Mode: "backend", ReplicaID: "bridge-1"})
	backend2 := topology.start(t, processSpec{Name: "bridge-2", Mode: "backend", ReplicaID: "bridge-2"})
	alternator := topology.start(t, processSpec{
		Name: "alternator",
		Mode: "alternator",
		Env: map[string]string{
			"SCION_TEST_BACKEND_1": backend1.URL(),
			"SCION_TEST_BACKEND_2": backend2.URL(),
		},
	})

	client := &http.Client{Timeout: 3 * time.Second}
	var replicas []string
	for range 4 {
		response, err := client.Get(alternator.URL() + "/request")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		replicas = append(replicas, strings.TrimSpace(string(body)))
	}
	if want := []string{"bridge-1", "bridge-2", "bridge-1", "bridge-2"}; !slices.Equal(replicas, want) {
		t.Fatalf("replicas = %v; want %v", replicas, want)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, alternator.URL()+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	scanner := bufio.NewScanner(response.Body)
	var streamReplicas []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			streamReplicas = append(streamReplicas, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(streamReplicas) != 3 {
		t.Fatalf("stream events = %v; want three", streamReplicas)
	}
	for _, replica := range streamReplicas[1:] {
		if replica != streamReplicas[0] {
			t.Fatalf("SSE connection moved replicas: %v", streamReplicas)
		}
	}
}

func TestCredentialRedactionFoundation(t *testing.T) {
	bearer := "SYNTHETIC_RUNTIME_BEARER_A_DO_NOT_USE"
	digest := sha256.Sum256([]byte(bearer))
	stableHash := hex.EncodeToString(digest[:])
	redactor := newCredentialRedactor(bearer)

	input := fmt.Sprintf("Authorization: Bearer %s token_hash=%s outcome=denied", bearer, stableHash)
	got := redactor.redact(input)
	for _, forbidden := range []string{bearer, stableHash} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("redacted log contains forbidden credential material %q: %s", forbidden, got)
		}
	}
	if !strings.Contains(got, "outcome=denied") {
		t.Fatalf("redaction removed safe observation: %s", got)
	}

	recorder := newObservationRecorder(redactor)
	recorder.record(observation{
		RequestID:      "request-001",
		ReplicaID:      "bridge-1",
		Outcome:        "denied",
		CacheStatus:    "miss",
		TaskID:         "task-001",
		ContextID:      "context-001",
		Cursor:         "cursor-001",
		PrincipalLabel: "user-a",
		LatencyMS:      12,
	})
	for _, forbidden := range []string{bearer, stableHash} {
		if strings.Contains(recorder.String(), forbidden) {
			t.Fatalf("structured observations contain forbidden material %q", forbidden)
		}
	}

	topology := newProcessTopology(t, redactor)
	topology.start(t, processSpec{
		Name:      "credential-logging-fixture",
		Mode:      "backend-log",
		ReplicaID: "bridge-credential-test",
		Env:       map[string]string{"SCION_TEST_SYNTHETIC_CREDENTIAL": bearer},
	})
	for _, forbidden := range []string{bearer, stableHash} {
		if strings.Contains(topology.logs.String(), forbidden) {
			t.Fatalf("captured subprocess logs contain forbidden material %q: %s", forbidden, topology.logs.String())
		}
	}
}

func TestFixtureMatricesContainApprovedCategories(t *testing.T) {
	identities := loadIdentityFixtures(t, "testdata/identity_matrix.json")
	assertExactCategories(t, identityCategories(identities), []string{
		"user-a", "user-b", "suspended", "valid", "rotated", "foreign", "expired",
		"missing-exp", "forged", "service-account", "changed-email", "non-authoritative",
		"hub-principal", "ge-principal",
	})
	protocols := loadProtocolFixtures(t, "testdata/protocol_matrix.json")
	assertExactCategories(t, protocolCategories(protocols), []string{
		"a2a-v1-discovery", "a2a-v1-jsonrpc-send", "a2a-v1-jsonrpc-stream",
		"a2a-v1-sse-reconnect", "ge-v0.3-discovery", "ge-v0.3-jsonrpc-send",
		"ge-v0.3-jsonrpc-stream", "ge-v0.3-multiturn",
	})
	agent := loadAgentFixture(t, "testdata/stable_agent.json")
	assertExactCategories(t, agent.Capabilities, []string{
		"initial-send", "follow-up-turn", "streaming-events", "cancellation", "delayed-opposite-replica-event",
	})
	if agent.TaskID == "" || agent.ContextID == "" || agent.InitialReplica == agent.DelayedEventReplica {
		t.Fatalf("stable agent fixture does not model cross-replica task/context flow: %+v", agent)
	}
	for _, path := range []string{"testdata/identity_matrix.json", "testdata/protocol_matrix.json", "testdata/stable_agent.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(data)), "bearer ") {
			t.Errorf("%s contains a bearer credential", path)
		}
		if regexp.MustCompile(`(?i)[a-f0-9]{64}`).Match(data) {
			t.Errorf("%s contains a stable SHA-256-shaped value", path)
		}
	}
}

func TestDatabaseRunNamingAndCleanup(t *testing.T) {
	allocator := newDatabaseRunAllocator()
	run, err := allocator.newRun("run-20260918-worker-07")
	if err != nil {
		t.Fatal(err)
	}
	if run.DatabaseName != "ge_a2a_run_20260918_worker_07" {
		t.Fatalf("database name = %q", run.DatabaseName)
	}
	if run.SchemaName != "harness_run_20260918_worker_07" {
		t.Fatalf("schema name = %q", run.SchemaName)
	}

	var cleanupOrder []string
	run.addTeardown(func(context.Context) error { cleanupOrder = append(cleanupOrder, "schema"); return nil })
	run.addTeardown(func(context.Context) error { cleanupOrder = append(cleanupOrder, "connection"); return nil })
	if err := run.teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"connection", "schema"}; !slices.Equal(cleanupOrder, want) {
		t.Fatalf("cleanup order = %v; want %v", cleanupOrder, want)
	}

	for _, invalid := range []string{"", "duplicate spaces", "UPPERCASE", "../escape"} {
		if _, err := allocator.newRun(invalid); err == nil {
			t.Errorf("newDatabaseRun(%q) succeeded; want error", invalid)
		}
	}
	if _, err := allocator.newRun("run-20260918-worker-07"); err == nil {
		t.Error("duplicate run ID succeeded; want error")
	}
}

func TestPostgreSQLSchemaAllocator(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("TEST_REQUIRE_DATABASE") == "1" || os.Getenv("CI") == "true" {
			t.Fatal("TEST_DATABASE_URL is required in fail-closed / CI mode")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	run, err := newDatabaseRunAllocator().newRun(fmt.Sprintf("run-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.allocateSchema(context.Background(), databaseURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := run.teardown(context.Background()); err != nil {
			t.Errorf("teardown PostgreSQL run: %v", err)
		}
	})
	if err := run.schemaExists(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceLayersMatchProvenScope(t *testing.T) {
	data, err := os.ReadFile("testdata/acceptance_layers.json")
	if err != nil {
		t.Fatal(err)
	}
	var scaffold acceptanceScaffold
	if err := json.Unmarshal(data, &scaffold); err != nil {
		t.Fatal(err)
	}
	if len(scaffold.Layers) != 11 {
		t.Fatalf("layers = %d; want 11", len(scaffold.Layers))
	}
	allowed := []string{"passing", "partial", "blocked-on-taskstore", "external-live-only"}
	wantPassing := map[string]bool{
		"TestGEEnvelopeCompatibility":        false,
		"TestTwoReplicaUserLifecycle":        true,
		"TestColdReplicaAndRotation":         true,
		"TestCrossReplicaStreamCursor":       true,
		"TestCrashLeaseBoundary":             true,
		"TestControlPlanePrincipalIsolation": true,
		"TestCombinedStartupMatrix":          true,
		"TestCredentialRedaction":            true,
		"CloudRunDeploymentConfig":           false,
		"KubernetesDeploymentConfig":         false,
		"CIAutomatedPostgresIntegration":     true,
	}
	for _, layer := range scaffold.Layers {
		if layer.Passing != wantPassing[layer.Name] {
			t.Errorf("layer %s passing = %t, want %t", layer.Name, layer.Passing, wantPassing[layer.Name])
		}
		if !slices.Contains(allowed, layer.Status) {
			t.Errorf("layer %s has unknown status %q", layer.Name, layer.Status)
		}
		if layer.Status == "blocked-on-taskstore" && layer.Passing {
			t.Errorf("taskstore-blocked layer %s must remain false", layer.Name)
		}
	}
}
