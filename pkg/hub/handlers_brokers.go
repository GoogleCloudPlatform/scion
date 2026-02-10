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

// Package hub provides the Scion Hub API server.
package hub

import (
	"net/http"
	"strings"
	"time"
)

// handleBrokersEndpoint handles POST /api/v1/brokers.
// Creates a new broker registration with join token.
// Requires admin authentication.
func (s *Server) handleBrokersEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	s.createBrokerRegistration(w, r)
}

// createBrokerRegistration creates a new broker with join token.
func (s *Server) createBrokerRegistration(w http.ResponseWriter, r *http.Request) {
	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Require admin authentication
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}
	if user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Parse request
	var req CreateBrokerRegistrationRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", map[string]interface{}{
			"field": "name",
		})
		return
	}

	// Create the broker registration
	resp, err := s.brokerAuthService.CreateBrokerRegistration(r.Context(), req, user.ID())
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to create broker registration: "+err.Error(), nil)
		return
	}

	// Log audit event
	LogRegistrationEvent(r.Context(), s.auditLogger, resp.BrokerID, req.Name, user.ID(), getClientIP(r))

	writeJSON(w, http.StatusCreated, resp)
}

// handleBrokerJoin handles POST /api/v1/brokers/join.
// Completes broker registration with join token exchange.
// This is an unauthenticated endpoint - the join token serves as authentication.
func (s *Server) handleBrokerJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Parse request
	var req BrokerJoinRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.BrokerID == "" {
		ValidationError(w, "brokerId is required", map[string]interface{}{
			"field": "brokerId",
		})
		return
	}
	if req.JoinToken == "" {
		ValidationError(w, "joinToken is required", map[string]interface{}{
			"field": "joinToken",
		})
		return
	}

	// Determine hub endpoint
	hubEndpoint := s.config.HubEndpoint
	if hubEndpoint == "" {
		// Fall back to constructing from request
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		hubEndpoint = scheme + "://" + r.Host
	}

	// Complete the join
	resp, err := s.brokerAuthService.CompleteBrokerJoin(r.Context(), req, hubEndpoint)
	if err != nil {
		// Log failed join attempt
		LogJoinEvent(r.Context(), s.auditLogger, req.BrokerID, getClientIP(r), false, err.Error())

		// Determine error type and return appropriate response
		errMsg := err.Error()
		switch {
		case errMsg == "invalid join token" || errMsg == "join token does not match broker":
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidJoinToken, errMsg, nil)
		case errMsg == "join token has expired":
			writeError(w, http.StatusUnauthorized, ErrCodeExpiredJoinToken, errMsg, nil)
		default:
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to complete broker join: "+errMsg, nil)
		}
		return
	}

	// Log successful join
	LogJoinEvent(r.Context(), s.auditLogger, req.BrokerID, getClientIP(r), true, "")

	writeJSON(w, http.StatusOK, resp)
}

// handleBrokerByIDRoutes handles routes under /api/v1/brokers/{id}/...
func (s *Server) handleBrokerByIDRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract broker ID and action from path: /api/v1/brokers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/brokers/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		NotFound(w, "broker")
		return
	}

	brokerID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "rotate-secret":
		s.handleBrokerRotateSecret(w, r, brokerID)
	default:
		NotFound(w, "broker action")
	}
}

// handleBrokerRotateSecret handles POST /api/v1/brokers/{id}/rotate-secret.
// Rotates the HMAC secret for a broker.
// Requires admin authentication or broker self-rotation.
func (s *Server) handleBrokerRotateSecret(w http.ResponseWriter, r *http.Request, brokerID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Check authorization - either admin user or the broker itself
	user := GetUserIdentityFromContext(r.Context())
	broker := GetBrokerIdentityFromContext(r.Context())

	authorized := false
	if user != nil && user.Role() == "admin" {
		authorized = true
	} else if broker != nil && broker.BrokerID() == brokerID {
		authorized = true
	}

	if !authorized {
		Forbidden(w)
		return
	}

	// Parse request (optional)
	var req RotateSecretRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	// Default grace period
	gracePeriod := req.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 5 * time.Minute
	}

	// Rotate the secret
	resp, err := s.brokerAuthService.RotateBrokerSecret(r.Context(), brokerID, gracePeriod)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to rotate secret: "+err.Error(), nil)
		return
	}

	// Log audit event
	actorID := ""
	actorType := "system"
	if user != nil {
		actorID = user.ID()
		actorType = "user"
	} else if broker != nil {
		actorID = broker.BrokerID()
		actorType = "broker"
	}
	LogRotateEvent(r.Context(), s.auditLogger, brokerID, actorID, actorType, getClientIP(r))

	writeJSON(w, http.StatusOK, resp)
}
