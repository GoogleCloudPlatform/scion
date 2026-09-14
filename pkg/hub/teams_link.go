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
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

const teamsLinkCodeTTL = 15 * time.Minute

// teamsPendingLink holds state for a pending Teams account linking.
type teamsPendingLink struct {
	Code        string
	TeamsUserID string // Azure AD object ID
	ExpiresAt   time.Time
	Status      string // "pending", "confirmed"
	UserID      string // Scion user ID (set after verify)
	UserEmail   string // Scion user email (set after verify)
}

// TeamsLinkService manages pending Teams account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
//
// NOTE: See TelegramLinkService for why the DB-delegation + in-memory-fallback
// pattern is intentionally duplicated across the three link services.
type TeamsLinkService struct {
	mu      sync.Mutex
	pending map[string]*teamsPendingLink // in-memory fallback

	verifyLimiter linkVerifyLimiter

	// db is the optional DB-backed link code store.
	db *ChatLinkStore

	closeOnce sync.Once
	done      chan struct{}
}

// NewTeamsLinkService creates a new TeamsLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewTeamsLinkService() *TeamsLinkService {
	s := &TeamsLinkService{
		pending:       make(map[string]*teamsPendingLink),
		verifyLimiter: newLinkVerifyLimiter(),
		done:          make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// SetStore sets the database-backed link code store. When set, all code
// operations delegate to it instead of the in-memory map.
func (s *TeamsLinkService) SetStore(store *ChatLinkStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = store
}

// RegisterCode stores a pending link code from the Teams plugin.
func (s *TeamsLinkService) RegisterCode(code, teamsUserID string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.RegisterCode(ctx, code, teamsUserID, chatlinkcode.ProviderTeams, teamsLinkCodeTTL); err != nil {
			slog.Error("Teams link: DB RegisterCode failed, falling back to in-memory", "error", err)
		} else {
			return
		}
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)

	// Remove any previous pending link for this Teams user.
	for c, p := range s.pending {
		if p.TeamsUserID == teamsUserID {
			delete(s.pending, c)
		}
	}
	s.pending[upperCode] = &teamsPendingLink{
		Code:        upperCode,
		TeamsUserID: teamsUserID,
		ExpiresAt:   time.Now().Add(teamsLinkCodeTTL),
		Status:      "pending",
	}
}

// VerifyCode attempts to confirm a pending link code with the given user.
// Returns the teamsUserID on success, or empty string with a reason.
func (s *TeamsLinkService) VerifyCode(code, userID, userEmail string) (teamsUserID string, err string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		uid, reason := db.VerifyCode(ctx, code, chatlinkcode.ProviderTeams, userID, userEmail)
		if reason == "" || reason == "code_not_found" || reason == "code_expired" {
			return uid, reason
		}
		slog.Error("Teams link: DB VerifyCode failed, falling back to in-memory", "reason", reason)
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)

	p, ok := s.pending[upperCode]
	if !ok {
		return "", "code_not_found"
	}
	if time.Now().After(p.ExpiresAt) {
		delete(s.pending, upperCode)
		return "", "code_expired"
	}
	if p.Status == "confirmed" {
		return p.TeamsUserID, ""
	}
	p.Status = "confirmed"
	p.UserID = userID
	p.UserEmail = userEmail
	return p.TeamsUserID, ""
}

// GetStatusByTeamsUser returns the linking status for a given Teams user ID.
func (s *TeamsLinkService) GetStatusByTeamsUser(teamsUserID string) (status, userID, userEmail string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, uid, email := db.GetStatusByUser(ctx, chatlinkcode.ProviderTeams, teamsUserID)
		if st != "db_error" {
			return st, uid, email
		}
		slog.Error("Teams link: DB GetStatusByUser failed, falling back to in-memory")
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range s.pending {
		if p.TeamsUserID == teamsUserID {
			if time.Now().After(p.ExpiresAt) {
				return "expired", "", ""
			}
			return p.Status, p.UserID, p.UserEmail
		}
	}
	return "not_found", "", ""
}

// ConsumePending removes a confirmed entry so it isn't returned again.
func (s *TeamsLinkService) ConsumePending(teamsUserID string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		db.ConsumePending(ctx, chatlinkcode.ProviderTeams, teamsUserID)
		return
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	for code, p := range s.pending {
		if p.TeamsUserID == teamsUserID {
			delete(s.pending, code)
			return
		}
	}
}

// AllowVerify checks whether the given IP is within the verify rate limit.
func (s *TeamsLinkService) AllowVerify(ip string) bool {
	return s.verifyLimiter.Allow(ip)
}

// Close stops the background cleanup goroutine.
func (s *TeamsLinkService) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *TeamsLinkService) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			now := time.Now()

			s.mu.Lock()
			for code, p := range s.pending {
				if now.After(p.ExpiresAt) {
					delete(s.pending, code)
				}
			}
			s.mu.Unlock()

			s.verifyLimiter.Cleanup(now)
		}
	}
}

// handleTeamsLink handles POST /api/v1/teams/link.
// This is called by the Teams plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleTeamsLink(w http.ResponseWriter, r *http.Request) {
	var register func(string, string)
	if s.teamsLinkService != nil {
		register = s.teamsLinkService.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: "Teams",
		userIDField:  "teamsUserId",
		userIDLogKey: "teams_user_id",
		decode:       decodeTeamsLinkRegistration,
		register:     register,
	})
}

// handleTeamsLinkVerify handles POST /api/v1/teams/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleTeamsLinkVerify(w http.ResponseWriter, r *http.Request) {
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if s.teamsLinkService != nil {
		allowVerify = s.teamsLinkService.AllowVerify
		verify = s.teamsLinkService.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      "Teams",
		userIDResponseKey: "teamsUserId",
		userIDLogKey:      "teams_user_id",
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

// handleTeamsLinkStatus handles GET /api/v1/teams/link/status.
// This is called by the Teams plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleTeamsLinkStatus(w http.ResponseWriter, r *http.Request) {
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if s.teamsLinkService != nil {
		getStatus = s.teamsLinkService.GetStatusByTeamsUser
		consume = s.teamsLinkService.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: "teams_user_id",
		getStatus:        getStatus,
		consume:          consume,
	})
}
