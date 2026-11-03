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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A chat integration message (surface and external_ref) to an agent sets
// the resolved conversation's default agent to that agent. Only a live
// agent is set: a message to a soft-deleted agent gets exactly the answer
// for an unknown agent and writes no default agent.

const refDefaultSurface = "gchat"

// refDefaultBroker is the chat integration that sends the messages.
func refDefaultBroker() BrokerIdentity {
	return NewBrokerIdentity(tid("ref-default-broker"))
}

// refDefaultRequest is a plain message to an agent naming externalRef on
// the chat surface.
func refDefaultRequest(externalRef string) MessageRequest {
	return MessageRequest{Message: "hello", Surface: refDefaultSurface, ExternalRef: externalRef}
}

// softDeletedAgentB stores a soft-deleted agent in project B of f.
func softDeletedAgentB(t *testing.T, f acceptanceFixture, name string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              tid(name),
		Slug:            name,
		Name:            name,
		ProjectID:       f.projectB,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{f.ownerB.ID},
		Phase:           "running",
		RuntimeBrokerID: f.hubAgentB.RuntimeBrokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
		DeletedAt:       time.Now().UTC().Add(-time.Minute),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.False(t, got.DeletedAt.IsZero(), "precondition: the agent is soft-deleted")
	return a
}

// unknownAgentAnswer is the handler's answer for a chat-reference message
// to an agent that is not stored (unknownAgentLike), as a reference.
func unknownAgentAnswer(t *testing.T, f acceptanceFixture) refAnswer {
	t.Helper()
	unknown := unknownAgentLike(f.hubAgentB)
	_, lookupErr := f.store.GetAgent(context.Background(), unknown.ID)
	require.True(t, errors.Is(lookupErr, store.ErrNotFound), "precondition: the unknown agent is not stored (err=%v)", lookupErr)
	got := postThreadRequestAs(t, f.srv, refDefaultBroker(), unknown, refDefaultRequest("spaces/ref-default/threads/unknown-probe"))
	require.Equal(t, http.StatusNotFound, got.status, "precondition: an unknown agent is not found: %s", got.body)
	return got
}

func TestAgentMessageReference_SoftDeletedRecipientCreatesNoConversation(t *testing.T) {
	f := acceptanceSetup(t)
	deleted := softDeletedAgentB(t, f, "ref-default-deleted-new")
	const externalRef = "spaces/ref-default/threads/new"

	got := postThreadRequestAs(t, f.srv, refDefaultBroker(), deleted, refDefaultRequest(externalRef))

	conv, lookupErr := f.store.GetConversationByExternalRef(context.Background(), refDefaultSurface, externalRef)
	if lookupErr == nil && conv != nil && conv.DefaultAgentID != nil {
		t.Logf("conversation %s has default agent %s", conv.ID, *conv.DefaultAgentID)
	}
	assert.True(t, errors.Is(lookupErr, store.ErrNotFound),
		"default agent check: no conversation is created for a soft-deleted recipient (err=%v)", lookupErr)
	requireSameAnswer(t, unknownAgentAnswer(t, f), got)
	assert.Equal(t, 0, agentMessageCount(t, f.store, deleted.ID), "no message row is written")
}

func TestAgentMessageReference_SoftDeletedRecipientKeepsExistingDefault(t *testing.T) {
	f := acceptanceSetup(t)
	deleted := softDeletedAgentB(t, f, "ref-default-deleted-existing")
	const externalRef = "spaces/ref-default/threads/existing"
	projectID := f.projectB
	liveID := f.hubAgentB.ID
	seeded, err := f.store.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "group", Surface: refDefaultSurface, ExternalRef: externalRef,
		ProjectID: &projectID, DefaultAgentID: &liveID, DriftState: "active",
	})
	require.NoError(t, err)
	require.NotNil(t, seeded.DefaultAgentID, "precondition: the conversation has a default agent")
	require.Equal(t, liveID, *seeded.DefaultAgentID, "precondition: the default agent is the live agent")

	got := postThreadRequestAs(t, f.srv, refDefaultBroker(), deleted, refDefaultRequest(externalRef))

	conv, err := f.store.GetConversationByExternalRef(context.Background(), refDefaultSurface, externalRef)
	require.NoError(t, err)
	require.NotNil(t, conv.DefaultAgentID, "the conversation keeps a default agent")
	assert.Equal(t, liveID, *conv.DefaultAgentID,
		"default agent check: the existing default agent is kept for a soft-deleted recipient")
	assert.Equal(t, seeded.ID, conv.ID, "the same conversation row")
	requireSameAnswer(t, unknownAgentAnswer(t, f), got)
	assert.Equal(t, 0, agentMessageCount(t, f.store, deleted.ID), "no message row is written")
}

func TestAgentMessageReference_UnknownRecipientMatchesSoftDeleted(t *testing.T) {
	f := acceptanceSetup(t)
	deleted := softDeletedAgentB(t, f, "ref-default-deleted-parity")
	const externalRef = "spaces/ref-default/threads/parity"

	deletedAnswer := postThreadRequestAs(t, f.srv, refDefaultBroker(), deleted, refDefaultRequest(externalRef))
	unknownAnswer := unknownAgentAnswer(t, f)

	assert.Equal(t, unknownAnswer, deletedAnswer,
		"parity check: the soft-deleted answer equals the unknown-agent answer")
	_, convErr := f.store.GetConversationByExternalRef(context.Background(), refDefaultSurface, externalRef)
	assert.True(t, errors.Is(convErr, store.ErrNotFound), "no conversation is created (err=%v)", convErr)
}

func TestAgentMessageReference_LiveRecipientSetsDefault(t *testing.T) {
	f := acceptanceSetup(t)
	live := f.hubAgentB
	const externalRef = "spaces/ref-default/threads/live"

	got := postThreadRequestAs(t, f.srv, refDefaultBroker(), live, refDefaultRequest(externalRef))

	conv, err := f.store.GetConversationByExternalRef(context.Background(), refDefaultSurface, externalRef)
	require.NoError(t, err, "the conversation is created for a live recipient (status %d: %s)", got.status, got.body)
	require.NotNil(t, conv.DefaultAgentID, "the conversation has a default agent")
	assert.Equal(t, live.ID, *conv.DefaultAgentID, "the default agent is the live recipient")
	assert.NotEqual(t, http.StatusNotFound, got.status, got.body)
}
