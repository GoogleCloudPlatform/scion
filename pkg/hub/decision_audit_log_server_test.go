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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Production-constructor tests for decision logging (remaining-audit P1,
// ACs P1-3..P1-7, P1-9, P1-11). Each test installs a capturing handler
// with slog.SetDefault before New (the production capture point), builds
// the server through the real constructor, restores the previous default
// immediately after New, and drives real HTTP requests. Records are
// written asynchronously, so tests synchronize by closing the audit writer
// (which drains it) or on the capture's entered channel; there are no
// sleeps. These tests change the process default logger and must not run
// in parallel.

// newDecisionLogServer builds a full-chain server (live
// OperationalSettings, request logger installed) whose audit writer wraps
// capture.
func newDecisionLogServer(t *testing.T, capture slog.Handler) (*Server, store.Store) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	s := newBareTestStore(t)
	srv := newTestServerFromStore(t, s, nil)
	slog.SetDefault(prev) // the writer captured the handler at New
	// Production always installs a request logger; it is what assigns the
	// request ID used as the correlation ID.
	srv.SetRequestLogger(slog.New(slog.DiscardHandler))
	return srv, s
}

func setDecisionLogFlag(t *testing.T, srv *Server, on bool) {
	t.Helper()
	ops := srv.GetOperationalSettings()
	require.NotNil(t, ops)
	rev := ops.ExperimentsSnapshot().Revision
	doc := fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.AuthorizationDecisionAuditV2, on)
	_, err := ops.Update(context.Background(), "experiments", json.RawMessage(doc), "test", rev, "managed")
	require.NoError(t, err)
	require.Equal(t, on, srv.experimentEnabled(experiments.AuthorizationDecisionAuditV2))
}

// seedDecisionLogProject creates a project, a member with project read and
// an outsider without it.
func seedDecisionLogProject(t *testing.T, s store.Store) (member, outsider *store.User, projectID string) {
	t.Helper()
	projectID = tid("dl-project")
	createDelegateTestProject(t, s, projectID, "dl-project", "test")
	member = createTestUser(t, s, "dl-member", "dl-member@example.com")
	createTestUserWithProjectRole(t, s, member.ID, member.Email, projectID, store.ProjectRoleMember)
	outsider = createTestUser(t, s, "dl-outsider", "dl-outsider@example.com")
	return member, outsider, projectID
}

// userRequest sends a request authenticated with a real user session token.
func userRequest(t *testing.T, srv *Server, user *store.User, method, path string, body []byte, headers map[string]string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec, token
}

// seedDecisionLogAdmin creates a hub-admin user (holds agent.stop_all on
// the hub).
func seedDecisionLogAdmin(t *testing.T, s store.Store) *store.User {
	t.Helper()
	id := tid("dl-admin")
	createTestUserWithRole(t, s, id, "dl-admin@example.com", "admin", store.SystemRoleHubAdmin)
	ensureHubMembership(context.Background(), s, id)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// stopAllPath is POST /api/v1/agents/stop-all (global). Its handler decides
// agent.stop_all on Resource{Type: "agent", ID: "hub"} with an explicit
// Permission, which is a system-scoped, in-domain decision.
const stopAllPath = "/api/v1/agents/stop-all"

func membersPath(projectID string) string { return "/api/v1/projects/" + projectID + "/members" }

func auditGroup(t *testing.T, line map[string]any, key string) map[string]any {
	t.Helper()
	g, ok := line[key].(map[string]any)
	require.True(t, ok, "missing group %q in %v", key, line)
	return g
}

// P1-3. Reports the real routes that produce system-scoped records. A
// decision is in the recorded domain only when its caller passed an explicit
// registered Permission (Decision.PermissionID comes only from
// AuthzRequest.Permission), on an unparented project or agent resource.
// s.authorize/CheckAccess sets no Permission, so project routes such as
// GET /api/v1/projects/{id}/members are excluded_permission (asserted
// below). The measured in-domain route is POST /api/v1/agents/stop-all:
// a hub admin is allowed and a member is denied.
func TestDecisionLog_P1_3_ProductionConstructorProjectRoute(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, _, projectID := seedDecisionLogProject(t, s)
	admin := seedDecisionLogAdmin(t, s)
	setDecisionLogFlag(t, srv, true)
	counts := srv.decisionAuditLogger.counts

	allowRec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusOK, allowRec.Code, allowRec.Body.String())
	allowID := allowRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, allowID)
	require.Equal(t, uint64(1), counts.get(decisionAuditEnqueued, true), "fail fast: the allow decision must be in domain")

	denyRec, _ := userRequest(t, srv, member, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusForbidden, denyRec.Code, denyRec.Body.String())
	denyID := denyRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, denyID)
	require.Equal(t, uint64(1), counts.get(decisionAuditEnqueued, false), "fail fast: the deny decision must be in domain")

	// The authorize-path project route is excluded_permission and logs
	// nothing.
	beforePerm := counts.get(decisionAuditExcludedPermission, true)
	membersRec, _ := userRequest(t, srv, member, http.MethodGet, membersPath(projectID), nil, nil)
	require.Equal(t, http.StatusOK, membersRec.Code, membersRec.Body.String())
	membersID := membersRec.Header().Get("X-Request-ID")
	assert.Greater(t, counts.get(decisionAuditExcludedPermission, true), beforePerm)

	require.NoError(t, srv.CloseAuditWriter(context.Background())) // drains
	assert.Empty(t, capture.recordsFor(membersID), "authorize-path project route is not recorded")

	allow := capture.recordsFor(allowID)
	require.Len(t, allow, 1, "exactly one scion.audit record for the allowed request; all: %v", capture.records())
	line := allow[0]
	assert.Equal(t, "authorization", line["family"])
	assert.Equal(t, "decide", line["action"])
	assert.Equal(t, "decision", line["phase"])
	assert.Equal(t, "allow", line["outcome"])
	assert.Equal(t, "info", line["severity"])
	assert.Equal(t, "INFO", line["level"])
	assert.Equal(t, allowID, line["correlation_id"])
	assert.Equal(t, allowID, auditGroup(t, line, "request")["id"])
	assert.Equal(t, map[string]any{"kind": "user", "id": admin.ID}, auditGroup(t, line, "principal"))
	assert.Equal(t, map[string]any{"kind": "agent", "id": "hub"}, auditGroup(t, line, "resource"))
	payload := auditGroup(t, line, "payload")
	assert.Equal(t, "agent.stop_all", payload["permission_id"])
	assert.Equal(t, "false", payload["sampled"])
	assert.NotEmpty(t, payload["reason"])
	if cred, ok := line["credential"].(map[string]any); ok {
		assert.NotContains(t, cred, "name")
		assert.NotContains(t, cred, "labels")
	}
	for _, absent := range []string{"policy", "policy_id", "matched_policy", "matched_grant", "actor", "purpose"} {
		assert.NotContains(t, line, absent)
		assert.NotContains(t, payload, absent)
	}

	deny := capture.recordsFor(denyID)
	require.Len(t, deny, 1, "exactly one scion.audit record for the denied request; all: %v", capture.records())
	assert.Equal(t, "deny", deny[0]["outcome"])
	assert.Equal(t, "warning", deny[0]["severity"])
	assert.Equal(t, "WARN", deny[0]["level"])
	assert.Equal(t, map[string]any{"kind": "user", "id": member.ID}, auditGroup(t, deny[0], "principal"))

	snap := srv.auditWriter.Snapshot()
	assert.Equal(t, uint64(2), snap.Enqueued, "%+v", snap)
	assert.Equal(t, snap.Enqueued, snap.Written, "every enqueued record was written: %+v", snap)
}

// P1-4. Flag off (the default): zero records, disabled counted.
func TestDecisionLog_P1_4_FlagOffRecordsNothing(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, outsider, projectID := seedDecisionLogProject(t, s)
	require.False(t, srv.experimentEnabled(experiments.AuthorizationDecisionAuditV2), "default must be off")

	rec, _ := userRequest(t, srv, member, http.MethodGet, membersPath(projectID), nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	rec, _ = userRequest(t, srv, outsider, http.MethodGet, membersPath(projectID), nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Explicitly off after having been on behaves the same.
	setDecisionLogFlag(t, srv, true)
	setDecisionLogFlag(t, srv, false)
	rec, _ = userRequest(t, srv, member, http.MethodGet, membersPath(projectID), nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	assert.Empty(t, capture.records())
	counts := srv.decisionAuditLogger.counts
	assert.GreaterOrEqual(t, counts.get(decisionAuditDisabled, true), uint64(2))
	assert.GreaterOrEqual(t, counts.get(decisionAuditDisabled, false), uint64(1))
	for d := decisionAuditDisposition(0); d < decisionAuditDispositionCount; d++ {
		if d == decisionAuditDisabled {
			continue
		}
		assert.Zero(t, counts.get(d, true)+counts.get(d, false), "disposition %s", d)
	}
	assert.Zero(t, srv.auditWriter.Snapshot().Enqueued)
}

// The experiment fails closed: a malformed experiments row (even one that
// names the flag) means disabled, through the production constructor.
func TestDecisionLog_MalformedExperimentsRowFailsClosed(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, _, projectID := seedDecisionLogProject(t, s)
	_, err := s.UpsertHubSetting(context.Background(), "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:"yes"}}`, experiments.AuthorizationDecisionAuditV2)), "other-replica", 0, "managed")
	require.NoError(t, err)
	_, err = srv.GetOperationalSettings().Refresh(context.Background())
	require.NoError(t, err)
	require.True(t, srv.GetOperationalSettings().ExperimentsSnapshot().Malformed, "fixture must be malformed")
	require.False(t, srv.experimentEnabled(experiments.AuthorizationDecisionAuditV2))

	rec, _ := userRequest(t, srv, member, http.MethodGet, membersPath(projectID), nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	assert.Empty(t, capture.records())
	assert.GreaterOrEqual(t, srv.decisionAuditLogger.counts.get(decisionAuditDisabled, true), uint64(1))
	assert.Zero(t, srv.auditWriter.Snapshot().Enqueued)
}

// P1-5 production-constructor case: a decision outside the resource domain
// is counted excluded_resource and not logged.
func TestDecisionLog_P1_5_ExcludedResourceThroughServer(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, _, _ := seedDecisionLogProject(t, s)
	setDecisionLogFlag(t, srv, true)

	identity := NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, member.Role, "web")
	ctx := logging.ContextWithRequestMeta(contextWithIdentity(context.Background(), identity),
		&logging.RequestMeta{RequestID: "dl-excluded-resource"})
	before := srv.decisionAuditLogger.counts.get(decisionAuditExcludedResource, false) +
		srv.decisionAuditLogger.counts.get(decisionAuditExcludedResource, true)
	srv.authzService.Decide(ctx, AuthzRequestFromContext(ctx, Resource{Type: "template", ID: "tpl-1"}, ActionRead))
	after := srv.decisionAuditLogger.counts.get(decisionAuditExcludedResource, false) +
		srv.decisionAuditLogger.counts.get(decisionAuditExcludedResource, true)
	assert.Equal(t, before+1, after)

	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	assert.Empty(t, capture.recordsFor("dl-excluded-resource"))
	assert.Zero(t, srv.auditWriter.Snapshot().Enqueued)
}

// P1-6 (with architect ruling R2, msg 7f72a355). Authorization outcomes and
// HTTP results are unchanged while the audit writer is blocked
// (noncooperative inner handler) and full. Probes drive every adapter
// branch a real Decide can reach (in-domain user and agent principals, the
// delegated-agent AlwaysAudit allow, UAT bearer allow and gate deny with
// credential decoration, a dependency-unavailable deny, and the
// excluded_scope/principal/correlation/credential dispositions); each must
// match the flag-off baseline and move its expected disposition counter.
// It also pins panic isolation (sink and mapping faults through test seams)
// and that emission mutates nothing reachable from the Decision or the
// request. The test finishes while the inner handler is still blocked; it
// is released only in cleanup.
func TestDecisionLog_P1_6_OutcomesUnchangedWithWriterBlockedAndFull(t *testing.T) {
	capture := &decisionLogCapture{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	srv, s := newDecisionLogServer(t, capture)
	l := srv.decisionAuditLogger
	// Registered after the server's Shutdown cleanup, so it runs first.
	t.Cleanup(func() {
		close(capture.block)
		if err := srv.CloseAuditWriter(context.Background()); err != nil {
			t.Errorf("CloseAuditWriter after release: %v", err)
		}
		final := srv.auditWriter.Snapshot()
		if final.Enqueued != final.Written+final.WriteErrors+final.WriteTimeouts+final.DroppedShutdown {
			t.Errorf("conservation after drain: %+v", final)
		}
	})
	ctx := context.Background()
	member, outsider, projectID := seedDecisionLogProject(t, s)

	// Agent fixtures (owner-delegated agents) on their own project.
	dcProject, dcOwner := tid("dl6-dc-project"), tid("dl6-dc-owner")
	liveAgent, errAgent := tid("dl6-live-agent"), tid("dl6-err-agent")
	createDCProject(t, s, dcProject, "dl6-dc-project")
	createDCUser(t, s, dcOwner, "dl6-owner@example.com", dcProject, store.ProjectRoleOwner)
	for _, agentID := range []string{liveAgent, errAgent} {
		createDCAgent(t, s, agentID, dcProject, dcOwner, AgentRoleFull)
		createDCEdge(t, s, store.DelegationPrincipalUser, dcOwner, store.DelegationPrincipalAgent, agentID,
			store.RoleScopeProject, dcProject, string(AgentRoleFull))
	}
	// Dependency-unavailable (H2-style) path: a delegation-edge store fault,
	// emitting through the production decision logger.
	faultyAuthz := NewAuthzService(&edgeLookupErrStore{Store: s, failID: errAgent}, slog.Default())
	faultyAuthz.SetDecisionAuditEmitter(l)
	hubDelivery, err := srv.authzService.newHubDeliveryIdentity(ctx, liveAgent)
	require.NoError(t, err)

	// UAT bearer fixtures: a project-scoped token on its own project.
	uatProject, uatOwner := setupUATProjectAndOwner(t, s, "dl6-uat")
	uatKey := mintScopedUAT(t, srv, uatOwner, uatProject, []string{"project:read"})
	uatIdentity, err := srv.uatService.ValidateToken(ctx, uatKey)
	require.NoError(t, err)
	require.NotNil(t, uatIdentity.Decoration(), "production UAT identity carries its decoration")
	admin := seedDecisionLogAdmin(t, s)

	withRequestID := func(c context.Context, id string) context.Context {
		return logging.ContextWithRequestMeta(c, &logging.RequestMeta{RequestID: id})
	}
	asIdentity := func(id Identity) func(string) context.Context {
		return func(reqID string) context.Context { return withRequestID(contextWithIdentity(ctx, id), reqID) }
	}
	uatCtx := func(reqID string) context.Context {
		c := contextWithCredentialContext(contextWithIdentity(ctx, uatIdentity), credentialContextForIdentity(uatIdentity))
		return withRequestID(c, reqID)
	}
	asUser := func(u *store.User) func(string) context.Context {
		return asIdentity(NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web"))
	}
	projectReq := func(id string, action Action) func(context.Context) AuthzRequest {
		return func(c context.Context) AuthzRequest {
			return AuthzRequestFromContext(c, Resource{Type: "project", ID: id}, action)
		}
	}
	withPermission := func(f func(context.Context) AuthzRequest, perm string) func(context.Context) AuthzRequest {
		return func(c context.Context) AuthzRequest { r := f(c); r.Permission = perm; return r }
	}

	type probe struct {
		name        string
		authz       *AuthzService
		ctx         func(reqID string) context.Context
		req         func(context.Context) AuthzRequest
		want        decisionAuditDisposition
		alwaysAudit bool
	}
	probes := []probe{
		// In-domain user decisions carry an explicit registered Permission
		// (the only source of Decision.PermissionID).
		{name: "member read allow", ctx: asUser(member), req: withPermission(projectReq(projectID, ActionRead), "project.read"), want: decisionAuditNotEnqueued},
		{name: "outsider read deny", ctx: asUser(outsider), req: withPermission(projectReq(projectID, ActionRead), "project.read"), want: decisionAuditNotEnqueued},
		{name: "member delete deny", ctx: asUser(member), req: withPermission(projectReq(projectID, ActionDelete), "project.delete"), want: decisionAuditNotEnqueued},
		{name: "outsider manage deny", ctx: asUser(outsider), req: withPermission(projectReq(projectID, ActionManage), "project.manage"), want: decisionAuditNotEnqueued},
		// The authorize/CheckAccess shape (no Permission) is excluded_permission.
		{name: "excluded_permission: authorize-shaped member read", ctx: asUser(member), req: projectReq(projectID, ActionRead), want: decisionAuditExcludedPermission},
		{name: "delegated agent allow", ctx: asIdentity(dcAgentIdentity(liveAgent, dcProject, AgentRoleFull)),
			req: withPermission(projectReq(dcProject, ActionRead), "project.read"), want: decisionAuditNotEnqueued},
		{name: "dependency-unavailable deny", authz: faultyAuthz, ctx: asIdentity(dcAgentIdentity(errAgent, dcProject, AgentRoleFull)),
			req: withPermission(projectReq(dcProject, ActionRead), "project.read"), want: decisionAuditNotEnqueued},
		{name: "excluded_scope: project-contained agent resource", ctx: asUser(member),
			req: func(c context.Context) AuthzRequest {
				return AuthzRequestFromContext(c, Resource{Type: "agent", ID: liveAgent, ParentType: "project", ParentID: dcProject}, ActionRead)
			}, want: decisionAuditExcludedScope},
		{name: "excluded_principal: dev principal", ctx: asIdentity(NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})),
			req: projectReq(projectID, ActionRead), want: decisionAuditExcludedPrincipal},
		{name: "excluded_correlation: no request ID", ctx: func(string) context.Context {
			return contextWithIdentity(ctx, NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, member.Role, "web"))
		}, req: withPermission(projectReq(projectID, ActionRead), "project.read"), want: decisionAuditExcludedCorrelation},
		{name: "excluded_credential: hub_delivery credential", ctx: asIdentity(hubDelivery),
			req: withPermission(projectReq(dcProject, ActionRead), "project.read"), want: decisionAuditExcludedCredential},
		// Synthetic (R3 #5): no production branch sets AlwaysAudit; this
		// covers that mapping path under a blocked, full writer.
		{name: "synthetic: request AlwaysAudit allow", ctx: asUser(member),
			req: func(c context.Context) AuthzRequest {
				r := withPermission(projectReq(projectID, ActionRead), "project.read")(c)
				r.AlwaysAudit = true
				return r
			}, want: decisionAuditNotEnqueued, alwaysAudit: true},
		// Synthetic (R3 #4): a UAT allow in the recorded domain has no real
		// route, so drive Decide from the production decoration context
		// (token validated by the UAT service, credential context built as
		// the bearer middleware does) with an explicit project.read.
		{name: "synthetic: UAT bearer allow with production decoration", ctx: uatCtx,
			req: withPermission(projectReq(uatProject, ActionRead), "project.read"), want: decisionAuditNotEnqueued},
	}
	run := func(p probe, reqID string) Decision {
		authz := p.authz
		if authz == nil {
			authz = srv.authzService
		}
		c := p.ctx(reqID)
		return authz.Decide(c, p.req(c))
	}
	moved := func(d decisionAuditDisposition) uint64 { return l.counts.get(d, true) + l.counts.get(d, false) }
	// HTTP probes: two authorize-path project requests (excluded_permission)
	// and three in-domain stop-all requests: hub-admin allow, member deny,
	// and a real UAT bearer gate deny with decoration (NewRef path).
	httpStatuses := func() []int {
		a, _ := userRequest(t, srv, member, http.MethodGet, membersPath(projectID), nil, nil)
		b, _ := userRequest(t, srv, outsider, http.MethodGet, membersPath(projectID), nil, nil)
		c, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
		d, _ := userRequest(t, srv, member, http.MethodPost, stopAllPath, nil, nil)
		e := doRequestWithUAT(t, srv, uatKey, http.MethodPost, stopAllPath, nil)
		return []int{a.Code, b.Code, c.Code, d.Code, e.Code}
	}
	assertParity := func(label string, want, got Decision) {
		t.Helper()
		assert.Equal(t, want.Allowed, got.Allowed, label)
		assert.Equal(t, want.Reason, got.Reason, label)
		assert.Equal(t, want.PermissionID, got.PermissionID, label)
		assert.Equal(t, want.DeniedBy, got.DeniedBy, label)
		assert.Equal(t, want.DenyCause, got.DenyCause, label)
		assert.Equal(t, want.AlwaysAudit, got.AlwaysAudit, label)
	}

	// Baseline: flag off (status quo).
	baseline := make([]Decision, len(probes))
	for i, p := range probes {
		before := moved(decisionAuditDisabled)
		baseline[i] = run(p, fmt.Sprintf("dl6-base-%d", i))
		require.Greater(t, moved(decisionAuditDisabled), before, "%s: baseline counted disabled", p.name)
		if p.alwaysAudit {
			require.True(t, baseline[i].Allowed, "%s: probe must be an allow: %+v", p.name, baseline[i])
		}
	}
	require.False(t, baseline[0].Allowed == baseline[1].Allowed, "probes must cover both allow and deny")
	require.Equal(t, DenyCauseCeilingError, baseline[6].DenyCause, "dependency-unavailable probe: %q", baseline[6].Reason)
	baseHTTP := httpStatuses()
	require.Equal(t, []int{http.StatusOK, http.StatusForbidden, http.StatusOK, http.StatusForbidden}, baseHTTP[:4], "baseline HTTP results")
	require.NotEqual(t, http.StatusOK, baseHTTP[4], "UAT gate deny on stop-all")

	// Flag on; block the worker in the inner handler, then fill the queue.
	setDecisionLogFlag(t, srv, true)
	run(probes[0], "dl6-fill-0")
	require.Equal(t, uint64(1), l.counts.get(decisionAuditEnqueued, true),
		"fail fast: the first record must be enqueued before waiting on the worker")
	<-capture.entered // the worker is now blocked inside the inner handler
	for i := 1; srv.auditWriter.Snapshot().DroppedFull == 0; i++ {
		require.Less(t, i, 3*2048, "queue never filled")
		run(probes[0], fmt.Sprintf("dl6-fill-%d", i))
	}
	snap := srv.auditWriter.Snapshot()
	require.Equal(t, 2048, snap.Queued, "count bound reached: %+v", snap)

	// 1. Every mapping branch: parity plus the expected disposition.
	for i, p := range probes {
		want := p.want
		if want == decisionAuditExcludedCredential && !isKnownPermission(baseline[i].PermissionID) {
			// Mapping checks permission before credential; record which
			// disposition this real decision reaches instead.
			t.Logf("%s: decision has no registered permission (%q); excluded_credential not reachable through Decide, expecting excluded_permission",
				p.name, baseline[i].PermissionID)
			want = decisionAuditExcludedPermission
		}
		before := moved(want)
		got := run(p, fmt.Sprintf("dl6-full-%d", i))
		assertParity(p.name, baseline[i], got)
		assert.Greater(t, moved(want), before, "%s: disposition %s did not move", p.name, want)
	}
	beforeHTTP := moved(decisionAuditNotEnqueued)
	beforePerm := moved(decisionAuditExcludedPermission)
	beforeCred := moved(decisionAuditExcludedCredential)
	assert.Equal(t, baseHTTP, httpStatuses(), "HTTP results unchanged (members ×2, stop-all admin/member, UAT gate deny)")
	assert.GreaterOrEqual(t, moved(decisionAuditNotEnqueued), beforeHTTP+3, "each stop-all probe reached the full writer")
	assert.GreaterOrEqual(t, moved(decisionAuditExcludedPermission), beforePerm+2, "authorize-path probes are excluded_permission")
	assert.Equal(t, beforeCred, moved(decisionAuditExcludedCredential),
		"UAT decoration maps to a credential reference (NewRef path), never excluded_credential")

	// 2. Panic isolation through test seams: the decision is returned
	// unchanged and the panic is counted, not propagated.
	savedSink := l.sink
	l.sink = panickingAuditSink{}
	before := moved(decisionAuditNotEnqueued)
	require.NotPanics(t, func() { assertParity("sink panic", baseline[0], run(probes[0], "dl6-panic-sink")) })
	l.sink = savedSink
	assert.Equal(t, before+1, moved(decisionAuditNotEnqueued), "sink panic counted not_enqueued")

	savedEnabled := l.enabled
	l.enabled = func() bool { panic("injected mapping fault") }
	before = moved(decisionAuditInvalid)
	require.NotPanics(t, func() { assertParity("mapping panic", baseline[1], run(probes[1], "dl6-panic-map")) })
	l.enabled = savedEnabled
	assert.Equal(t, before+1, moved(decisionAuditInvalid), "mapping panic counted invalid")

	// 3. No shared mutation: everything reachable from the Decision and the
	// request that the record builder and adapter read is identical after
	// emission (snapshot by value through JSON of the exported graph).
	for _, res := range []Resource{
		{Type: "project", ID: projectID, OwnerID: member.ID, Labels: map[string]string{"team": "a"}},
		{Type: "agent", ID: liveAgent, ParentType: "project", ParentID: dcProject, Ancestry: []string{"root", dcProject}, Labels: map[string]string{"k": "v"}},
	} {
		c := withRequestID(contextWithIdentity(ctx, NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, member.Role, "web")), "dl6-mutation")
		req := AuthzRequestFromContext(c, res, ActionRead)
		req.Permission = res.Type + ".read" // in-domain mapping path for the project case
		req.Actor = &DecisionActor{Kind: PrincipalKindUser, ID: member.ID}
		req.Purpose = "parity-check"
		dec := srv.authzService.decide(c, req)
		dec.ExplainTrace = []DecisionStep{{Step: "s", Detail: "d"}}
		dec.Actor = &DecisionActor{Kind: PrincipalKindUser, ID: member.ID}
		snapshot := func() string {
			b, err := json.Marshal(struct {
				D        Decision
				R        Resource
				A        *DecisionActor
				P        string
				Perm     string
				Always   bool
				Princ    string
				CredKind CredentialKind
			}{dec, req.Resource, req.Actor, req.Purpose, req.Permission, req.AlwaysAudit, req.Principal.ID, req.Credential.Kind})
			require.NoError(t, err)
			return string(b)
		}
		pre := snapshot()
		srv.authzService.emitDecisionAudit(c, req, dec)
		assert.Equal(t, pre, snapshot(), "emission mutated the decision or request (%s)", res.Type)
	}
}

// P1-7. Health: healthy, then degraded on a write failure; the composite
// is degraded (never unhealthy), readiness is unaffected, and the retired
// keys are absent.
func TestDecisionLog_P1_7_HealthReportsWriterFailures(t *testing.T) {
	capture := &decisionLogCapture{entered: make(chan struct{}, 4), fail: errors.New("inner handler down")}
	srv, s := newDecisionLogServer(t, capture)

	info := srv.GetHealthInfo(context.Background())
	assert.Equal(t, "healthy", info.Checks[auditLogWriterHealthKey])
	assert.Equal(t, HealthStatusHealthy, info.Status)
	assert.False(t, criticalHealthChecks[auditLogWriterHealthKey])
	for key := range info.Checks {
		assert.False(t, strings.HasPrefix(key, "authorization_decision_audit_"), "retired key %s present", key)
	}

	setDecisionLogFlag(t, srv, true)
	admin := seedDecisionLogAdmin(t, s)
	for i := 0; i < 2; i++ {
		rec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, "outcome unaffected by the failing writer")
	}
	require.Equal(t, uint64(2), srv.decisionAuditLogger.counts.get(decisionAuditEnqueued, true),
		"fail fast: both records must be enqueued before waiting on the worker")
	// The single worker counts record 1's outcome before it starts record 2.
	<-capture.entered
	<-capture.entered

	info = srv.GetHealthInfo(context.Background())
	assert.Equal(t, "degraded: recent write failures", info.Checks[auditLogWriterHealthKey])
	assert.Equal(t, HealthStatusDegraded, info.Status)
	health := httptest.NewRecorder()
	srv.handleHealthz(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, health.Code)
	assert.Contains(t, health.Body.String(), `"audit_log_writer":"degraded: recent write failures"`)
	ready := httptest.NewRecorder()
	srv.handleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, ready.Code)
	assert.GreaterOrEqual(t, srv.auditWriter.Snapshot().WriteErrors, uint64(1))
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var summary HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &summary))
	assert.Equal(t, HealthStatusDegraded, summary.Status)
	assert.Equal(t, "degraded: recent write failures", summary.Hub.Checks[auditLogWriterHealthKey])
	assert.Contains(t, summary.Hub.UnhealthyChecks, auditLogWriterHealthKey+": degraded: recent write failures")
	assert.Equal(t, "healthy", summary.Database.Status)

	// Closed while still serving.
	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	info = srv.GetHealthInfo(context.Background())
	assert.Equal(t, "degraded: writer closed", info.Checks[auditLogWriterHealthKey])
	assert.Equal(t, HealthStatusDegraded, info.Status)
}

// P1-9 (architect ruling, msg e50dbb8d): Server.Shutdown closes the audit
// writer after the HTTP drain, so a record emitted by a handler that is
// still in flight during the drain is written, not dropped as closed.
func TestDecisionLog_P1_9_ShutdownClosesWriterAfterDrain(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, _ := newDecisionLogServer(t, capture)
	setDecisionLogFlag(t, srv, true)

	drainStarted := make(chan struct{})
	inHandler := make(chan struct{})
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-drainStarted // Shutdown is draining while this request is in flight
		rec := inDomainDecisionRecord()
		rec.CredentialType, rec.CredentialID = "", ""
		rec.CredentialBoundaryKind, rec.CredentialBoundaryProjectID = "", ""
		srv.decisionAuditLogger.EmitDecisionAudit(decisionCtx(), rec)
		w.WriteHeader(http.StatusNoContent)
	})}
	hs.RegisterOnShutdown(func() { close(drainStarted) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = hs.Serve(ln) }()
	srv.mu.Lock()
	srv.httpServer = hs
	srv.mu.Unlock()

	respDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			respDone <- -1
			return
		}
		_ = resp.Body.Close()
		respDone <- resp.StatusCode
	}()
	<-inHandler
	require.NoError(t, srv.Shutdown(context.Background()))
	require.Equal(t, http.StatusNoContent, <-respDone)

	snap := srv.auditWriter.Snapshot()
	assert.True(t, snap.Closed)
	assert.Equal(t, uint64(1), snap.Written, "%+v", snap)
	assert.Zero(t, snap.DroppedClosed, "%+v", snap)
	assert.Len(t, capture.recordsFor(testDecisionRequestID), 1)
}

// CleanupResources alone (direct callers, the New failure path) closes the
// writer; CleanupBackgroundResources (combined mode's pre-drain hook)
// leaves it open until CloseAuditWriter.
func TestDecisionLog_P1_9_CleanupVariants(t *testing.T) {
	t.Run("CleanupResources closes the writer", func(t *testing.T) {
		srv, _ := newDecisionLogServer(t, &decisionLogCapture{})
		require.NoError(t, srv.CleanupResources(context.Background()))
		assert.True(t, srv.auditWriter.Snapshot().Closed)
		assert.Equal(t, asyncwrite.HealthClosed, srv.auditWriter.Health(time.Now()).Reason)
	})
	t.Run("background-only cleanup leaves the writer open", func(t *testing.T) {
		capture := &decisionLogCapture{}
		srv, _ := newDecisionLogServer(t, capture)
		setDecisionLogFlag(t, srv, true)
		require.NoError(t, srv.CleanupBackgroundResources(context.Background()))
		require.False(t, srv.auditWriter.Snapshot().Closed)
		rec := inDomainDecisionRecord()
		rec.CredentialType, rec.CredentialID = "", ""
		rec.CredentialBoundaryKind, rec.CredentialBoundaryProjectID = "", ""
		srv.decisionAuditLogger.EmitDecisionAudit(decisionCtx(), rec)
		require.NoError(t, srv.CloseAuditWriter(context.Background()))
		snap := srv.auditWriter.Snapshot()
		assert.True(t, snap.Closed)
		assert.Equal(t, uint64(1), snap.Written, "%+v", snap)
		assert.Len(t, capture.records(), 1)
		// Idempotent.
		require.NoError(t, srv.CloseAuditWriter(context.Background()))
		require.NoError(t, srv.CleanupResources(context.Background()))
	})
}

// P1-11. Secret/token canaries in the body, headers and query never reach
// the audit output, and neither does the bearer token.
func TestDecisionLog_P1_11_BodyFreeAndSentinels(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, _, projectID := seedDecisionLogProject(t, s)
	setDecisionLogFlag(t, srv, true)

	const canary = "CANARY-7f3a"
	body := []byte(`{"secret":"` + canary + `-body","token":"` + canary + `-token"}`)
	headers := map[string]string{"X-Api-Secret": canary + "-header", "Cookie": "session=" + canary + "-cookie"}
	path := stopAllPath + "?token=" + canary + "-query&secret=" + canary + "-query2"
	admin := seedDecisionLogAdmin(t, s)
	var tokens []string
	for _, who := range []*store.User{admin, member} {
		rec, token := userRequest(t, srv, who, http.MethodPost, path, body, headers)
		require.NotEqual(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		tokens = append(tokens, token)
	}
	// Also a project route carrying the canaries (excluded, but exercised).
	_, token := userRequest(t, srv, member, http.MethodGet, membersPath(projectID)+"?token="+canary+"-q3", body, headers)
	tokens = append(tokens, token)
	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	lines := capture.records()
	require.GreaterOrEqual(t, len(lines), 2, "the sentinel check must observe records")
	raw, err := json.Marshal(lines)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), canary)
	for _, token := range tokens {
		assert.NotContains(t, string(raw), token)
	}
}
