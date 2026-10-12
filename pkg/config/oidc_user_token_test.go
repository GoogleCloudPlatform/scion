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

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCProviderConfig_ValidateUserTokens(t *testing.T) {
	tests := []struct {
		name    string
		cfg     OIDCProviderConfig
		wantErr string
	}{
		{name: "zero value", cfg: OIDCProviderConfig{}},
		{name: "valid audiences and lifetime", cfg: OIDCProviderConfig{
			UserTokenAudiences: []string{"https://peer-a.example.com", "http://peer-b.internal:8080/"},
			UserTokenLifetime:  time.Hour,
		}},
		{name: "negative lifetime", cfg: OIDCProviderConfig{UserTokenLifetime: -time.Minute}, wantErr: "must not be negative"},
		{name: "lifetime over one hour", cfg: OIDCProviderConfig{UserTokenLifetime: time.Hour + time.Second}, wantErr: "exceeds the maximum"},
		{name: "lifetime with fractional seconds", cfg: OIDCProviderConfig{UserTokenLifetime: 90*time.Second + 500*time.Millisecond}, wantErr: "whole number of seconds"},
		{name: "lifetime under one second", cfg: OIDCProviderConfig{UserTokenLifetime: 500 * time.Millisecond}, wantErr: "whole number of seconds"},
		{name: "lifetime of one second", cfg: OIDCProviderConfig{UserTokenLifetime: time.Second}},
		{name: "empty audience", cfg: OIDCProviderConfig{UserTokenAudiences: []string{" "}}, wantErr: "user_token_audiences[0]: must not be empty"},
		{name: "non-http scheme", cfg: OIDCProviderConfig{UserTokenAudiences: []string{"ftp://peer.example.com"}}, wantErr: "https or http"},
		{name: "no host", cfg: OIDCProviderConfig{UserTokenAudiences: []string{"https://"}}, wantErr: "no host"},
		{name: "query", cfg: OIDCProviderConfig{UserTokenAudiences: []string{"https://peer.example.com?x=1"}}, wantErr: "without user info, query or fragment"},
		{name: "user info", cfg: OIDCProviderConfig{UserTokenAudiences: []string{"https://u@peer.example.com"}}, wantErr: "without user info, query or fragment"},
		{name: "duplicate after normalization", cfg: OIDCProviderConfig{UserTokenAudiences: []string{"https://peer.example.com", "https://peer.example.com/"}}, wantErr: "duplicate audience"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ValidateUserTokens()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestOIDCProviderConfig_AllowsUserTokenAudience(t *testing.T) {
	cfg := OIDCProviderConfig{UserTokenAudiences: []string{"https://peer.example.com/"}}
	assert.True(t, cfg.AllowsUserTokenAudience("https://peer.example.com"))
	assert.True(t, cfg.AllowsUserTokenAudience(" https://peer.example.com/ "))
	assert.False(t, cfg.AllowsUserTokenAudience("https://other.example.com"))
	assert.False(t, cfg.AllowsUserTokenAudience("https://peer.example.com/sub"))
	assert.False(t, cfg.AllowsUserTokenAudience(""))

	var empty OIDCProviderConfig
	assert.False(t, empty.AllowsUserTokenAudience("https://peer.example.com"), "an empty list allows nothing")
}

func TestOIDCProviderConfig_EffectiveUserTokenLifetime(t *testing.T) {
	assert.Equal(t, 15*time.Minute, OIDCProviderConfig{}.EffectiveUserTokenLifetime())
	assert.Equal(t, 30*time.Minute, OIDCProviderConfig{UserTokenLifetime: 30 * time.Minute}.EffectiveUserTokenLifetime())
}

func TestOIDCUserTokenSettings_V1RoundTrip(t *testing.T) {
	gc := &GlobalConfig{}
	gc.OIDC.UserTokenAudiences = []string{"https://peer.example.com"}
	gc.OIDC.UserTokenLifetime = 20 * time.Minute

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.OIDC, "user token settings alone keep the oidc section")
	assert.Equal(t, []string{"https://peer.example.com"}, v1.OIDC.UserTokenAudiences)
	assert.Equal(t, 20*time.Minute, v1.OIDC.UserTokenLifetime)

	back := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, gc.OIDC.UserTokenAudiences, back.OIDC.UserTokenAudiences)
	assert.Equal(t, gc.OIDC.UserTokenLifetime, back.OIDC.UserTokenLifetime)
}
