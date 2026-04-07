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

package identity

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	tokenIssuer   = "scion-hub"
	tokenAudience = "scion-hub-api"
	// impersonationTokenDuration is how long per-request impersonation tokens last.
	impersonationTokenDuration = 15 * time.Minute
)

// tokenClaims mirrors the hub's UserTokenClaims structure.
type tokenClaims struct {
	jwt.Claims
	UserID      string `json:"uid"`
	Email       string `json:"email"`
	DisplayName string `json:"name,omitempty"`
	Role        string `json:"role"`
	TokenType   string `json:"type"`
	ClientType  string `json:"client"`
}

// TokenMinter creates signed user JWTs using the hub's signing key.
type TokenMinter struct {
	signer jose.Signer
}

// NewTokenMinter creates a minter from a raw signing key (32 bytes, HS256).
func NewTokenMinter(signingKey []byte) (*TokenMinter, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: signingKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating signer: %w", err)
	}
	return &TokenMinter{signer: signer}, nil
}

// MintToken creates a signed JWT for the given user identity.
func (m *TokenMinter) MintToken(userID, email, role string, duration time.Duration) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		Claims: jwt.Claims{
			Issuer:    tokenIssuer,
			Subject:   userID,
			Audience:  jwt.Audience{tokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(duration)),
			NotBefore: jwt.NewNumericDate(now),
		},
		UserID:     userID,
		Email:      email,
		Role:       role,
		TokenType:  "access",
		ClientType: "api",
	}

	token, err := jwt.Signed(m.signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("signing token: %w", err)
	}
	return token, nil
}

// MintingAuth implements apiclient.Authenticator by minting fresh JWTs
// on demand. Tokens are cached and re-minted when they approach expiry.
type MintingAuth struct {
	minter   *TokenMinter
	userID   string
	email    string
	role     string
	duration time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewMintingAuth creates an authenticator that mints tokens for the given user.
func NewMintingAuth(minter *TokenMinter, userID, email, role string, duration time.Duration) *MintingAuth {
	return &MintingAuth{
		minter:   minter,
		userID:   userID,
		email:    email,
		role:     role,
		duration: duration,
	}
}

// ApplyAuth sets the Authorization header with a fresh or cached token.
func (a *MintingAuth) ApplyAuth(req *http.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Re-mint if token is missing or within 1 minute of expiry.
	if a.token == "" || time.Now().After(a.expires.Add(-1*time.Minute)) {
		token, err := a.minter.MintToken(a.userID, a.email, a.role, a.duration)
		if err != nil {
			return fmt.Errorf("minting auth token: %w", err)
		}
		a.token = token
		a.expires = time.Now().Add(a.duration)
	}

	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

// Refresh is a no-op; MintingAuth handles refresh transparently in ApplyAuth.
func (a *MintingAuth) Refresh() (bool, error) { return false, nil }

// ChatUserInfo holds basic chat user information for identity mapping.
type ChatUserInfo struct {
	PlatformID  string
	DisplayName string
	Email       string
}

// UserLookup retrieves chat platform user info by ID.
type UserLookup interface {
	GetUser(ctx context.Context, userID string) (*ChatUserInfo, error)
}

// Mapper handles user identity mapping between chat platforms and the Hub.
type Mapper struct {
	store     *state.Store
	hubClient hubclient.Client
	hubURL    string
	minter    *TokenMinter
	log       *slog.Logger
}

// NewMapper creates a new identity mapper.
func NewMapper(store *state.Store, hubClient hubclient.Client, hubURL string, minter *TokenMinter, log *slog.Logger) *Mapper {
	return &Mapper{
		store:     store,
		hubClient: hubClient,
		hubURL:    hubURL,
		minter:    minter,
		log:       log,
	}
}

// Resolve looks up the Hub user for a chat platform user.
// Returns the UserMapping or nil if not registered.
func (m *Mapper) Resolve(platformUserID, platform string) (*state.UserMapping, error) {
	return m.store.GetUserMapping(platformUserID, platform)
}

// AutoRegister attempts to register a chat user by matching their email
// to a Hub user. Returns the mapping if successful, nil if no match.
func (m *Mapper) AutoRegister(ctx context.Context, chatUser *ChatUserInfo, platform string) (*state.UserMapping, error) {
	if chatUser.Email == "" {
		return nil, nil
	}

	users, err := m.hubClient.Users().List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing hub users: %w", err)
	}

	for _, u := range users.Users {
		if u.Email == chatUser.Email {
			mapping := &state.UserMapping{
				PlatformUserID: chatUser.PlatformID,
				Platform:       platform,
				HubUserID:      u.ID,
				HubUserEmail:   u.Email,
				RegisteredBy:   "auto",
			}
			if err := m.store.SetUserMapping(mapping); err != nil {
				return nil, fmt.Errorf("saving user mapping: %w", err)
			}
			m.log.Info("auto-registered user",
				"platform_user_id", chatUser.PlatformID,
				"platform", platform,
				"hub_user_id", u.ID,
				"hub_email", u.Email,
			)
			return mapping, nil
		}
	}

	return nil, nil
}

// Register creates a manual user mapping.
func (m *Mapper) Register(platformUserID, platform, hubUserID, hubUserEmail string) error {
	mapping := &state.UserMapping{
		PlatformUserID: platformUserID,
		Platform:       platform,
		HubUserID:      hubUserID,
		HubUserEmail:   hubUserEmail,
		RegisteredBy:   "manual",
	}
	return m.store.SetUserMapping(mapping)
}

// Unregister removes a user mapping.
func (m *Mapper) Unregister(platformUserID, platform string) error {
	return m.store.DeleteUserMapping(platformUserID, platform)
}

// ClientFor creates a hubclient.Client authenticated as the mapped Hub user.
func (m *Mapper) ClientFor(_ context.Context, mapping *state.UserMapping) (hubclient.Client, error) {
	token, err := m.minter.MintToken(mapping.HubUserID, mapping.HubUserEmail, "member", impersonationTokenDuration)
	if err != nil {
		return nil, fmt.Errorf("minting impersonation token: %w", err)
	}

	return hubclient.New(m.hubURL, hubclient.WithBearerToken(token))
}

// ResolveOrAutoRegister tries to resolve a user, and if not found, attempts auto-registration.
func (m *Mapper) ResolveOrAutoRegister(ctx context.Context, lookup UserLookup, platformUserID, platform string) (*state.UserMapping, error) {
	mapping, err := m.Resolve(platformUserID, platform)
	if err != nil {
		return nil, err
	}
	if mapping != nil {
		return mapping, nil
	}

	chatUser, err := lookup.GetUser(ctx, platformUserID)
	if err != nil {
		m.log.Warn("failed to get chat user for auto-registration",
			"platform_user_id", platformUserID,
			"error", err,
		)
		return nil, nil
	}

	return m.AutoRegister(ctx, chatUser, platform)
}
