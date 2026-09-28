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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Server-derived provenance stamping tests (#1690)
// ---------------------------------------------------------------------------

func TestStampProvenance_SameProject(t *testing.T) {
	sender := &store.Agent{ID: "sender-1", ProjectID: "proj-a", Slug: "sender"}
	target := &store.Agent{ID: "target-1", ProjectID: "proj-a", Slug: "target"}
	msg := &store.Message{SenderID: sender.ID, RecipientID: target.ID}

	StampProvenance(msg, sender, target)

	require.NotNil(t, msg.SenderProjectID, "SenderProjectID must be set")
	require.NotNil(t, msg.RecipientProjectID, "RecipientProjectID must be set")
	assert.Equal(t, "proj-a", *msg.SenderProjectID)
	assert.Equal(t, "proj-a", *msg.RecipientProjectID)
}

func TestStampProvenance_CrossProject(t *testing.T) {
	sender := &store.Agent{ID: "sender-1", ProjectID: "proj-a", Slug: "sender"}
	target := &store.Agent{ID: "target-1", ProjectID: "proj-b", Slug: "target"}
	msg := &store.Message{SenderID: sender.ID, RecipientID: target.ID}

	StampProvenance(msg, sender, target)

	require.NotNil(t, msg.SenderProjectID)
	require.NotNil(t, msg.RecipientProjectID)
	assert.Equal(t, "proj-a", *msg.SenderProjectID)
	assert.Equal(t, "proj-b", *msg.RecipientProjectID)
}

func TestStampProvenance_SharedSlugDifferentProjects(t *testing.T) {
	// Two agents with the same slug but different projects. Provenance
	// must use the agent records, not the slug.
	sender := &store.Agent{ID: "agent-aaa", ProjectID: "proj-x", Slug: "deploy"}
	target := &store.Agent{ID: "agent-bbb", ProjectID: "proj-y", Slug: "deploy"}
	msg := &store.Message{
		Sender:    "agent:deploy",
		SenderID:  sender.ID,
		Recipient: "agent:deploy",
	}

	StampProvenance(msg, sender, target)

	require.NotNil(t, msg.SenderProjectID)
	require.NotNil(t, msg.RecipientProjectID)
	assert.Equal(t, "proj-x", *msg.SenderProjectID)
	assert.Equal(t, "proj-y", *msg.RecipientProjectID)
}

func TestStampProvenance_IgnoresClientMetadata(t *testing.T) {
	// Even if the message already has project IDs set (e.g. from client
	// metadata), StampProvenance overwrites them with server-derived values.
	staleProj := "stale-proj"
	sender := &store.Agent{ID: "sender-1", ProjectID: "correct-proj-a"}
	target := &store.Agent{ID: "target-1", ProjectID: "correct-proj-b"}
	msg := &store.Message{
		SenderProjectID:    &staleProj,
		RecipientProjectID: &staleProj,
	}

	StampProvenance(msg, sender, target)

	assert.Equal(t, "correct-proj-a", *msg.SenderProjectID)
	assert.Equal(t, "correct-proj-b", *msg.RecipientProjectID)
}

func TestStampProvenance_NilInputs(t *testing.T) {
	// StampProvenance is a no-op when any input is nil.
	sender := &store.Agent{ID: "s", ProjectID: "p"}
	target := &store.Agent{ID: "t", ProjectID: "p"}
	msg := &store.Message{}

	StampProvenance(nil, sender, target)
	StampProvenance(msg, nil, target)
	StampProvenance(msg, sender, nil)

	assert.Nil(t, msg.SenderProjectID, "should not stamp when inputs are nil")
	assert.Nil(t, msg.RecipientProjectID, "should not stamp when inputs are nil")
}

func TestStampProvenance_Idempotent(t *testing.T) {
	sender := &store.Agent{ID: "sender-1", ProjectID: "proj-a"}
	target := &store.Agent{ID: "target-1", ProjectID: "proj-b"}
	msg := &store.Message{}

	StampProvenance(msg, sender, target)
	firstSender := *msg.SenderProjectID
	firstRecipient := *msg.RecipientProjectID

	StampProvenance(msg, sender, target)
	assert.Equal(t, firstSender, *msg.SenderProjectID)
	assert.Equal(t, firstRecipient, *msg.RecipientProjectID)
}

// ---------------------------------------------------------------------------
// ValidateProvenance tests
// ---------------------------------------------------------------------------

func TestValidateProvenance_MatchingRecords(t *testing.T) {
	sender := &store.Agent{ID: "s", ProjectID: "proj-a"}
	target := &store.Agent{ID: "t", ProjectID: "proj-b"}
	msg := &store.Message{}
	StampProvenance(msg, sender, target)

	assert.True(t, ValidateProvenance(msg, sender, target))
}

func TestValidateProvenance_MismatchedSender(t *testing.T) {
	sender := &store.Agent{ID: "s", ProjectID: "proj-a"}
	target := &store.Agent{ID: "t", ProjectID: "proj-b"}
	msg := &store.Message{}
	StampProvenance(msg, sender, target)

	// Change the sender record — validation should fail.
	wrongSender := &store.Agent{ID: "s", ProjectID: "wrong-proj"}
	assert.False(t, ValidateProvenance(msg, wrongSender, target))
}

func TestValidateProvenance_MismatchedRecipient(t *testing.T) {
	sender := &store.Agent{ID: "s", ProjectID: "proj-a"}
	target := &store.Agent{ID: "t", ProjectID: "proj-b"}
	msg := &store.Message{}
	StampProvenance(msg, sender, target)

	wrongTarget := &store.Agent{ID: "t", ProjectID: "wrong-proj"}
	assert.False(t, ValidateProvenance(msg, sender, wrongTarget))
}

func TestValidateProvenance_UnstampedMessage(t *testing.T) {
	sender := &store.Agent{ID: "s", ProjectID: "proj-a"}
	target := &store.Agent{ID: "t", ProjectID: "proj-b"}
	msg := &store.Message{}

	// Message not stamped — validation fails.
	assert.False(t, ValidateProvenance(msg, sender, target))
}

func TestValidateProvenance_NilInputs(t *testing.T) {
	sender := &store.Agent{ID: "s", ProjectID: "p"}
	target := &store.Agent{ID: "t", ProjectID: "p"}
	msg := &store.Message{}

	assert.False(t, ValidateProvenance(nil, sender, target))
	assert.False(t, ValidateProvenance(msg, nil, target))
	assert.False(t, ValidateProvenance(msg, sender, nil))
}
