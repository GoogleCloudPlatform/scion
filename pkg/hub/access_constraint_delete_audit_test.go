// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

// P6 (#2404 remainder): boundary DELETE emits exactly one structured-log
// event after commit; the process-memory boundary audit writer is gone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func p6CommitDelete(t *testing.T, ctx context.Context, gs *GovernanceService, ps *PreviewService, created *store.AccessConstraint, actor PrincipalContext) (*CommitResult, error) {
	t.Helper()
	preview, err := ps.GeneratePreview(context.Background(), PreviewRequest{
		Operation:    "delete",
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)
	return gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "delete",
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		PreviewToken: preview.PreviewToken,
		PreviewID:    preview.PreviewID,
		DraftHash:    preview.DraftHash,
		Actor:        actor,
	})
}

// ---------------------------------------------------------------------------
// T4: DELETE emits exactly one structured-log event after commit
// ---------------------------------------------------------------------------

// p6DeleteAuditSink records delete events and checks, at Emit time, that the
// delete has already committed (live row gone, history cascaded).
type p6DeleteAuditSink struct {
	store    store.Store
	err      error
	calls    int
	events   []auditevent.EnvelopeV1
	rendered [][]byte
	problems []string
}

func (s *p6DeleteAuditSink) Emit(ctx context.Context, event auditevent.EnvelopeV1) error {
	s.calls++
	s.events = append(s.events, event)
	if event.Resource != nil {
		if _, err := s.store.GetAccessConstraint(ctx, event.Resource.ID); !errors.Is(err, store.ErrNotFound) {
			s.problems = append(s.problems, fmt.Sprintf("constraint still visible at emit: %v", err))
		}
		rows, err := s.store.ListConstraintHistory(ctx, event.Resource.ID)
		if err != nil || len(rows) != 0 {
			s.problems = append(s.problems, fmt.Sprintf("history not cascaded at emit: rows=%d err=%v", len(rows), err))
		}
	}
	rendered, err := auditevent.Render(event)
	if err != nil {
		s.problems = append(s.problems, "render: "+err.Error())
	}
	s.rendered = append(s.rendered, rendered)
	return s.err
}

func TestGovernanceDeleteAudit_OneEnvelopeAfterCommit(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID, created := p6SeedUpdatableConstraint(t, gs, ps, s, "p6-delete-secret-name")
	deleteSink := &p6DeleteAuditSink{store: s}
	gs.deleteAuditSink = deleteSink
	createSink := auditevent.NewCaptureSink()
	gs.auditSink = createSink

	result, err := p6CommitDelete(t, p6OperationContext(t, "request-p6-delete"), gs, ps, created, pvTestActor(adminID))
	require.NoError(t, err)
	assert.Equal(t, "delete", result.Operation)
	assert.Equal(t, ClassificationRelax, result.Classification)
	p6RequireUUID(t, result.AuditID)

	require.Equal(t, 1, deleteSink.calls, "exactly one delete event")
	assert.Empty(t, deleteSink.problems, "the event is dispatched only after the delete commits")
	assert.Empty(t, createSink.Records(), "DELETE uses its own sink, not the CREATE sink")
	event := deleteSink.events[0]
	assert.Equal(t, result.AuditID, event.EventID, "delete auditId is the log event's event_id")
	assert.Equal(t, "access_boundary", event.Family)
	assert.Equal(t, "delete", event.Action)
	assert.Equal(t, auditevent.PhaseCommit, event.Phase)
	assert.Equal(t, auditevent.OutcomeSucceeded, event.Outcome)
	assert.Equal(t, auditevent.SeverityInfo, event.Severity)
	assert.Equal(t, "request-p6-delete", event.CorrelationID)
	require.NotNil(t, event.Principal)
	assert.Equal(t, auditevent.IdentityRef{Kind: auditevent.IdentityUser, ID: adminID}, *event.Principal)
	require.NotNil(t, event.Resource)
	assert.Equal(t, auditevent.ResourceRef{Kind: "access_constraint", ID: created.ID, Scope: auditevent.ResourceScopeSystem}, *event.Resource)

	var rendered struct {
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(deleteSink.rendered[0], &rendered))
	leaves := make([]string, 0, len(rendered.Payload))
	for leaf := range rendered.Payload {
		leaves = append(leaves, leaf)
	}
	sort.Strings(leaves)
	assert.Equal(t, []string{"before_revision", "classification"}, leaves,
		"the payload holds only classification and before_revision")
	assert.Equal(t, "relax", rendered.Payload["classification"])
	assert.EqualValues(t, created.Revision, rendered.Payload["before_revision"])

	// Redaction: the same set as the CREATE redaction test.
	text := string(deleteSink.rendered[0])
	for _, sensitive := range []string{
		"p6-delete-secret-name-admin", "p6-delete-secret-name-target",
		created.Name, created.Purpose, "@",
	} {
		assert.NotContains(t, text, sensitive)
	}
}

func TestGovernanceDeleteAudit_SinkFailurePreservesCommit(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID, created := p6SeedUpdatableConstraint(t, gs, ps, s, "p6-delete-sink-failure")
	const secretCanary = "SECRET-DELETE-SINK-CANARY-2404"
	deleteSink := &p6DeleteAuditSink{store: s, err: errors.New(secretCanary)}
	gs.deleteAuditSink = deleteSink
	logCapture := &capturingHandler{}
	gs.logger = slog.New(logCapture)

	result, err := p6CommitDelete(t, p6OperationContext(t, "request-p6-delete-sink"), gs, ps, created, pvTestActor(adminID))
	require.NoError(t, err, "a sink failure never fails or compensates the committed delete")
	require.Equal(t, 1, deleteSink.calls)
	assert.Empty(t, deleteSink.problems)
	p6RequireUUID(t, result.AuditID)
	_, err = s.GetAccessConstraint(context.Background(), created.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the delete stays committed")

	var failure map[string]any
	for _, record := range logCapture.all() {
		attrs := recordAttrs(record)
		assert.NotContains(t, record.Message, secretCanary)
		for key, value := range attrs {
			assert.NotContains(t, key, secretCanary)
			assert.NotContains(t, fmt.Sprint(value), secretCanary)
		}
		if record.Message == "failed to dispatch committed access boundary audit event" {
			failure = attrs
		}
	}
	require.NotNil(t, failure)
	assert.Equal(t, "audit_sink_emit_failed", failure["failure_code"])
	assert.Equal(t, result.AuditID, failure["event_id"])
	assert.Equal(t, created.ID, failure["constraint_id"])
	assert.NotContains(t, failure, "error")
}

// O2: a delete whose log event cannot be built still commits; the failure
// is logged with its own code and no error text, nothing is dispatched, and
// the delete's auditId is empty.
func TestGovernanceDeleteAudit_BuildFailurePreservesCommit(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID, created := p6SeedUpdatableConstraint(t, gs, ps, s, "p6-delete-build-failure")
	deleteSink := &p6DeleteAuditSink{store: s}
	gs.deleteAuditSink = deleteSink
	logCapture := &capturingHandler{}
	gs.logger = slog.New(logCapture)
	// A malformed trusted correlation passes preview validation and
	// authorization but fails envelope validation in the post-commit build.
	ctx := auditevent.ContextWithOperation(context.Background(), auditevent.AuditOperationContext{CorrelationID: "bad\ncorrelation"})

	result, err := p6CommitDelete(t, ctx, gs, ps, created, pvTestActor(adminID))
	require.NoError(t, err, "a build failure never fails or compensates the committed delete")
	require.NotNil(t, result)
	assert.Empty(t, result.AuditID, "no event was built")
	assert.Zero(t, deleteSink.calls, "nothing is dispatched")
	_, err = s.GetAccessConstraint(context.Background(), created.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the delete stays committed")

	var failure map[string]any
	for _, record := range logCapture.all() {
		if record.Message == "failed to build committed access boundary delete audit event" {
			require.Nil(t, failure, "exactly one build-failure line")
			failure = recordAttrs(record)
		}
	}
	require.NotNil(t, failure)
	assert.Equal(t, "audit_event_build_failed", failure["failure_code"])
	assert.Equal(t, created.ID, failure["constraint_id"])
	assert.NotContains(t, failure, "error")
}

// R16 Q1: the production constructor routes DELETE through a sink over the
// server's shared asynchronous audit writer, and leaves CREATE on its own
// synchronous sink.
func TestInitBoundaryServices_DeleteSinkUsesSharedAsyncWriter(t *testing.T) {
	srv, s := b7TestServer(t)
	gs := srv.governanceService
	require.NotNil(t, gs)
	require.NotNil(t, srv.auditWriter)
	require.NotNil(t, gs.deleteAuditSink, "initBoundaryServices must inject the delete sink")
	assert.Same(t, srv.decisionAuditLogger.sink, gs.deleteAuditSink,
		"the delete sink is the structured-log sink over s.auditWriter")
	assert.NotSame(t, gs.auditSink, gs.deleteAuditSink, "CREATE keeps its synchronous sink")

	targetID := pvSeedUser(t, s, "p6-prod-sink-target")
	enqueuedBefore := srv.auditWriter.Snapshot().Enqueued
	created, err := commitAuditedConstraint(t, p6OperationContext(t, "request-audit-create"), gs, srv.previewService, &store.AccessConstraint{
		Name:                 "p6-prod-sink-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   &targetID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "p6 production sink",
		CreatedBy:            DevUserID,
	}, pvTestActor(DevUserID))
	require.NoError(t, err)
	assert.Equal(t, enqueuedBefore, srv.auditWriter.Snapshot().Enqueued, "CREATE does not use the async writer")

	result, err := p6CommitDelete(t, p6OperationContext(t, "request-p6-prod-delete"), gs, srv.previewService, created.Constraint, pvTestActor(DevUserID))
	require.NoError(t, err)
	p6RequireUUID(t, result.AuditID)
	assert.Equal(t, enqueuedBefore+1, srv.auditWriter.Snapshot().Enqueued,
		"the committed delete enqueues exactly one record on the shared async writer")
}

// ---------------------------------------------------------------------------
// T8: auditId on UPDATE and DELETE responses is a UUID
// ---------------------------------------------------------------------------

func TestAccessConstraintMutationResponses_AuditIDIsUUID(t *testing.T) {
	srv, s := b7TestServer(t)
	// The request-log middleware mints the request ID the handlers put on
	// the audit request ref (a test server has no request logger by default).
	srv.SetRequestLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	deleteSink := &p6DeleteAuditSink{store: s}
	srv.governanceService.deleteAuditSink = deleteSink
	targetID := pvSeedUser(t, s, "p6-http-target")
	draft := func(permissions []string) map[string]any {
		return map[string]any{
			"name":    "p6-http-boundary",
			"purpose": "p6 http auditId",
			"subject": map[string]any{
				"kind":          "principal",
				"principalType": "user",
				"principalId":   targetID,
			},
			"scope":              map[string]any{"type": "system"},
			"maximumPermissions": permissions,
		}
	}
	previewToken := func(body map[string]any) string {
		t.Helper()
		response := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraint-previews", body)
		require.Equal(t, http.StatusOK, response.Code, "preview: %s", response.Body.String())
		var preview PreviewResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
		require.NotEmpty(t, preview.PreviewToken)
		return preview.PreviewToken
	}

	createBody := draft([]string{"agent.read", "agent.create"})
	createBody["previewToken"] = previewToken(map[string]any{"operation": "create", "draft": draft([]string{"agent.read", "agent.create"})})
	createResponse := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraints", createBody)
	require.Equal(t, http.StatusCreated, createResponse.Code, "create: %s", createResponse.Body.String())
	var created mutationResponse
	require.NoError(t, json.Unmarshal(createResponse.Body.Bytes(), &created))

	updateBody := draft([]string{"agent.read"})
	updateBody["previewToken"] = previewToken(map[string]any{
		"operation":    "update",
		"constraintId": created.ID,
		"baseRevision": created.Revision,
		"draft":        draft([]string{"agent.read"}),
	})
	updateResponse := doRequestHeaders(t, srv, http.MethodPut, "/api/v1/admin/access-constraints/"+created.ID, updateBody,
		map[string]string{"If-Match": fmt.Sprintf(`"%s"`, created.Revision)})
	require.Equal(t, http.StatusOK, updateResponse.Code, "update: %s", updateResponse.Body.String())
	var updated mutationResponse
	require.NoError(t, json.Unmarshal(updateResponse.Body.Bytes(), &updated))
	p6RequireUUID(t, updated.AuditID)
	assert.False(t, strings.HasPrefix(updated.AuditID, "aud_"))

	// The UPDATE auditId is retrievable through the audit endpoint while the
	// constraint lives.
	auditResponse := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID+"/audit", nil)
	require.Equal(t, http.StatusOK, auditResponse.Code, "audit: %s", auditResponse.Body.String())
	page := decodeAuditPage(t, auditResponse.Body.Bytes())
	var listed bool
	for _, item := range page.Items {
		if item.ID == updated.AuditID {
			listed = true
			assert.Equal(t, "update", item.Operation)
		}
	}
	assert.True(t, listed, "UPDATE auditId must be a listed history event")

	deleteToken := previewToken(map[string]any{
		"operation":    "delete",
		"constraintId": created.ID,
		"baseRevision": updated.Revision,
	})
	deleteResponse := doRequestHeaders(t, srv, http.MethodDelete, "/api/v1/admin/access-constraints/"+created.ID, nil,
		map[string]string{"X-Preview-Token": deleteToken})
	require.Equal(t, http.StatusOK, deleteResponse.Code, "delete: %s", deleteResponse.Body.String())
	var deleted struct {
		AuditID string `json:"auditId"`
	}
	require.NoError(t, json.Unmarshal(deleteResponse.Body.Bytes(), &deleted))
	p6RequireUUID(t, deleted.AuditID)
	assert.False(t, strings.HasPrefix(deleted.AuditID, "aud_"))
	assert.NotEqual(t, updated.AuditID, deleted.AuditID)

	// O1: the DELETE envelope carries the same request ref as CREATE.
	require.Equal(t, 1, deleteSink.calls)
	assert.Empty(t, deleteSink.problems)
	event := deleteSink.events[0]
	assert.Equal(t, deleted.AuditID, event.EventID)
	requestID := deleteResponse.Header().Get("X-Request-ID")
	require.NotEmpty(t, requestID)
	require.NotNil(t, event.Request)
	assert.Equal(t, auditevent.RequestRef{
		ID:      requestID,
		Method:  http.MethodDelete,
		Route:   "/api/v1/admin/access-constraints/{id}",
		Surface: "api",
	}, *event.Request)
	assert.Equal(t, requestID, event.CorrelationID)
	assert.NotContains(t, string(deleteSink.rendered[0]), "@", "no email in the envelope")

	// The DELETE auditId names a log event only: the deleted constraint's
	// history endpoint answers the non-enumerating 404.
	gone := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, gone.Code, "body: %s", gone.Body.String())
}

// ---------------------------------------------------------------------------
// T6: the process-memory boundary audit writer is gone from production code
// ---------------------------------------------------------------------------

func TestBoundaryAuditWriterRemoved(t *testing.T) {
	forbiddenIdents := map[string]bool{
		"BoundaryAuditWriter":     true,
		"NewBoundaryAuditWriter":  true,
		"BoundaryAuditEntry":      true,
		"WriteAuditEntry":         true,
		"GetEntriesForConstraint": true,
		"compensateAuditFailure":  true,
		"computeStateFingerprint": true,
		"generateAuditID":         true,
		"validateAuditEntry":      true,
		"rollbackDisable":         true,
		"rollbackRecovery":        true,
		"computeImpactCounts":     true,
	}
	// AuditRequest stays a CommitRequest field name, so only a type
	// declaration with these names is forbidden.
	forbiddenTypes := map[string]bool{"AuditRequest": true, "ImpactCounts": true}

	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs: pkg/hub and every area moved out of it.
	files, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	fset := token.NewFileSet()
	var found []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, parseErr, file)
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				if forbiddenIdents[n.Name] {
					found = append(found, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), n.Name))
				}
			case *ast.TypeSpec:
				if forbiddenTypes[n.Name.Name] {
					found = append(found, fmt.Sprintf("%s: type %s", fset.Position(n.Pos()), n.Name.Name))
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && strings.Contains(n.Value, "aud_") {
					found = append(found, fmt.Sprintf("%s: literal %s", fset.Position(n.Pos()), n.Value))
				}
			}
			return true
		})
	}
	assert.Empty(t, found, "removed boundary audit writer symbols must have zero production references")
}
