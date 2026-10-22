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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// streamRecheckMode names how a long-lived stream re-checks the credential
// that opened it.
type streamRecheckMode string

const (
	// streamRecheckToken: the stream re-checks a user access token
	// (streamTokenRecheck for SSE, ptyTokenCredentialCheck for PTY).
	streamRecheckToken streamRecheckMode = "token"
	// streamRecheckEvery: the stream re-checks every credential
	// (streamCredentialStillAuthorized on each tick).
	streamRecheckEvery streamRecheckMode = "every"
)

// streamRecheckOperations lists every catalog operation that admits a
// token on an SSE or WebSocket entry point, with the re-check its streams
// run. The agent operations cover both PTY paths (broker and agent), which
// share the one WebSocket entry point.
var streamRecheckOperations = map[authzop.OperationID]streamRecheckMode{
	"agent.logs.read":          streamRecheckToken,
	"agent.messages.read":      streamRecheckToken,
	"project.messagelogs.read": streamRecheckToken,
	"agent.attach":             streamRecheckToken,
	"hub.diagnostics.read":     streamRecheckEvery,
}

// streamRecheckExemptCredentials lists the catalog credential kinds a
// token-re-checked stream admits without the token re-check, each with
// the rule that applies to it instead.
var streamRecheckExemptCredentials = map[authzop.CredentialKind]string{
	authzop.CredentialSessionJWT: "a session is not re-checked on a stream: it is decided when the stream opens, and its own expiry and revocation end the session",
	authzop.CredentialAgentJWT:   "an agent credential is bound to its running agent and project and is not a user access token; an agent caller is decided when the stream opens",
}

// streamRecheckExemptPerOperation names credential kinds exempt from the
// token re-check on one operation only.
var streamRecheckExemptPerOperation = map[authzop.OperationID]map[authzop.CredentialKind]string{
	"agent.attach": {
		authzop.CredentialDelegatedAgent: "an agent delegated credential is authorized at the PTY handshake and at reconnect only; an established session outlives the credential's expiry and revocation by design (.design/agent-delegation.md §16.6)",
	},
}

// streamRecheckViolations returns, for each operation in ops, every
// problem with its streams' credential coverage: an operation that admits
// a token on an SSE or WebSocket entry point but is not in modes, and,
// for a token-re-checked operation, an admitted credential kind that is
// neither the token kind nor exempt, globally (exempt) or for that
// operation (perOp).
func streamRecheckViolations(specs []authzop.OperationSpec, modes map[authzop.OperationID]streamRecheckMode, exempt map[authzop.CredentialKind]string, perOp map[authzop.OperationID]map[authzop.CredentialKind]string) []string {
	var problems []string
	for _, spec := range specs {
		streaming := false
		for _, ep := range spec.EntryPoints {
			if ep.Kind == authzop.EntryPointSSE || ep.Kind == authzop.EntryPointWebSocket {
				streaming = true
				break
			}
		}
		if !streaming || !spec.Bearer.Kind.AdmitsToken() {
			continue
		}
		mode, ok := modes[spec.ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: admits a token on a stream entry point and names no re-check", spec.ID))
			continue
		}
		if mode == streamRecheckEvery {
			continue
		}
		for _, ck := range spec.Credentials {
			if ck == authzop.CredentialScopedUAT {
				continue
			}
			if exempt[ck] == "" && perOp[spec.ID][ck] == "" {
				problems = append(problems, fmt.Sprintf("%s: admits credential %s, which the token re-check does not cover and no exemption names", spec.ID, ck))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// TestStreamRecheck_CoversEveryAdmittedCredentialKind requires every
// catalog operation that admits a token on an SSE or WebSocket entry point
// to name the re-check its streams run, and every credential kind a
// token-re-checked operation admits to be the user access token kind or an
// exempt kind with a stated rule. It also requires the re-check helpers to
// select exactly the user access token kind, and the rule to reject an
// unclassified stream operation and an uncovered credential kind.
func TestStreamRecheck_CoversEveryAdmittedCredentialKind(t *testing.T) {
	assert.Empty(t, streamRecheckViolations(authzop.Catalog, streamRecheckOperations, streamRecheckExemptCredentials, streamRecheckExemptPerOperation))

	byID := map[authzop.OperationID]authzop.OperationSpec{}
	for _, spec := range authzop.Catalog {
		byID[spec.ID] = spec
	}
	for id := range streamRecheckOperations {
		spec, ok := byID[id]
		require.True(t, ok, "%s is a catalog operation", id)
		assert.True(t, spec.Bearer.Kind.AdmitsToken(), "%s admits a token", id)
		assert.Contains(t, spec.Credentials, authzop.CredentialScopedUAT, "%s lists the user access token credential", id)
		streaming := false
		for _, ep := range spec.EntryPoints {
			if ep.Kind == authzop.EntryPointSSE || ep.Kind == authzop.EntryPointWebSocket {
				streaming = true
			}
		}
		assert.True(t, streaming, "%s has a stream entry point", id)
	}
	for ck, reason := range streamRecheckExemptCredentials {
		assert.NotEqual(t, authzop.CredentialScopedUAT, ck, "the token kind is re-checked, never exempt")
		assert.NotEmpty(t, reason, "exemption %s states its rule", ck)
	}
	for id, kinds := range streamRecheckExemptPerOperation {
		spec, ok := byID[id]
		require.True(t, ok, "%s is a catalog operation", id)
		_, recheck := streamRecheckOperations[id]
		assert.True(t, recheck, "%s names its re-check", id)
		for ck, reason := range kinds {
			assert.NotEqual(t, authzop.CredentialScopedUAT, ck, "the token kind is re-checked, never exempt")
			assert.NotEmpty(t, reason, "exemption %s on %s states its rule", ck, id)
			assert.Contains(t, spec.Credentials, ck, "%s admits the exempt credential %s", id, ck)
		}
	}

	// The helpers select the user access token kind and nothing else.
	user := NewAuthenticatedUser(tid("recheck-kinds"), "kinds@test.com", "Kinds", store.UserRoleMember, "web")
	agent := &store.Agent{ID: tid("recheck-kinds-agent"), ProjectID: tid("recheck-kinds-project")}
	srv := &Server{}
	with := func(kind CredentialKind) context.Context {
		return contextWithCredentialContext(contextWithIdentity(context.Background(), user), CredentialContext{Kind: kind, ID: "tok"})
	}
	token := with(CredentialKindUAT)
	ch, stop := streamTokenRecheck(token)
	assert.NotNil(t, ch, "a token stream is re-checked")
	stop()
	assert.NotNil(t, srv.ptyTokenCredentialCheck(token, agent), "a token PTY session is re-checked")
	for _, kind := range []CredentialKind{
		CredentialKindInteractive, CredentialKindAgentJWT, CredentialKindFederation, CredentialKindBroker,
		CredentialKindDev, CredentialKindDelegatedAgent, CredentialKindHubDelivery,
	} {
		ctx := with(kind)
		ch, stop := streamTokenRecheck(ctx)
		assert.Nil(t, ch, "%s: no token re-check", kind)
		stop()
		assert.Nil(t, srv.ptyTokenCredentialCheck(ctx, agent), "%s: no token re-check", kind)
	}

	// The rule rejects an unclassified stream operation and an uncovered
	// credential kind.
	unclassified := authzop.OperationSpec{
		ID:          "proof.stream.unclassified",
		EntryPoints: []authzop.EntryPoint{{Kind: authzop.EntryPointSSE, Pattern: "/proof/stream", Method: "GET"}},
		Credentials: []authzop.CredentialKind{authzop.CredentialScopedUAT},
		Bearer:      authzop.AdmitOn(authzop.BearerTargetAgentRecord, authzop.BearerBoundaryProject),
	}
	assert.Len(t, streamRecheckViolations([]authzop.OperationSpec{unclassified}, streamRecheckOperations, streamRecheckExemptCredentials, streamRecheckExemptPerOperation), 1)
	uncovered := unclassified
	uncovered.ID = "agent.logs.read"
	uncovered.Credentials = []authzop.CredentialKind{authzop.CredentialScopedUAT, authzop.CredentialDelegatedAgent}
	assert.Len(t, streamRecheckViolations([]authzop.OperationSpec{uncovered}, streamRecheckOperations, streamRecheckExemptCredentials, streamRecheckExemptPerOperation), 1)
	selfAdmitted := unclassified
	selfAdmitted.ID = "proof.stream.self"
	selfAdmitted.Bearer = authzop.BearerDisposition{Kind: authzop.BearerAdmitSelf}
	assert.Len(t, streamRecheckViolations([]authzop.OperationSpec{selfAdmitted}, streamRecheckOperations, streamRecheckExemptCredentials, streamRecheckExemptPerOperation), 1,
		"an admit_self stream operation also names its re-check")
}

// TestAgentMessageLogParticipant_NonUserWithoutFullHistoryRefused requires
// the message-log participant filter to refuse, with 403, a caller that is
// not a user and has no full history, so such a caller never gets an
// unfiltered query; with the full history no filter applies, and a user
// without it is filtered to its own ID.
func TestAgentMessageLogParticipant_NonUserWithoutFullHistoryRefused(t *testing.T) {
	agentCtx := contextWithIdentity(context.Background(), agentIdentityFor(tid("mlp-agent"), tid("mlp-project"), ScopeProjectRead))

	rec := httptest.NewRecorder()
	id, ok := agentMessageLogParticipant(agentCtx, rec, false)
	assert.False(t, ok)
	assert.Equal(t, "", id)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = httptest.NewRecorder()
	id, ok = agentMessageLogParticipant(agentCtx, rec, true)
	assert.True(t, ok)
	assert.Equal(t, "", id, "the full history has no participant filter")

	user := NewAuthenticatedUser(tid("mlp-user"), "mlp@test.com", "MLP", store.UserRoleMember, "web")
	rec = httptest.NewRecorder()
	id, ok = agentMessageLogParticipant(contextWithIdentity(context.Background(), user), rec, false)
	assert.True(t, ok)
	assert.Equal(t, user.ID(), id)
}
