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
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// workflowRunRequest is the JSON body sent by the hub when dispatching a run.
type workflowRunRequest struct {
	RunID          string `json:"runId"`
	GroveID        string `json:"groveId"`
	SourceYAML     string `json:"sourceYaml"`
	InputsJSON     string `json:"inputsJson"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// activeWorkflowRun tracks a workflow run that is currently executing.
type activeWorkflowRun struct {
	runID       string
	groveID     string
	cancel      context.CancelFunc
	startedAt   time.Time
	containerID string // set once the container is started; guarded by containerMu
	containerMu sync.Mutex
}

func (r *activeWorkflowRun) setContainerID(id string) {
	r.containerMu.Lock()
	r.containerID = id
	r.containerMu.Unlock()
}

func (r *activeWorkflowRun) getContainerID() string {
	r.containerMu.Lock()
	defer r.containerMu.Unlock()
	return r.containerID
}

// workflowRuntime is a subset of pkg/runtime.Runtime used by the executor.
// Defined as an interface to allow dependency injection in tests without
// requiring a real Docker/Apple container runtime.
type workflowRuntime interface {
	Run(ctx context.Context, config scionrt.RunConfig) (string, error)
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	GetLogs(ctx context.Context, id string) (string, error)
	GetLogsSince(ctx context.Context, id string, since time.Time) (string, error)
	Inspect(ctx context.Context, id string) (scionrt.ContainerState, error)
	List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
}

// WorkflowExecutor executes duckflux workflow runs on this broker using
// ephemeral containers provisioned via pkg/runtime.
//
// Each run is executed by:
//  1. Writing workflow.yaml and inputs.json to a host-side temp directory.
//  2. Starting an ephemeral container with the workflow dir volume-mounted at /workflow.
//  3. Running `quack run` inside the container; logs are polled every 500ms
//     and forwarded to the Hub as workflow_log events.
//  4. Waiting for the container to exit (via GetLogs + List status polling).
//  5. Reading trace files from the host-side trace dir; inlined into resultJson.
//  6. Deleting the container and temp directory.
//
// Cancel and timeout call Stop+Delete on the container rather than killing a
// host process.
type WorkflowExecutor struct {
	brokerID   string
	rt         workflowRuntime
	agentImage string
	log        *slog.Logger

	// getControlChannel returns the control channel client for the connection
	// identified by connectionName (may be empty for the default connection).
	// Returns nil when no channel is available.
	getControlChannel func(connectionName string) *ControlChannelClient

	// testEventSink, when non-nil, is called for every event instead of sending
	// via the control channel. Used in tests to capture emitted events without
	// a real WebSocket connection.
	testEventSink func(eventType string, payload interface{})

	mu   sync.Mutex
	runs map[string]*activeWorkflowRun
}

// NewWorkflowExecutor creates a WorkflowExecutor.
//
// rt is the container runtime used to provision ephemeral workflow containers.
// agentImage is the container image that has quack baked into PATH.
// getControlChannel is a function that returns a *ControlChannelClient for
// sending events back to the Hub.
func NewWorkflowExecutor(
	brokerID string,
	rt scionrt.Runtime,
	agentImage string,
	getControlChannel func(connectionName string) *ControlChannelClient,
	log *slog.Logger,
) *WorkflowExecutor {
	return &WorkflowExecutor{
		brokerID:          brokerID,
		rt:                rt,
		agentImage:        agentImage,
		log:               log,
		getControlChannel: getControlChannel,
		runs:              make(map[string]*activeWorkflowRun),
	}
}

// newWorkflowExecutorWithRuntime creates a WorkflowExecutor with an explicit
// workflowRuntime implementation (used in tests to inject a fake).
func newWorkflowExecutorWithRuntime(
	brokerID string,
	rt workflowRuntime,
	agentImage string,
	getControlChannel func(connectionName string) *ControlChannelClient,
	log *slog.Logger,
) *WorkflowExecutor {
	return &WorkflowExecutor{
		brokerID:          brokerID,
		rt:                rt,
		agentImage:        agentImage,
		log:               log,
		getControlChannel: getControlChannel,
		runs:              make(map[string]*activeWorkflowRun),
	}
}

// HandleCreateWorkflowRun handles POST /api/v1/workflow-runs.
// It validates the request, registers the run, and starts execution
// asynchronously so the 202 can be returned immediately.
func (e *WorkflowExecutor) HandleCreateWorkflowRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	var req workflowRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "invalid JSON body: "+err.Error())
		return
	}
	if req.RunID == "" {
		BadRequest(w, "runId is required")
		return
	}
	if strings.TrimSpace(req.SourceYAML) == "" {
		BadRequest(w, "sourceYaml is required")
		return
	}

	// Capture the connection name so the async goroutine can find the right
	// control channel even after the request context is gone.
	connName := r.Header.Get("X-Scion-Hub-Connection")

	e.mu.Lock()
	if _, exists := e.runs[req.RunID]; exists {
		e.mu.Unlock()
		// Idempotent: already accepted this run.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "runId": req.RunID})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &activeWorkflowRun{
		runID:     req.RunID,
		groveID:   req.GroveID,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	e.runs[req.RunID] = run
	e.mu.Unlock()

	go e.executeRun(ctx, connName, req, run, cancel)

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "runId": req.RunID})
}

// HandleCancelWorkflowRun handles DELETE /api/v1/workflow-runs/{runID}.
func (e *WorkflowExecutor) HandleCancelWorkflowRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w)
		return
	}

	// Extract runID from the path /api/v1/workflow-runs/{runID}
	runID := strings.TrimPrefix(r.URL.Path, "/api/v1/workflow-runs/")
	runID = strings.TrimSuffix(runID, "/")
	if runID == "" {
		BadRequest(w, "runId missing in path")
		return
	}

	e.mu.Lock()
	run, ok := e.runs[runID]
	e.mu.Unlock()

	if !ok {
		// Not found or already finished — idempotent 404.
		NotFound(w, "workflow run")
		return
	}

	// Canceling the context causes executeRun to detect ctx.Err() != nil,
	// which stops and deletes the container before emitting the canceled status.
	run.cancel()

	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling", "runId": runID})
}

// executeRun performs the actual workflow execution in a goroutine.
func (e *WorkflowExecutor) executeRun(ctx context.Context, connName string, req workflowRunRequest, run *activeWorkflowRun, cancel context.CancelFunc) {
	defer cancel()

	defer func() {
		e.mu.Lock()
		delete(e.runs, req.RunID)
		e.mu.Unlock()
	}()

	log := e.log.With("runID", req.RunID, "groveID", req.GroveID)
	log.Info("WorkflowExecutor: starting run")

	// Emit workflow_status: running
	e.sendStatusEvent(connName, req.RunID, "running")

	// Create a host-side temp directory that will be volume-mounted into the
	// container at /workflow. Contains workflow.yaml, inputs.json, and trace/.
	workDir, err := os.MkdirTemp("", "scion-workflow-"+req.RunID+"-")
	if err != nil {
		log.Error("WorkflowExecutor: failed to create workdir", "error", err)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("internal error: "+err.Error()), "")
		return
	}
	defer os.RemoveAll(workDir)

	// Write workflow.yaml.
	sourceFile := filepath.Join(workDir, "workflow.yaml")
	if err := os.WriteFile(sourceFile, []byte(req.SourceYAML), 0o600); err != nil {
		log.Error("WorkflowExecutor: failed to write source YAML", "error", err)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("failed to write source: "+err.Error()), "")
		return
	}

	// Write inputs.json.
	inputsFile := filepath.Join(workDir, "inputs.json")
	inputs := req.InputsJSON
	if inputs == "" {
		inputs = "{}"
	}
	if err := os.WriteFile(inputsFile, []byte(inputs), 0o600); err != nil {
		log.Error("WorkflowExecutor: failed to write inputs JSON", "error", err)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("failed to write inputs: "+err.Error()), "")
		return
	}

	// Pre-create trace directory so the volume mount target is a directory.
	traceDir := filepath.Join(workDir, "trace")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		log.Error("WorkflowExecutor: failed to create trace dir", "error", err)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("failed to create trace dir: "+err.Error()), "")
		return
	}

	// Determine timeout.
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = 3600
	}
	runCtx, runCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer runCancel()

	// Derive a short container name from the run ID.
	runPrefix := req.RunID
	if len(runPrefix) > 12 {
		runPrefix = runPrefix[:12]
	}
	containerName := "workflow-" + runPrefix

	// Build the RunConfig for the ephemeral container.
	// Thin provisioning: no Workspace, GitClone, SharedDirs, ResolvedSecrets, Harness, HomeDir.
	runCfg := scionrt.RunConfig{
		Name:  containerName,
		Image: e.agentImage,
		Labels: map[string]string{
			"scion.scion/kind":            "workflow-run",
			"scion.scion/workflow-run-id": req.RunID,
		},
		Volumes: []api.VolumeMount{
			{
				Source: workDir,
				Target: "/workflow",
			},
		},
		// CommandArgs carries the quack invocation. With Harness == nil,
		// buildCommonRunArgs (pkg/runtime/common.go) takes the thin-provisioning
		// branch and appends CommandArgs directly after the image, bypassing the
		// tmux/harness wrapping used for interactive agent containers.
		CommandArgs: []string{
			"quack", "run",
			"/workflow/workflow.yaml",
			"--input-file", "/workflow/inputs.json",
			"--trace-dir", "/workflow/trace/",
		},
		BrokerMode: true,
	}

	// Start the ephemeral container.
	containerID, err := e.rt.Run(runCtx, runCfg)
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			log.Warn("WorkflowExecutor: timeout before container started")
			e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("timed_out"), "")
			return
		}
		if ctx.Err() != nil {
			log.Warn("WorkflowExecutor: canceled before container started")
			e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("canceled"), "")
			return
		}
		log.Error("WorkflowExecutor: failed to start container", "error", err)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("failed to start container: "+err.Error()), "")
		return
	}

	run.setContainerID(containerID)
	log.Info("WorkflowExecutor: container started", "containerID", containerID)

	// Ensure container cleanup on any exit path (cancel, timeout, success, error).
	defer func() {
		bgCtx := context.Background()
		if stopErr := e.rt.Stop(bgCtx, containerID); stopErr != nil {
			log.Debug("WorkflowExecutor: stop container (cleanup)", "error", stopErr)
		}
		if delErr := e.rt.Delete(bgCtx, containerID); delErr != nil {
			log.Debug("WorkflowExecutor: delete container (cleanup)", "error", delErr)
		}
	}()

	// Poll logs at 500ms intervals (option (a) from spec — simple and reliable).
	// Returns when the container exits or a context is canceled/timed-out.
	exitCode, pollErr := e.pollContainerUntilDone(runCtx, ctx, connName, req.RunID, containerID, log)

	// Determine terminal state from context errors first.
	if runCtx.Err() == context.DeadlineExceeded {
		log.Warn("WorkflowExecutor: run timed out", "containerID", containerID)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("timed_out"), "")
		return
	}
	if ctx.Err() != nil {
		log.Warn("WorkflowExecutor: run canceled", "containerID", containerID)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("canceled"), "")
		return
	}
	if pollErr != nil {
		log.Error("WorkflowExecutor: polling error", "error", pollErr)
		e.sendOutputEvent(connName, req.RunID, 2, nil, ptrStr("polling error: "+pollErr.Error()), "")
		return
	}

	// Capture trace files from the host-side trace directory and inline them.
	// TODO(blob-upload): upload traceDir to blob storage and set traceKey
	// instead of inlining (see workflows.md §4.3 "trace capture" follow-up).
	var resultJSON *string
	if exitCode == 0 {
		if traceData := readTraceFiles(traceDir, log); traceData != "" {
			resultJSON = &traceData
		}
	}

	var errStr *string
	if exitCode != 0 {
		errStr = ptrStr("quack exited with code " + strconv.Itoa(exitCode))
	}

	log.Info("WorkflowExecutor: run completed", "exitCode", exitCode)
	e.sendOutputEvent(connName, req.RunID, exitCode, resultJSON, errStr, "")
}

// pollContainerUntilDone polls the container logs every 500ms, streaming new
// log bytes to the Hub as workflow_log events. It exits when the container
// reaches a terminal phase (stopped/error) or either context is done.
//
// Log streaming choice: GetLogs + totalBytesSent offset. This approach captures
// all logs from container start (no startup log loss) and never re-sends bytes
// (no duplication). Works uniformly across Docker, Apple, and K8s.
//
// Returns the raw exit code (preserving quack semantics: 0=success,
// 1=CLI error, 2=workflow failure) and any non-context error during polling.
func (e *WorkflowExecutor) pollContainerUntilDone(
	runCtx context.Context,
	cancelCtx context.Context,
	connName, runID, containerID string,
	log *slog.Logger,
) (int, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	totalBytesSent := 0
	exitCode := 0
	goneCount := 0

	for {
		select {
		case <-runCtx.Done():
			return 0, nil
		case <-cancelCtx.Done():
			return 0, nil
		case <-ticker.C:
		}

		allLogs, err := e.rt.GetLogs(runCtx, containerID)
		if err != nil {
			if runCtx.Err() != nil || cancelCtx.Err() != nil {
				return 0, nil
			}
			log.Debug("WorkflowExecutor: GetLogs transient error", "error", err)
			continue
		}

		if len(allLogs) > totalBytesSent {
			e.sendLogEvent(connName, runID, "stdout", allLogs[totalBytesSent:])
			totalBytesSent = len(allLogs)
		}

		// Check container phase via List with label filter.
		agents, listErr := e.rt.List(runCtx, map[string]string{
			"scion.scion/workflow-run-id": runID,
		})
		if listErr != nil {
			if runCtx.Err() != nil || cancelCtx.Err() != nil {
				return 0, nil
			}
			log.Debug("WorkflowExecutor: List transient error during poll", "error", listErr)
			continue
		}

		if len(agents) == 0 {
			// Container vanished (deleted externally or not visible yet).
			// Wait two consecutive misses before treating as done.
			goneCount++
			if goneCount >= 2 {
				break
			}
			continue
		}
		goneCount = 0

		phase := agents[0].Phase
		if phase == "stopped" || phase == "error" || phase == scionrt.LegacyAgentPhaseEnded {
			// Drain remaining logs using the same offset.
			finalLogs, _ := e.rt.GetLogs(runCtx, containerID)
			if len(finalLogs) > totalBytesSent {
				e.sendLogEvent(connName, runID, "stdout", finalLogs[totalBytesSent:])
			}
			// Use structured inspection to preserve raw exit code
			// (quack 0=success / 1=CLI error / 2=workflow failure semantics).
			if st, ierr := e.rt.Inspect(runCtx, containerID); ierr == nil {
				exitCode = st.ExitCode
			} else if phase == "error" {
				exitCode = 1
			}
			break
		}
	}

	return exitCode, nil
}

// sendEvent dispatches an event either to the testEventSink (in tests) or to
// the real control channel identified by connName.
func (e *WorkflowExecutor) sendEvent(connName, eventType string, payload interface{}) {
	if e.testEventSink != nil {
		e.testEventSink(eventType, payload)
		return
	}
	cc := e.getControlChannel(connName)
	if cc == nil {
		e.log.Warn("WorkflowExecutor: no control channel", "eventType", eventType)
		return
	}
	if err := cc.SendEvent(eventType, payload); err != nil {
		e.log.Warn("WorkflowExecutor: failed to send event", "eventType", eventType, "error", err)
	}
}

// sendStatusEvent sends a workflow_status event to the Hub.
func (e *WorkflowExecutor) sendStatusEvent(connName, runID, status string) {
	payload := wsprotocol.WorkflowStatusPayload{
		RunID:  runID,
		Status: status,
		At:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	e.sendEvent(connName, wsprotocol.EventWorkflowStatus, payload)
}

// sendLogEvent sends a workflow_log event line to the Hub.
func (e *WorkflowExecutor) sendLogEvent(connName, runID, stream string, line string) {
	payload := wsprotocol.WorkflowLogPayload{
		RunID:     runID,
		Stream:    stream,
		Line:      line,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	e.sendEvent(connName, wsprotocol.EventWorkflowLog, payload)
}

// sendOutputEvent sends the terminal workflow_output event to the Hub.
func (e *WorkflowExecutor) sendOutputEvent(connName, runID string, exitCode int, resultJSON, errStr *string, traceKey string) {
	payload := wsprotocol.WorkflowOutputPayload{
		RunID:      runID,
		ExitCode:   exitCode,
		ResultJSON: resultJSON,
		Error:      errStr,
		TraceKey:   traceKey,
	}
	e.sendEvent(connName, wsprotocol.EventWorkflowOutput, payload)
}

// Inlined-trace size limits. Files above traceFileMaxBytes are skipped
// (they belong in blob storage); aggregate bytes above traceAggregateMaxBytes
// stop consumption of further trace files for this run.
const (
	traceFileMaxBytes      = 256 << 10 // 256 KB per file
	traceAggregateMaxBytes = 256 << 10 // 256 KB aggregate cap across all files
)

// readTraceFiles reads all JSON files from the trace directory and returns
// them as a JSON string of the form {"traces":[...]}.
// Files larger than traceFileMaxBytes are skipped (they should be uploaded to
// blob storage). Returns "" if the directory is empty or all files are too large.
func readTraceFiles(traceDir string, log *slog.Logger) string {

	entries, err := os.ReadDir(traceDir)
	if err != nil {
		return ""
	}

	var traceItems []json.RawMessage
	var totalSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > traceFileMaxBytes {
			continue
		}
		if totalSize+info.Size() > traceAggregateMaxBytes {
			log.Debug("WorkflowExecutor: trace aggregate cap reached, skipping remaining files")
			break
		}
		data, err := os.ReadFile(filepath.Join(traceDir, entry.Name()))
		if err != nil {
			log.Debug("WorkflowExecutor: failed to read trace file", "name", entry.Name(), "error", err)
			continue
		}
		if json.Valid(data) {
			traceItems = append(traceItems, json.RawMessage(data))
			totalSize += info.Size()
		}
	}

	if len(traceItems) == 0 {
		return ""
	}

	result := struct {
		Traces []json.RawMessage `json:"traces"`
	}{Traces: traceItems}

	out, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	return string(out)
}

// ptrStr returns a pointer to a string.
func ptrStr(s string) *string {
	return &s
}

