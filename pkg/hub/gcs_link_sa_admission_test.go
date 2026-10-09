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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A gs:// link opens only through a service account the sending agent could
// mint a token for right now (admissibleGCPServiceAccount). Every other
// state answers the same not-found response; the audit event carries the
// reason.

// gcsSAScenario is one agent-sent message in a project topic, read by a
// project member, with the agent assigned the service account built by
// makeSA.
type gcsSAScenario struct {
	viewer *store.User
	msg    *store.Message
	sa     *store.GCPServiceAccount
}

func gcsSAAdmissionScenario(t *testing.T, f *gcsFixture, seed string, makeSA func(project *store.Project) *store.GCPServiceAccount) gcsSAScenario {
	t.Helper()
	project := gcsTestProject(t, f, seed+"-project")
	sa := makeSA(project)
	agent := gcsTestAgent(t, f.store, project, seed+"-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, seed+"-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: seed + "-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "see gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, seed+"-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	return gcsSAScenario{viewer: viewer, msg: msg, sa: sa}
}

func (sc gcsSAScenario) fetch(t *testing.T, f *gcsFixture) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, sc.viewer, http.MethodGet, gcsRequestPath(sc.msg.ID, "bkt", "o.txt"), nil)
}

// requireGCSUniformDeny asserts the uniform not-found answer with no mint and
// no storage call, and that the single audit event carries want.
func requireGCSUniformDeny(t *testing.T, f *gcsFixture, rec *httptest.ResponseRecorder, want GCSLinkFetchReason) {
	t.Helper()
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "deny", events[0].Decision)
	require.Equal(t, want, events[0].Reason)
	require.Empty(t, events[0].SAEmail, "no service account is recorded for a refused fetch")
}

// gcsUseLoggingAuditRecorder replaces the fixture's audit recorder with one
// whose audit log lines go to the returned buffer.
func gcsUseLoggingAuditRecorder(f *gcsFixture) *bytes.Buffer {
	var buf bytes.Buffer
	rec := &gcsAuditRecorder{LogAuditLogger: &LogAuditLogger{
		prefix: "[test]",
		log:    slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}}
	f.srv.SetAuditLogger(rec)
	f.audit = rec
	return &buf
}

func gcsUnverifiedSA(t *testing.T, f *gcsFixture, seed string) func(*store.Project) *store.GCPServiceAccount {
	return func(project *store.Project) *store.GCPServiceAccount {
		return gcsTestSAWith(t, f.store, project, seed+"-sa", "sa-"+seed+"@test.iam.gserviceaccount.com", func(sa *store.GCPServiceAccount) {
			sa.Verified = false
			sa.VerificationStatus = store.GCPVerificationUnverified
		})
	}
}

func gcsOtherProjectSA(t *testing.T, f *gcsFixture, seed string) func(*store.Project) *store.GCPServiceAccount {
	return func(*store.Project) *store.GCPServiceAccount {
		other := gcsTestProject(t, f, seed+"-other-project")
		return gcsTestSA(t, f.store, other, seed+"-sa", "sa-"+seed+"@test.iam.gserviceaccount.com")
	}
}

func gcsHubScopedSA(t *testing.T, f *gcsFixture, seed string) func(*store.Project) *store.GCPServiceAccount {
	return func(project *store.Project) *store.GCPServiceAccount {
		return gcsTestSAWith(t, f.store, project, seed+"-sa", "sa-"+seed+"@test.iam.gserviceaccount.com", func(sa *store.GCPServiceAccount) {
			sa.Scope = store.ScopeHub
			sa.ScopeID = "test-hub"
		})
	}
}

// gcsSALookupFaultStore fails GetGCPServiceAccount for one account ID with a
// store error (not ErrNotFound).
type gcsSALookupFaultStore struct {
	store.Store
	saID string
}

func (s *gcsSALookupFaultStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	if id == s.saID {
		return nil, errors.New("gcs test: store fault for test")
	}
	return s.Store.GetGCPServiceAccount(ctx, id)
}

func TestGCSLink_Deny_UnverifiedServiceAccount(t *testing.T) {
	f := newGCSFixture(t)
	sc := gcsSAAdmissionScenario(t, f, "unverified", gcsUnverifiedSA(t, f, "unverified"))

	requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSANotAdmissible)
}

func TestGCSLink_Deny_ServiceAccountNotReachableFromAgentProject(t *testing.T) {
	f := newGCSFixture(t)
	// The assignment is written to the store directly; the assignment
	// handlers would not accept it.
	sc := gcsSAAdmissionScenario(t, f, "otherproj", gcsOtherProjectSA(t, f, "otherproj"))
	require.True(t, gcpServiceAccountVerified(sc.sa), "only reachability differs from the allowed case")

	requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonNoSA)
}

func TestGCSLink_Deny_HubServiceAccountWhenEnforceModeOff(t *testing.T) {
	for name, mode := range map[string]string{"off": SAAssignCheckOff, "unset": ""} {
		t.Run(name, func(t *testing.T) {
			f := newGCSFixture(t)
			setMode(f.srv, mode)
			sc := gcsSAAdmissionScenario(t, f, "hubmode-"+name, gcsHubScopedSA(t, f, "hubmode-"+name))

			requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSANotAdmissible)
		})
	}
}

func TestGCSLink_Deny_ServiceAccountLookupFaultAnswersUniform404(t *testing.T) {
	f := newGCSFixture(t)
	sc := gcsSAAdmissionScenario(t, f, "salookupfault", func(project *store.Project) *store.GCPServiceAccount {
		return gcsTestSA(t, f.store, project, "salookupfault-sa", "sa-salookupfault@test.iam.gserviceaccount.com")
	})
	logs := gcsUseLoggingAuditRecorder(f)
	f.srv.store = &gcsSALookupFaultStore{Store: f.srv.store, saID: sc.sa.ID}

	rec := sc.fetch(t, f)
	requireGCSUniformDeny(t, f, rec, GCSLinkReasonSALookupFailed)

	// The reason is recorded server-side only: the audit log line names it,
	// the response body does not.
	require.Contains(t, logs.String(), "reason="+string(GCSLinkReasonSALookupFailed))
	require.NotContains(t, rec.Body.String(), string(GCSLinkReasonSALookupFailed))
	require.NotContains(t, rec.Body.String(), "store fault")
}

func TestGCSLink_Allowed_VerifiedProjectServiceAccount(t *testing.T) {
	t.Run("project scoped", func(t *testing.T) {
		f := newGCSFixture(t)
		sc := gcsSAAdmissionScenario(t, f, "verifiedproj", func(project *store.Project) *store.GCPServiceAccount {
			return gcsTestSA(t, f.store, project, "verifiedproj-sa", "sa-verifiedproj@test.iam.gserviceaccount.com")
		})

		rec := sc.fetch(t, f)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "hi", rec.Body.String())
		call, ok := f.gen.lastCall()
		require.True(t, ok, "expected a mint call")
		require.Equal(t, sc.sa.Email, call.email)
		events := f.audit.all()
		require.Len(t, events, 1)
		require.Equal(t, GCSLinkReasonOK, events[0].Reason)
		require.Equal(t, sc.sa.Email, events[0].SAEmail)
	})

	t.Run("hub scoped with enforce check mode", func(t *testing.T) {
		f := newGCSFixture(t)
		setMode(f.srv, SAAssignCheckEnforce)
		sc := gcsSAAdmissionScenario(t, f, "verifiedhub", gcsHubScopedSA(t, f, "verifiedhub"))

		rec := sc.fetch(t, f)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		call, ok := f.gen.lastCall()
		require.True(t, ok, "expected a mint call")
		require.Equal(t, sc.sa.Email, call.email)
	})
}

func TestGCSLink_DenyBodiesIdenticalAcrossServiceAccountStates(t *testing.T) {
	type state struct {
		name  string
		setup func(t *testing.T, f *gcsFixture) gcsSAScenario
	}
	states := []state{
		{"no service account", func(t *testing.T, f *gcsFixture) gcsSAScenario {
			project := gcsTestProject(t, f, "bodies-nosa-project")
			agent := gcsTestAgent(t, f.store, project, "bodies-nosa-agent", nil)
			conv := gcsTestTopic(t, f.store, project, "bodies-nosa-topic")
			msg := gcsTestMessage(t, f.store, gcsMessageSpec{
				seed: "bodies-nosa-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
				body: "see gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
			})
			viewer := gcsTestUser(t, f.store, "bodies-nosa-viewer")
			addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
			return gcsSAScenario{viewer: viewer, msg: msg}
		}},
		{"unverified", func(t *testing.T, f *gcsFixture) gcsSAScenario {
			return gcsSAAdmissionScenario(t, f, "bodies-unverified", gcsUnverifiedSA(t, f, "bodies-unverified"))
		}},
		{"other project", func(t *testing.T, f *gcsFixture) gcsSAScenario {
			return gcsSAAdmissionScenario(t, f, "bodies-otherproj", gcsOtherProjectSA(t, f, "bodies-otherproj"))
		}},
		{"hub scoped without enforce", func(t *testing.T, f *gcsFixture) gcsSAScenario {
			setMode(f.srv, SAAssignCheckOff)
			return gcsSAAdmissionScenario(t, f, "bodies-hubmode", gcsHubScopedSA(t, f, "bodies-hubmode"))
		}},
		{"lookup fault", func(t *testing.T, f *gcsFixture) gcsSAScenario {
			sc := gcsSAAdmissionScenario(t, f, "bodies-fault", func(project *store.Project) *store.GCPServiceAccount {
				return gcsTestSA(t, f.store, project, "bodies-fault-sa", "sa-bodies-fault@test.iam.gserviceaccount.com")
			})
			f.srv.store = &gcsSALookupFaultStore{Store: f.srv.store, saID: sc.sa.ID}
			return sc
		}},
	}

	var base *httptest.ResponseRecorder
	for _, st := range states {
		f := newGCSFixture(t)
		rec := st.setup(t, f).fetch(t, f)
		require.Equal(t, http.StatusNotFound, rec.Code, st.name)
		require.Zero(t, f.gen.mintCount(), st.name)
		if base == nil {
			base = rec
			require.JSONEq(t, gcsExpected404Body, base.Body.String())
			continue
		}
		require.Equal(t, base.Body.String(), rec.Body.String(), "%s body differs from the no service account body", st.name)
		require.Equal(t, base.Header().Get("Content-Type"), rec.Header().Get("Content-Type"), "%s Content-Type differs", st.name)
		require.Equal(t, strings.Join(base.Header().Values("Cache-Control"), ","), strings.Join(rec.Header().Values("Cache-Control"), ","), "%s Cache-Control differs", st.name)
	}
}
