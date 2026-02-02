// Package hub provides the Scion Hub API server.
package hub

import (
	"net/http"
	"strings"
	"time"
)

// handleHostsEndpoint handles POST /api/v1/hosts.
// Creates a new host registration with join token.
// Requires admin authentication.
func (s *Server) handleHostsEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	s.createHostRegistration(w, r)
}

// createHostRegistration creates a new host with join token.
func (s *Server) createHostRegistration(w http.ResponseWriter, r *http.Request) {
	// Check if host auth service is available
	if s.hostAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"host authentication service not configured", nil)
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
	var req CreateHostRegistrationRequest
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

	// Create the host registration
	resp, err := s.hostAuthService.CreateHostRegistration(r.Context(), req, user.ID())
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to create host registration: "+err.Error(), nil)
		return
	}

	// Log audit event
	LogRegistrationEvent(r.Context(), s.auditLogger, resp.HostID, req.Name, user.ID(), getClientIP(r))

	writeJSON(w, http.StatusCreated, resp)
}

// handleHostJoin handles POST /api/v1/hosts/join.
// Completes host registration with join token exchange.
// This is an unauthenticated endpoint - the join token serves as authentication.
func (s *Server) handleHostJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Check if host auth service is available
	if s.hostAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"host authentication service not configured", nil)
		return
	}

	// Parse request
	var req HostJoinRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.HostID == "" {
		ValidationError(w, "hostId is required", map[string]interface{}{
			"field": "hostId",
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
	resp, err := s.hostAuthService.CompleteHostJoin(r.Context(), req, hubEndpoint)
	if err != nil {
		// Log failed join attempt
		LogJoinEvent(r.Context(), s.auditLogger, req.HostID, getClientIP(r), false, err.Error())

		// Determine error type and return appropriate response
		errMsg := err.Error()
		switch {
		case errMsg == "invalid join token" || errMsg == "join token does not match host":
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidJoinToken, errMsg, nil)
		case errMsg == "join token has expired":
			writeError(w, http.StatusUnauthorized, ErrCodeExpiredJoinToken, errMsg, nil)
		default:
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to complete host join: "+errMsg, nil)
		}
		return
	}

	// Log successful join
	LogJoinEvent(r.Context(), s.auditLogger, req.HostID, getClientIP(r), true, "")

	writeJSON(w, http.StatusOK, resp)
}

// handleHostByIDRoutes handles routes under /api/v1/hosts/{id}/...
func (s *Server) handleHostByIDRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract host ID and action from path: /api/v1/hosts/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/hosts/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		NotFound(w, "host")
		return
	}

	hostID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "rotate-secret":
		s.handleHostRotateSecret(w, r, hostID)
	default:
		NotFound(w, "host action")
	}
}

// handleHostRotateSecret handles POST /api/v1/hosts/{id}/rotate-secret.
// Rotates the HMAC secret for a host.
// Requires admin authentication or host self-rotation.
func (s *Server) handleHostRotateSecret(w http.ResponseWriter, r *http.Request, hostID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Check if host auth service is available
	if s.hostAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"host authentication service not configured", nil)
		return
	}

	// Check authorization - either admin user or the host itself
	user := GetUserIdentityFromContext(r.Context())
	host := GetHostIdentityFromContext(r.Context())

	authorized := false
	if user != nil && user.Role() == "admin" {
		authorized = true
	} else if host != nil && host.HostID() == hostID {
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
	resp, err := s.hostAuthService.RotateHostSecret(r.Context(), hostID, gracePeriod)
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
	} else if host != nil {
		actorID = host.HostID()
		actorType = "host"
	}
	LogRotateEvent(r.Context(), s.auditLogger, hostID, actorID, actorType, getClientIP(r))

	writeJSON(w, http.StatusOK, resp)
}
