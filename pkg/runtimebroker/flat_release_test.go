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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Flat-instance create tests for releasing a failed create's slug
// reservation (ptone/scion#5768) and for the slug-held refusal
// (ptone/scion#5769).

// flatSlugHolder returns the agent holding slug in the flat test project.
func flatSlugHolder(t *testing.T, f *flatInstanceFixture, slug string) string {
	t.Helper()
	h, err := f.srv.ownership.SlugHolder(flatTestProjectID, slug)
	require.NoError(t, err)
	return h
}

// stateAtHeaderWriter records, when the response status is written, what
// check reports at that moment.
type stateAtHeaderWriter struct {
	*httptest.ResponseRecorder
	check       func() bool
	checked, ok bool
}

func (w *stateAtHeaderWriter) WriteHeader(code int) {
	if !w.checked {
		w.checked, w.ok = true, w.check()
	}
	w.ResponseRecorder.WriteHeader(code)
}

// flatTemplateIDCreate is a create whose Hub template must be fetched
// (no preflight hydration), which fails in the fixture: its Hub endpoint is
// a closed local port.
func flatTemplateIDCreate(f *flatInstanceFixture, requestID, name string, extra map[string]interface{}) string {
	body := map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
		"config": map[string]interface{}{"template": "claude", "templateId": "tpl-unreachable"}}
	for k, v := range extra {
		body[k] = v
	}
	return flatCreateBody(requestID, name, body)
}

// TestFlatRelease_TemplateFetchFailureReleasesSlug: a create that fails
// fetching its template (after the reservation, before the agent exists)
// releases the slug before the error response is written; a create of the
// same slug by another agent then succeeds, and so does one with the same
// agent and run IDs.
func TestFlatRelease_TemplateFetchFailureReleasesSlug(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	// No harness-config policy gate: the template is fetched in
	// buildStartContext, after the reservation (start_context.go), as in the
	// live case.
	f.srv.config.AllowContainerScriptHarnesses = true
	releases := 0
	testHookBetweenSlugAndRecordRemoval = func() { releases++ }
	t.Cleanup(func() { testHookBetweenSlugAndRecordRemoval = nil })
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents",
		strings.NewReader(flatTemplateIDCreate(f, "req-tpl", "flat-agent", map[string]interface{}{"runId": "run-tpl"})))
	req.Header.Set("Content-Type", "application/json")
	w := &stateAtHeaderWriter{ResponseRecorder: httptest.NewRecorder(), check: func() bool {
		_, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-flat-agent")
		return err == nil && !ok && flatSlugHolder(t, f, "flat-agent") == ""
	}}
	f.srv.Handler().ServeHTTP(w, req)
	require.GreaterOrEqual(t, w.Code, 400, "the template fetch fails: %s", w.Body.String())
	require.True(t, w.checked)
	assert.True(t, w.ok, "the reservation is released before the error response is written")
	assert.Equal(t, 1, releases, "the create reserved the slug and released it")
	assert.Contains(t, strings.ToLower(w.Body.String()), "hub", "the failure is the template fetch from the Hub: %s", w.Body.String())
	noOwnedRecord(t, f, "agent-id-flat-agent")
	assert.Empty(t, flatSlugHolder(t, f, "flat-agent"))
	assert.Empty(t, f.srv.ownedHolds, "no in-flight hold is left")

	same := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-tpl-2", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "runId": "run-tpl",
			"config": map[string]interface{}{"template": "claude"}}))
	require.Equal(t, http.StatusCreated, same.Code, "same agent and run IDs: %s", same.Body.String())

	other := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	other.srv.config.AllowContainerScriptHarnesses = true
	w2 := serveFlat(other.srv, http.MethodPost, "/api/v1/agents", flatTemplateIDCreate(other, "req-a", "shared-slug", nil))
	require.GreaterOrEqual(t, w2.Code, 400)
	assert.Equal(t, 2, releases, "the second instance's failed create also released its reservation")
	w3 := serveFlat(other.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-b", "shared-slug",
		map[string]interface{}{"expectedRuntimeTargetId": other.identity.RuntimeTarget.ID, "id": "agent-id-second",
			"config": map[string]interface{}{"template": "claude"}}))
	require.Equal(t, http.StatusCreated, w3.Code, "another agent can take the released slug: %s", w3.Body.String())
	assert.Equal(t, "agent-id-second", flatSlugHolder(t, other, "shared-slug"))
}

// TestFlatRelease_ProvisionOnlyFailure: a provision-only create whose
// Provision fails releases the slug when it left no agent files, and keeps
// it (removing only its run) when it did.
func TestFlatRelease_ProvisionOnlyFailure(t *testing.T) {
	for _, leaveFiles := range []bool{false, true} {
		name := map[bool]string{false: "no files", true: "files left"}[leaveFiles]
		t.Run(name, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.mgr.provisionErr = errors.New("provision failed")
			releases := 0
			testHookBetweenSlugAndRecordRemoval = func() { releases++ }
			t.Cleanup(func() { testHookBetweenSlugAndRecordRemoval = nil })
			if leaveFiles {
				f.mgr.beforeProvision = func(opts api.StartOptions) {
					projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
					require.NoError(t, err)
					require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents", opts.Name), 0o755))
				}
			}
			w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-prov", "prov-agent",
				map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "provisionOnly": true,
					"config": map[string]interface{}{"template": "claude"}}))
			require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
			rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-prov-agent")
			require.NoError(t, err)
			if !leaveFiles {
				assert.False(t, ok, "no files: released")
				assert.Equal(t, 1, releases, "reserved, then released")
				assert.Empty(t, flatSlugHolder(t, f, "prov-agent"))
				return
			}
			require.True(t, ok, "files left: kept")
			assert.Empty(t, rec.Runs, "only the failed run is removed")
			assert.Equal(t, "agent-id-prov-agent", flatSlugHolder(t, f, "prov-agent"))
		})
	}
}

// TestFlatRelease_WorkspaceDownloadFailure: a create whose workspace
// download fails after it may have written agent files releases the slug
// only through the files proof. It also checks that the request's mirror
// release never drops a same-run resend's mirror.
func TestFlatRelease_WorkspaceDownloadFailure(t *testing.T) {
	for _, leaveFiles := range []bool{false, true} {
		name := map[bool]string{false: "no files", true: "files left"}[leaveFiles]
		t.Run(name, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			resendMirror := &ownedStart{}
			testHookCreateBeforeWorkspaceDownload = func(opts api.StartOptions) {
				if leaveFiles {
					projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
					require.NoError(t, err)
					require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents", opts.Name, "workspace"), 0o755))
				}
				// A same-run resend installed its own mirror meanwhile.
				f.srv.ownedStarts.Store(opts.RunID, resendMirror)
			}
			t.Cleanup(func() { testHookCreateBeforeWorkspaceDownload = nil })
			w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-gcs", "gcs-agent",
				map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "runId": "run-gcs",
					"workspaceStoragePath": "gs://bucket/workspaces/gcs-agent", "workspaceMode": "",
					"config": map[string]interface{}{"template": "claude"}}))
			require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
			v, ok := f.srv.ownedStarts.Load("run-gcs")
			assert.True(t, ok && v == resendMirror, "the resend's mirror is kept")
			_, recorded, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-gcs-agent")
			require.NoError(t, err)
			assert.Equal(t, leaveFiles, recorded, "kept exactly when files were left")
		})
	}
}

// TestFlatRelease_PanicBeforeStartAbandons: a create that panics before
// it starts the agent still gives its run back.
func TestFlatRelease_PanicBeforeStartAbandons(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	testHookSyncCreateBeforeBind = func(CreateAgentRequest) { panic("injected") }
	t.Cleanup(func() { testHookSyncCreateBeforeBind = nil })
	w := flatCreateAccepted(f, "panic-agent")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	// The request had taken its start marker (it may have written agent
	// files), so the files proof decides; it finds none, so the record is
	// deleted and the slug released.
	noOwnedRecord(t, f, "agent-id-panic-agent")
	assert.Empty(t, flatSlugHolder(t, f, "panic-agent"))
	assert.Empty(t, f.srv.ownedHolds)
}

// TestFlatRelease_SlugHeldRefusal: a create refused because another agent
// of the instance holds the slug gets 409 with the stable code
// agent_slug_held, the holder, and the remedy (ptone/scion#5769).
func TestFlatRelease_SlugHeldRefusal(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	require.NoError(t, f.srv.ownership.BeginRun(flatTestProjectID, "another-agent-id", "flat-agent", "run-x"))
	w := flatCreateAccepted(f, "flat-agent")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	apiErr := decodeFlatError(t, w)
	assert.Equal(t, "agent_slug_held", apiErr.Code)
	assert.Equal(t, api.BrokerErrCodeAgentSlugHeld, apiErr.Code)
	assert.Equal(t, "flat-agent", apiErr.Details["slug"])
	assert.Equal(t, "another-agent-id", apiErr.Details["holderAgentId"])
	assert.Equal(t, f.identity.RuntimeBrokerID, apiErr.Details["runtimeBrokerId"])
	assert.Contains(t, apiErr.Message, "another-agent-id")
	assert.Contains(t, apiErr.Message, "Delete that agent")
	assert.Contains(t, apiErr.Message, "restarting the Runtime Broker server that hosts this instance")
	assert.Contains(t, apiErr.Message, "hub-managed and not on nfs workspace storage", "a restart never releases it for an nfs-backed project")
	assert.Equal(t, "another-agent-id", flatSlugHolder(t, f, "flat-agent"), "the holder is untouched")
}

// TestOwnershipStartContextError maps each refused reservation.
func TestOwnershipStartContextError(t *testing.T) {
	s := abandonServer(t, "docker")
	k8s := abandonServer(t, "kubernetes")
	held := &slugHeldError{projectID: "p", slug: "w", holder: "agent-h"}
	sce := s.ownershipStartContextError(held, "w")
	assert.Equal(t, http.StatusConflict, sce.Status)
	assert.Equal(t, api.BrokerErrCodeAgentSlugHeld, sce.Code)
	assert.Equal(t, map[string]interface{}{"slug": "w", "holderAgentId": "agent-h", "runtimeBrokerId": "broker-a"}, sce.Details)
	assert.Contains(t, sce.Message, "restarting")
	assert.NotContains(t, k8s.ownershipStartContextError(held, "w").Message, "restarting", "no restart hint where a restart cannot release it")

	elsewhere := &slugReservedElsewhereError{projectID: "p", slug: "w", instance: "broker-b", holder: "agent-e"}
	sce = s.ownershipStartContextError(elsewhere, "w")
	assert.Equal(t, api.BrokerErrCodeAgentSlugHeld, sce.Code)
	assert.Equal(t, "broker-b", sce.Details["runtimeBrokerId"])
	assert.Contains(t, sce.Message, "agent-e")

	unreadable := &slugReservedElsewhereError{projectID: "p", slug: "w", instance: "broker-b", readErr: errors.New("io")}
	sce = s.ownershipStartContextError(unreadable, "w")
	assert.Empty(t, sce.Code, "an unreadable reservation keeps its response")
	assert.Equal(t, `Agent slug "w" is reserved in this project by Runtime Broker instance broker-b of the same host`, sce.Message)

	pending := &slugReservedElsewhereError{projectID: "p", slug: "w", instance: "broker-b", holder: "agent-e", pending: true}
	sce = s.ownershipStartContextError(pending, "w")
	assert.Empty(t, sce.Code)
	assert.Contains(t, sce.Message, "not yet confirmed")

	sce = s.ownershipStartContextError(errors.New("disk full"), "w")
	assert.Empty(t, sce.Code)
	assert.Equal(t, "The agent's ownership cannot be recorded by this Runtime Broker instance", sce.Message)
}

// TestFlatRelease_SyncStartFailureByRuntimeTarget: a synchronous create
// whose Start fails is cleaned up (its run finished, its files removed).
// The record is then retired only where the files proof can establish
// absence: on a Docker instance, not on a Kubernetes one, where an agent
// can keep state the broker cannot see (ptone/scion#5778).
func TestFlatRelease_SyncStartFailureByRuntimeTarget(t *testing.T) {
	for _, tc := range []struct {
		target  string
		retired bool
	}{{"docker", true}, {"kubernetes", false}} {
		t.Run(tc.target, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: tc.target}
			f.mgr.startErr = errors.New("start failed")
			w := flatCreateAccepted(f, "sync-agent")
			require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
			rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-sync-agent")
			require.NoError(t, err)
			if tc.retired {
				assert.False(t, ok)
				assert.Empty(t, flatSlugHolder(t, f, "sync-agent"))
				return
			}
			require.True(t, ok)
			require.Len(t, rec.Runs, 1)
			assert.Equal(t, OwnershipStateDeleted, rec.Runs[0].State, "the run is finished, so the Hub's delete can retire the record")
			assert.Equal(t, "agent-id-sync-agent", flatSlugHolder(t, f, "sync-agent"))
		})
	}
}

// TestFlatRelease_WorktreeStepThenFailureKeepsSlug: a create that enters
// buildStartContext's worktree step may have written agent files from
// there, so a later failure (here the async Preflight refusing the create)
// releases the slug only through the files proof, even though the request
// created the record. The agent's files exist, so the record is kept and
// only the failed run is removed.
func TestFlatRelease_WorktreeStepThenFailureKeepsSlug(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	root := flatProjectRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "wt-agent", "home"), 0o755))
	f.mgr.preflightErr = errors.New("runtime not ready")
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "wt-agent", map[string]interface{}{
		"workspaceMode": "worktree-per-agent",
		"config": map[string]interface{}{"template": "claude",
			"gitClone": map[string]interface{}{"url": "https://example.invalid/repo.git"}},
	}))
	require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-wt-agent")
	require.NoError(t, err)
	require.True(t, ok, "kept: the agent's files exist")
	assert.Empty(t, rec.Runs, "only the failed run is removed")
	assert.Equal(t, "agent-id-wt-agent", flatSlugHolder(t, f, "wt-agent"))
}

// TestFlatRelease_NoAbandonAfterStartEntered: once a synchronous create
// has entered Manager.Start, its error response never releases the
// reservation, even when the start's own cleanup left the run provisioning
// with no recorded object. Here the run's ownership mirror is gone (so
// completeOwnedStart cannot finish the run) and a newer start took the
// name's marker (so the failure's file cleanup, and with it the retire
// check, is skipped). Objects may exist that no record shows.
func TestFlatRelease_NoAbandonAfterStartEntered(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.startErr = errors.New("start failed")
	f.mgr.beforeStart = func(opts api.StartOptions) {
		f.srv.ownedStarts.Delete(opts.RunID)
		require.NoError(t, writeLaunchMarker(opts.ProjectPath, opts.SharedWorkspace, opts.Name, "a-newer-start"))
	}
	w := flatCreateAccepted(f, "started-agent")
	require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-started-agent")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rec.Runs, 1)
	assert.Equal(t, OwnershipStateProvisioning, rec.Runs[0].State, "untouched by the error response")
	assert.Equal(t, "agent-id-started-agent", flatSlugHolder(t, f, "started-agent"))
}
