package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestProductionLifecycleInputRequiredContinuationAndTerminal(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	project := "accepted-ha-input-" + randomSuffix()
	cleanupStore := testPostgresTaskStore(t)
	t.Cleanup(func() {
		cleanupStore.db.ExecContext(context.Background(), `DELETE FROM a2a_task_events
			WHERE task_id IN (SELECT id FROM a2a_sdk_tasks WHERE project_id=$1)`, project)
		cleanupStore.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE project_id=$1`, project)
	})
	procA := startProductionServer(t, dbURL, project, "agent-input", "")
	procB := startProductionServer(t, dbURL, project, "agent-input", "")
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "accepted-input", "method": "SendMessage",
			"params": map[string]any{"message": map[string]any{"messageId": "accepted-input-1", "role": "ROLE_USER", "parts": []map[string]any{{"text": "need input"}}}},
		})
		resp, err := http.Post(procA.URL(), "application/json", bytes.NewReader(payload))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{body: body, err: err}
	}()
	var taskID string
	deadline := time.Now().Add(5 * time.Second)
	for taskID == "" && time.Now().Before(deadline) {
		captures := getHubSends(t, procA.URL())
		if len(captures) > 0 {
			taskID = captures[0].TaskID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if taskID == "" {
		t.Fatal("no Hub send captured")
	}
	postBrokerMessage(t, procB.URL(), fmt.Sprintf("scion.project.%s.user.admin.messages", project), &messages.StructuredMessage{
		Sender: "agent:agent-input", Type: messages.TypeStateChange, Msg: "WAITING_FOR_INPUT",
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metadata: map[string]string{"msgId": "accepted-input-status", "a2aTaskId": taskID},
	})
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !bytes.Contains(got.body, []byte("TASK_STATE_INPUT_REQUIRED")) {
			t.Fatalf("response did not expose input-required: %s", got.body)
		}
	case <-time.After(1200 * time.Millisecond):
		t.Fatal("accepted HA kept blocking after durable WAITING_FOR_INPUT; caller cannot continue task")
	}

	// Continue the same nonterminal task through replica B. The subsequent
	// result must come from the new event, never the reflected input-required
	// event behind last_event_cursor.
	continued := make(chan result, 1)
	go func() {
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "accepted-continue", "method": "SendMessage",
			"params": map[string]any{"message": map[string]any{
				"messageId": "accepted-continue-1", "role": "ROLE_USER", "taskId": taskID,
				"parts": []map[string]any{{"text": "continued input"}},
			}},
		})
		resp, err := http.Post(procB.URL(), "application/json", bytes.NewReader(payload))
		if err != nil {
			continued <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		continued <- result{body: body, err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	postBrokerMessage(t, procA.URL(), fmt.Sprintf("scion.project.%s.user.admin.messages", project), &messages.StructuredMessage{
		Sender: "agent:agent-input", Type: messages.TypeAssistantReply, Msg: "continued terminal response",
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metadata: map[string]string{"msgId": "accepted-continue-final", "a2aTaskId": taskID},
	})
	continuedResult := <-continued
	if continuedResult.err != nil {
		t.Fatal(continuedResult.err)
	}
	if !bytes.Contains(continuedResult.body, []byte("TASK_STATE_COMPLETED")) ||
		!bytes.Contains(continuedResult.body, []byte("continued terminal response")) {
		t.Fatalf("continuation replayed old state or missed terminal response: %s", continuedResult.body)
	}

	// Correct owner can resubscribe to the terminal snapshot. Wrong caller,
	// project, and agent receive no task metadata.
	for name, headers := range map[string]map[string]string{
		"wrong caller":  {"X-Test-Caller-ID": "attacker"},
		"wrong project": {"X-Test-Project": "other-project"},
		"wrong agent":   {"X-Test-Agent": "other-agent"},
	} {
		t.Run(name, func(t *testing.T) {
			code, message := jsonRPCExpectErrorWithHeaders(t, procB.URL(), "GetTask", map[string]any{"id": taskID}, headers)
			if code == 0 || strings.Contains(message, taskID) {
				t.Fatalf("metadata leak code=%d message=%q", code, message)
			}
		})
	}
	terminal := jsonRPC(t, procB.URL(), "GetTask", map[string]any{"id": taskID})
	if !bytes.Contains(terminal, []byte("TASK_STATE_COMPLETED")) {
		t.Fatalf("terminal snapshot missing: %s", terminal)
	}
	stream := subscribeSSE(t, procB.URL(), taskID, nil)
	defer stream.Body.Close()
	events := readSSEEvents(t, stream.Body, 2, time.Second)
	if len(events) != 1 || !bytes.Contains(events[0], []byte("TASK_STATE_COMPLETED")) {
		t.Fatalf("terminal resubscribe replayed old events: %q", events)
	}

	// A separate active task can be canceled through the other replica and is
	// immediately visible as terminal through the production JSON-RPC route.
	cancelDone := make(chan result, 1)
	go func() {
		payload := []byte(`{"jsonrpc":"2.0","id":"cancel-send","method":"SendMessage","params":{"message":{"messageId":"cancel-message","role":"ROLE_USER","parts":[{"text":"cancel me"}]}}}`)
		resp, err := http.Post(procA.URL(), "application/json", bytes.NewReader(payload))
		if err != nil {
			cancelDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		cancelDone <- result{body: body, err: err}
	}()
	var cancelTaskID string
	cancelDeadline := time.Now().Add(5 * time.Second)
	for cancelTaskID == "" && time.Now().Before(cancelDeadline) {
		captures := getHubSends(t, procA.URL())
		if len(captures) >= 2 {
			cancelTaskID = captures[len(captures)-1].TaskID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cancelTaskID == "" {
		t.Fatal("cancel task Hub send not captured")
	}
	canceled := jsonRPC(t, procB.URL(), "CancelTask", map[string]any{"id": cancelTaskID})
	if !bytes.Contains(canceled, []byte("TASK_STATE_CANCELED")) {
		t.Fatalf("cross-replica cancel failed: %s", canceled)
	}
	_ = cancelDone
}

func TestContinuationWaitStartsAfterOwnedTaskCursor(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	suffix := randomSuffix()
	taskID := "accepted-cursor-" + suffix
	sdkStore := testPostgresTaskStore(t)
	stateStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	ctx := ctxForRouteAndCaller("accepted-cursor-project-"+suffix, "agent-cursor", "caller-cursor")
	_, err = sdkStore.Create(ctx, &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := sdkStore.ClaimExecution(ctx, taskID, OwnerID(), 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	oldID, err := stateStore.AppendTaskEvent(ctx, &state.TaskEvent{TaskID: taskID, Kind: "message", Payload: json.RawMessage(`{"old":true}`), DedupKey: "accepted-old-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdkStore.db.ExecContext(ctx, `UPDATE a2a_sdk_tasks SET last_event_cursor=$1 WHERE id=$2`, oldID, taskID); err != nil {
		t.Fatal(err)
	}
	newID, err := stateStore.AppendTaskEvent(ctx, &state.TaskEvent{TaskID: taskID, Kind: "message", Payload: json.RawMessage(`{"new":true}`), DedupKey: "accepted-new-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{store: stateStore, log: slog.Default(), config: &Config{}}
	event, err := b.waitForTaskEvent(ctx, taskID, time.Second, sdkStore)
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != newID {
		t.Fatalf("accepted HA replayed event id=%d behind durable cursor=%d; want subsequent id=%d", event.ID, oldID, newID)
	}
}

func TestContinuationCursorOwnershipDenials(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	suffix := randomSuffix()
	taskID := "accepted-owner-" + suffix
	sdkStore := testPostgresTaskStore(t)
	stateStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	ownerCtx := ctxForRouteAndCaller("owner-project-"+suffix, "owner-agent", "owner-user")
	_, err = sdkStore.Create(ownerCtx, &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := sdkStore.ClaimExecution(ownerCtx, taskID, OwnerID(), 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	_, err = stateStore.AppendTaskEvent(ownerCtx, &state.TaskEvent{TaskID: taskID, Kind: "message", Payload: json.RawMessage(`{"private":true}`), DedupKey: "private-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{store: stateStore, log: slog.Default(), config: &Config{}}
	for name, deniedCtx := range map[string]context.Context{
		"caller":        ctxForRouteAndCaller("owner-project-"+suffix, "owner-agent", "attacker"),
		"project":       ctxForRouteAndCaller("other-project", "owner-agent", "owner-user"),
		"agent":         ctxForRouteAndCaller("owner-project-"+suffix, "other-agent", "owner-user"),
		"missing route": context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			event, waitErr := b.waitForTaskEvent(deniedCtx, taskID, 100*time.Millisecond, sdkStore)
			if waitErr == nil || event != nil {
				t.Fatalf("unauthorized cursor read succeeded: event=%+v err=%v", event, waitErr)
			}
			if strings.Contains(waitErr.Error(), taskID) {
				t.Fatalf("task id leaked in denial: %v", waitErr)
			}
		})
	}
}
