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
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// delegatedAgentIdentityFromContext returns the request's agent delegated
// identity, or nil when the caller is anything else.
func delegatedAgentIdentityFromContext(ctx context.Context) *DelegatedAgentIdentity {
	d, ok := GetIdentityFromContext(ctx).(*DelegatedAgentIdentity)
	if !ok || d == nil {
		return nil
	}
	return d
}

// delegatedMessageRequestRefusal returns the name of the first field of req
// an agent delegated credential may not use, or "" when req is a direct
// text message it may send. A delegated sender reaches only the route's own
// agent (.design/agent-delegation.md §12.3), so it uses the same allow-list
// as a project visitor: a text message, an instruction, chat or reply type,
// and no group recipient, thread, conversation, mention, notification
// subscription or interrupt. A refused field is named by its JSON name.
func delegatedMessageRequestRefusal(req *MessageRequest) string {
	return visitorMessageRequestRefusal(req)
}

// delegatedSenderAddress returns the stored sender address and sender ID of
// a message sent with an agent delegated credential: the actor agent,
// "agent:<slug>" and its ID (.design/agent-delegation.md §14.5). The
// issuer is never the sender. The agent's slug is read from the request's
// loaded agent row, falling back to the agent ID.
func (s *Server) delegatedSenderAddress(ctx context.Context, d *DelegatedAgentIdentity) (sender, senderID string) {
	slug := d.ID()
	if st := delegatedStateFromContext(ctx); st != nil && st.identity == d && st.agent != nil &&
		st.agent.ID == d.ID() && st.agent.Slug != "" {
		slug = st.agent.Slug
	}
	return "agent:" + slug, d.ID()
}

// delegatedSenderProjectID returns the project of the actor agent of a
// delegated send, stamped as the stored message's SenderProjectID: the
// request's loaded agent row, falling back to the identity's agent project.
func delegatedSenderProjectID(ctx context.Context, d *DelegatedAgentIdentity) string {
	if st := delegatedStateFromContext(ctx); st != nil && st.identity == d && st.agent != nil &&
		st.agent.ID == d.ID() && st.agent.ProjectID != "" {
		return st.agent.ProjectID
	}
	return d.AgentProjectID()
}

// stampDelegatedMessageAttribution records, on a message sent with an
// agent delegated credential, the grant issuer as the authorizing user and
// the grant (.design/agent-delegation.md §14.5). The sender columns stay
// the actor agent's. Any other sender leaves both empty.
func stampDelegatedMessageAttribution(ctx context.Context, msg *store.Message) {
	delegated := delegatedAgentIdentityFromContext(ctx)
	if delegated == nil || msg == nil {
		return
	}
	msg.AuthorizingUserID = delegated.AuthorizingUserID()
	msg.DelegationGrantID = delegated.GrantID()
}

// senderAgentProject returns the project of the agent that sends on the
// request in ctx: an agent caller's own project, or the actor agent's
// project for an agent delegated credential. ok is false for any other
// caller (a human sender has no sender project).
func senderAgentProject(ctx context.Context) (projectID string, ok bool) {
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		return agentIdent.ProjectID(), true
	}
	if delegated := delegatedAgentIdentityFromContext(ctx); delegated != nil {
		return delegatedSenderProjectID(ctx, delegated), true
	}
	return "", false
}

// senderAgentProjectID is senderAgentProject's project, or "" for a caller
// that is not an agent sender.
func senderAgentProjectID(ctx context.Context) string {
	projectID, _ := senderAgentProject(ctx)
	return projectID
}

// delegatedDeliveryRefusal applies to a delegated send the delivery checks
// ExecuteAgentDM applies to the actor agent's own send, in the same order and
// with the same refusals, before anything is written: dispatch availability
// (checkDispatchAvailability), then, unless delivery is deferred for a
// migrating target, the target's hold (a held target is refused like a
// suspended one; a lookup fault refuses) and its phase
// (validateAgentDeliverable). It returns nil when the send may proceed.
func (s *Server) delegatedDeliveryRefusal(ctx context.Context, target *store.Agent, deferred bool) *AgentDMError {
	if dmErr := s.checkDispatchAvailability(&AgentDMInput{TargetAgent: target}); dmErr != nil {
		return dmErr
	}
	if deferred {
		return nil
	}
	held, err := s.agentHeld(ctx, target.ID)
	if err != nil {
		return heldLookupFaultDMError()
	}
	if held {
		return heldTargetDMError(target)
	}
	return validateAgentDeliverable(target)
}
