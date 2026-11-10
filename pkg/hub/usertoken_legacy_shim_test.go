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

import "time"

// Test-only token minting helpers.
//
// Production code has no way to mint a hub user token without choosing a
// session generation: it must call MintTokenPair, MintAccessToken or
// MintAccessTokenWithTTL with an explicit value. The methods below keep the
// older names available to tests only (Go allows methods to be declared in a
// package's _test.go files). They mint with session generation 0, which is
// the generation of a never-revoked user, the state the existing fixtures
// use. Tests that exercise session generations call the Mint* methods
// directly.

// GenerateTokenPair mints a token pair with session generation 0.
func (s *UserTokenService) GenerateTokenPair(userID, email, displayName, role string, clientType ClientType) (accessToken, refreshToken string, expiresIn int64, err error) {
	return s.MintTokenPair(userID, email, displayName, role, 0, clientType)
}

// GenerateAccessToken mints an access token with session generation 0.
func (s *UserTokenService) GenerateAccessToken(userID, email, displayName, role string, clientType ClientType) (string, int64, error) {
	return s.MintAccessToken(userID, email, displayName, role, 0, clientType)
}

// GenerateAccessTokenWithTTL mints an access token with session generation 0.
func (s *UserTokenService) GenerateAccessTokenWithTTL(userID, email, displayName, role string, clientType ClientType, ttl time.Duration) (string, int64, error) {
	return s.MintAccessTokenWithTTL(userID, email, displayName, role, 0, clientType, ttl)
}
