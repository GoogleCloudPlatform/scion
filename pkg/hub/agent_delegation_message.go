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
