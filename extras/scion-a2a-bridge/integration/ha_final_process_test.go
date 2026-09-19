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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	bridgestate "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const haControlAudience = "https://ha-bridge-control.example.invalid"

type haFinalTopology struct {
	topology   *processTopology
	fakeGoogle *testProcess
	hub        *testProcess
	bridgeA    *testProcess
	bridgeB    *testProcess
	alternator *testProcess
	userToken  string
	hubToken   string
}

func startHAFinalTopology(t *testing.T) *haFinalTopology {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("TEST_REQUIRE_DATABASE") == "1" {
			t.Fatal("TEST_DATABASE_URL is required in fail-closed mode")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	topology := newProcessTopology(t, nil)
	fakeGoogle := topology.start(t, processSpec{Name: "fake-google", Mode: "fake-google", ReplicaID: "fake-google"})
	hubProcess := topology.start(t, processSpec{Name: "hub", Mode: "hub", ReplicaID: "hub", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(),
		"SCION_TEST_HUB_DATABASE":    t.TempDir() + "/hub.db",
	}})
	bridgeEnv := map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL":  fakeGoogle.URL(),
		"SCION_TEST_HUB_URL":          hubProcess.URL(),
		"SCION_TEST_CONTROL_AUDIENCE": haControlAudience,
	}
	bridgeA := topology.start(t, processSpec{Name: "ha-bridge-a", Mode: "ha-bridge", ReplicaID: "ha-bridge-a", Env: bridgeEnv})
	bridgeB := topology.start(t, processSpec{Name: "ha-bridge-b", Mode: "ha-bridge", ReplicaID: "ha-bridge-b", Env: bridgeEnv})
	alternator := topology.start(t, processSpec{Name: "ha-alternator", Mode: "alternator", ReplicaID: "ha-alternator", Env: map[string]string{
		"SCION_TEST_BACKEND_1": bridgeA.URL(),
		"SCION_TEST_BACKEND_2": bridgeB.URL(),
	}})
	return &haFinalTopology{
		topology: topology, fakeGoogle: fakeGoogle, hub: hubProcess,
		bridgeA: bridgeA, bridgeB: bridgeB, alternator: alternator,
		userToken: fetchMintedToken(t, fakeGoogle.URL(), nil),
		hubToken: fetchMintedToken(t, fakeGoogle.URL(), url.Values{
			"audience": {haControlAudience},
			"email":    {"hub-sa@hub-project.iam.gserviceaccount.com"},
			"sub":      {"ha-hub-service"},
		}),
	}
}

func (h *haFinalTopology) restartBridge(t *testing.T, old *testProcess, name string) *testProcess {
	t.Helper()
	h.topology.stopProcess(t, old)
	return h.topology.start(t, processSpec{Name: name, Mode: "ha-bridge", ReplicaID: name, Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL":  h.fakeGoogle.URL(),
		"SCION_TEST_HUB_URL":          h.hub.URL(),
		"SCION_TEST_CONTROL_AUDIENCE": haControlAudience,
	}})
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type asyncRPCResult struct {
	reply rpcReply
	raw   []byte
	err   error
}

func callRPC(t *testing.T, endpoint, token, method string, params any) (rpcReply, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": fmt.Sprintf("%s-%d", method, time.Now().UnixNano()),
		"method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/projects/proj1/agents/agent1", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", method, response.StatusCode, body)
	}
	var reply rpcReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode %s response: %v body=%s", method, err, body)
	}
	return reply, body
}

func callRPCAsync(endpoint, token, method string, params any) <-chan asyncRPCResult {
	result := make(chan asyncRPCResult, 1)
	go func() {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": fmt.Sprintf("%s-%d", method, time.Now().UnixNano()),
			"method": method, "params": params,
		})
		if err != nil {
			result <- asyncRPCResult{err: err}
			return
		}
		request, err := http.NewRequest(http.MethodPost, endpoint+"/projects/proj1/agents/agent1", bytes.NewReader(payload))
		if err != nil {
			result <- asyncRPCResult{err: err}
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			result <- asyncRPCResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			result <- asyncRPCResult{err: err}
			return
		}
		var reply rpcReply
		if err := json.Unmarshal(body, &reply); err != nil {
			result <- asyncRPCResult{raw: body, err: err}
			return
		}
		result <- asyncRPCResult{reply: reply, raw: body}
	}()
	return result
}

func waitHubMessages(t *testing.T, h *haFinalTopology, want int) hubProcessStats {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		stats := getJSON[hubProcessStats](t, h.hub.URL()+"/__test/stats")
		if stats.Messages >= want && stats.LastUserID != "" {
			return stats
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d Hub messages\nprocess logs:\n%s", want, h.topology.logs.String())
	return hubProcessStats{}
}

func awaitRPC(t *testing.T, result <-chan asyncRPCResult) asyncRPCResult {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.reply.Error != nil {
			t.Fatalf("JSON-RPC error %d: %s raw=%s", got.reply.Error.Code, got.reply.Error.Message, got.raw)
		}
		return got
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for JSON-RPC response")
		return asyncRPCResult{}
	}
}

func taskIdentity(t *testing.T, raw json.RawMessage) (string, string, string) {
	t.Helper()
	var result struct {
		Task *struct {
			ID        string `json:"id"`
			ContextID string `json:"contextId"`
			Status    struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
		ID        string `json:"id"`
		ContextID string `json:"contextId"`
		Status    struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Task != nil {
		return result.Task.ID, result.Task.ContextID, result.Task.Status.State
	}
	return result.ID, result.ContextID, result.Status.State
}

func publishBrokerMessage(t *testing.T, bridgeProcess *testProcess, hubToken, userID, taskID, messageID, messageType, text string) {
	t.Helper()
	creds := grpcbroker.NewTokenSourceCredentials(&fixedTokenSource{token: hubToken}, false, grpcbroker.WithCloudRunHeader())
	connection, err := grpc.NewClient(strings.TrimPrefix(bridgeProcess.URL(), "http://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	metadata := map[string]string{"msgId": messageID}
	if taskID != "" {
		metadata["a2aTaskId"] = taskID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = brokerv1.NewBrokerServiceClient(connection).Publish(ctx, &brokerv1.PublishRequest{
		Topic: fmt.Sprintf("scion.project.proj1.user.%s.messages", userID),
		Message: &brokerv1.StructuredMessage{
			Version: 1, Sender: "agent:agent1", Recipient: "user:" + userID,
			Msg: text, Type: messageType, Timestamp: time.Now().UTC().Format(time.RFC3339), Metadata: metadata,
		},
	})
	if err != nil {
		t.Fatalf("BrokerService.Publish: %v", err)
	}
}

func newMessageParams(id, text, taskID, contextID string) map[string]any {
	message := map[string]any{
		"messageId": id, "role": "ROLE_USER", "parts": []map[string]any{{"text": text}},
	}
	if taskID != "" {
		message["taskId"] = taskID
	}
	if contextID != "" {
		message["contextId"] = contextID
	}
	return map[string]any{"message": message}
}

func TestTwoReplicaUserLifecycle(t *testing.T) {
	h := startHAFinalTopology(t)

	first := callRPCAsync(h.alternator.URL(), h.userToken, "SendMessage",
		newMessageParams("lifecycle-1", "first request", "", ""))
	stats := waitHubMessages(t, h, 1)
	publishBrokerMessage(t, h.bridgeB, h.hubToken, stats.LastUserID, "", "lifecycle-input-1", messages.TypeStateChange, "WAITING_FOR_INPUT")
	firstResult := awaitRPC(t, first)
	taskID, contextID, state := taskIdentity(t, firstResult.reply.Result)
	if taskID == "" || contextID == "" || state != "TASK_STATE_INPUT_REQUIRED" {
		t.Fatalf("first task id=%q context=%q state=%q raw=%s", taskID, contextID, state, firstResult.raw)
	}

	continued := callRPCAsync(h.bridgeB.URL(), h.userToken, "SendMessage",
		newMessageParams("lifecycle-2", "continue on replica B", taskID, contextID))
	stats = waitHubMessages(t, h, 2)
	publishBrokerMessage(t, h.bridgeA, h.hubToken, stats.LastUserID, taskID, "lifecycle-reply-2", messages.TypeAssistantReply, "continued reply")
	continuedResult := awaitRPC(t, continued)
	continuedID, _, continuedState := taskIdentity(t, continuedResult.reply.Result)
	if continuedID != taskID || continuedState != "TASK_STATE_COMPLETED" {
		t.Fatalf("continued id=%q state=%q want id=%q completed raw=%s", continuedID, continuedState, taskID, continuedResult.raw)
	}

	pending := callRPCAsync(h.bridgeA.URL(), h.userToken, "SendMessage",
		newMessageParams("cancel-1", "cancel me", "", ""))
	_ = pending
	_ = waitHubMessages(t, h, 3)
	list, _ := callRPC(t, h.bridgeB.URL(), h.userToken, "ListTasks", map[string]any{"pageSize": 100})
	var listed struct {
		Tasks []struct {
			ID     string `json:"id"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatal(err)
	}
	var cancelID string
	for _, task := range listed.Tasks {
		if task.ID != taskID && task.Status.State != "TASK_STATE_COMPLETED" {
			cancelID = task.ID
		}
	}
	if cancelID == "" {
		t.Fatalf("active cancel target not found: %s", list.Result)
	}
	canceled, _ := callRPC(t, h.bridgeB.URL(), h.userToken, "CancelTask", map[string]any{"id": cancelID})
	gotCancelID, _, canceledState := taskIdentity(t, canceled.Result)
	if gotCancelID != cancelID || canceledState != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel result id=%q state=%q raw=%s", gotCancelID, canceledState, canceled.Result)
	}

	attacker := fetchMintedToken(t, h.fakeGoogle.URL(), url.Values{"sub": {"attacker-subject"}, "email": {"attacker@gmail.com"}})
	wrongCaller, _ := callRPC(t, h.bridgeB.URL(), attacker, "GetTask", map[string]any{"id": taskID})
	if wrongCaller.Error == nil || strings.Contains(string(wrongCaller.Result), taskID) {
		t.Fatalf("wrong caller received task metadata: result=%s error=%+v", wrongCaller.Result, wrongCaller.Error)
	}
	for _, endpoint := range []string{
		h.bridgeB.URL() + "/projects/wrong/agents/agent1",
		h.bridgeB.URL() + "/projects/proj1/agents/wrong",
	} {
		status, body := postBearer(t, endpoint, h.userToken, `{"jsonrpc":"2.0","id":"privacy","method":"GetTask","params":{"id":"`+taskID+`"}}`)
		if status != http.StatusOK || strings.Contains(string(body), taskID) {
			t.Fatalf("route privacy failed endpoint=%s status=%d body=%s", endpoint, status, body)
		}
	}

	stats = getJSON[hubProcessStats](t, h.hub.URL()+"/__test/stats")
	if stats.Messages != 4 { // first, continue, pending, cancel interrupt
		t.Fatalf("Hub messages=%d want 4; duplicate or missing side effect", stats.Messages)
	}
}

type sseStream struct {
	response *http.Response
	events   <-chan sseWireEvent
}

type sseWireEvent struct {
	data       []byte
	eventType  string
	id         string
	receivedAt time.Time
}

func subscribeTask(t *testing.T, endpoint, token, taskID string) *sseStream {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "subscribe-" + taskID,
		"method": "SubscribeToTask", "params": map[string]any{"id": taskID},
	})
	request, err := http.NewRequest(http.MethodPost, endpoint+"/projects/proj1/agents/agent1", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan sseWireEvent, 8)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		var eventType, eventID string
		for scanner.Scan() {
			line := scanner.Bytes()
			if bytes.HasPrefix(line, []byte("event: ")) {
				eventType = string(bytes.TrimPrefix(line, []byte("event: ")))
				continue
			}
			if bytes.HasPrefix(line, []byte("id: ")) {
				eventID = string(bytes.TrimPrefix(line, []byte("id: ")))
				continue
			}
			if bytes.HasPrefix(line, []byte("data: ")) {
				events <- sseWireEvent{
					data:       append([]byte(nil), bytes.TrimPrefix(line, []byte("data: "))...),
					eventType:  eventType,
					id:         eventID,
					receivedAt: time.Now().UTC(),
				}
				eventType, eventID = "", ""
			}
		}
	}()
	return &sseStream{response: response, events: events}
}

func nextSSE(t *testing.T, stream *sseStream, timeout time.Duration) sseWireEvent {
	t.Helper()
	select {
	case event, ok := <-stream.events:
		if !ok {
			t.Fatal("SSE stream closed before event")
		}
		return event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for SSE event")
		return sseWireEvent{}
	}
}

func assertNoSSE(t *testing.T, stream *sseStream, wait time.Duration) {
	t.Helper()
	select {
	case event, ok := <-stream.events:
		if ok {
			t.Fatalf("unexpected replayed SSE event: %s", cursorSSEEvidence(event))
		}
	case <-time.After(wait):
	}
}

// Diagnostic output is intentionally allowlisted: no wire payload, history, owner,
// URL, error body, or arbitrary identifier can enter a CI log via these helpers.
func cursorUUID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil {
		return "[not-uuid]"
	}
	return id.String()
}

func cursorEventType(value string) string {
	switch value {
	case "", "message", "error", "status", "artifact":
		return value
	default:
		return "[other]"
	}
}

func cursorState(value string) string {
	switch value {
	case "", "working", "completed", "TASK_STATE_WORKING", "TASK_STATE_COMPLETED", "TASK_STATE_CANCELED", "TASK_STATE_FAILED":
		return value
	default:
		return "[other]"
	}
}

func cursorPayloadEvidence(payload []byte) string {
	var shape struct {
		ID     string `json:"id"`
		TaskID string `json:"taskId"`
		Result struct {
			Task *struct {
				ID      string            `json:"id"`
				History []json.RawMessage `json:"history"`
				Status  struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"task"`
			StatusUpdate *struct {
				TaskID string `json:"taskId"`
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"statusUpdate"`
			ArtifactUpdate *struct {
				TaskID string `json:"taskId"`
			} `json:"artifactUpdate"`
		} `json:"result"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		History  []json.RawMessage          `json:"history"`
		Artifact json.RawMessage            `json:"artifact"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	hash := sha256.Sum256(payload)
	if json.Unmarshal(payload, &shape) != nil {
		return fmt.Sprintf("bytes=%d sha256=%x json=invalid", len(payload), hash)
	}
	kind, taskID, state, historyLen := "other", shape.TaskID, shape.Status.State, 0
	switch {
	case shape.Result.Task != nil:
		kind, taskID, state, historyLen = "snapshot", shape.Result.Task.ID, shape.Result.Task.Status.State, len(shape.Result.Task.History)
	case shape.Result.StatusUpdate != nil:
		kind, taskID, state = "status-update", shape.Result.StatusUpdate.TaskID, shape.Result.StatusUpdate.Status.State
	case shape.Result.ArtifactUpdate != nil:
		kind, taskID = "artifact-update", shape.Result.ArtifactUpdate.TaskID
	case shape.ID != "" && shape.TaskID == "":
		kind, taskID, historyLen = "stored-snapshot", shape.ID, len(shape.History)
	case len(shape.Artifact) > 0:
		kind = "durable-artifact"
	case shape.TaskID != "":
		kind = "durable-status"
	}
	_, internalCursor := shape.Metadata["_bridgeEventID"]
	return fmt.Sprintf("bytes=%d sha256=%x kind=%s task_id=%s state=%s history_count=%d internal_cursor_present=%t artifact_present=%t",
		len(payload), hash, kind, cursorUUID(taskID), cursorState(state), historyLen, internalCursor, len(shape.Artifact) > 0)
}

func cursorSSEEvidence(event sseWireEvent) string {
	return fmt.Sprintf("received_at=%s event_type=%s sse_id=%s %s",
		event.receivedAt.UTC().Format(time.RFC3339Nano), cursorEventType(event.eventType), cursorUUID(event.id), cursorPayloadEvidence(event.data))
}

func TestCursorDiagnosticQuoteFree(t *testing.T) {
	for _, eventType := range []string{"", "message", "error", "status", "artifact", "secret\"canary"} {
		evidence := cursorSSEEvidence(sseWireEvent{
			eventType: eventType,
			id:        "550e8400-e29b-41d4-a716-446655440000",
			data:      []byte(`{"token":"secret-canary"}`),
		})
		if strings.ContainsAny(evidence, "\"\\\n\r") || strings.Contains(evidence, "secret") {
			t.Fatal("diagnostic contains unsafe characters or raw content")
		}
		if !strings.Contains(evidence, "sse_id=550e8400-e29b-41d4-a716-446655440000") || !strings.Contains(evidence, "sha256=") {
			t.Fatal("diagnostic lost correlation fields")
		}
	}
}

func TestCrossReplicaStreamCursor(t *testing.T) {
	h := startHAFinalTopology(t)
	var taskID string
	t.Cleanup(func() {
		if t.Failed() {
			if taskID != "" {
				logCursorFailureEvidence(t, taskID)
			}
			logs := []byte(h.topology.logs.String())
			t.Logf("topology logs withheld bytes=%d sha256=%x", len(logs), sha256.Sum256(logs))
		}
	})
	send := callRPCAsync(h.bridgeA.URL(), h.userToken, "SendMessage", newMessageParams("cursor-1", "stream me", "", ""))
	stats := waitHubMessages(t, h, 1)

	// Obtain the production task ID without direct store access.
	list, _ := callRPC(t, h.bridgeB.URL(), h.userToken, "ListTasks", map[string]any{"pageSize": 100})
	var listed struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil || len(listed.Tasks) != 1 {
		t.Fatalf("active task lookup: decode_error=%t tasks=%d %s", err != nil, len(listed.Tasks), cursorPayloadEvidence(list.Result))
	}
	taskID = listed.Tasks[0].ID

	publishBrokerMessage(t, h.bridgeB, h.hubToken, stats.LastUserID, taskID, "cursor-working", messages.TypeStateChange, "working")
	first := subscribeTask(t, h.bridgeA.URL(), h.userToken, taskID)
	firstClosed := false
	defer func() {
		if !firstClosed {
			_ = first.response.Body.Close()
		}
	}()
	firstSnapshot := nextSSE(t, first, 3*time.Second)
	if !bytes.Contains(firstSnapshot.data, []byte(taskID)) || bytes.Contains(firstSnapshot.data, []byte("_bridgeEventID")) {
		t.Fatalf("invalid first snapshot: %s", cursorSSEEvidence(firstSnapshot))
	}
	_ = first.response.Body.Close() // explicit client disconnect
	firstClosed = true

	h.bridgeB = h.restartBridge(t, h.bridgeB, "ha-bridge-b-restarted")
	reconnected := subscribeTask(t, h.bridgeB.URL(), h.userToken, taskID)
	defer reconnected.response.Body.Close()
	restartSnapshot := nextSSE(t, reconnected, 3*time.Second)
	if !bytes.Contains(restartSnapshot.data, []byte(taskID)) || bytes.Contains(restartSnapshot.data, []byte("_bridgeEventID")) {
		t.Fatalf("invalid restart snapshot: %s", cursorSSEEvidence(restartSnapshot))
	}
	if bytes.Contains(restartSnapshot.data, []byte("cursor-working")) {
		t.Fatalf("intermediate working event must be absent from snapshot payload/history: %s", cursorSSEEvidence(restartSnapshot))
	}

	// Verify durable intermediate event has event_id > last_event_cursor before streaming.
	storePre, err := bridgestate.NewPostgres(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	storePreClosed := false
	defer func() {
		if !storePreClosed {
			storePre.Close()
		}
	}()
	var snapCursor, workingEventID int64
	if err := storePre.DB().QueryRow(`SELECT last_event_cursor FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&snapCursor); err != nil {
		t.Fatal(err)
	}
	if err := storePre.DB().QueryRow(`SELECT id FROM a2a_task_events WHERE task_id=$1 AND dedup_key='cursor-working'`, taskID).Scan(&workingEventID); err != nil {
		t.Fatal(err)
	}
	storePre.Close()
	storePreClosed = true
	if !(workingEventID > snapCursor) {
		t.Fatalf("expected working event id (%d) > last_event_cursor (%d)", workingEventID, snapCursor)
	}

	// Per no-old-replay contract, the unreflected working event after last_event_cursor must stream.
	workingEvent := nextSSE(t, reconnected, 3*time.Second)
	if !bytes.Contains(workingEvent.data, []byte("TASK_STATE_WORKING")) || bytes.Contains(workingEvent.data, []byte("_bridgeEventID")) {
		t.Fatalf("invalid unreflected working event: %s", cursorSSEEvidence(workingEvent))
	}
	t.Logf("cursor evidence task_id=%s snapshot_cursor=%d working_event_id=%d first_snapshot={%s} restart_snapshot={%s} working_sse={%s}",
		cursorUUID(taskID), snapCursor, workingEventID, cursorSSEEvidence(firstSnapshot), cursorSSEEvidence(restartSnapshot), cursorSSEEvidence(workingEvent))
	assertNoSSE(t, reconnected, 250*time.Millisecond)

	publishBrokerMessage(t, h.bridgeB, h.hubToken, stats.LastUserID, taskID, "cursor-final", messages.TypeAssistantReply, "final once")
	publishBrokerMessage(t, h.bridgeB, h.hubToken, stats.LastUserID, taskID, "cursor-final", messages.TypeAssistantReply, "final once")
	for {
		ev := nextSSE(t, reconnected, 3*time.Second)
		if bytes.Contains(ev.data, []byte("_bridgeEventID")) {
			t.Fatalf("bridge event ID leaked on wire: %s", cursorSSEEvidence(ev))
		}
		if bytes.Contains(ev.data, []byte("TASK_STATE_COMPLETED")) {
			break
		}
	}
	assertNoSSE(t, reconnected, 250*time.Millisecond)
	result := awaitRPC(t, send)
	_, _, finalState := taskIdentity(t, result.reply.Result)
	if finalState != "TASK_STATE_COMPLETED" {
		t.Fatalf("send state=%s %s", cursorState(finalState), cursorPayloadEvidence(result.reply.Result))
	}

	store, err := bridgestate.NewPostgres(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var cursor, distinct, total int64
	if err := store.DB().QueryRow(`SELECT last_event_cursor FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(DISTINCT dedup_key), COUNT(*) FROM a2a_task_events WHERE task_id=$1`, taskID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if cursor <= 0 || distinct != total {
		t.Fatalf("cursor=%d distinct_dedup=%d total_events=%d", cursor, distinct, total)
	}
}

func logCursorFailureEvidence(t *testing.T, taskID string) {
	t.Helper()
	store, err := bridgestate.NewPostgres(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Log("cursor failure evidence unavailable: store open failed; error withheld")
		return
	}
	defer store.Close()
	var payload []byte
	var cursor int64
	var ownerPresent, execOwnerPresent bool
	var execHeartbeat *time.Time
	err = store.DB().QueryRow(`SELECT payload, last_event_cursor, owner_key <> '', COALESCE(exec_owner, '') <> '', exec_heartbeat
		FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&payload, &cursor, &ownerPresent, &execOwnerPresent, &execHeartbeat)
	if err != nil {
		t.Logf("cursor failure snapshot query task_id=%s: failed; error withheld", cursorUUID(taskID))
		return
	}
	t.Logf("cursor failure snapshot task_id=%s cursor=%d owner_present=%t exec_owner_present=%t exec_heartbeat=%v %s",
		cursorUUID(taskID), cursor, ownerPresent, execOwnerPresent, execHeartbeat, cursorPayloadEvidence(payload))
	rows, err := store.DB().Query(`SELECT id, kind, final, COALESCE(dedup_key, ''), created_at, payload
		FROM a2a_task_events WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Logf("cursor failure events query task_id=%s: failed; error withheld", cursorUUID(taskID))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var kind, dedupKey string
		var final bool
		var createdAt time.Time
		var eventPayload []byte
		if err := rows.Scan(&eventID, &kind, &final, &dedupKey, &createdAt, &eventPayload); err != nil {
			t.Logf("cursor failure event scan task_id=%s: failed; error withheld", cursorUUID(taskID))
			return
		}
		dedupClass := "other"
		switch {
		case dedupKey == "cursor-working":
			dedupClass = "cursor-working"
		case dedupKey == "cursor-final:message":
			dedupClass = "cursor-final:message"
		case strings.HasPrefix(dedupKey, "cursor-final:artifact:"):
			dedupClass = "cursor-final:artifact"
		}
		t.Logf("cursor failure event task_id=%s event_id=%d kind=%s final=%t dedup_class=%s created_at=%s %s",
			cursorUUID(taskID), eventID, cursorEventType(kind), final, dedupClass, createdAt.UTC().Format(time.RFC3339Nano), cursorPayloadEvidence(eventPayload))
	}
	if err := rows.Err(); err != nil {
		t.Logf("cursor failure event iteration task_id=%s: failed; error withheld", cursorUUID(taskID))
	}
}

func TestCrashLeaseBoundary(t *testing.T) {
	h := startHAFinalTopology(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Topology logs:\n%s", h.topology.logs.String())
		}
	})
	send := callRPCAsync(h.bridgeA.URL(), h.userToken, "SendMessage", newMessageParams("crash-1", "crash after send", "", ""))
	_ = send
	stats := waitHubMessages(t, h, 1)

	list, _ := callRPC(t, h.bridgeB.URL(), h.userToken, "ListTasks", map[string]any{"pageSize": 100})
	var listed struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil || len(listed.Tasks) != 1 {
		t.Fatalf("active task lookup: err=%v result=%s", err, list.Result)
	}
	taskID := listed.Tasks[0].ID

	h.topology.stopProcess(t, h.bridgeA)

	store, err := bridgestate.NewPostgres(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Await observable lease expiration eligibility rather than a fixed sleep.
	leaseDeadline := time.Now().Add(10 * time.Second)
	var leaseExpired bool
	for time.Now().Before(leaseDeadline) {
		err := store.DB().QueryRow(
			`SELECT exec_heartbeat < NOW() - interval '2 seconds' FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&leaseExpired)
		if err == nil && leaseExpired {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !leaseExpired {
		t.Fatalf("task %s lease did not become eligible for reaping within deadline", taskID)
	}

	h.bridgeA = h.topology.start(t, processSpec{Name: "ha-bridge-a-restarted", Mode: "ha-bridge", ReplicaID: "ha-bridge-a-restarted", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": h.fakeGoogle.URL(), "SCION_TEST_HUB_URL": h.hub.URL(),
		"SCION_TEST_CONTROL_AUDIENCE": haControlAudience,
	}})

	var state string
	var got rpcReply
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = callRPC(t, h.bridgeB.URL(), h.userToken, "GetTask", map[string]any{"id": taskID})
		if got.Error == nil {
			_, _, state = taskIdentity(t, got.Result)
			if state == "TASK_STATE_FAILED" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "TASK_STATE_FAILED" {
		t.Fatalf("crash-reaped task state=%q want failed result=%s", state, got.Result)
	}
	stream := subscribeTask(t, h.bridgeB.URL(), h.userToken, taskID)
	streamClosed := false
	defer func() {
		if !streamClosed {
			_ = stream.response.Body.Close()
		}
	}()
	terminal := nextSSE(t, stream, 3*time.Second)
	_ = stream.response.Body.Close()
	streamClosed = true
	if !bytes.Contains(terminal.data, []byte("TASK_STATE_FAILED")) {
		t.Fatalf("other replica did not observe durable failed terminal event: %s", terminal.data)
	}
	// Deterministically synchronize via post-terminal janitor/poll cycle across both replicas.
	// This proves a full maintenance/janitor pass has completed post-terminal before asserting
	// that no automatic replay was dispatched to the Hub.
	type janitorResult struct {
		Status      string `json:"status"`
		Replica     string `json:"replica"`
		ReapedCount int    `json:"reapedCount"`
		Timestamp   string `json:"timestamp"`
	}
	janitorA := getJSON[janitorResult](t, h.bridgeA.URL()+"/__test/janitor-cycle")
	if janitorA.Status != "ok" {
		t.Fatalf("bridge A janitor cycle failed: %+v", janitorA)
	}
	janitorB := getJSON[janitorResult](t, h.bridgeB.URL()+"/__test/janitor-cycle")
	if janitorB.Status != "ok" {
		t.Fatalf("bridge B janitor cycle failed: %+v", janitorB)
	}
	// Regression assertion making synchronization causal: the post-terminal janitor pass
	// must observe zero newly reaped active tasks (the crashed task was already terminal).
	if janitorA.ReapedCount != 0 {
		t.Fatalf("post-terminal janitor on bridge A reaped %d tasks, want 0", janitorA.ReapedCount)
	}
	if janitorB.ReapedCount != 0 {
		t.Fatalf("post-terminal janitor on bridge B reaped %d tasks, want 0", janitorB.ReapedCount)
	}
	// Verify that completing the janitor cycles caused zero additional messages to the Hub.
	if gotStats := getJSON[hubProcessStats](t, h.hub.URL()+"/__test/stats"); gotStats.Messages != 1 {
		t.Fatalf("crash triggered automatic Hub replay: messages=%d", gotStats.Messages)
	}

	// A retry is explicit and creates a new task/Hub send; success is never
	// fabricated for the possibly-completed pre-crash side effect.
	retry := callRPCAsync(h.bridgeA.URL(), h.userToken, "SendMessage", newMessageParams("crash-manual-retry", "manual retry", "", ""))
	stats = waitHubMessages(t, h, 2)
	publishBrokerMessage(t, h.bridgeB, h.hubToken, stats.LastUserID, "", "crash-retry-final", messages.TypeAssistantReply, "manual retry complete")
	retryResult := awaitRPC(t, retry)
	retryID, _, retryState := taskIdentity(t, retryResult.reply.Result)
	if retryID == taskID || retryState != "TASK_STATE_COMPLETED" {
		t.Fatalf("manual retry id=%q original=%q state=%q raw=%s", retryID, taskID, retryState, retryResult.raw)
	}
}
