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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #48 — the existence oracle on service-account IDs, and the tests that make it
// falsifiable.
//
// A caller who may create agents in their own project names a service account
// by ID. Two ways that can fail: the ID matches no account at all, or it
// matches one that is not reachable from this project. IF THE TWO FAILURES LOOK
// DIFFERENT, the endpoint answers "does this ID exist?" for accounts the caller
// cannot see — and since the caller supplies the ID, they can ask about any ID
// they like, including ones they guessed or harvested from another project's
// logs. Enumeration is then a loop over the difference.
//
// Before #48:
//   - agent create answered "not found" vs "does not belong to this project",
//     same 400, different message;
//   - agent PATCH answered 404 vs 400 — distinguishable without reading the
//     body at all;
//   - project settings collapsed both, correctly, and had done since it was
//     written. It was the standard the other two did not follow.
//
// ⚠️ THESE TESTS ASSERT AN EQUALITY, WHICH IS A SHAPE THAT PASSES TOO EASILY.
// "Two responses are identical" is also true when the handler returns the same
// thing for everything, or when the harness cannot see the difference it is
// asked about. TestSAOracle_TheComparisonCanSeeADifference is the control for
// exactly that and should be read as part of every test here — without it, the
// equalities below are green whether or not the collapse is the reason.

// oracleProbe is one request's observable answer: everything a caller can see.
type oracleProbe struct {
	status int
	body   string
}

func probe(rec *httptest.ResponseRecorder) oracleProbe {
	return oracleProbe{status: rec.Code, body: rec.Body.String()}
}

// requireIndistinguishable compares two answers byte for byte, not by asserting
// that each matches some expected message.
//
// ⚠️ AND THE COLLAPSE TOOK SOMETHING AWAY FROM EVERY OTHER TEST HERE. The
// response no longer says WHICH branch produced the 400. So an assertion of the
// form "seed an unreachable account, expect 400 and this message" is now
// satisfied just as well by a fixture that never persisted the account at all —
// a mistyped ID, a seeding helper whose error was swallowed, a store reset
// between calls. That green used to be impossible to get by accident, because
// nonexistence said something different; it is now the default failure mode of
// a broken fixture.
//
// Two instruments, and they answer different questions:
//
//   - THE COLLAPSE HOLDS: same request, account present-but-unreachable versus
//     absent, answers identical. That is this function.
//   - THE PREDICATE STILL RUNS: the account must be genuinely PRESENT in both
//     arms and only its reachability varied — reachable is admitted, unreachable
//     is refused. Nothing in this file does that, and it is not this file's job.
//     TestBypassAgents_UpdateAgentServiceAccountChecks holds the tightest pair —
//     "another project is rejected" against "verified in-project is still
//     accepted", one fixture, reachability the only variable. Create's is
//     TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin against
//     TestAgentCreate_OtherProjectSA_StillRejected. Those admitted arms are what
//     stop the refusals here being vacuous.
//
// Asserting the collapse without the second instrument somewhere is how a
// deleted scope check would pass review: every refusal test still refuses, for
// the wrong reason, and nothing says so.
//
// The property under test is that the two cases are THE SAME ANSWER, and an
// expected-message assertion does not test that: two branches can both contain
// msgSANotAvailableInProject and still differ in status, in error code, or in a
// field added later. Comparing the whole response tests the property directly
// and keeps testing it when the response shape changes — a future field that
// leaks the distinction fails here without anyone having to think of it.
func requireIndistinguishable(t *testing.T, missing, unreachable oracleProbe) {
	t.Helper()
	assert.Equal(t, missing.status, unreachable.status,
		"a nonexistent SA and an unreachable one must not differ by status code:\n"+
			"  nonexistent -> %d\n  unreachable -> %d\n"+
			"status alone is enough to enumerate IDs; the body need never be read",
		missing.status, unreachable.status)
	assert.Equal(t, missing.body, unreachable.body,
		"a nonexistent SA and an unreachable one must not differ by response body:\n"+
			"  nonexistent -> %s\n  unreachable -> %s",
		missing.body, unreachable.body)
}

// ============================================================================
// Agent create
// ============================================================================

func TestSAOracle_AgentCreate_MissingAndUnreachableAreOneAnswer(t *testing.T) {
	f := bypassAgentsSetup(t)
	unreachable := bypassAgentsCreateSA(t, f, f.other.ID, true)

	// Never created. The store's parseGetID maps an unparseable ID to
	// ErrNotFound as well, which the malformed-ID test below relies on; this one
	// uses a well-formed UUID so the two probes differ ONLY in whether a row
	// exists.
	missingID := uuid.New().String()

	missing := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-create-missing",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: missingID,
		},
	}))
	other := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-create-unreachable",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: unreachable.ID,
		},
	}))

	require.Equal(t, http.StatusBadRequest, missing.status, "body: %s", missing.body)
	requireIndistinguishable(t, missing, other)
	assert.Contains(t, missing.body, msgSANotAvailableInProject)
}

func TestSAOracle_AgentCreate_MalformedIDIsTheSameAnswerToo(t *testing.T) {
	// A non-UUID reaches ErrNotFound through parseGetID rather than through a
	// miss on the table. Different route, same disclosure if answered
	// differently, so it is pinned rather than assumed.
	f := bypassAgentsSetup(t)
	unreachable := bypassAgentsCreateSA(t, f, f.other.ID, true)

	malformed := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-create-malformed",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: "not-a-uuid-at-all",
		},
	}))
	other := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-create-malformed-twin",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: unreachable.ID,
		},
	}))

	requireIndistinguishable(t, malformed, other)
}

// ============================================================================
// Agent PATCH — the surface that differed by STATUS
// ============================================================================

func TestSAOracle_AgentPatch_MissingAndUnreachableAreOneAnswer(t *testing.T) {
	f := bypassAgentsSetup(t)
	unreachable := bypassAgentsCreateSA(t, f, f.other.ID, true)
	missingID := uuid.New().String()

	a1 := pendingAgentForPatch(t, f, "oracle-patch-missing")
	a2 := pendingAgentForPatch(t, f, "oracle-patch-unreachable")

	missing := probe(patchAgentSAAsOwner(t, f, a1.ID, missingID))
	other := probe(patchAgentSAAsOwner(t, f, a2.ID, unreachable.ID))

	requireIndistinguishable(t, missing, other)
	assert.Contains(t, missing.body, msgSANotAvailableInProject)
}

// The status change, pinned on its own and named, because it is the one
// wire-visible behaviour change in #48 and a reader of a failing test deserves
// to know it was deliberate.
func TestSAOracle_AgentPatch_NonexistentSAIsNowBadRequestNotNotFound(t *testing.T) {
	f := bypassAgentsSetup(t)
	a := pendingAgentForPatch(t, f, "oracle-patch-status")

	rec := patchAgentSAAsOwner(t, f, a.ID, uuid.New().String())

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"PATCH with a nonexistent service account must answer 400, matching the "+
			"not-reachable branch. It answered 404 before #48, which distinguished "+
			"existence from reachability without the caller reading the body. If this "+
			"fails, the oracle is back; do not 'restore' the 404. Body: %s",
		rec.Body.String())
	assert.NotEqual(t, http.StatusNotFound, rec.Code)
}

// The parameter misuse, pinned separately from the status because it was a
// separate defect with a separate way of coming back.
//
// The old call was writeErrorFromErr(w, err, "GCP service account not found"),
// whose third parameter is requestID, not message. So the string was emitted in
// the response's requestId field. Anyone re-fixing this by "improving the
// message" would keep the leak; anyone checking only `message` would not see
// it. This asserts against the WHOLE body for that reason.
func TestSAOracle_AgentPatch_DoesNotLeakTheOldStringInAnyField(t *testing.T) {
	f := bypassAgentsSetup(t)
	a := pendingAgentForPatch(t, f, "oracle-patch-requestid")

	rec := patchAgentSAAsOwner(t, f, a.ID, uuid.New().String())

	assert.NotContains(t, rec.Body.String(), "GCP service account not found",
		"the not-found wording must not appear anywhere in the response, including "+
			"requestId, where it used to be shipped by a misused parameter. Body: %s",
		rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "does not belong to this project",
		"nor may the reachability wording appear; both cases are one answer now")
}

// ============================================================================
// Project settings — the site that was already correct, now sharing the const
// ============================================================================

// The standard had no test. #48 moved its literal into
// msgSANotAvailableInProject, so an edit to the const now moves this site too —
// which is the point of sharing it, and the reason it needs a test it did not
// have before.
func TestSAOracle_ProjectDefault_MissingAndUnreachableAreOneAnswer(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	elsewhere := &store.Project{
		ID:         tid("oracle-other-project"),
		Name:       "Oracle Other",
		Slug:       "oracle-other-project",
		Visibility: "private",
	}
	require.NoError(t, s.CreateProject(t.Context(), elsewhere))

	unreachable := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   elsewhere.ID,
		Email:     "oracle-elsewhere@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), unreachable))

	put := func(said string) oracleProbe {
		return probe(doRequest(t, srv, http.MethodPut,
			"/api/v1/projects/"+project.ID+"/settings", hubclient.ProjectSettings{
				DefaultGCPIdentityMode:             string(store.GCPMetadataModeAssign),
				DefaultGCPIdentityServiceAccountID: said,
			}))
	}

	missing := put(uuid.New().String())
	other := put(unreachable.ID)

	require.Equal(t, http.StatusBadRequest, missing.status, "body: %s", missing.body)
	requireIndistinguishable(t, missing, other)
	assert.Contains(t, missing.body, msgSANotAvailableInProject)
}

// ============================================================================
// The control
// ============================================================================

// ⚠️ THIS IS WHAT STOPS EVERY EQUALITY ABOVE FROM BEING VACUOUS. Each of them
// asserts that two responses match. That assertion is equally green on a
// handler that has genuinely collapsed the two cases and on one that returns an
// identical answer to everything — or on a harness comparing something that
// cannot vary. Rule 15: an outcome invariant over the whole input range says
// nothing about which component produced it.
//
// So: a third failure mode at the same site, reached one line later, MUST come
// back different. A reachable-but-unverified account is refused with its own
// message, deliberately — once the account is reachable from the caller's
// project, naming its state discloses nothing the caller could not already
// read, and the specificity is useful. If this test ever passes by finding the
// unverified answer identical to the not-available one, the collapse has been
// over-applied and the tests above have stopped measuring anything.
func TestSAOracle_TheComparisonCanSeeADifference(t *testing.T) {
	f := bypassAgentsSetup(t)
	unverified := bypassAgentsCreateSA(t, f, f.proj.ID, false) // in-project, not verified

	missing := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-control-missing",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: uuid.New().String(),
		},
	}))
	unverifiedProbe := probe(createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "oracle-control-unverified",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: unverified.ID,
		},
	}))

	require.Equal(t, http.StatusBadRequest, missing.status, "body: %s", missing.body)
	require.Equal(t, http.StatusBadRequest, unverifiedProbe.status, "body: %s", unverifiedProbe.body)
	assert.NotEqual(t, missing.body, unverifiedProbe.body,
		"the harness must be able to distinguish two different 400s, or the "+
			"indistinguishability tests in this file prove nothing")
	assert.Contains(t, unverifiedProbe.body, "is not verified")
}
