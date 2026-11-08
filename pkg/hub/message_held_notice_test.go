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

// Hub notices to held agents (ptone/scion#7145): a held agent receives no
// artifact review notice and no notice about the fate of a message it sent;
// the same notice to an agent that is not held is still sent. A hold lookup
// fault withholds the notice too.

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldNoticeCount counts the recorded dispatches to slug.
func heldNoticeCount(d *brokerMockDispatcher, slug string) int {
	n := 0
	for _, m := range d.getMessages() {
		if m.agentSlug == slug {
			n++
		}
	}
	return n
}

// heldNoticeSenderMessage is a message sent by the agent slug/id to target.
func heldNoticeSenderMessage(senderSlug, senderID, target string) *messages.StructuredMessage {
	msg := messages.NewInstruction("agent:"+senderSlug, "agent:"+target, "hello")
	msg.SenderID = senderID
	return msg
}

// A held owner gets no artifact review notice; an owner that is not held
// gets one.
func TestHeldNotice_ArtifactReview(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "notice-review", "notice-review-held", "notice-review-free")
	held, free := a["notice-review-held"], a["notice-review-free"]
	holdAgentFor(t, s, held.ID, projectID, tid("notice-review-root"))
	ctx := context.Background()

	for _, owner := range []string{held.ID, free.ID} {
		require.NoError(t, srv.deliverArtifactReview(ctx, artifacts.ReviewNotice{
			ArtifactID: "a", Seq: 2, OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: owner,
			ReviewerKind: artifacts.PrincipalKindUser, ReviewerRef: "u",
		}))
	}

	assert.Zero(t, heldNoticeCount(dispatcher, held.Slug), "a held owner gets no review notice")
	require.Equal(t, 1, heldNoticeCount(dispatcher, free.Slug), "an owner that is not held gets the notice")
}

// A held agent sender gets no DELIVERY_FAILED notice from the message
// broker; a sender that is not held gets one.
func TestHeldNotice_BrokerDeliveryFailed(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	held := setupBrokerTestAgent(t, s, projectID, "notice-failed-held", "running")
	free := setupBrokerTestAgent(t, s, projectID, "notice-failed-free", "running")
	holdAgentFor(t, s, held.ID, projectID, tid("notice-failed-root"))
	proxy, dispatcher := heldBrokerProxy(t, s)
	ctx := context.Background()

	for _, sender := range []struct{ slug, id string }{{held.Slug, held.ID}, {free.Slug, free.ID}} {
		proxy.publishDeliveryFailed(ctx, projectID, "notice-failed-target",
			heldNoticeSenderMessage(sender.slug, sender.id, "notice-failed-target"), errors.New("broker refused"))
	}

	assert.Zero(t, heldNoticeCount(dispatcher, held.Slug), "a held sender gets no DELIVERY_FAILED notice")
	msgs := dispatcher.getMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, free.Slug, msgs[0].agentSlug)
	assert.Equal(t, "DELIVERY_FAILED", msgs[0].structured.Status)
}

// A held agent sender gets no DELIVERY_DEFERRED notice from the message
// broker; a sender that is not held gets one.
func TestHeldNotice_BrokerDeliveryDeferred(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	held := setupBrokerTestAgent(t, s, projectID, "notice-deferred-held", "running")
	free := setupBrokerTestAgent(t, s, projectID, "notice-deferred-free", "running")
	holdAgentFor(t, s, held.ID, projectID, tid("notice-deferred-root"))
	proxy, dispatcher := heldBrokerProxy(t, s)
	ctx := context.Background()

	for _, sender := range []struct{ slug, id string }{{held.Slug, held.ID}, {free.Slug, free.ID}} {
		proxy.publishDeliveryDeferred(ctx, "notice-deferred-target",
			heldNoticeSenderMessage(sender.slug, sender.id, "notice-deferred-target"))
	}

	assert.Zero(t, heldNoticeCount(dispatcher, held.Slug), "a held sender gets no DELIVERY_DEFERRED notice")
	msgs := dispatcher.getMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, free.Slug, msgs[0].agentSlug)
	assert.Equal(t, "DELIVERY_DEFERRED", msgs[0].structured.Status)
}

// A held agent sender gets no broadcast DELIVERY_FAILED notice; a sender
// that is not held gets one.
func TestHeldNotice_BroadcastDeliveryFailed(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "notice-bcast",
		"notice-bcast-held", "notice-bcast-free", "notice-bcast-target")
	held, free, target := a["notice-bcast-held"], a["notice-bcast-free"], a["notice-bcast-target"]
	holdAgentFor(t, s, held.ID, projectID, tid("notice-bcast-root"))
	ctx := context.Background()

	for _, sender := range []struct{ slug, id string }{{held.Slug, held.ID}, {free.Slug, free.ID}} {
		srv.publishBroadcastDeliveryFailed(ctx, target,
			heldNoticeSenderMessage(sender.slug, sender.id, target.Slug), errors.New("broker refused"))
	}

	assert.Zero(t, heldNoticeCount(dispatcher, held.Slug), "a held sender gets no broadcast DELIVERY_FAILED notice")
	assert.Zero(t, heldNoticeCount(dispatcher, target.Slug), "the broadcast target is not told")
	msgs := dispatcher.getMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, free.Slug, msgs[0].agentSlug)
	assert.Equal(t, "DELIVERY_FAILED", msgs[0].structured.Status)
}

// heldNoticeWithheld fails closed: a hold lookup fault withholds the notice
// and logs the agent and the notice kind, as does a missing hold checker.
// Through the paths, a sender whose hold cannot be read gets no
// DELIVERY_FAILED notice from the message broker and no broadcast
// DELIVERY_FAILED notice.
func TestHeldNotice_LookupFaultWithholds(t *testing.T) {
	srv, _, _, a, dispatcher, faulty, sw := fanoutHoldFaultFixture(t, "notice-fault",
		"notice-fault-sender", "notice-fault-target")
	sender, target := a["notice-fault-sender"], a["notice-fault-target"]
	faulty.agentID = sender.ID
	ctx := context.Background()
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	assert.False(t, heldNoticeWithheld(ctx, faulty, log, sender, heldNoticeDeliveryFailed),
		"an agent that is not held, with a readable hold, gets the notice")
	assert.True(t, heldNoticeWithheld(ctx, nil, log, sender, heldNoticeDeliveryFailed),
		"no hold checker withholds the notice")

	sw.Arm()
	assert.True(t, heldNoticeWithheld(ctx, faulty, log, sender, heldNoticeDeliveryFailed),
		"a hold lookup fault withholds the notice")
	out := buf.String()
	assert.Contains(t, out, "recipient hold lookup failed")
	assert.Contains(t, out, "agent_id="+sender.ID)
	assert.Contains(t, out, "notice_kind="+heldNoticeDeliveryFailed)
	assert.Contains(t, out, "log_kind=event", "a withheld notice is logged as an event, not a message record")

	srv.publishBroadcastDeliveryFailed(ctx, target,
		heldNoticeSenderMessage(sender.Slug, sender.ID, target.Slug), errors.New("broker refused"))
	proxy, proxyDispatcher := heldBrokerProxy(t, faulty)
	proxy.publishDeliveryFailed(ctx, sender.ProjectID, target.Slug,
		heldNoticeSenderMessage(sender.Slug, sender.ID, target.Slug), errors.New("broker refused"))

	assert.Zero(t, heldNoticeCount(dispatcher, sender.Slug), "no broadcast notice when the hold cannot be read")
	assert.Zero(t, heldNoticeCount(proxyDispatcher, sender.Slug), "no broker notice when the hold cannot be read")
}
