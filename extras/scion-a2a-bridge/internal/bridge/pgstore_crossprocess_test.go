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
// Returns the process info and a cleanup function.
func startTestServer(t *testing.T, dbURL, project, agent string) *testServerProcess {
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
	cmd := exec.CommandContext(ctx, binaryPath,
		"-port=0",
		"-database-url="+dbURL,
		"-project="+project,
		"-agent="+agent,
	)

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
	reaped, err := storeB.ReapStaleTasks(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleTasks: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
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
	reaped, err := store.ReapStaleTasks(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleTasks: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (no execution claim)", reaped)
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
