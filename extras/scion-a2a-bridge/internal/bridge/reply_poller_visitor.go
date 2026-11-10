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

package bridge

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	hubstore "github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A federated user reads its reply from its own direct conversation with
// the agent, as the federated user: the hub admits it to that conversation's
// message history under the chat route (by the conversation's key), not to
// the conversation list a local caller's reader uses. Each read goes through
// federatedUserGet, the same request the access re-check makes: the caller's
// credential in the federation header and the bridge's credential as the
// bearer. The hub serves the request as the federated user and refuses it
// (401, 403 or 404) without serving it as anyone else.
//
// The page is the conversation's newest messages, across every channel,
// followed by its cursor. The anchor and the selection rule are the same as
// for a local caller (scanPages, selectReply). With the hub's conversation
// envelope switch off, the page lists only messages filed under the
// conversation key as a thread, which the bridge's send is not: the anchor
// is then never seen and the call times out.

// visitorDMReader reads a federated user's own direct conversation with an
// agent.
type visitorDMReader struct {
	bridge *Bridge
	caller *CallerIdentity
	path   string
}

// visitorDMPage mirrors the part of the hub's chat history answer the
// reader uses.
type visitorDMPage struct {
	Messages   []hubstore.Message `json:"messages"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// newVisitorDMReader returns the reader of the federated user caller's own
// conversation with agent agentID. The conversation key is derived from the
// agent ID and the caller's hub account ID; a key that cannot be derived is
// a refusal (errFederatedUserRefused), as for the access re-check.
func newVisitorDMReader(b *Bridge, caller *CallerIdentity, agentID string) (*visitorDMReader, error) {
	key, err := messages.DMConversationKey("agent", agentID, "user", caller.UserID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFederatedUserRefused, err)
	}
	return &visitorDMReader{
		bridge: b,
		caller: caller,
		path:   "/api/v1/chat/conversations/" + url.PathEscape(key) + "/messages",
	}, nil
}

// page reads one newest-first page of up to replyPollPageSize messages,
// from cursor ("" for the newest page). It is the poller's page read
// (replyPageFetch). A 200 answer of JSON null carries no page and is a
// failed read (errNoReadResult), as for a local caller's reader, never an
// empty page: an empty page is the hub's answer for a conversation with no
// messages, which this was not.
func (r *visitorDMReader) page(ctx context.Context, cursor string) ([]hubstore.Message, string, error) {
	query := url.Values{"limit": []string{strconv.Itoa(replyPollPageSize)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var page *visitorDMPage
	if err := r.bridge.federatedUserGet(ctx, r.caller, r.path, query, &page); err != nil {
		return nil, "", err
	}
	if page == nil {
		return nil, "", errNoReadResult
	}
	return page.Messages, page.NextCursor, nil
}

// check reads one message from the newest page and decodes nothing: the
// access read of the preflight and of the re-check before a reply is
// forwarded (recheckFederatedUserAccess).
func (r *visitorDMReader) check(ctx context.Context) error {
	return r.bridge.federatedUserGet(ctx, r.caller, r.path, url.Values{"limit": []string{"1"}}, nil)
}

// visitorReadOutcome maps a failed read of a federated user's conversation
// to its outcome: a hub refusal (401, 403 or 404) is the uniform refusal;
// anything else (503 and other 5xx, transport errors and timeouts, a read
// with no result) is retried until the deadline. The conversation exists once the send has
// returned, and a never-used one answers 200 with no messages, so a 404 is
// a refusal here too.
func visitorReadOutcome(err error) pollOutcome {
	if isHubRefusal(err) {
		return pollRefused
	}
	return pollError
}

// visitorReplyPreflight checks, before the send, that the federated user
// can read its conversation with the agent: the same read the access
// re-check makes (check). A refusal ends the call
// before anything is sent. Other failures are not refusals: the call goes
// on, and the poll maps them again after the send. A never-used
// conversation answers 200 with no messages, so first contact passes.
func visitorReplyPreflight(ctx context.Context, r *visitorDMReader) pollOutcome {
	if err := r.check(ctx); err != nil {
		if o := visitorReadOutcome(err); o == pollRefused {
			return o
		}
	}
	return ""
}

// newVisitorReplyPoller returns a poller for one call of a federated user,
// reading through visitor (the caller's own conversation with the agent).
// anchorID is the hub message ID the send returned; it must not be empty.
func newVisitorReplyPoller(b *Bridge, visitor *visitorDMReader, taskID, agentSlug, agentID, anchorID string, endCall func(error)) *replyPoller {
	p := newReplyPoller(b, nil, taskID, agentSlug, agentID, anchorID, "", endCall)
	p.visitor = visitor
	return p
}
