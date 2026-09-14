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

const discordLinkCodeTTL = 15 * time.Minute

// discordPendingLink holds state for a pending Discord account linking.
type discordPendingLink struct {
	Code          string
	DiscordUserID string
	ExpiresAt     time.Time
	Status        string // "pending", "confirmed"
	UserID        string
	UserEmail     string
}

// DiscordLinkService manages pending Discord account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
//
// NOTE: See TelegramLinkService for why the DB-delegation + in-memory-fallback
// pattern is intentionally duplicated across the three link services.
type DiscordLinkService struct {
	mu      sync.Mutex
	pending map[string]*discordPendingLink // in-memory fallback

	verifyLimiter linkVerifyLimiter

	// db is the optional DB-backed link code store.
	db *ChatLinkStore

	closeOnce sync.Once
	done      chan struct{}
}

// NewDiscordLinkService creates a new DiscordLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewDiscordLinkService() *DiscordLinkService {
	s := &DiscordLinkService{
		pending:       make(map[string]*discordPendingLink),
		verifyLimiter: newLinkVerifyLimiter(),
		done:          make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// SetStore sets the database-backed link code store. When set, all code
// operations delegate to it instead of the in-memory map.
func (s *DiscordLinkService) SetStore(store *ChatLinkStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = store
}

// RegisterCode stores a pending link code from the Discord plugin.
func (s *DiscordLinkService) RegisterCode(code, discordUserID string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.RegisterCode(ctx, code, discordUserID, chatlinkcode.ProviderDiscord, discordLinkCodeTTL); err != nil {
			slog.Error("Discord link: DB RegisterCode failed, falling back to in-memory", "error", err)
		} else {
			return
		}
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)

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
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		uid, reason := db.VerifyCode(ctx, code, chatlinkcode.ProviderDiscord, userID, userEmail)
		if reason == "" || reason == "code_not_found" || reason == "code_expired" {
			return uid, reason
		}
		slog.Error("Discord link: DB VerifyCode failed, falling back to in-memory", "reason", reason)
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
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, uid, email := db.GetStatusByUser(ctx, chatlinkcode.ProviderDiscord, discordUserID)
		if st != "db_error" {
			return st, uid, email
		}
		slog.Error("Discord link: DB GetStatusByUser failed, falling back to in-memory")
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

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
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		db.ConsumePending(ctx, chatlinkcode.ProviderDiscord, discordUserID)
		return
	}

	// In-memory fallback.
	s.mu.Lock()
	defer s.mu.Unlock()

	for code, p := range s.pending {
		if p.DiscordUserID == discordUserID {
			delete(s.pending, code)
			return
		}
	}
}

// AllowVerify checks whether the given IP is within the verify rate limit.
func (s *DiscordLinkService) AllowVerify(ip string) bool {
	return s.verifyLimiter.Allow(ip)
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

// handleDiscordLink handles POST /api/v1/discord/link.
// This is called by the Discord plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleDiscordLink(w http.ResponseWriter, r *http.Request) {
	var register func(string, string)
	if s.discordLinkService != nil {
		register = s.discordLinkService.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: "Discord",
		userIDField:  "discordUserId",
		userIDLogKey: "discord_user_id",
		decode:       decodeDiscordLinkRegistration,
		register:     register,
	})
}

// handleDiscordLinkVerify handles POST /api/v1/discord/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleDiscordLinkVerify(w http.ResponseWriter, r *http.Request) {
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if s.discordLinkService != nil {
		allowVerify = s.discordLinkService.AllowVerify
		verify = s.discordLinkService.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      "Discord",
		userIDResponseKey: "discordUserId",
		userIDLogKey:      "discord_user_id",
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

// handleDiscordLinkStatus handles GET /api/v1/discord/link/status.
// This is called by the Discord plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleDiscordLinkStatus(w http.ResponseWriter, r *http.Request) {
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if s.discordLinkService != nil {
		getStatus = s.discordLinkService.GetStatusByDiscordUser
		consume = s.discordLinkService.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: "discord_user_id",
		getStatus:        getStatus,
		consume:          consume,
	})
}
