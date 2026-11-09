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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// membershipChangedSubject is the prefix of an internal event subject: a
// user's project membership changed, so the long-lived streams that user
// holds re-run their authorization (the web events stream and the agent
// messages stream). Each user has their own subject,
// membershipChangedSubjectFor(userID), and a stream subscribes only to its
// own user's, so other users' changes never queue on it. It travels on the
// shared EventPublisher, so streams on other hub instances receive it too.
// It is never a client subscription (authorizeSSESubjects refuses the hub
// namespace) and is never written to a client. A project leave publishes it
// after commit (ptone/scion#7494).
const membershipChangedSubject = "hub.membership.changed"

// membershipChangedSubjectFor returns userID's membership-changed subject,
// or "" for an empty userID. The last token is the lowercase hex SHA-256
// of userID: a fixed-length token of subject characters for every user ID,
// federated IDs (<issuer>:<sub>) included, with no ID text in the subject.
// The publisher and every stream subscriber use this one function.
func membershipChangedSubjectFor(userID string) string {
	if userID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(userID))
	return membershipChangedSubject + "." + hex.EncodeToString(sum[:])
}

// subscribeMembershipChanged subscribes to userID's membership-changed
// subject. With an empty userID or no publisher it returns a nil channel,
// which never becomes ready in a select, and a no-op unsubscribe.
func subscribeMembershipChanged(events EventPublisher, userID string) (<-chan Event, func()) {
	subject := membershipChangedSubjectFor(userID)
	if subject == "" || events == nil {
		return nil, func() {}
	}
	return events.Subscribe(subject)
}

// Stream kinds passed to streamAuthorizedHook.
const (
	streamKindWebEvents     = "web-events"
	streamKindAgentMessages = "agent-messages"
)

// streamAuthorizedHook, when set, is called by the web events and agent
// messages stream handlers right after their authorization check passes,
// before the stream loop starts. It is nil in production. A test sets it
// to publish a membership change in that setup window and checks that the
// stream, which subscribed to the change before authorizing, re-checks and
// closes.
var streamAuthorizedHook func(kind string)

// membershipChangedEvent is the payload of membershipChangedSubject.
type membershipChangedEvent struct {
	UserID    string `json:"userId"`
	ProjectID string `json:"projectId"`
}

// publishMembershipChanged publishes userID's membership-changed subject
// for projectID. It does not depend on any experiment. With no event
// publisher, or an empty userID, it does nothing.
func (s *Server) publishMembershipChanged(userID, projectID string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	subject := membershipChangedSubjectFor(userID)
	if events == nil || subject == "" {
		return
	}
	events.PublishRaw(subject, membershipChangedEvent{UserID: userID, ProjectID: projectID})
}

// membershipChangedNames reports whether a membership-changed event concerns
// userID. The per-user subject already selects the user; this checks the
// payload as well. A payload that does not decode, or names no user,
// concerns the stream: it re-checks rather than skip a change.
func membershipChangedNames(evt Event, userID string) bool {
	var m membershipChangedEvent
	if err := json.Unmarshal(evt.Data, &m); err != nil || m.UserID == "" {
		return true
	}
	return m.UserID == userID
}

// sseStreamStillAuthorized re-runs the subject authorization of an open
// web events stream after a membership change. Every subject must still
// be authorized; authorizeSSESubjects fails closed on a lookup error, so
// an error ends the stream. A variable only so a mutant test can skip the
// re-check and see the direct chat stream test fail.
var sseStreamStillAuthorized = func(ws *WebServer, r *http.Request, subjects []string) bool {
	return len(ws.authorizeSSESubjects(r, subjects)) == 0
}
