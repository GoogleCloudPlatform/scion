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

package artifacts

import (
	"encoding/json"
	"net/http"
)

// Route patterns the service serves, in net/http ServeMux syntax.
const (
	// RouteCollection serves the artifact collection.
	RouteCollection = "/api/v1/artifacts"
	// RouteByID serves everything under an artifact.
	RouteByID = "/api/v1/artifacts/"
	// RouteShared serves share links. Its requests authenticate by link
	// token only, so the host must mount it without requiring a session.
	RouteShared = "/api/v1/artifacts/shared/"
)

// RoutePatterns returns every pattern RegisterRoutes mounts.
func RoutePatterns() []string {
	return []string{RouteCollection, RouteByID, RouteShared}
}

// Mux is the subset of *http.ServeMux that RegisterRoutes needs.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Guard wraps the handler for pattern with the host's authentication and
// feature gating before it is mounted. The host decides what the guard does
// for each pattern; RouteShared must not require a session.
type Guard func(pattern string, handler http.Handler) http.Handler

// Service is the artifact service.
type Service struct {
	host Host
}

// NewService returns a service that identifies and authorizes callers
// through host.
func NewService(host Host) *Service {
	return &Service{host: host}
}

// Host returns the host the service was built with.
func (s *Service) Host() Host { return s.host }

// Handler returns the service's HTTP handler for every route pattern.
func (s *Service) Handler() http.Handler { return s }

// RegisterRoutes mounts the service on mux at every pattern in
// RoutePatterns, each wrapped by guard. A nil guard mounts the handler
// unwrapped.
func (s *Service) RegisterRoutes(mux Mux, guard Guard) {
	for _, pattern := range RoutePatterns() {
		var h http.Handler = s
		if guard != nil {
			h = guard(pattern, h)
		}
		mux.Handle(pattern, h)
	}
}

// ServeHTTP answers every request with 404. The service has no behaviour
// yet; later phases add the routes behind this handler.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeNotFound(w)
}

// errorResponse matches the hub's JSON error envelope so clients see one
// shape whichever side answered.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: errorBody{Code: "not_found", Message: "route not found"}})
}
