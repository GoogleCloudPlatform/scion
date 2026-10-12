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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Identifying details a Kubernetes refusal carries. None may reach a
// client body (ptone/scion#4555).
const (
	causeTestNamespace      = "team-ns-4555"
	causeTestServiceAccount = "scion-sa-4555"
	causeTestPod            = "agent-pod-4555"
)

// fixedTextStartError mirrors the Kubernetes runtime's opaque start error:
// fixed text, with the cause reachable only through Unwrap.
type fixedTextStartError struct {
	msg   string
	cause error
}

func (e *fixedTextStartError) Error() string { return e.msg }
func (e *fixedTextStartError) Unwrap() error { return e.cause }

// k8sStartFailure builds a Manager.Start error shaped like the real chain:
// pkg/agent's "failed to launch container: %w" around the runtime's
// fixed-text start error around the Kubernetes API status error.
func k8sStartFailure(apiErr error) error {
	return fmt.Errorf("failed to launch container: %w",
		&fixedTextStartError{msg: "failed to read the existing agent pod before start", cause: apiErr})
}

func forbiddenPodsError() error {
	return k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, causeTestPod,
		fmt.Errorf("User %q cannot get resource \"pods\" in API group \"\" in the namespace %q",
			"system:serviceaccount:"+causeTestNamespace+":"+causeTestServiceAccount, causeTestNamespace))
}

func TestRuntimeFailureCause(t *testing.T) {
	forbidden := forbiddenPodsError()
	unauthorized := k8serrors.NewUnauthorized("token expired")
	notFound := k8serrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, causeTestNamespace)
	leaf := errors.New("exec: docker: permission denied")

	cases := []struct {
		name       string
		err        error
		wantCause  error
		wantReason metav1.StatusReason
		wantCode   string
	}{
		{"forbidden", k8sStartFailure(forbidden), forbidden, metav1.StatusReasonForbidden, api.BrokerRuntimeCauseForbidden},
		{"unauthorized", k8sStartFailure(unauthorized), unauthorized, metav1.StatusReasonUnauthorized, api.BrokerRuntimeCauseUnauthorized},
		{"not found has no code", k8sStartFailure(notFound), notFound, metav1.StatusReasonNotFound, ""},
		{"multi-wrapped forbidden", fmt.Errorf("%w: cannot read: %w", errors.New("previous_pod_unconfirmed"), forbidden), forbidden, metav1.StatusReasonForbidden, api.BrokerRuntimeCauseForbidden},
		{"plain error: innermost leaf", fmt.Errorf("failed to launch container: %w", fmt.Errorf("run: %w", leaf)), leaf, "", ""},
		{"nil", nil, nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause, reason, code := runtimeFailureCause(tc.err)
			if cause != tc.wantCause {
				t.Errorf("cause = %v, want %v", cause, tc.wantCause)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// findCauseLogRecord returns the first JSON log record with message msg.
func findCauseLogRecord(t *testing.T, logs, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no %q log record; logs: %s", msg, logs)
	return nil
}

// assertNoK8sIdentity checks body carries none of the Kubernetes refusal's
// identifying details or its API server message.
func assertNoK8sIdentity(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{causeTestNamespace, causeTestServiceAccount, causeTestPod, "system:serviceaccount", "is forbidden"} {
		if strings.Contains(body, s) {
			t.Errorf("client body leaked %q: %s", s, body)
		}
	}
}

// assertCauseLogged checks a terminal create failure's ERROR record carries
// the agent id, the cause text, the Kubernetes reason and the cause code.
func assertCauseLogged(t *testing.T, rec map[string]any, agentID string) {
	t.Helper()
	if rec["level"] != "ERROR" {
		t.Errorf("log level = %v, want ERROR", rec["level"])
	}
	if rec["agent_id"] != agentID {
		t.Errorf("log agent_id = %v, want %q", rec["agent_id"], agentID)
	}
	if got, _ := rec["cause"].(string); !strings.Contains(got, "is forbidden") || !strings.Contains(got, causeTestServiceAccount) {
		t.Errorf("log cause = %q, want the Kubernetes refusal", got)
	}
	if rec["k8s_reason"] != string(metav1.StatusReasonForbidden) {
		t.Errorf("log k8s_reason = %v, want Forbidden", rec["k8s_reason"])
	}
	if rec["cause_code"] != api.BrokerRuntimeCauseForbidden {
		t.Errorf("log cause_code = %v, want %q", rec["cause_code"], api.BrokerRuntimeCauseForbidden)
	}
}

// postSyncCreateFailure posts a synchronous create whose Manager.Start
// returns startErr, and returns the response and the broker's logs.
func postSyncCreateFailure(t *testing.T, name string, startErr error) (int, string, ErrorResponse, string) {
	t.Helper()
	mgr := newAsyncManager()
	mgr.setStartErr(startErr)
	srv, _ := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	w := postCreate(t, srv, map[string]any{
		"id": name, "name": name, "projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"config": map[string]any{"template": "claude"},
	})
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	return w.Code, w.Body.String(), resp, logs.String()
}

// TestSyncCreate_K8sForbidden_CauseCode: a synchronous create whose runtime
// start is refused as Forbidden answers 500 runtime_error with the fixed
// text and details.cause=runtime_forbidden only, and its ERROR record
// carries the cause (ptone/scion#4555).
func TestSyncCreate_K8sForbidden_CauseCode(t *testing.T) {
	const name = "agent-sync-forbidden"
	status, body, resp, logs := postSyncCreateFailure(t, name, k8sStartFailure(forbiddenPodsError()))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if resp.Error.Code != ErrCodeRuntimeError {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeRuntimeError)
	}
	if resp.Error.Message != "Failed to create agent" {
		t.Errorf("message = %q, want the fixed text", resp.Error.Message)
	}
	if got := resp.Error.Details["cause"]; got != api.BrokerRuntimeCauseForbidden {
		t.Errorf("details.cause = %v, want %q (details %v)", got, api.BrokerRuntimeCauseForbidden, resp.Error.Details)
	}
	if len(resp.Error.Details) != 1 {
		t.Errorf("details = %v, want only cause", resp.Error.Details)
	}
	assertNoK8sIdentity(t, body)
	assertCauseLogged(t, findCauseLogRecord(t, logs, "Agent create failed"), name)
}

// TestSyncCreate_K8sUnauthorized_CauseCode: the same as the Forbidden case
// for a runtime whose own credentials are refused: details.cause is
// runtime_unauthorized end to end, and the ERROR record names the reason.
func TestSyncCreate_K8sUnauthorized_CauseCode(t *testing.T) {
	const name = "agent-sync-unauthorized"
	status, body, resp, logs := postSyncCreateFailure(t, name, k8sStartFailure(k8serrors.NewUnauthorized("token for "+causeTestServiceAccount+" expired")))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if resp.Error.Code != ErrCodeRuntimeError || resp.Error.Message != "Failed to create agent" {
		t.Errorf("error = %+v, want runtime_error with the fixed text", resp.Error)
	}
	if got := resp.Error.Details["cause"]; got != api.BrokerRuntimeCauseUnauthorized || len(resp.Error.Details) != 1 {
		t.Errorf("details = %v, want only cause=%q", resp.Error.Details, api.BrokerRuntimeCauseUnauthorized)
	}
	assertNoK8sIdentity(t, body)
	rec := findCauseLogRecord(t, logs, "Agent create failed")
	if rec["agent_id"] != name {
		t.Errorf("log agent_id = %v, want %q", rec["agent_id"], name)
	}
	if rec["k8s_reason"] != string(metav1.StatusReasonUnauthorized) {
		t.Errorf("log k8s_reason = %v, want Unauthorized", rec["k8s_reason"])
	}
	if rec["cause_code"] != api.BrokerRuntimeCauseUnauthorized {
		t.Errorf("log cause_code = %v, want %q", rec["cause_code"], api.BrokerRuntimeCauseUnauthorized)
	}
}

// TestSyncCreate_NonK8sFailure_Unchanged: a runtime failure with no
// Kubernetes status keeps the plain 500 with no details, and its ERROR
// record names the innermost cause but no reason or code.
func TestSyncCreate_NonK8sFailure_Unchanged(t *testing.T) {
	const name = "agent-sync-plain"
	status, body, resp, logs := postSyncCreateFailure(t, name,
		fmt.Errorf("failed to launch container: %w", errors.New("docker run scion-ctr-1: exit status 125")))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if resp.Error.Code != ErrCodeRuntimeError || resp.Error.Message != "Failed to create agent" {
		t.Errorf("error = %+v, want runtime_error with the fixed text", resp.Error)
	}
	if resp.Error.Details != nil {
		t.Errorf("details = %v, want none", resp.Error.Details)
	}
	rec := findCauseLogRecord(t, logs, "Agent create failed")
	if got, _ := rec["cause"].(string); got != "docker run scion-ctr-1: exit status 125" {
		t.Errorf("log cause = %q, want the innermost error", got)
	}
	for _, k := range []string{"k8s_reason", "cause_code"} {
		if _, ok := rec[k]; ok {
			t.Errorf("log record has %s = %v, want none for a non-Kubernetes failure", k, rec[k])
		}
	}
}

// TestAsyncCreate_K8sForbidden_CauseLogged: the async launch logs the same
// cause on its terminal ERROR record, and its failed report to the hub is
// unchanged: runtime_error with the fixed text and no identifying details.
func TestAsyncCreate_K8sForbidden_CauseLogged(t *testing.T) {
	const name = "agent-async-forbidden"
	report, logs := asyncCreateFailure(t, name, k8sStartFailure(forbiddenPodsError()))
	if report.ErrorCode != "runtime_error" {
		t.Errorf("report errorCode = %q, want runtime_error", report.ErrorCode)
	}
	if report.Message != "Failed to create agent" {
		t.Errorf("report message = %q, want the fixed text", report.Message)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	assertNoK8sIdentity(t, string(raw))
	assertCauseLogged(t, findCauseLogRecord(t, logs, "runLaunch: agent start failed"), name)
}
