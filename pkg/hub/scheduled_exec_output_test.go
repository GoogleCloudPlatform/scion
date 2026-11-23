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
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecOutputReaders guards A15: in pkg/hub, only the allow-listed output
// handler file may call GetScheduledExecRunOutput.
func TestExecOutputReaders(t *testing.T) {
	allowedCallers := map[string]bool{
		"handlers_scheduled_exec_output.go": true,
	}
	fset := token.NewFileSet()
	// pkgmove:scan-covers guardScanSubdirs
	files, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "GetScheduledExecRunOutput" {
				if !allowedCallers[name] {
					t.Errorf("%s: %s calls GetScheduledExecRunOutput; only allow-listed gated handlers may read exec_run_outputs",
						fset.Position(sel.Pos()), name)
				}
			}
			return true
		})
	}
}

// TestScheduledExecCapture_OneShotJSONIntegration verifies the Commit 1
// vertical slice on Linux: schedule a one-shot `["sh","-c","echo '{\"a\":1}'"]`
// with captureOutput:"json", fire through real sciontool over Conduit, verify
// execSummary metadata, read /output as an attach holder (200) and a scheduler
// reader (403), and verify the output canary never appears in logs, audit,
// event errors, or other responses (A9, A10).
func TestScheduledExecCapture_OneShotJSONIntegration(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)

	// Capture slog output to assert no output canary ever reaches logs (A10).
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captureLogWriter{w: &logBuf, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	canary := "CAPTURECANARY-" + uuid.NewString()
	eventsURL := "/api/v1/projects/" + f.launched.ProjectID + "/scheduled-events"
	rec := requestAsIdentity(t, f.srv, f.owner, http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType:     "exec",
		FireIn:        "1h",
		AgentID:       f.launched.ID,
		Command:       []string{"sh", "-c", `echo '{"a":1,"canary":"` + canary + `"}'`},
		CaptureOutput: "json",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	evt := f.getEvent(t, created.ID)
	f.fire(*evt)
	run := f.waitFinished(t, created.ID)
	require.Equal(t, store.ScheduledExecOutcomeOK, run.Outcome)
	require.Equal(t, agentcontrol.ExecCaptureJSON, run.CaptureMode)
	require.Equal(t, agentcontrol.ExecOutputCaptured, run.OutputStatus)

	// Event detail shows execSummary metadata (captureMode + outputStatus) and
	// never includes the captured output bytes.
	eventURL := eventsURL + "/" + created.ID
	recEvt := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, eventURL, nil)
	require.Equal(t, http.StatusOK, recEvt.Code, recEvt.Body.String())
	var evtResp ScheduledEventResponse
	require.NoError(t, json.Unmarshal(recEvt.Body.Bytes(), &evtResp))
	require.NotNil(t, evtResp.ExecSummary)
	assert.Equal(t, agentcontrol.ExecCaptureJSON, evtResp.ExecSummary.CaptureMode)
	assert.Equal(t, agentcontrol.ExecOutputCaptured, evtResp.ExecSummary.OutputStatus)
	assert.Empty(t, evtResp.Error)

	// Attach holder reads /output -> 200 with Cache-Control: no-store.
	outputURL := eventURL + "/output"
	recOut := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, outputURL, nil)
	require.Equal(t, http.StatusOK, recOut.Code, recOut.Body.String())
	assert.Equal(t, "no-store", recOut.Header().Get("Cache-Control"))
	var outResp struct {
		CaptureMode  string         `json:"captureMode"`
		OutputStatus string         `json:"outputStatus"`
		Output       map[string]any `json:"output"`
	}
	require.NoError(t, json.Unmarshal(recOut.Body.Bytes(), &outResp))
	assert.Equal(t, "json", outResp.CaptureMode)
	assert.Equal(t, "captured", outResp.OutputStatus)
	assert.Equal(t, float64(1), outResp.Output["a"])
	assert.Equal(t, canary, outResp.Output["canary"])

	// Scheduler reader without attach -> 403 and no canary.
	reader := setupNonAdminUser(t, f.store, []string{"scheduled_event.read", "scheduled_event.list"})
	recDenied := requestAsIdentity(t, f.srv, reader, http.MethodGet, outputURL, nil)
	assert.Equal(t, http.StatusForbidden, recDenied.Code)
	assert.NotContains(t, recDenied.Body.String(), canary)

	// Soft-deleted target agent -> 403 even for the owner (A9).
	a, err := f.store.GetAgent(context.Background(), f.launched.ID)
	require.NoError(t, err)
	a.DeletedAt = time.Now().UTC()
	require.NoError(t, f.store.UpdateAgent(context.Background(), a))
	recDeleted := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, outputURL, nil)
	assert.Equal(t, http.StatusForbidden, recDeleted.Code)
	assert.NotContains(t, recDeleted.Body.String(), canary)

	// Verify canary never appeared in slog output (A10).
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	assert.NotContains(t, logged, canary, "captured output canary must never appear in logs")
}

// TestScheduledExecCapture_OldSciontoolRefusedCaptureUnsupported verifies A2:
// an agent advertising exec-start/exec-status WITHOUT exec_capture.v1 refuses
// a capture fire BEFORE sending exec-start, recording outcome=error,
// event.status=failed, event.error="capture_unsupported", output_status="",
// while a default non-capture fire on the same agent still succeeds.
func TestScheduledExecCapture_OldSciontoolRefusedCaptureUnsupported(t *testing.T) {
	f := newExecRouteFixture(t)
	// Start an "old sciontool" that advertises exec routes but NOT exec_capture.v1.
	f.ca = f.startControlAgentAsWithFeatures(t, f.public.URL, f.launched.RunID, true, false, f.opts, f.capture.wrap)

	eventsURL := "/api/v1/projects/" + f.launched.ProjectID + "/scheduled-events"
	rec := requestAsIdentity(t, f.srv, f.owner, http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType:     "exec",
		FireIn:        "1h",
		AgentID:       f.launched.ID,
		Command:       []string{"sh", "-c", `echo '{"ok":true}'`},
		CaptureOutput: "json",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	f.fire(*f.getEvent(t, created.ID))
	run := f.waitFinished(t, created.ID)
	assert.Equal(t, store.ScheduledExecOutcomeError, run.Outcome)
	assert.Equal(t, agentcontrol.ExecCaptureJSON, run.CaptureMode)
	assert.Equal(t, "", run.OutputStatus)
	evt := f.getEvent(t, created.ID)
	assert.Equal(t, store.ScheduledEventFailed, evt.Status)
	assert.Equal(t, execReasonCaptureUnsupported, evt.Error)
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "no exec-start must be sent when feature is absent")

	// Default non-capture fire on the same old agent still succeeds.
	recDef := requestAsIdentity(t, f.srv, f.owner, http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType: "exec",
		FireIn:    "1h",
		AgentID:   f.launched.ID,
		Command:   []string{"true"},
	})
	require.Equal(t, http.StatusCreated, recDef.Code, recDef.Body.String())
	var createdDef struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(recDef.Body.Bytes(), &createdDef))
	f.fire(*f.getEvent(t, createdDef.ID))
	runDef := f.waitFinished(t, createdDef.ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, runDef.Outcome)
	assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
}

// TestScheduledExecCapture_StartReplay200PollsStatus verifies A16b: when
// exec-start for a capture run returns a 200 finished replay (which never
// carries output), the Hub hands the run to the tracker instead of finishing
// without content, and exec-status retrieves and stores the output.
func TestScheduledExecCapture_StartReplay200PollsStatus(t *testing.T) {
	f := newExecRouteFixture(t)
	f.ca = f.startControlAgentAsWithFeatures(t, f.public.URL, f.launched.RunID, true, true, f.opts, func(_ conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			switch req.GetPath() {
			case execRoutePath(agentcontrol.RouteExecStart):
				return &conduitv1.RpcResponse{
					Status: http.StatusOK,
					Body:   []byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":3,"output_bytes":7,"replayed":true}`),
				}
			case execRoutePath(agentcontrol.RouteExecStatus):
				return &conduitv1.RpcResponse{
					Status: http.StatusOK,
					Body:   []byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":3,"output_bytes":7,"output_status":"captured","output":{"k":"v"}}`),
				}
			default:
				return &conduitv1.RpcResponse{Status: http.StatusNotFound}
			}
		})
	})

	eventsURL := "/api/v1/projects/" + f.launched.ProjectID + "/scheduled-events"
	rec := requestAsIdentity(t, f.srv, f.owner, http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType:     "exec",
		FireIn:        "1h",
		AgentID:       f.launched.ID,
		Command:       []string{"true"},
		CaptureOutput: "json",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	f.fire(*f.getEvent(t, created.ID))
	run := f.waitFinished(t, created.ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, run.Outcome)
	assert.Equal(t, agentcontrol.ExecOutputCaptured, run.OutputStatus)

	recOut := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, eventsURL+"/"+created.ID+"/output", nil)
	require.Equal(t, http.StatusOK, recOut.Code, recOut.Body.String())
	assert.Contains(t, recOut.Body.String(), `"k":"v"`)
}

// TestScheduledExecCapture_Output404AndValidation checks 404 cases on /output
// (non-exec event, no run yet, non-capture run) and authoring validation for
// captureOutput.
func TestScheduledExecCapture_Output404AndValidation(t *testing.T) {
	f := execAuthoringFixture(t)
	eventsURL := "/api/v1/projects/" + f.proj.ID + "/scheduled-events"

	// Invalid captureOutput is rejected with 400 naming only the field.
	recBad := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType:     "exec",
		FireIn:        "1h",
		AgentID:       f.sibling.ID,
		Command:       []string{"true"},
		CaptureOutput: "SECRET-BOGUS-VALUE",
	})
	require.Equal(t, http.StatusBadRequest, recBad.Code, recBad.Body.String())
	assert.Contains(t, recBad.Body.String(), "captureOutput")
	assert.NotContains(t, recBad.Body.String(), "SECRET-BOGUS-VALUE")

	// "none" is accepted and normalized to absent (canonical default form).
	recNone := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType:     "exec",
		FireIn:        "1h",
		AgentID:       f.sibling.ID,
		Command:       []string{"true"},
		CaptureOutput: "none",
	})
	require.Equal(t, http.StatusCreated, recNone.Code, recNone.Body.String())
	var noneResp struct {
		ID      string `json:"id"`
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(recNone.Body.Bytes(), &noneResp))
	assert.NotContains(t, noneResp.Payload, "captureOutput")

	// Pending event (no run yet) -> 404 on /output.
	recNoRun := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodGet, eventsURL+"/"+noneResp.ID+"/output", nil)
	assert.Equal(t, http.StatusNotFound, recNoRun.Code)

	// Non-exec event -> 404 on /output.
	recMsg := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, eventsURL, CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		AgentID:   f.sibling.ID,
		Message:   "hi",
	})
	require.Equal(t, http.StatusCreated, recMsg.Code, recMsg.Body.String())
	var msgResp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(recMsg.Body.Bytes(), &msgResp))
	recMsgOut := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodGet, eventsURL+"/"+msgResp.ID+"/output", nil)
	assert.Equal(t, http.StatusNotFound, recMsgOut.Code)
}

type captureLogWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *captureLogWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestScheduledExecCapture_RecurringScheduleAuthorityAndTail verifies A11:
// creating a recurring exec schedule with captureOutput:"tail" and PATCHing
// captureOutput requires live agent.attach (same authority as command changes)
// and bumps AuthorizationRevision.
func TestScheduledExecCapture_RecurringScheduleAuthorityAndTail(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()
	schedURL := "/api/v1/projects/" + f.proj.ID + "/schedules"
	owner := authUser(f.owner)

	// Non-exec schedule rejects captureOutput convenience field.
	recMsg := requestAsIdentity(t, f.srv, owner, http.MethodPost, schedURL, map[string]any{
		"name":          "msg-no-cap",
		"cronExpr":      "0 * * * *",
		"eventType":     "message",
		"agentName":     f.sibling.Slug,
		"message":       "hi",
		"captureOutput": "tail",
	})
	assert.Equal(t, http.StatusBadRequest, recMsg.Code, recMsg.Body.String())

	// Create recurring exec schedule with captureOutput:"tail".
	recCreate := requestAsIdentity(t, f.srv, owner, http.MethodPost, schedURL, map[string]any{
		"name":          "exec-tail-sched",
		"cronExpr":      "0 * * * *",
		"eventType":     "exec",
		"agentId":       f.sibling.ID,
		"command":       []string{"sh", "-c", "echo hi"},
		"captureOutput": "tail",
	})
	require.Equal(t, http.StatusCreated, recCreate.Code, recCreate.Body.String())
	var created struct {
		ID                    string `json:"id"`
		Payload               string `json:"payload"`
		AuthorizationRevision int    `json:"authorizationRevision"`
	}
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &created))
	assert.Equal(t, 1, created.AuthorizationRevision)
	assert.Contains(t, created.Payload, `"captureOutput":"tail"`)

	// Non-attach reader sees captureOutput in redactedExecPayload, without argv.
	reader := setupNonAdminUser(t, f.store, []string{"scheduled_event.read", "scheduled_event.list", "scheduled_event.update"})
	recRed := requestAsIdentity(t, f.srv, reader, http.MethodGet, schedURL+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, recRed.Code, recRed.Body.String())
	assert.Contains(t, recRed.Body.String(), `\"captureOutput\":\"tail\"`)
	assert.Contains(t, recRed.Body.String(), `\"commandRedacted\":true`)

	// Non-attach reader cannot PATCH captureOutput.
	recDenied := requestAsIdentity(t, f.srv, reader, http.MethodPatch, schedURL+"/"+created.ID, map[string]any{
		"captureOutput": "json",
	})
	assert.GreaterOrEqual(t, recDenied.Code, http.StatusBadRequest)
	afterDenied, err := f.store.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, afterDenied.AuthorizationRevision)

	// Attach holder PATCHing captureOutput tail -> json bumps AuthorizationRevision.
	recPatch1 := requestAsIdentity(t, f.srv, owner, http.MethodPatch, schedURL+"/"+created.ID, map[string]any{
		"captureOutput": "json",
	})
	require.Equal(t, http.StatusOK, recPatch1.Code, recPatch1.Body.String())
	after1, err := f.store.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, after1.AuthorizationRevision)
	assert.Contains(t, after1.Payload, `"captureOutput":"json"`)

	// PATCHing captureOutput to "none" clears it and bumps AuthorizationRevision again.
	recPatch2 := requestAsIdentity(t, f.srv, owner, http.MethodPatch, schedURL+"/"+created.ID, map[string]any{
		"captureOutput": "none",
	})
	require.Equal(t, http.StatusOK, recPatch2.Code, recPatch2.Body.String())
	after2, err := f.store.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, after2.AuthorizationRevision)
	assert.NotContains(t, after2.Payload, "captureOutput")
}

// TestScheduledExecCapture_Abandon404AndSweeper verifies A12 (concurrent
// finishers CAS winner only), A13 (abandon / 404 -> output_status=lost, no
// output row), and sweeper recovery of tail output.
func TestScheduledExecCapture_Abandon404AndSweeper(t *testing.T) {
	t.Run("abandon past lease records lost with no output row", func(t *testing.T) {
		f := newExecRouteFixture(t)
		ctx := context.Background()
		firedAt := time.Now().Add(-10 * time.Minute)
		evt := withSessionRevision(store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, EventType: scheduledEventTypeExec,
			FireAt: firedAt, Payload: `{"agentId":"` + f.launched.ID + `","command":["true"],"captureOutput":"tail"}`,
			Status: store.ScheduledEventPending, CreatedBy: f.launched.OwnerID,
		}, f.launched.OwnerID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, evt.ID, store.ScheduledEventFired, &firedAt, ""))

		run := store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: f.launched.ProjectID, TargetAgentID: f.launched.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 10,
			StartedAt: firedAt, NotAfter: firedAt.Add(time.Minute), LeaseUntil: firedAt.Add(2 * time.Minute),
			Outcome: store.ScheduledExecOutcomeRunning, CaptureMode: agentcontrol.ExecCaptureTail, LaunchID: f.launched.RunID,
		}
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &run))

		f.srv.sweepScheduledExecRuns(ctx)

		got := f.getRun(t, evt.ID)
		assert.Equal(t, store.ScheduledExecOutcomeAbandoned, got.Outcome)
		assert.Equal(t, agentcontrol.ExecOutputLost, got.OutputStatus)
		_, err := f.store.GetScheduledExecRunOutput(ctx, evt.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)

		// /output returns 200 with outputStatus=lost and no output field for attach holder.
		rec := requestAsIdentity(t, f.srv, f.owner, http.MethodGet,
			"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evt.ID+"/output", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ScheduledExecOutputResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, agentcontrol.ExecCaptureTail, resp.CaptureMode)
		assert.Equal(t, agentcontrol.ExecOutputLost, resp.OutputStatus)
		assert.Nil(t, resp.Output)
	})

	t.Run("404 while running records abandoned and lost", func(t *testing.T) {
		f := newExecRouteFixture(t)
		f.startAgent(t) // fresh sciontool knows nothing about the run -> 404 unknown
		ctx := context.Background()
		firedAt := time.Now().Add(-time.Minute)
		evt := withSessionRevision(store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, EventType: scheduledEventTypeExec,
			FireAt: firedAt, Payload: `{"agentId":"` + f.launched.ID + `","command":["true"],"captureOutput":"json"}`,
			Status: store.ScheduledEventPending, CreatedBy: f.launched.OwnerID,
		}, f.launched.OwnerID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, evt.ID, store.ScheduledEventFired, &firedAt, ""))

		run := store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: f.launched.ProjectID, TargetAgentID: f.launched.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 60,
			StartedAt: firedAt, NotAfter: firedAt.Add(time.Minute), LeaseUntil: time.Now().Add(5 * time.Minute),
			Outcome: store.ScheduledExecOutcomeRunning, CaptureMode: agentcontrol.ExecCaptureJSON, LaunchID: f.launched.RunID,
		}
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &run))

		f.srv.sweepScheduledExecRuns(ctx)

		got := f.getRun(t, evt.ID)
		assert.Equal(t, store.ScheduledExecOutcomeAbandoned, got.Outcome)
		assert.Equal(t, agentcontrol.ExecOutputLost, got.OutputStatus)
		_, err := f.store.GetScheduledExecRunOutput(ctx, evt.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("concurrent finishers CAS winner writes output and loser writes nothing", func(t *testing.T) {
		f := newExecRouteFixture(t)
		ctx := context.Background()
		firedAt := time.Now().Add(-time.Minute)
		evt := withSessionRevision(store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, EventType: scheduledEventTypeExec,
			FireAt: firedAt, Payload: `{"agentId":"` + f.launched.ID + `","command":["true"],"captureOutput":"tail"}`,
			Status: store.ScheduledEventPending, CreatedBy: f.launched.OwnerID,
		}, f.launched.OwnerID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, evt.ID, store.ScheduledEventFired, &firedAt, ""))

		run := store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: f.launched.ProjectID, TargetAgentID: f.launched.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 60,
			StartedAt: firedAt, NotAfter: firedAt.Add(time.Minute), LeaseUntil: time.Now().Add(5 * time.Minute),
			Outcome: store.ScheduledExecOutcomeRunning, CaptureMode: agentcontrol.ExecCaptureTail, LaunchID: f.launched.RunID,
		}
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &run))

		resA := &agentcontrol.ExecResult{
			State: agentcontrol.ExecStateFinished, Outcome: agentcontrol.ExecOutcomeOK,
			OutputStatus: agentcontrol.ExecOutputCaptured, Output: []byte("from-tracker"),
		}
		resB := &agentcontrol.ExecResult{
			State: agentcontrol.ExecStateFinished, Outcome: agentcontrol.ExecOutcomeOK,
			OutputStatus: agentcontrol.ExecOutputCaptured, Output: []byte("from-sweeper"),
		}
		var wg sync.WaitGroup
		var wonA, wonB bool
		var errA, errB error
		wg.Add(2)
		go func() {
			defer wg.Done()
			wonA, errA = f.srv.finishScheduledExecRun(ctx, run, store.ScheduledExecOutcomeOK, resA, "", "")
		}()
		go func() {
			defer wg.Done()
			wonB, errB = f.srv.finishScheduledExecRun(ctx, run, store.ScheduledExecOutcomeOK, resB, "", "")
		}()
		wg.Wait()
		require.NoError(t, errA)
		require.NoError(t, errB)
		assert.True(t, wonA != wonB, "exactly one finisher must win CAS (wonA=%v, wonB=%v)", wonA, wonB)

		gotOut, err := f.store.GetScheduledExecRunOutput(ctx, evt.ID)
		require.NoError(t, err)
		if wonA {
			assert.Equal(t, "from-tracker", string(gotOut))
		} else {
			assert.Equal(t, "from-sweeper", string(gotOut))
		}
	})
}
