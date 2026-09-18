package bridge

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// TestCrossReplicaCancelTerminatesOriginalWaiter is the production-path proof
// for cancel convergence. The test server configures SendMessage=15s and no
// notifier; waitForTaskEvent polls from 100ms with a documented 2s cap. Once
// cancellation is durably visible, 3s therefore covers one maximum polling
// interval plus scheduling margin without confusing request timeout with
// cancellation propagation.
func TestCrossReplicaCancelTerminatesOriginalWaiter(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	project := "cancel-boundary-" + randomSuffix()
	procA := startProductionServer(t, dbURL, project, "agent-cancel", "")
	procB := startProductionServer(t, dbURL, project, "agent-cancel", "")
	t.Logf("processes A=%d B=%d send_timeout=15s poll_initial=100ms poll_cap=2s propagation_bound=3s notifier=false",
		procA.pid, procB.pid)

	type sendResult struct {
		body []byte
		err  error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		payload := []byte(`{"jsonrpc":"2.0","id":"cancel-send","method":"SendMessage","params":{"message":{"messageId":"cancel-boundary-message","role":"ROLE_USER","parts":[{"text":"cancel me"}]}}}`)
		resp, err := http.Post(procA.URL(), "application/json", bytes.NewReader(payload))
		if err != nil {
			sendDone <- sendResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		sendDone <- sendResult{body: body, err: err}
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
		t.Fatal("original Hub send not captured")
	}

	// Cancellation authorization is enforced by the same authenticated
	// route/caller tuple as every other task operation. Denials must neither
	// expose the task ID nor mutate the task/event state.
	for name, headers := range map[string]map[string]string{
		"wrong caller":  {"X-Test-Caller-ID": "attacker"},
		"wrong project": {"X-Test-Project": "other-project"},
		"wrong agent":   {"X-Test-Agent": "other-agent"},
	} {
		t.Run(name, func(t *testing.T) {
			code, message := jsonRPCExpectErrorWithHeaders(t, procB.URL(), "CancelTask", map[string]any{"id": taskID}, headers)
			if code == 0 || bytes.Contains([]byte(message), []byte(taskID)) {
				t.Fatalf("cancel ownership leak code=%d message=%q", code, message)
			}
		})
	}

	canceled := jsonRPC(t, procB.URL(), "CancelTask", map[string]any{"id": taskID})
	if !bytes.Contains(canceled, []byte("TASK_STATE_CANCELED")) {
		t.Fatalf("cross-replica cancel failed: %s", canceled)
	}

	store := testPostgresTaskStore(t)
	var state, leaseOwner string
	var cursor int64
	if err := store.db.QueryRow(`SELECT payload->'status'->>'state', COALESCE(exec_owner, ''), last_event_cursor
		FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&state, &leaseOwner, &cursor); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	var hasFinal bool
	if err := store.db.QueryRow(`SELECT COUNT(*), COALESCE(BOOL_OR(final), false)
		FROM a2a_task_events WHERE task_id=$1`, taskID).Scan(&eventCount, &hasFinal); err != nil {
		t.Fatal(err)
	}
	t.Logf("durable task=%s state=%s lease_owner=%q cursor=%d event_count=%d has_final=%v",
		taskID, state, leaseOwner, cursor, eventCount, hasFinal)

	var got sendResult
	select {
	case got = <-sendDone:
	case <-time.After(3 * time.Second):
		t.Errorf("original waiter did not terminate within propagation bound (send timeout remains 15s)")
	}
	if got.err != nil {
		t.Errorf("original waiter error: %v", got.err)
	} else if got.body != nil && !bytes.Contains(got.body, []byte("TASK_STATE_CANCELED")) {
		t.Errorf("original waiter returned non-canceled result: %s", got.body)
	}
	if state != "TASK_STATE_CANCELED" {
		t.Errorf("durable SDK state=%q, want TASK_STATE_CANCELED", state)
	}
	if eventCount == 0 || !hasFinal {
		t.Errorf("cancel did not publish a durable final event: count=%d final=%v", eventCount, hasFinal)
	}

	var releasedOwner string
	if err := store.db.QueryRow(`SELECT COALESCE(exec_owner, '')
		FROM a2a_sdk_tasks WHERE id=$1`, taskID).Scan(&releasedOwner); err != nil {
		t.Fatal(err)
	}
	if releasedOwner != "" {
		t.Errorf("original execution lease not released after cancel convergence: %q", releasedOwner)
	}

	// A late agent reply after cancellation must be fenced by the terminal SDK
	// snapshot and must not add an event or revive/replay Hub execution.
	postBrokerMessage(t, procB.URL(), fmt.Sprintf("scion.project.%s.user.admin.messages", project), &messages.StructuredMessage{
		Sender: "agent:agent-cancel", Type: messages.TypeAssistantReply, Msg: "late reply after cancel",
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metadata: map[string]string{"msgId": "late-after-cancel", "a2aTaskId": taskID},
	})
	var afterLateCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM a2a_task_events WHERE task_id=$1`, taskID).Scan(&afterLateCount); err != nil {
		t.Fatal(err)
	}
	if afterLateCount != eventCount {
		t.Errorf("late event emitted after cancellation: before=%d after=%d", eventCount, afterLateCount)
	}

	// Cancellation is sent by replica B. Replica A must never replay the
	// original Hub send while its waiter converges.
	captures := getHubSends(t, procA.URL())
	if len(captures) != 1 {
		t.Errorf("original Hub send replayed: replica_a_captures=%d", len(captures))
	}
}

func TestCanceledUpdateAtomicRollbackAndOwnership(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	store := testPostgresTaskStore(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "cancel-rollback-" + suffix
	constraintName := "test_block_cancel_" + suffix
	dedupKey := "sdk-cancel:" + taskID
	ctx := ctxForRouteAndCaller("cancel-rollback-project-"+suffix, "cancel-agent", "cancel-owner")

	t.Cleanup(func() {
		store.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE a2a_task_events DROP CONSTRAINT IF EXISTS %s`, constraintName))
		store.db.ExecContext(ctx, `DELETE FROM a2a_task_events WHERE task_id=$1`, taskID)
		store.db.ExecContext(ctx, `DELETE FROM a2a_sdk_tasks WHERE id=$1`, taskID)
	})

	task := &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	version, err := store.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	leaseOwner := "cancel-rollback-owner:" + suffix
	claimed, err := store.ClaimExecution(ctx, taskID, leaseOwner, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	canceled := *task
	canceled.Status.State = a2a.TaskStateCanceled

	// A mismatched authenticated caller cannot transition the task or advance
	// its cursor before the atomic path is exercised.
	wrongCtx := ctxForRouteAndCaller("cancel-rollback-project-"+suffix, "cancel-agent", "attacker")
	if _, err := store.Update(wrongCtx, &taskstore.UpdateRequest{
		Task: &canceled, PrevVersion: version,
	}); err == nil {
		t.Fatal("wrong owner canceled task")
	}

	if _, err := store.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE a2a_task_events ADD CONSTRAINT %s
			CHECK (dedup_key IS NULL OR dedup_key != '%s')`, constraintName, dedupKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, &taskstore.UpdateRequest{
		Task: &canceled, PrevVersion: version,
	}); err == nil {
		t.Fatal("cancel update succeeded despite injected final-event failure")
	}

	var gotVersion, cursor int64
	var gotState, gotOwner string
	if err := store.db.QueryRowContext(ctx, `SELECT version, payload->'status'->>'state',
		COALESCE(exec_owner, ''), last_event_cursor FROM a2a_sdk_tasks WHERE id=$1`, taskID).
		Scan(&gotVersion, &gotState, &gotOwner, &cursor); err != nil {
		t.Fatal(err)
	}
	if gotVersion != int64(version) || gotState != "TASK_STATE_WORKING" || gotOwner != leaseOwner || cursor != 0 {
		t.Fatalf("cancel rollback failed: version=%d state=%s owner=%q cursor=%d", gotVersion, gotState, gotOwner, cursor)
	}
	var eventCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM a2a_task_events WHERE task_id=$1`, taskID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("cancel rollback left %d events", eventCount)
	}

	if _, err := store.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE a2a_task_events DROP CONSTRAINT %s`, constraintName)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, &taskstore.UpdateRequest{
		Task: &canceled, PrevVersion: version,
	}); err != nil {
		t.Fatal(err)
	}
	var final bool
	if err := store.db.QueryRowContext(ctx, `SELECT s.version, s.payload->'status'->>'state',
		COALESCE(s.exec_owner, ''), s.last_event_cursor, e.final
		FROM a2a_sdk_tasks s JOIN a2a_task_events e ON e.id=s.last_event_cursor
		WHERE s.id=$1 AND e.task_id=s.id AND e.dedup_key=$2`, taskID, dedupKey).
		Scan(&gotVersion, &gotState, &gotOwner, &cursor, &final); err != nil {
		t.Fatal(err)
	}
	if gotVersion != int64(version)+1 || gotState != "TASK_STATE_CANCELED" || gotOwner != "" || cursor == 0 || !final {
		t.Fatalf("atomic cancel incoherent: version=%d state=%s owner=%q cursor=%d final=%v",
			gotVersion, gotState, gotOwner, cursor, final)
	}
}
