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
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SetChatIntegrationSecret stores a chat integration secret (Telegram bot token,
// Discord bot token, etc.) via the secrets backend, following the GitHub App
// pattern from handlers_github_app.go.
func (s *Server) SetChatIntegrationSecret(ctx context.Context, name, value, description, userID string) error {
	if s.secretBackend != nil {
		_, _, err := s.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name:          name,
			Value:         value,
			SecretType:    secret.TypeVariable,
			Scope:         store.ScopeHub,
			ScopeID:       s.hubID,
			Description:   description,
			InjectionMode: "as_needed",
			CreatedBy:     userID,
			UpdatedBy:     userID,
		})
		return err
	}

	sec := &store.Secret{
		ID:             fmt.Sprintf("hub-chat-%s", strings.ToLower(strings.ReplaceAll(name, "_", "-"))),
		Key:            name,
		EncryptedValue: value,
		Scope:          store.ScopeHub,
		ScopeID:        s.hubID,
		SecretType:     store.SecretTypeVariable,
		Description:    description,
		Version:        1,
		CreatedBy:      userID,
		UpdatedBy:      userID,
	}
	_, err := s.store.UpsertSecret(ctx, sec)
	return err
}

// LoadChatIntegrationSecret loads a chat integration secret from the secrets
// backend, with fallback to the database.
func (s *Server) LoadChatIntegrationSecret(ctx context.Context, name string) (string, error) {
	if s.secretBackend != nil {
		sv, err := s.secretBackend.Get(ctx, name, store.ScopeHub, s.hubID)
		if err != nil {
			return "", err
		}
		if sv == nil {
			return "", nil
		}
		return sv.Value, nil
	}

	if s.store == nil {
		return "", nil
	}
	return s.store.GetSecretValue(ctx, name, store.ScopeHub, s.hubID)
}

// HasChatIntegrationSecret checks whether a chat integration secret exists
// in either the secrets backend or the database.
func (s *Server) HasChatIntegrationSecret(ctx context.Context, name string) bool {
	if s.secretBackend != nil {
		meta, err := s.secretBackend.GetMeta(ctx, name, store.ScopeHub, s.hubID)
		return err == nil && meta != nil
	}

	if s.store == nil {
		return false
	}
	val, err := s.store.GetSecretValue(ctx, name, store.ScopeHub, s.hubID)
	return err == nil && val != ""
}

// ChatSecretDescription returns a human-readable description for a chat secret key.
func ChatSecretDescription(secretKey string) string {
	switch secretKey {
	case config.SecretTelegramBotToken:
		return "Telegram bot token"
	case config.SecretTelegramWebhookKey:
		return "Telegram webhook secret"
	case config.SecretDiscordBotToken:
		return "Discord bot token"
	case config.SecretDiscordPublicKey:
		return "Discord application public key"
	case config.SecretGChatSigningKey:
		return "Google Chat signing key"
	default:
		slog.Warn("Unknown chat secret key", "key", secretKey)
		return "Chat integration secret"
	}
}
