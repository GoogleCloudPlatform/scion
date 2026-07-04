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
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/discordpendinglink"
)

const discordLinkCodeTTL = 15 * time.Minute

// discordPendingLink holds state for a pending Discord account linking.
// Used only by the in-memory fallback when no Ent client is available.
type discordPendingLink struct {
	Code          string
	DiscordUserID string
	ExpiresAt     time.Time
	Status        string // "pending", "confirmed"
	UserID        string
	UserEmail     string
}

// DiscordLinkService manages pending Discord account link codes.
// When an Ent client is available (Postgres mode), pending links are stored
// in the discord_pending_links table for cross-instance consistency.
// When no Ent client is set, falls back to an in-memory map (single-node).
type DiscordLinkService struct {
	mu      sync.Mutex
	pending map[string]*discordPendingLink // in-memory fallback

	entClient *ent.Client // nil when running on SQLite

	verifyMu       sync.Mutex
	verifyLimiters map[string]*tokenBucket

	closeOnce sync.Once
	done      chan struct{}
}

// NewDiscordLinkService creates a new DiscordLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewDiscordLinkService() *DiscordLinkService {
	s := &DiscordLinkService{
		pending:        make(map[string]*discordPendingLink),
		verifyLimiters: make(map[string]*tokenBucket),
		done:           make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// SetEntClient enables DB-backed storage for pending link codes.
func (s *DiscordLinkService) SetEntClient(client *ent.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entClient = client
}

func (s *DiscordLinkService) useDB() bool {
	return s.entClient != nil
}

// RegisterCode stores a pending link code from the Discord plugin.
func (s *DiscordLinkService) RegisterCode(code, discordUserID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)

	if s.useDB() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Remove any existing pending code for this discord user.
		_, _ = s.entClient.DiscordPendingLink.Delete().
			Where(discordpendinglink.DiscordUserIDEQ(discordUserID)).
			Exec(ctx)

		err := s.entClient.DiscordPendingLink.Create().
			SetCode(upperCode).
			SetDiscordUserID(discordUserID).
			SetStatus("pending").
			SetExpiresAt(time.Now().Add(discordLinkCodeTTL)).
			Exec(ctx)
		if err != nil {
			slog.Error("Failed to register discord link code in DB", "error", err)
		}
		return
	}

	// In-memory fallback.
	for c, p := range s.pending {
		if p.DiscordUserID == discordUserID {
			delete(s.pending, c)
		}
	}
	s.pending[upperCode] = &discordPendingLink{
		Code:          upperCode,
		DiscordUserID: discordUserID,
		ExpiresAt:     time.Now().Add(discordLinkCodeTTL),
		Status:        "pending",
	}
}

// VerifyCode attempts to confirm a pending link code with the given user.
// Returns the discordUserID on success, or empty string with a reason.
func (s *DiscordLinkService) VerifyCode(code, userID, userEmail string) (discordUserID string, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)

	if s.useDB() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		row, queryErr := s.entClient.DiscordPendingLink.Query().
			Where(discordpendinglink.CodeEQ(upperCode)).
			Only(ctx)
		if queryErr != nil {
			if ent.IsNotFound(queryErr) {
				return "", "code_not_found"
			}
			slog.Error("Failed to query discord link code", "error", queryErr)
			return "", "code_not_found"
		}
		if time.Now().After(row.ExpiresAt) {
			_ = s.entClient.DiscordPendingLink.DeleteOne(row).Exec(ctx)
			return "", "code_expired"
		}
		if row.Status == "confirmed" {
			return row.DiscordUserID, ""
		}

		_, updateErr := s.entClient.DiscordPendingLink.UpdateOne(row).
			SetStatus("confirmed").
			SetUserID(userID).
			SetUserEmail(userEmail).
			Save(ctx)
		if updateErr != nil {
			slog.Error("Failed to confirm discord link code", "error", updateErr)
			return "", "code_not_found"
		}
		return row.DiscordUserID, ""
	}

	// In-memory fallback.
	p, ok := s.pending[upperCode]
	if !ok {
		return "", "code_not_found"
	}
	if time.Now().After(p.ExpiresAt) {
		delete(s.pending, upperCode)
		return "", "code_expired"
	}
	if p.Status == "confirmed" {
		return p.DiscordUserID, ""
	}
	p.Status = "confirmed"
	p.UserID = userID
	p.UserEmail = userEmail
	return p.DiscordUserID, ""
}

// GetStatusByDiscordUser returns the linking status for a given Discord user ID.
func (s *DiscordLinkService) GetStatusByDiscordUser(discordUserID string) (status, userID, userEmail string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.useDB() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		row, err := s.entClient.DiscordPendingLink.Query().
			Where(discordpendinglink.DiscordUserIDEQ(discordUserID)).
			Only(ctx)
		if err != nil {
			return "not_found", "", ""
		}
		if time.Now().After(row.ExpiresAt) {
			return "expired", "", ""
		}
		return row.Status, row.UserID, row.UserEmail
	}

	// In-memory fallback.
	for _, p := range s.pending {
		if p.DiscordUserID == discordUserID {
			if time.Now().After(p.ExpiresAt) {
				return "expired", "", ""
			}
			return p.Status, p.UserID, p.UserEmail
		}
	}
	return "not_found", "", ""
}

// ConsumePending removes a confirmed entry so it isn't returned again.
func (s *DiscordLinkService) ConsumePending(discordUserID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.useDB() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.entClient.DiscordPendingLink.Delete().
			Where(discordpendinglink.DiscordUserIDEQ(discordUserID)).
			Exec(ctx)
		return
	}

	// In-memory fallback.
	for code, p := range s.pending {
		if p.DiscordUserID == discordUserID {
			delete(s.pending, code)
			return
		}
	}
}

// AllowVerify checks whether the given IP is within the verify rate limit.
func (s *DiscordLinkService) AllowVerify(ip string) bool {
	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()

	now := time.Now()
	b, ok := s.verifyLimiters[ip]
	if !ok {
		b = &tokenBucket{
			tokens:    float64(verifyBurst) - 1,
			lastCheck: now,
		}
		s.verifyLimiters[ip] = b
		return true
	}

	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * verifyRatePerSecond
	if b.tokens > float64(verifyBurst) {
		b.tokens = float64(verifyBurst)
	}
	b.lastCheck = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Close stops the background cleanup goroutine.
func (s *DiscordLinkService) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *DiscordLinkService) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			now := time.Now()

			s.mu.Lock()
			if s.useDB() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := s.entClient.DiscordPendingLink.Delete().
					Where(discordpendinglink.ExpiresAtLT(now)).
					Exec(ctx)
				cancel()
				if err != nil {
					slog.Warn("Failed to clean up expired discord link codes", "error", err)
				}
			} else {
				for code, p := range s.pending {
					if now.After(p.ExpiresAt) {
						delete(s.pending, code)
					}
				}
			}
			s.mu.Unlock()

			// Clean up stale verify rate limiter entries.
			s.verifyMu.Lock()
			cutoff := now.Add(-30 * time.Minute)
			for ip, b := range s.verifyLimiters {
				if b.lastCheck.Before(cutoff) {
					delete(s.verifyLimiters, ip)
				}
			}
			s.verifyMu.Unlock()
		}
	}
}

// handleDiscordLink handles POST /api/v1/discord/link.
// This is called by the Discord plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleDiscordLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	var req struct {
		Code          string `json:"code"`
		DiscordUserID string `json:"discordUserId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Code == "" || req.DiscordUserID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code and discordUserId are required", nil)
		return
	}

	if s.discordLinkService == nil {
		InternalError(w)
		return
	}

	s.discordLinkService.RegisterCode(req.Code, req.DiscordUserID)

	slog.Info("Discord link code registered",
		"code_prefix", req.Code[:3]+"***",
		"discord_user_id", req.DiscordUserID,
		"broker_id", broker.BrokerID(),
	)

	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

// handleDiscordLinkVerify handles POST /api/v1/discord/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleDiscordLinkVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	// Rate limit by client IP to prevent brute-force attacks on link codes.
	if s.discordLinkService != nil {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr // fallback if no port
		}
		if !s.discordLinkService.AllowVerify(ip) {
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many verify attempts, try again later", nil)
			return
		}
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Code == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code is required", nil)
		return
	}

	if s.discordLinkService == nil {
		InternalError(w)
		return
	}

	discordUserID, errReason := s.discordLinkService.VerifyCode(req.Code, user.ID(), user.Email())
	if errReason != "" {
		switch errReason {
		case "code_not_found":
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "code not found or expired", nil)
		case "code_expired":
			writeError(w, http.StatusGone, ErrCodeNotFound, "code has expired", nil)
		default:
			InternalError(w)
		}
		return
	}

	slog.Info("Discord account linked",
		"discord_user_id", discordUserID,
		"user_id", user.ID(),
		"user_email", user.Email(),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "confirmed",
		"discordUserId": discordUserID,
		"user": map[string]string{
			"id":    user.ID(),
			"email": user.Email(),
		},
	})
}

// handleDiscordLinkStatus handles GET /api/v1/discord/link/status.
// This is called by the Discord plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleDiscordLinkStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	discordUserID := r.URL.Query().Get("discord_user_id")
	if discordUserID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "discord_user_id query parameter is required", nil)
		return
	}

	if s.discordLinkService == nil {
		InternalError(w)
		return
	}

	status, userID, userEmail := s.discordLinkService.GetStatusByDiscordUser(discordUserID)

	resp := map[string]interface{}{
		"status": status,
	}
	if status == "confirmed" {
		resp["user"] = map[string]string{
			"id":    userID,
			"email": userEmail,
		}
	}

	writeJSON(w, http.StatusOK, resp)

	// Clean up confirmed entries after sending the response so the
	// Discord plugin receives the confirmation exactly once.
	if status == "confirmed" {
		s.discordLinkService.ConsumePending(discordUserID)
	}
}
