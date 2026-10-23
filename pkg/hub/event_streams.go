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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventstreams"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

// eventStreamKeyFromRequest builds the registry key of an events stream
// from its request: the session user, the optional browser, tab and kind
// query parameters, and a fresh connection id. A malformed id is dropped
// (the stream falls back to its connection id) and an unknown kind is
// "other"; neither refuses the stream.
func eventStreamKeyFromRequest(r *http.Request) eventstreams.Key {
	q := r.URL.Query()
	k := eventstreams.Key{
		BrowserID: eventstreams.SanitizeClientID(q.Get(eventstreams.BrowserParam)),
		TabID:     eventstreams.SanitizeClientID(q.Get(eventstreams.TabParam)),
		Kind:      eventstreams.ParseKind(q.Get(eventstreams.KindParam)),
		ConnID:    eventstreams.NewConnID(),
	}
	if u := getWebSessionUser(r.Context()); u != nil {
		k.UserID = u.UserID
	}
	return k
}

// publishMetricsSetter is implemented by the event publishers that count
// scion.hub.events.published (the in-memory and Postgres publishers).
type publishMetricsSetter interface {
	SetPublishMetrics(rec ssemetrics.Recorder)
}

// SetEventStreamMetrics wires the scion.hub.sse.* and
// scion.hub.events.published recorder (ptone/scion#5228). The current
// event publisher, and any publisher set later, counts published events on
// it. The web server's stream registry is wired separately, with
// WebServer.SetEventStreamMetrics(EventStreamMetrics()).
func (s *Server) SetEventStreamMetrics(rec ssemetrics.Recorder) {
	s.mu.Lock()
	s.eventStreamMetrics = rec
	ep := s.events
	s.mu.Unlock()
	if p, ok := ep.(publishMetricsSetter); ok {
		p.SetPublishMetrics(rec)
	}
}

// EventStreamMetrics returns the recorder set by SetEventStreamMetrics, or
// nil.
func (s *Server) EventStreamMetrics() ssemetrics.Recorder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eventStreamMetrics
}
