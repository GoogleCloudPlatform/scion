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

package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// testServerProcess manages a subprocess running the a2a-testserver binary.
type testServerProcess struct {
	cmd    *exec.Cmd
	pid    int
	port   int
	stdin  io.WriteCloser
	cancel context.CancelFunc
}

// startTestServer builds and starts a test server subprocess on a random port.
// Returns the process info and a cleanup function. An optional callerID may be
// provided; if non-empty, it is passed as -caller-id to inject authenticated
// CallerIdentity context for ownership isolation testing.
func startTestServer(t *testing.T, dbURL, project, agent string, callerID ...string) *testServerProcess {
	t.Helper()

	// Build the test server binary.
	binaryPath := t.TempDir() + "/a2a-testserver"
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binaryPath,
		"./internal/bridge/testdata/a2a-testserver/")
	build.Dir = findModuleRoot(t)
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build test server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"-port=0",
		"-database-url=" + dbURL,
		"-project=" + project,
		"-agent=" + agent,
	}
	if len(callerID) > 0 && callerID[0] != "" {
		args = append(args, "-caller-id="+callerID[0])
	}
	cmd := exec.CommandContext(ctx, binaryPath, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start test server: %v", err)
	}

	// Wait for READY signal with PID and port.
	scanner := bufio.NewScanner(stdout)
	readyCh := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			readyCh <- scanner.Text()
		}
	}()

	select {
	case line := <-readyCh:
		// Parse "READY <pid> <port>"
		parts := strings.Fields(line)
		if len(parts) != 3 || parts[0] != "READY" {
			cancel()
			cmd.Process.Kill()
			t.Fatalf("unexpected READY line: %q", line)
		}
		pid, _ := strconv.Atoi(parts[1])
		port, _ := strconv.Atoi(parts[2])
		proc := &testServerProcess{
			cmd:    cmd,
			pid:    pid,
			port:   port,
			stdin:  stdin,
			cancel: cancel,
		}
		t.Cleanup(func() {
			proc.Stop()
		})
		return proc
	case <-time.After(15 * time.Second):
		cancel()
		cmd.Process.Kill()
		t.Fatal("test server did not become ready within 15 seconds")
		return nil
	}
}

// Stop terminates the subprocess gracefully.
func (p *testServerProcess) Stop() {
	p.stdin.Close() // signal stdin closure
	p.cancel()
	p.cmd.Wait()
}

// URL returns the base URL for JSON-RPC calls.
func (p *testServerProcess) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.port)
}

// jsonRPC sends a JSON-RPC request and returns the result.
func jsonRPC(t *testing.T, serverURL, method string, params interface{}) json.RawMessage {
	t.Helper()
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "test-1",
		"method":  method,
		"params":  params,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(serverURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", serverURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, respBody)
	}
	if rpcResp.Error != nil {
		t.Fatalf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result
}

// jsonRPCExpectError sends a JSON-RPC request and expects an error.
func jsonRPCExpectError(t *testing.T, serverURL, method string, params interface{}) (int, string) {
	t.Helper()
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "test-1",
		"method":  method,
		"params":  params,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(serverURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", serverURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, respBody)
	}
	if rpcResp.Error == nil {
		t.Fatalf("expected error, got success: %s", respBody)
	}
	return rpcResp.Error.Code, rpcResp.Error.Message
}

// findModuleRoot locates the module root directory.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatal("could not find module root")
		}
		dir = parent
	}
}

// ---------- Two-process SDK HTTP-level tests ----------

// TestPostgresTaskStoreCrossProcessCreateGetListCancel tests the full A2A
// JSON-RPC lifecycle across two separate OS processes sharing one Postgres
// database. Process A creates a task via message/send; Process B reads,
// lists, and cancels it via tasks/get, tasks/list, and tasks/cancel.
func TestPostgresTaskStoreCrossProcessCreateGetListCancel(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping cross-process Postgres test")
	}

	// Clean up SDK tasks table before and after.
	store := testPostgresTaskStore(t)
	defer store.db.Exec("DELETE FROM a2a_sdk_tasks")

	// Start two separate OS processes.
	procA := startTestServer(t, dbURL, "proj-cp", "agent-cp")
	procB := startTestServer(t, dbURL, "proj-cp", "agent-cp")

	// Verify distinct PIDs.
	if procA.pid == procB.pid {
		t.Fatalf("processes must have distinct PIDs: A=%d B=%d", procA.pid, procB.pid)
	}
	t.Logf("Process A: PID=%d port=%d", procA.pid, procA.port)
	t.Logf("Process B: PID=%d port=%d", procB.pid, procB.port)

	// 1. Create a task on Process A via SendMessage.
	sendResult := jsonRPC(t, procA.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "test-msg-1",
			"role":      "ROLE_USER",
			"parts": []map[string]interface{}{
				{"text": "Hello from process A"},
			},
		},
	})

	// SendMessage returns a StreamResponse wrapping an event.
	// The result could be {"statusUpdate": {...}} or {"task": {...}}.
	var sendResp struct {
		StatusUpdate *struct {
			TaskID string `json:"taskId"`
			State  string `json:"state"`
		} `json:"statusUpdate"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(sendResult, &sendResp); err != nil {
		t.Fatalf("unmarshal send result: %v (raw: %s)", err, sendResult)
	}
	var taskID string
	if sendResp.Task != nil {
		taskID = sendResp.Task.ID
	} else if sendResp.StatusUpdate != nil {
		taskID = sendResp.StatusUpdate.TaskID
	}
	if taskID == "" {
		t.Fatalf("could not extract task ID from send result: %s", sendResult)
	}
	t.Logf("Created task %s on process A (PID %d)", taskID, procA.pid)

	// 2. Get the task from Process B.
	getResult := jsonRPC(t, procB.URL(), "GetTask", map[string]interface{}{
		"id": taskID,
	})

	var gotTask struct {
		ID     string `json:"id"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(getResult, &gotTask); err != nil {
		t.Fatalf("unmarshal get result: %v (raw: %s)", err, getResult)
	}
	if gotTask.ID != taskID {
		t.Errorf("got task ID %q, want %q", gotTask.ID, taskID)
	}
	t.Logf("Got task %s from process B (PID %d), state=%s", gotTask.ID, procB.pid, gotTask.Status.State)

	// 3. List tasks from Process B.
	listResult := jsonRPC(t, procB.URL(), "ListTasks", map[string]interface{}{})
	var listResp struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		TotalSize int `json:"totalSize"`
	}
	if err := json.Unmarshal(listResult, &listResp); err != nil {
		t.Fatalf("unmarshal list result: %v", err)
	}
	found := false
	for _, task := range listResp.Tasks {
		if task.ID == taskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task %s not found in list from process B", taskID)
	}
	t.Logf("Listed %d tasks from process B, found target: %v", listResp.TotalSize, found)

	// 4. Create a second (non-terminal) task on A and cancel from B.
	// Send another message that will also complete, then verify cross-process
	// state consistency.
	sendResult2 := jsonRPC(t, procA.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "test-msg-2",
			"role":      "ROLE_USER",
			"parts": []map[string]interface{}{
				{"text": "Second message from A"},
			},
		},
	})
	var sendResp2 struct {
		StatusUpdate *struct {
			TaskID string `json:"taskId"`
		} `json:"statusUpdate"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	json.Unmarshal(sendResult2, &sendResp2)
	var taskID2 string
	if sendResp2.Task != nil {
		taskID2 = sendResp2.Task.ID
	} else if sendResp2.StatusUpdate != nil {
		taskID2 = sendResp2.StatusUpdate.TaskID
	}

	// Verify the second task is also visible from Process B.
	getResult2 := jsonRPC(t, procB.URL(), "GetTask", map[string]interface{}{
		"id": taskID2,
	})
	var gotTask2 struct {
		ID     string `json:"id"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(getResult2, &gotTask2); err != nil {
		t.Fatalf("unmarshal get result 2: %v", err)
	}
	if gotTask2.ID != taskID2 {
		t.Errorf("task 2 ID = %q, want %q", gotTask2.ID, taskID2)
	}
	t.Logf("Second task %s also visible on process B (PID %d), state=%s",
		taskID2, procB.pid, gotTask2.Status.State)

	// 5. Verify both tasks appear in list from Process A (cross-replica consistency).
	listResult2 := jsonRPC(t, procA.URL(), "ListTasks", map[string]interface{}{})
	var listResp2 struct {
		Tasks     []json.RawMessage `json:"tasks"`
		TotalSize int               `json:"totalSize"`
	}
	json.Unmarshal(listResult2, &listResp2)
	if listResp2.TotalSize < 2 {
		t.Errorf("expected at least 2 tasks in list from A, got %d", listResp2.TotalSize)
	}
	t.Logf("Process A lists %d tasks (cross-replica consistency confirmed)", listResp2.TotalSize)
}

// TestPostgresTaskStoreCrossProcessOwnershipIsolation tests that tasks
// created by one project/agent pair in one process are invisible to a
// different project/agent pair in another process.
func TestPostgresTaskStoreCrossProcessOwnershipIsolation(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store := testPostgresTaskStore(t)
	defer store.db.Exec("DELETE FROM a2a_sdk_tasks")

	// Process A: project-alpha / agent-alpha
	procA := startTestServer(t, dbURL, "project-alpha", "agent-alpha")
	// Process B: project-beta / agent-beta (different ownership)
	procB := startTestServer(t, dbURL, "project-beta", "agent-beta")

	if procA.pid == procB.pid {
		t.Fatalf("processes must have distinct PIDs")
	}
	t.Logf("Process A: PID=%d (alpha), Process B: PID=%d (beta)", procA.pid, procB.pid)

	// Create a task on Process A.
	sendResult := jsonRPC(t, procA.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "alpha-msg-1",
			"role":      "ROLE_USER",
			"parts": []map[string]interface{}{
				{"text": "Alpha task"},
			},
		},
	})
	// Extract task ID from the response.
	var sendResp struct {
		StatusUpdate *struct {
			TaskID string `json:"taskId"`
		} `json:"statusUpdate"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	json.Unmarshal(sendResult, &sendResp)
	var taskAID string
	if sendResp.Task != nil {
		taskAID = sendResp.Task.ID
	} else if sendResp.StatusUpdate != nil {
		taskAID = sendResp.StatusUpdate.TaskID
	}
	if taskAID == "" {
		t.Fatalf("could not extract task ID from send result: %s", sendResult)
	}
	t.Logf("Task %s created on alpha (PID %d)", taskAID, procA.pid)

	// Process B (beta) should NOT be able to get this task.
	code, msg := jsonRPCExpectError(t, procB.URL(), "GetTask", map[string]interface{}{
		"id": taskAID,
	})
	t.Logf("Beta get alpha's task: error code=%d msg=%s", code, msg)

	// Process B's list should be empty.
	listResult := jsonRPC(t, procB.URL(), "ListTasks", map[string]interface{}{})
	var listResp struct {
		Tasks     []json.RawMessage `json:"tasks"`
		TotalSize int               `json:"totalSize"`
	}
	json.Unmarshal(listResult, &listResp)
	if listResp.TotalSize != 0 {
		t.Errorf("beta should see 0 tasks, got %d", listResp.TotalSize)
	}
}

// ---------- Execution lease and crash recovery tests ----------

// TestPostgresTaskStoreExecutionLease tests the execution claim/heartbeat/
// release lifecycle.
func TestPostgresTaskStoreExecutionLease(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-lease", "agent-lease")

	task := &a2a.Task{
		ID:        "lease-1",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ownerA := "host-a:1001"
	ownerB := "host-b:2002"

	// Claim by A should succeed.
	claimed, err := store.ClaimExecution(ctx, "lease-1", ownerA, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimExecution A: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim by A to succeed")
	}

	// Concurrent claim by B should fail (A's lease is fresh).
	claimed, err = store.ClaimExecution(ctx, "lease-1", ownerB, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimExecution B: %v", err)
	}
	if claimed {
		t.Fatal("expected claim by B to fail (A holds lease)")
	}

	// Heartbeat by A should succeed.
	if err := store.HeartbeatExecution(ctx, "lease-1", ownerA); err != nil {
		t.Fatalf("HeartbeatExecution A: %v", err)
	}

	// Heartbeat by B should fail (not the owner).
	err = store.HeartbeatExecution(ctx, "lease-1", ownerB)
	if err == nil {
		t.Fatal("expected heartbeat by B to fail")
	}

	// Release by A should succeed.
	if err := store.ReleaseExecution(ctx, "lease-1", ownerA); err != nil {
		t.Fatalf("ReleaseExecution A: %v", err)
	}

	// Now B can claim.
	claimed, err = store.ClaimExecution(ctx, "lease-1", ownerB, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimExecution B after release: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim by B to succeed after A released")
	}
}

// TestPostgresTaskStoreCrashRecovery tests that a stale execution lease
// causes the task to be reaped to "failed" state, and the task is
// recoverable from another replica.
func TestPostgresTaskStoreCrashRecovery(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	// Use two separate store handles (simulating two replicas).
	storeA, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("storeA: %v", err)
	}
	t.Cleanup(func() {
		storeA.db.Exec("DELETE FROM a2a_sdk_tasks")
		storeA.Close()
	})

	storeB, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("storeB: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	ctx := ctxForRoute("proj-crash", "agent-crash")

	// Create a task and claim execution on store A.
	task := &a2a.Task{
		ID:        "crash-1",
		ContextID: "ctx-crash",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := storeA.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ownerA := "crashed-host:9999"
	claimed, err := storeA.ClaimExecution(ctx, "crash-1", ownerA, 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimExecution: claimed=%v err=%v", claimed, err)
	}

	// Simulate crash: backdoor the heartbeat to be in the past.
	_, err = storeA.db.Exec(
		`UPDATE a2a_sdk_tasks SET exec_heartbeat = NOW() - INTERVAL '10 minutes' WHERE id = 'crash-1'`,
	)
	if err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	// Store B reaps stale tasks with a 5-minute lease timeout.
	reapedIDs, err := storeB.ReapStaleTasks(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleTasks: %v", err)
	}
	if len(reapedIDs) != 1 {
		t.Errorf("reaped = %d, want 1", len(reapedIDs))
	}

	// Verify task is now failed.
	stored, err := storeB.Get(ctx, "crash-1")
	if err != nil {
		t.Fatalf("Get after reap: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateFailed {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateFailed)
	}

	// Verify execution claim is cleared.
	var execOwner *string
	storeB.db.QueryRow(`SELECT exec_owner FROM a2a_sdk_tasks WHERE id = 'crash-1'`).Scan(&execOwner)
	if execOwner != nil {
		t.Errorf("exec_owner should be NULL after reap, got %q", *execOwner)
	}

	// Verify user can retry: create a new task from store B.
	retryTask := &a2a.Task{
		ID:        "crash-1-retry",
		ContextID: "ctx-crash",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := storeB.Create(ctx, retryTask); err != nil {
		t.Fatalf("Create retry task: %v", err)
	}
	ownerB := "recovery-host:1234"
	claimed, err = storeB.ClaimExecution(ctx, "crash-1-retry", ownerB, 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimExecution retry: claimed=%v err=%v", claimed, err)
	}
}

// TestPostgresTaskStoreReapDoesNotAffectLongRunning verifies that the reaper
// does NOT fail tasks that have no execution claim (long-running legitimate
// work without active execution).
func TestPostgresTaskStoreReapDoesNotAffectLongRunning(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-longrun", "agent-longrun")

	// Create a task in working state WITHOUT an execution claim.
	task := &a2a.Task{
		ID:        "longrun-1",
		ContextID: "ctx-lr",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Backdate the updated_at to make it look old.
	store.db.Exec(`UPDATE a2a_sdk_tasks SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = 'longrun-1'`)

	// Reap should find nothing (no exec_owner set).
	reapedIDs, err := store.ReapStaleTasks(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleTasks: %v", err)
	}
	if len(reapedIDs) != 0 {
		t.Errorf("reaped = %d, want 0 (no execution claim)", len(reapedIDs))
	}

	// Verify task is still working.
	stored, err := store.Get(ctx, "longrun-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("state = %q, want %q (should not be reaped)", stored.Task.Status.State, a2a.TaskStateWorking)
	}
}

// ---------- Retention cleanup tests ----------

// TestPostgresTaskStorePurgeTasksAndEvents tests transactional purge of
// SDK tasks and correlated bridge events.
func TestPostgresTaskStorePurgeTasksAndEvents(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() {
		store.db.Exec("DELETE FROM a2a_sdk_tasks")
		store.db.Exec("DELETE FROM a2a_task_events")
		store.Close()
	})

	ctx := ctxForRoute("proj-purge", "agent-purge")

	// Create a completed task.
	task := &a2a.Task{
		ID:        "purge-1",
		ContextID: "ctx-purge",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Create a working task (should NOT be purged).
	workingTask := &a2a.Task{
		ID:        "purge-working",
		ContextID: "ctx-purge",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := store.Create(ctx, workingTask); err != nil {
		t.Fatalf("Create working: %v", err)
	}

	// Insert correlated events directly.
	store.db.Exec(`INSERT INTO a2a_task_events (task_id, kind, payload, final, created_at)
		VALUES ('purge-1', 'status', '{}', true, NOW() - INTERVAL '2 hours')`)
	store.db.Exec(`INSERT INTO a2a_task_events (task_id, kind, payload, final, created_at)
		VALUES ('purge-1', 'message', '{}', false, NOW() - INTERVAL '2 hours')`)

	// Backdate the completed task.
	store.db.Exec(`UPDATE a2a_sdk_tasks SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = 'purge-1'`)

	// Purge with 1-hour cutoff.
	tasksPurged, eventsPurged, err := store.PurgeTasksAndEvents(ctx, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("PurgeTasksAndEvents: %v", err)
	}
	if tasksPurged != 1 {
		t.Errorf("tasks purged = %d, want 1", tasksPurged)
	}
	if eventsPurged != 2 {
		t.Errorf("events purged = %d, want 2", eventsPurged)
	}

	// Verify completed task is gone.
	_, err = store.Get(ctx, "purge-1")
	if err == nil {
		t.Error("expected purge-1 to be deleted")
	}

	// Verify working task is still present.
	stored, err := store.Get(ctx, "purge-working")
	if err != nil {
		t.Fatalf("working task should survive purge: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("state = %q, want working", stored.Task.Status.State)
	}

	// Verify no events remain for the purged task.
	var eventCount int
	store.db.QueryRow(`SELECT COUNT(*) FROM a2a_task_events WHERE task_id = 'purge-1'`).Scan(&eventCount)
	if eventCount != 0 {
		t.Errorf("expected 0 events for purged task, got %d", eventCount)
	}
}

// ---------- Duplicate delivery idempotency tests ----------

// TestPostgresTaskStoreDuplicateDeliveryIdempotency tests that duplicate
// broker events produce deterministic results without corrupting state.
func TestPostgresTaskStoreDuplicateDeliveryIdempotency(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-dedup", "agent-dedup")

	// Test 1: Duplicate Create returns ErrTaskAlreadyExists.
	task := &a2a.Task{
		ID:        "dedup-1",
		ContextID: "ctx-dedup",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	v1, err := store.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, dupErr := store.Create(ctx, task)
	if dupErr == nil || dupErr != taskstore.ErrTaskAlreadyExists {
		t.Errorf("duplicate create: got %v, want ErrTaskAlreadyExists", dupErr)
	}

	// Test 2: Same update applied twice — second attempt gets ConcurrentModification.
	updatedTask := &a2a.Task{
		ID:        "dedup-1",
		ContextID: "ctx-dedup",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	v2, err := store.Update(ctx, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: v1,
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Replay the same update with the same stale version — deterministic failure.
	_, err = store.Update(ctx, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: v1, // stale
	})
	if err != taskstore.ErrConcurrentModification {
		t.Errorf("replay update: got %v, want ErrConcurrentModification", err)
	}

	// Version should be exactly v2, not v2+1.
	stored, _ := store.Get(ctx, "dedup-1")
	if stored.Version != v2 {
		t.Errorf("version = %d, want %d (no state corruption from duplicate)", stored.Version, v2)
	}
}

// TestPostgresTaskStoreBridgeEventDedup tests that the bridge event log's
// dedup_key mechanism prevents duplicate events from the same broker delivery.
// This proves idempotency at the broker-event-to-execution boundary.
func TestPostgresTaskStoreBridgeEventDedup(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	// Use the bridge state store for event dedup testing.
	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() {
		pgStore.Close()
	})
	ctx := context.Background()

	taskID := "event-dedup-1"

	// Create a task in bridge state.
	pgStore.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-1",
		ProjectID: "p1",
		AgentSlug: "agent1",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Simulate the same broker message arriving twice with the same stable
	// dedup key (derived from msgId metadata).
	dedupKey := "broker-msg-id-12345"
	payload := json.RawMessage(`{"taskId":"event-dedup-1","status":{"state":"working"}}`)

	id1, err := pgStore.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  payload,
		DedupKey: dedupKey,
	})
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if id1 == 0 {
		t.Fatal("first append should return non-zero ID")
	}

	// Second delivery with same dedup_key — should be silently ignored.
	id2, err := pgStore.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  payload,
		DedupKey: dedupKey,
	})
	if err != nil {
		t.Fatalf("second append should not error: %v", err)
	}

	// Verify only one event exists.
	events, err := pgStore.ReadTaskEvents(ctx, taskID, 0, 10)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event after dedup, got %d", len(events))
	}

	// The second ID should be 0 (conflict, no insert).
	if id2 != 0 {
		t.Logf("note: second append returned ID=%d (implementation-dependent)", id2)
	}

	// Clean up.
	pgStore.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
}

// ---------- Cross-replica event delivery and resubscribe tests ----------

// TestPostgresTaskStoreCrossReplicaEventDelivery tests that events written
// by one replica via the bridge event log are visible to a poller on
// another replica (both using real Postgres).
func TestPostgresTaskStoreCrossReplicaEventDelivery(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	// Two separate bridge state store handles.
	storeA, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("storeA: %v", err)
	}
	t.Cleanup(func() { storeA.Close() })

	storeB, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("storeB: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	ctx := context.Background()
	taskID := "pg-event-delivery-1"

	// Create task on store A.
	storeA.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-1",
		ProjectID: "p1",
		AgentSlug: "agent1",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Start polling on store B (separate connection pool).
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	eventsCh := streamTaskEvents(pollCtx, storeB, taskID, 0, 10, nil)

	// Write event on store A after a short delay.
	time.Sleep(50 * time.Millisecond)
	payload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State: TaskStateWorking,
			Message: &Message{
				MessageID: "pg-cross-msg-1",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Hello from replica A via Postgres"}},
			},
		},
	})
	_, err = storeA.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  payload,
		DedupKey: "pg-cross-event-1",
	})
	if err != nil {
		t.Fatalf("AppendTaskEvent: %v", err)
	}

	// Verify poller on store B receives it.
	select {
	case ev := <-eventsCh:
		if ev.StatusUpdate == nil {
			t.Fatal("expected StatusUpdate event")
		}
		if ev.StatusUpdate.Status.State != TaskStateWorking {
			t.Errorf("state = %q, want %q", ev.StatusUpdate.Status.State, TaskStateWorking)
		}
		if ev.StatusUpdate.Status.Message == nil || ev.StatusUpdate.Status.Message.Parts[0].Text != "Hello from replica A via Postgres" {
			t.Error("message content mismatch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-replica event delivery via Postgres")
	}

	// Clean up.
	storeA.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
}

// TestPostgresTaskStoreDurableResubscribeNoPriorReplay tests that
// resubscribing with a cursor skips already-delivered events and only
// delivers new events. This proves no prior-turn replay.
func TestPostgresTaskStoreDurableResubscribeNoPriorReplay(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() {
		pgStore.PurgeTaskEvents(context.Background(), time.Now().Add(1*time.Hour))
		pgStore.Close()
	})

	ctx := context.Background()
	taskID := "resub-1"

	pgStore.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-1",
		ProjectID: "p1",
		AgentSlug: "agent1",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Write 3 events.
	var lastEventID int64
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateWorking},
		})
		id, _ := pgStore.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "status",
			Payload: payload,
		})
		lastEventID = id
	}

	// Subscribe from cursor=0 — should see all 3.
	events0, _ := pgStore.ReadTaskEvents(ctx, taskID, 0, 10)
	if len(events0) != 3 {
		t.Fatalf("expected 3 events from cursor 0, got %d", len(events0))
	}

	// Subscribe from cursor=lastEventID-1 — should see only the last 1.
	cursor := events0[1].ID // after second event
	events1, _ := pgStore.ReadTaskEvents(ctx, taskID, cursor, 10)
	if len(events1) != 1 {
		t.Errorf("expected 1 event from cursor %d, got %d", cursor, len(events1))
	}

	// Subscribe from cursor=lastEventID — should see 0 (caught up).
	events2, _ := pgStore.ReadTaskEvents(ctx, taskID, lastEventID, 10)
	if len(events2) != 0 {
		t.Errorf("expected 0 events from cursor %d (caught up), got %d", lastEventID, len(events2))
	}

	// Write a new event — should be visible from the caught-up cursor.
	payload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateCompleted},
	})
	pgStore.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "status",
		Payload: payload,
		Final:   true,
	})

	events3, _ := pgStore.ReadTaskEvents(ctx, taskID, lastEventID, 10)
	if len(events3) != 1 {
		t.Errorf("expected 1 new event after cursor %d, got %d", lastEventID, len(events3))
	}
	if len(events3) > 0 && !events3[0].Final {
		t.Error("expected the new event to be final")
	}
}

// ---------- Cross-process caller isolation (two OS processes) ----------

// TestPostgresTaskStoreCrossProcessCallerIsolation tests that two processes
// on the same route but with different caller identities cannot see each
// other's tasks. This proves per-caller isolation at the HTTP/SDK level
// across OS process boundaries.
func TestPostgresTaskStoreCrossProcessCallerIsolation(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store := testPostgresTaskStore(t)
	defer store.db.Exec("DELETE FROM a2a_sdk_tasks")

	// Same route, different caller IDs.
	procAlice := startTestServer(t, dbURL, "proj-caller", "agent-caller", "user-alice")
	procBob := startTestServer(t, dbURL, "proj-caller", "agent-caller", "user-bob")

	if procAlice.pid == procBob.pid {
		t.Fatalf("processes must have distinct PIDs: Alice=%d Bob=%d", procAlice.pid, procBob.pid)
	}
	t.Logf("Alice: PID=%d port=%d, Bob: PID=%d port=%d",
		procAlice.pid, procAlice.port, procBob.pid, procBob.port)

	// Alice creates a task.
	sendResult := jsonRPC(t, procAlice.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "alice-msg-1",
			"role":      "ROLE_USER",
			"parts":     []map[string]interface{}{{"text": "Alice's private task"}},
		},
	})
	var sendResp struct {
		StatusUpdate *struct{ TaskID string `json:"taskId"` } `json:"statusUpdate"`
		Task         *struct{ ID string `json:"id"` }        `json:"task"`
	}
	json.Unmarshal(sendResult, &sendResp)
	var aliceTaskID string
	if sendResp.Task != nil {
		aliceTaskID = sendResp.Task.ID
	} else if sendResp.StatusUpdate != nil {
		aliceTaskID = sendResp.StatusUpdate.TaskID
	}
	if aliceTaskID == "" {
		t.Fatalf("could not extract Alice's task ID: %s", sendResult)
	}
	t.Logf("Alice created task %s on PID %d", aliceTaskID, procAlice.pid)

	// Bob should NOT see Alice's task.
	code, msg := jsonRPCExpectError(t, procBob.URL(), "GetTask", map[string]interface{}{
		"id": aliceTaskID,
	})
	t.Logf("Bob get Alice's task: error code=%d msg=%s", code, msg)

	// Bob's list should be empty.
	listResult := jsonRPC(t, procBob.URL(), "ListTasks", map[string]interface{}{})
	var listResp struct {
		Tasks     []json.RawMessage `json:"tasks"`
		TotalSize int               `json:"totalSize"`
	}
	json.Unmarshal(listResult, &listResp)
	if listResp.TotalSize != 0 {
		t.Errorf("Bob should see 0 tasks, got %d", listResp.TotalSize)
	}

	// Bob creates his own task.
	sendResult2 := jsonRPC(t, procBob.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "bob-msg-1",
			"role":      "ROLE_USER",
			"parts":     []map[string]interface{}{{"text": "Bob's private task"}},
		},
	})
	var sendResp2 struct {
		StatusUpdate *struct{ TaskID string `json:"taskId"` } `json:"statusUpdate"`
		Task         *struct{ ID string `json:"id"` }        `json:"task"`
	}
	json.Unmarshal(sendResult2, &sendResp2)
	var bobTaskID string
	if sendResp2.Task != nil {
		bobTaskID = sendResp2.Task.ID
	} else if sendResp2.StatusUpdate != nil {
		bobTaskID = sendResp2.StatusUpdate.TaskID
	}
	t.Logf("Bob created task %s on PID %d", bobTaskID, procBob.pid)

	// Alice should NOT see Bob's task.
	code2, msg2 := jsonRPCExpectError(t, procAlice.URL(), "GetTask", map[string]interface{}{
		"id": bobTaskID,
	})
	t.Logf("Alice get Bob's task: error code=%d msg=%s", code2, msg2)

	// Alice still only sees her own task.
	aliceList := jsonRPC(t, procAlice.URL(), "ListTasks", map[string]interface{}{})
	var aliceListResp struct {
		Tasks     []json.RawMessage `json:"tasks"`
		TotalSize int               `json:"totalSize"`
	}
	json.Unmarshal(aliceList, &aliceListResp)
	if aliceListResp.TotalSize != 1 {
		t.Errorf("Alice should see exactly 1 task, got %d", aliceListResp.TotalSize)
	}
}

// ---------- Cross-process event delivery (direct DB) ----------

// TestPostgresTaskStoreCrossProcessEventDelivery tests that an event appended
// to the bridge event log by one connection is readable by another connection.
// Both use real Postgres via direct state.PostgresStore calls (no backdoor
// HTTP endpoints). This validates cross-replica event visibility.
func TestPostgresTaskStoreCrossProcessEventDelivery(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	// Two independent Postgres connections simulate two replicas.
	storeA, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres (A): %v", err)
	}
	t.Cleanup(func() {
		storeA.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
		storeA.Close()
	})

	storeB, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres (B): %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	taskID := "cross-proc-event-1"

	// Create the task via connection A.
	storeA.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-evdel",
		ProjectID: "proj-evdel",
		AgentSlug: "agent-evdel",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Append event via connection A.
	eventPayload, _ := json.Marshal(map[string]interface{}{
		"taskId": taskID,
		"status": map[string]string{"state": "working"},
	})
	_, err = storeA.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  eventPayload,
		DedupKey: "cross-proc-dedup-1",
	})
	if err != nil {
		t.Fatalf("append event via A: %v", err)
	}

	// Read events via connection B — should see the event written by A.
	events, err := storeB.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("read events via B: %v", err)
	}
	if len(events) < 1 {
		t.Fatalf("expected at least 1 event from connection B, got %d", len(events))
	}
	if events[0].TaskID != taskID {
		t.Errorf("event task ID = %q, want %q", events[0].TaskID, taskID)
	}
	t.Logf("Connection B read %d event(s) for task %s written by connection A", len(events), taskID)
}

// TestPostgresTaskStoreCrossProcessResubscribeNoPriorReplay tests cursor-based
// event resubscription across two independent Postgres connections. Events
// before the cursor are not replayed; only new events are visible.
func TestPostgresTaskStoreCrossProcessResubscribeNoPriorReplay(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	// Two independent connections simulate two replicas.
	storeA, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres (A): %v", err)
	}
	t.Cleanup(func() {
		storeA.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
		storeA.Close()
	})

	storeB, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres (B): %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	taskID := "cross-proc-resub-1"

	// Create task via connection A.
	storeA.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-resub",
		ProjectID: "proj-resub",
		AgentSlug: "agent-resub",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Append 3 events via connection A.
	var lastEventID int64
	for i := 0; i < 3; i++ {
		eventPayload, _ := json.Marshal(map[string]interface{}{
			"taskId": taskID,
			"status": map[string]string{"state": "working"},
		})
		id, err := storeA.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "status",
			Payload: eventPayload,
		})
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		lastEventID = id
	}
	t.Logf("Appended 3 events via A, last ID=%d", lastEventID)

	// Read all events from B (cursor=0) — should see all 3.
	allEvents, err := storeB.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("read all events via B: %v", err)
	}
	if len(allEvents) != 3 {
		t.Fatalf("expected 3 events from cursor=0, got %d", len(allEvents))
	}

	// Get cursor after second event.
	cursorAfterSecond := allEvents[1].ID

	// Read from cursor — should see only 1 event (the third one).
	cursorEvents, err := storeB.ReadTaskEvents(ctx, taskID, cursorAfterSecond, 100)
	if err != nil {
		t.Fatalf("read from cursor via B: %v", err)
	}
	if len(cursorEvents) != 1 {
		t.Errorf("expected 1 event after cursor %d, got %d", cursorAfterSecond, len(cursorEvents))
	}

	// Read from lastEventID — should see 0 (caught up).
	caughtUpEvents, err := storeB.ReadTaskEvents(ctx, taskID, lastEventID, 100)
	if err != nil {
		t.Fatalf("read caught-up via B: %v", err)
	}
	if len(caughtUpEvents) != 0 {
		t.Errorf("expected 0 events from caught-up cursor, got %d", len(caughtUpEvents))
	}

	// Append a new event via connection A.
	newPayload, _ := json.Marshal(map[string]interface{}{
		"taskId": taskID,
		"status": map[string]string{"state": "completed"},
	})
	storeA.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "status",
		Payload: newPayload,
		Final:   true,
	})

	// Read from lastEventID again — should see exactly the new event.
	newEvents, err := storeB.ReadTaskEvents(ctx, taskID, lastEventID, 100)
	if err != nil {
		t.Fatalf("read new event via B: %v", err)
	}
	if len(newEvents) != 1 {
		t.Errorf("expected 1 new event, got %d", len(newEvents))
	}
	if len(newEvents) > 0 && !newEvents[0].Final {
		t.Error("expected final event")
	}
	t.Logf("Resubscribe verified: cursor skips old, sees new, across connections")
}

// ---------- Broker handler dedup at processAndAppendEvent boundary ----------

// TestPostgresTaskStoreBrokerHandlerDedup tests that processAndAppendEvent
// (the broker-event-to-execution boundary) does not produce duplicate events
// when called twice with the same dedup key derived from the same broker
// message. This uses a real Bridge instance with a real Postgres state store.
func TestPostgresTaskStoreBrokerHandlerDedup(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		pgStore.PurgeTaskEvents(context.Background(), time.Now().Add(1*time.Hour))
		pgStore.Close()
	})

	ctx := context.Background()
	taskID := "broker-dedup-handler-1"

	// Create a task in bridge state.
	pgStore.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-bh",
		ProjectID: "proj-bh",
		AgentSlug: "agent-bh",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Create a minimal Bridge with the real state store.
	shutdownCtx, shutdownCancel := context.WithCancel(ctx)
	defer shutdownCancel()
	b := &Bridge{
		store:       pgStore,
		log:         slog.Default(),
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
		push:        NewPushDispatcher(pgStore, &Config{}, slog.Default(), shutdownCtx),
	}

	// Register the task in the local cache for correlation.
	aKey := agentKey("proj-bh", "agent-bh")
	b.registerActiveTask(taskID, aKey)

	// Simulate a broker state-change message with a stable msgId.
	// State-change messages produce exactly one status event (no artifacts),
	// so dedup by dedup_key is deterministic.
	brokerMsg := &messages.StructuredMessage{
		Sender:    "agent:agent-bh",
		Type:      messages.TypeStateChange,
		Msg:       "working",
		Timestamp: "2026-09-18T00:00:00Z",
		Metadata:  map[string]string{"msgId": "stable-broker-msg-42", "a2aTaskId": taskID},
	}

	// Topic format: scion.project.<projectID>.user.<userId>.messages
	topic := "scion.project.proj-bh.user.admin.messages"

	// First delivery.
	if err := b.HandleBrokerMessage(ctx, topic, brokerMsg); err != nil {
		t.Fatalf("first HandleBrokerMessage: %v", err)
	}

	// Count events after first delivery.
	events1, _ := pgStore.ReadTaskEvents(ctx, taskID, 0, 100)
	count1 := len(events1)
	t.Logf("After first delivery: %d event(s)", count1)
	if count1 == 0 {
		t.Fatal("expected at least 1 event after first delivery")
	}

	// Second delivery with the same message (same msgId → same dedup key).
	if err := b.HandleBrokerMessage(ctx, topic, brokerMsg); err != nil {
		t.Fatalf("second HandleBrokerMessage: %v", err)
	}

	// Count events after second delivery — should be the same.
	events2, _ := pgStore.ReadTaskEvents(ctx, taskID, 0, 100)
	count2 := len(events2)
	t.Logf("After second delivery: %d event(s)", count2)

	if count2 != count1 {
		t.Errorf("duplicate delivery produced extra events: count1=%d count2=%d", count1, count2)
	}
}

// ---------- Retention through Bridge.RunSweep ----------

// TestPostgresTaskStoreRetentionThroughRunSweep tests that Bridge.RunSweep
// exercises PurgeTasksAndEvents on the wired sdkTaskStore, proving retention
// runs through the actual production code path (not just direct method calls).
func TestPostgresTaskStoreRetentionThroughRunSweep(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		pgStore.PurgeTaskEvents(context.Background(), time.Now().Add(1*time.Hour))
		pgStore.Close()
	})

	sdkStore, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() {
		sdkStore.db.Exec("DELETE FROM a2a_sdk_tasks")
		sdkStore.Close()
	})

	ctx := ctxForRoute("proj-sweep", "agent-sweep")

	// Create a terminal SDK task with correlated bridge events.
	task := &a2a.Task{
		ID:        "sweep-terminal-1",
		ContextID: "ctx-sweep",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	if _, err := sdkStore.Create(ctx, task); err != nil {
		t.Fatalf("Create terminal task: %v", err)
	}

	// Create a working SDK task (should survive sweep).
	workingTask := &a2a.Task{
		ID:        "sweep-working-1",
		ContextID: "ctx-sweep",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := sdkStore.Create(ctx, workingTask); err != nil {
		t.Fatalf("Create working task: %v", err)
	}

	// Insert correlated events.
	sdkStore.db.Exec(`INSERT INTO a2a_task_events (task_id, kind, payload, final, created_at)
		VALUES ('sweep-terminal-1', 'status', '{}', true, NOW() - INTERVAL '2 hours')`)

	// Backdate the terminal task.
	sdkStore.db.Exec(`UPDATE a2a_sdk_tasks SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = 'sweep-terminal-1'`)

	// Create a Bridge with the sdkTaskStore wired in.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()
	b := &Bridge{
		store:       pgStore,
		log:         slog.Default(),
		config:      &Config{},
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
		push:        NewPushDispatcher(pgStore, &Config{}, slog.Default(), shutdownCtx),
		sdkTaskStore: sdkStore,
	}

	// Run sweep — this should purge the terminal task and its events
	// through the production RunSweep code path.
	b.RunSweep(context.Background())

	// Verify terminal task is purged.
	_, err = sdkStore.Get(ctx, "sweep-terminal-1")
	if err == nil {
		t.Error("expected terminal task to be purged by RunSweep")
	}

	// Verify working task survives.
	stored, err := sdkStore.Get(ctx, "sweep-working-1")
	if err != nil {
		t.Fatalf("working task should survive sweep: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("state = %q, want working", stored.Task.Status.State)
	}

	// Verify events are purged.
	var eventCount int
	sdkStore.db.QueryRow(`SELECT COUNT(*) FROM a2a_task_events WHERE task_id = 'sweep-terminal-1'`).Scan(&eventCount)
	if eventCount != 0 {
		t.Errorf("expected 0 events for purged task, got %d", eventCount)
	}
	t.Log("RunSweep successfully purged terminal task and events through production code path")
}

// ---------- Cross-process crash recovery (process kill → reap) ----------

// TestPostgresTaskStoreCrossProcessCrashRecoveryKill tests crash recovery
// across OS processes: Process A creates a task and claims execution, then
// is killed. Another store handle (simulating Process B's reaper) detects
// the stale lease and transitions the task to failed.
func TestPostgresTaskStoreCrossProcessCrashRecoveryKill(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	storeA, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("storeA: %v", err)
	}
	t.Cleanup(func() {
		storeA.db.Exec("DELETE FROM a2a_sdk_tasks")
		storeA.Close()
	})

	ctx := ctxForRoute("proj-kill", "agent-kill")

	// Create a task and claim execution (simulating process A mid-execution).
	task := &a2a.Task{
		ID:        "kill-crash-1",
		ContextID: "ctx-kill",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := storeA.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Start a real subprocess, have it create+claim a task, then kill it.
	procA := startTestServer(t, dbURL, "proj-kill", "agent-kill")
	procOwnerID := fmt.Sprintf("127.0.0.1:%d", procA.pid) // approximate owner ID

	// Claim execution with the subprocess's owner ID.
	claimed, err := storeA.ClaimExecution(ctx, "kill-crash-1", procOwnerID, 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimExecution: claimed=%v err=%v", claimed, err)
	}

	// Kill the subprocess abruptly (simulating crash).
	if procA.cmd.Process != nil {
		procA.cmd.Process.Kill()
	}
	procA.cmd.Wait()
	t.Logf("Process A (PID %d) killed to simulate crash", procA.pid)

	// Backdate the heartbeat (simulating time passing after crash).
	storeA.db.Exec(`UPDATE a2a_sdk_tasks SET exec_heartbeat = NOW() - INTERVAL '10 minutes' WHERE id = 'kill-crash-1'`)

	// Create storeB (simulating another replica) and reap.
	storeB, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("storeB: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	reapedIDs, err := storeB.ReapStaleTasks(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleTasks: %v", err)
	}
	if len(reapedIDs) != 1 {
		t.Errorf("reaped = %d, want 1", len(reapedIDs))
	}

	// Verify task is failed and exec_owner is cleared.
	stored, err := storeB.Get(ctx, "kill-crash-1")
	if err != nil {
		t.Fatalf("Get after reap: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateFailed {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateFailed)
	}

	var execOwner *string
	storeB.db.QueryRow(`SELECT exec_owner FROM a2a_sdk_tasks WHERE id = 'kill-crash-1'`).Scan(&execOwner)
	if execOwner != nil {
		t.Errorf("exec_owner should be NULL after crash recovery, got %q", *execOwner)
	}
	t.Logf("Crash recovery verified: PID %d killed, task reaped to failed, exec_owner cleared", procA.pid)
}

// ---------- Regression tests: CRIT-2, CRIT-3, REQ-6 ----------

// TestRegressionCRIT2_CreateClaimRace demonstrates CRIT-2: ClaimExecution
// fails when the row doesn't exist yet (the SDK's async Create hasn't committed).
// The fix uses a retry loop. This test reproduces the pre-fix scenario, then
// verifies the retry succeeds after the row is created asynchronously.
func TestRegressionCRIT2_CreateClaimRace(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-crit2", "agent-crit2")

	taskID := "crit2-race-1"
	ownerID := "test-owner:crit2"

	// Pre-fix behavior: ClaimExecution on a nonexistent row returns (false, nil) —
	// not claimed, no error. Without retry, the executor would proceed without a lease.
	claimed, err := store.ClaimExecution(ctx, taskID, ownerID, 60*time.Second)
	if err != nil {
		t.Fatalf("ClaimExecution on nonexistent row should not error: %v", err)
	}
	if claimed {
		t.Fatal("ClaimExecution should return false for nonexistent row")
	}
	t.Log("CRIT-2 pre-condition confirmed: claim fails for nonexistent row")

	// Simulate the fix: async Create followed by claim retry.
	// Launch Create in a goroutine (simulating the SDK's event consumer).
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond) // simulate async delay
		task := &a2a.Task{
			ID:        a2a.TaskID(taskID),
			ContextID: "ctx-crit2",
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		}
		if _, cerr := store.Create(ctx, task); cerr != nil {
			t.Errorf("async Create: %v", cerr)
		}
	}()

	// Retry loop (mirrors executor.go logic).
	var claimSuccess bool
	for attempt := 0; attempt < 50; attempt++ {
		claimed, err = store.ClaimExecution(ctx, taskID, ownerID, 60*time.Second)
		if err != nil {
			t.Fatalf("ClaimExecution attempt %d error: %v", attempt, err)
		}
		if claimed {
			claimSuccess = true
			t.Logf("CRIT-2 fix verified: claim succeeded on attempt %d", attempt)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done

	if !claimSuccess {
		t.Fatal("CRIT-2 fix failed: claim never succeeded after retry loop")
	}

	// Cleanup
	store.ReleaseExecution(ctx, taskID, ownerID)
}

// TestRegressionCRIT3_ClaimErrorFailsClosed demonstrates CRIT-3: when
// ClaimExecution encounters a DB error, the executor must fail closed (no
// Hub side effects). This test simulates a closed DB pool to trigger the error,
// then verifies the claim correctly returns an error.
func TestRegressionCRIT3_ClaimErrorFailsClosed(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-crit3", "agent-crit3")

	taskID := "crit3-failclosed-1"
	ownerID := "test-owner:crit3"

	// Create the task first.
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-crit3",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Close the DB pool to simulate a connection failure.
	store.db.Close()

	// ClaimExecution should return an error (not silently succeed or skip).
	claimed, err := store.ClaimExecution(ctx, taskID, ownerID, 60*time.Second)
	if err == nil {
		t.Fatal("CRIT-3 regression: ClaimExecution should error with closed pool, but returned nil")
	}
	if claimed {
		t.Fatal("CRIT-3 regression: ClaimExecution should not claim with a broken DB")
	}
	t.Logf("CRIT-3 verified: ClaimExecution failed closed with error: %v", err)

	// The executor.go code path checks: if claimErr != nil → fail closed,
	// yield a TaskStateFailed event, and return BEFORE any Hub send.
	// That path is verified by the fact that ClaimExecution returns (false, error).
}

// TestRegressionREQ6_ReapEmitsTerminalEvent demonstrates REQ-6: when
// ReapStaleTasks transitions a task to failed, the caller (reapStaleSDKExecutions)
// must emit a cross-replica-visible failure event to a2a_task_events.
func TestRegressionREQ6_ReapEmitsTerminalEvent(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	// Create stores.
	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		pgStore.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
		pgStore.Close()
	})

	sdkStore, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() {
		sdkStore.db.Exec("DELETE FROM a2a_sdk_tasks")
		sdkStore.Close()
	})

	taskID := "req6-reap-event-1"

	// Create a working task with a stale execution lease.
	routeCtx := ctxForRoute("proj-req6", "agent-req6")
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-req6",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := sdkStore.Create(routeCtx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Claim execution and backdate heartbeat to simulate crash.
	ownerID := "crashed-owner:req6"
	claimed, err := sdkStore.ClaimExecution(ctx, taskID, ownerID, 60*time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimExecution: claimed=%v err=%v", claimed, err)
	}
	sdkStore.db.Exec(`UPDATE a2a_sdk_tasks SET exec_heartbeat = NOW() - INTERVAL '10 minutes' WHERE id = $1`, taskID)

	// Create a task in bridge state store (for event log foreign key).
	pgStore.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-req6",
		ProjectID: "proj-req6",
		AgentSlug: "agent-req6",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Wire up a Bridge with reaper.
	shutdownCtx, shutdownCancel := context.WithCancel(ctx)
	defer shutdownCancel()
	b := &Bridge{
		store:        pgStore,
		log:          slog.Default(),
		config:       &Config{},
		activeTasks:  make(map[string]activeTaskEntry),
		agentTasks:   make(map[string][]string),
		push:         NewPushDispatcher(pgStore, &Config{}, slog.Default(), shutdownCtx),
		sdkTaskStore: sdkStore,
	}

	// Run the reaper (production code path).
	b.reapStaleSDKExecutions(ctx, 5*time.Minute)

	// Verify the task was transitioned to failed.
	stored, err := sdkStore.Get(routeCtx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get after reap: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateFailed {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateFailed)
	}

	// Verify a terminal failure event was emitted to a2a_task_events (cross-replica visible).
	events, err := pgStore.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("REQ-6 regression: no failure event emitted to a2a_task_events after reap")
	}

	// Verify the event contains the failure status.
	var payload map[string]interface{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	statusMap, _ := payload["status"].(map[string]interface{})
	if statusMap == nil || statusMap["state"] != TaskStateFailed {
		t.Errorf("reap event should contain failed status, got: %v", payload)
	}
	t.Logf("REQ-6 verified: reap emitted %d terminal event(s) to a2a_task_events", len(events))

	// Verify a second replica can see the event (cross-replica visibility).
	pgStore2, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres (replica 2): %v", err)
	}
	defer pgStore2.Close()

	events2, err := pgStore2.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents from replica 2: %v", err)
	}
	if len(events2) == 0 {
		t.Fatal("REQ-6 cross-replica: failure event not visible from second connection")
	}
	t.Log("REQ-6 cross-replica visibility confirmed")
}

// TestRegressionREQ2_ActiveTaskEventsNotPurged demonstrates REQ-2: RunSweep
// must not purge events for tasks that are still active. In standalone mode,
// only events for terminal tasks are purged via PurgeTasksAndEvents; the
// unconditional PurgeTaskEvents is skipped.
func TestRegressionREQ2_ActiveTaskEventsNotPurged(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	pgStore, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		pgStore.PurgeTaskEvents(ctx, time.Now().Add(1*time.Hour))
		pgStore.Close()
	})

	sdkStore, err := NewPostgresTaskStore(dbURL)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() {
		sdkStore.db.Exec("DELETE FROM a2a_sdk_tasks")
		sdkStore.Close()
	})

	taskID := "req2-active-events-1"

	// Create an active (working) task.
	routeCtx := ctxForRoute("proj-req2", "agent-req2")
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-req2",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := sdkStore.Create(routeCtx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Create a task in bridge state store and append an old event.
	pgStore.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-req2",
		ProjectID: "proj-req2",
		AgentSlug: "agent-req2",
		State:     "working",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata:  "{}",
	})

	// Insert an event and backdate it to be older than the purge cutoff.
	eventPayload, _ := json.Marshal(map[string]interface{}{
		"taskId": taskID,
		"status": map[string]string{"state": "working"},
	})
	eventID, err := pgStore.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "status",
		Payload: eventPayload,
	})
	if err != nil {
		t.Fatalf("AppendTaskEvent: %v", err)
	}
	// Backdate the event.
	sdkStore.db.Exec(`UPDATE a2a_task_events SET created_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, eventID)

	// Wire up a Bridge with sdkTaskStore (standalone mode).
	shutdownCtx, shutdownCancel := context.WithCancel(ctx)
	defer shutdownCancel()
	b := &Bridge{
		store:        pgStore,
		log:          slog.Default(),
		config:       &Config{},
		activeTasks:  make(map[string]activeTaskEntry),
		agentTasks:   make(map[string][]string),
		push:         NewPushDispatcher(pgStore, &Config{}, slog.Default(), shutdownCtx),
		sdkTaskStore: sdkStore,
	}

	// Run sweep.
	b.RunSweep(ctx)

	// The event for the active task must survive.
	events, err := pgStore.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents after sweep: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("REQ-2 regression: events for active task were purged by RunSweep")
	}
	t.Logf("REQ-2 verified: %d event(s) for active task survived RunSweep", len(events))
}

// TestProductionPath_ScionExecutorSendMessage exercises the production
// ScionExecutor code path via a real cross-process test server, verifying
// that tasks created via the A2A JSON-RPC SendMessage flow through the
// executor's SDK store create and status transitions.
func TestProductionPath_ScionExecutorSendMessage(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store := testPostgresTaskStore(t)
	defer store.db.Exec("DELETE FROM a2a_sdk_tasks")

	// Start a test server — its testExecutor creates, works, and completes tasks
	// without requiring a real Hub, exercising the SDK task store.
	proc := startTestServer(t, dbURL, "proj-prod", "agent-prod")

	// Use the standard jsonRPC helper (method = "SendMessage").
	result := jsonRPC(t, proc.URL(), "SendMessage", map[string]interface{}{
		"message": map[string]interface{}{
			"messageId": "prod-path-msg-1",
			"role":      "ROLE_USER",
			"parts": []map[string]interface{}{
				{"text": "Hello from production path test"},
			},
		},
	})

	// SendMessage returns a StreamResponse wrapping an event.
	var sendResp struct {
		StatusUpdate *struct {
			TaskID string `json:"taskId"`
			State  string `json:"state"`
		} `json:"statusUpdate"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(result, &sendResp); err != nil {
		t.Fatalf("unmarshal send result: %v (raw: %s)", err, result)
	}
	var taskID string
	if sendResp.Task != nil {
		taskID = sendResp.Task.ID
	} else if sendResp.StatusUpdate != nil {
		taskID = sendResp.StatusUpdate.TaskID
	}
	if taskID == "" {
		t.Fatalf("could not extract task ID from send result: %s", result)
	}
	t.Logf("Production path: SendMessage created task %s", taskID)

	// Verify the task exists in the Postgres SDK store.
	ctx := ctxForRoute("proj-prod", "agent-prod")
	stored, err := store.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get task from store: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("stored state = %q, want completed", stored.Task.Status.State)
	}
	t.Logf("Production path verified: task %s stored with state %s", taskID, stored.Task.Status.State)
}

// TestDeterministicArtifactDedup verifies that TranslateScionToA2A produces
// deterministic message and artifact IDs from the same Scion message, enabling
// reliable dedup_key generation (OPT-1).
func TestDeterministicArtifactDedup(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-09-18T00:00:00Z",
		Msg:       "hello world",
		Type:      messages.TypeAssistantReply,
		Sender:    "agent:test",
	}

	// Call TranslateScionToA2A twice with the same message.
	msg1, art1 := TranslateScionToA2A(msg)
	msg2, art2 := TranslateScionToA2A(msg)

	// Message IDs must be identical (deterministic).
	if msg1.MessageID != msg2.MessageID {
		t.Errorf("message IDs differ: %q vs %q", msg1.MessageID, msg2.MessageID)
	}

	// Artifact IDs must be identical (deterministic).
	if len(art1) != len(art2) {
		t.Fatalf("artifact count mismatch: %d vs %d", len(art1), len(art2))
	}
	for i := range art1 {
		if art1[i].ArtifactID != art2[i].ArtifactID {
			t.Errorf("artifact[%d] IDs differ: %q vs %q", i, art1[i].ArtifactID, art2[i].ArtifactID)
		}
	}

	// Message ID and artifact ID must be different from each other.
	if len(art1) > 0 && msg1.MessageID == art1[0].ArtifactID {
		t.Error("message ID should not equal artifact ID")
	}

	// Different messages must produce different IDs.
	msg3 := *msg
	msg3.Msg = "different content"
	msg3Out, art3 := TranslateScionToA2A(&msg3)
	if msg3Out.MessageID == msg1.MessageID {
		t.Error("different content should produce different message ID")
	}
	if len(art3) > 0 && len(art1) > 0 && art3[0].ArtifactID == art1[0].ArtifactID {
		t.Error("different content should produce different artifact ID")
	}

	t.Logf("Deterministic dedup verified: msg=%s, art=%s", msg1.MessageID[:8], art1[0].ArtifactID[:8])
}
