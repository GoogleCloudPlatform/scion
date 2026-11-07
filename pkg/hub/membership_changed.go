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
	"encoding/json"
	"net/http"
)

// membershipChangedSubject is an internal event subject: a user's project
// membership changed, so the long-lived streams that user holds re-run
// their authorization (the web events stream and the agent messages
// stream). It travels on the shared EventPublisher, so streams on other
// hub instances receive it too. It is never a client subscription
// (authorizeSSESubjects refuses the hub namespace) and is never written to
// a client. A project leave publishes it after commit (ptone/scion#7494).
const membershipChangedSubject = "hub.membership.changed"

// membershipChangedEvent is the payload of membershipChangedSubject.
type membershipChangedEvent struct {
	UserID    string `json:"userId"`
	ProjectID string `json:"projectId"`
}

// publishMembershipChanged publishes membershipChangedSubject for userID
// and projectID. It does not depend on any experiment. With no event
// publisher it does nothing.
func (s *Server) publishMembershipChanged(userID, projectID string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return
	}
	events.PublishRaw(membershipChangedSubject, membershipChangedEvent{UserID: userID, ProjectID: projectID})
}

// membershipChangedNames reports whether a membershipChangedSubject event
// concerns userID. A payload that does not decode, or names no user,
// concerns every stream: the stream re-checks rather than skip a change.
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
