// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

const (
	chatLinkCodeTTL      = 15 * time.Minute
	chatLinkStoreTimeout = 5 * time.Second
)

type pendingChatLink struct {
	Code           string
	ProviderUserID string
	ExpiresAt      time.Time
	Status         string
	UserID         string
	UserEmail      string
}

type chatLinkService struct {
	mu      sync.Mutex
	pending map[string]*pendingChatLink

	verifyLimiter linkVerifyLimiter
	db            *ChatLinkStore
	provider      chatlinkcode.Provider
	providerName  string

	closeOnce sync.Once
	done      chan struct{}
}

func newChatLinkService(provider chatlinkcode.Provider, providerName string) *chatLinkService {
	s := &chatLinkService{
		pending:       make(map[string]*pendingChatLink),
		verifyLimiter: newLinkVerifyLimiter(),
		provider:      provider,
		providerName:  providerName,
		done:          make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

func (s *chatLinkService) SetStore(store *ChatLinkStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = store
}

func (s *chatLinkService) RegisterCode(code, providerUserID string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), chatLinkStoreTimeout)
		defer cancel()
		if err := db.RegisterCode(ctx, code, providerUserID, s.provider, chatLinkCodeTTL); err != nil {
			slog.Error(s.providerName+" link: DB RegisterCode failed, falling back to in-memory", "error", err)
		} else {
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)
	for existingCode, pending := range s.pending {
		if pending.ProviderUserID == providerUserID {
			delete(s.pending, existingCode)
		}
	}
	s.pending[upperCode] = &pendingChatLink{
		Code:           upperCode,
		ProviderUserID: providerUserID,
		ExpiresAt:      time.Now().Add(chatLinkCodeTTL),
		Status:         "pending",
	}
}

func (s *chatLinkService) VerifyCode(code, userID, userEmail string) (string, string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), chatLinkStoreTimeout)
		defer cancel()
		providerUserID, reason := db.VerifyCode(ctx, code, s.provider, userID, userEmail)
		if reason == "" || reason == "code_not_found" || reason == "code_expired" {
			return providerUserID, reason
		}
		slog.Error(s.providerName+" link: DB VerifyCode failed, falling back to in-memory", "reason", reason)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	upperCode := strings.ToUpper(code)
	pending, ok := s.pending[upperCode]
	if !ok {
		return "", "code_not_found"
	}
	if time.Now().After(pending.ExpiresAt) {
		delete(s.pending, upperCode)
		return "", "code_expired"
	}
	if pending.Status == "confirmed" {
		return pending.ProviderUserID, ""
	}

	pending.Status = "confirmed"
	pending.UserID = userID
	pending.UserEmail = userEmail
	return pending.ProviderUserID, ""
}

func (s *chatLinkService) GetStatusByUser(providerUserID string) (status, userID, userEmail string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), chatLinkStoreTimeout)
		defer cancel()
		status, userID, userEmail := db.GetStatusByUser(ctx, s.provider, providerUserID)
		if status != "db_error" {
			return status, userID, userEmail
		}
		slog.Error(s.providerName + " link: DB GetStatusByUser failed, falling back to in-memory")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, pending := range s.pending {
		if pending.ProviderUserID == providerUserID {
			if time.Now().After(pending.ExpiresAt) {
				return "expired", "", ""
			}
			return pending.Status, pending.UserID, pending.UserEmail
		}
	}
	return "not_found", "", ""
}

func (s *chatLinkService) ConsumePending(providerUserID string) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), chatLinkStoreTimeout)
		defer cancel()
		db.ConsumePending(ctx, s.provider, providerUserID)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for code, pending := range s.pending {
		if pending.ProviderUserID == providerUserID {
			delete(s.pending, code)
			return
		}
	}
}

func (s *chatLinkService) AllowVerify(ip string) bool {
	return s.verifyLimiter.Allow(ip)
}

func (s *chatLinkService) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *chatLinkService) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for code, pending := range s.pending {
				if now.After(pending.ExpiresAt) {
					delete(s.pending, code)
				}
			}
			s.mu.Unlock()
			s.verifyLimiter.Cleanup(now)
		}
	}
}
