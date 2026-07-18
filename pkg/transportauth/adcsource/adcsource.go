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

// Package adcsource provides a transportauth.TokenSource backed by Application
// Default Credentials via google.golang.org/api/idtoken. It lives in a
// subpackage so the lean sciontool binary never links the Google API client
// libraries.
package adcsource

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

// ADCSource implements transportauth.TokenSource using Application Default
// Credentials via google.golang.org/api/idtoken. It supports gcloud ADC login,
// SA impersonation, and JSON SA key files.
type ADCSource struct {
	audience string

	mu     sync.RWMutex
	token  string
	expiry time.Time
	ts     oauth2.TokenSource
}

// New creates an ADCSource that mints OIDC ID tokens for the given audience.
// It validates that ADC is available by creating a token source upfront.
func New(audience string) (transportauth.TokenSource, error) {
	ts, err := idtoken.NewTokenSource(context.Background(), audience)
	if err != nil {
		return nil, fmt.Errorf("ADC not available: %w", err)
	}
	return &ADCSource{audience: audience, ts: ts}, nil
}

// newTokenSourceFunc is overridable for testing.
var newTokenSourceFunc func(ctx context.Context, audience string, opts ...idtoken.ClientOption) (oauth2.TokenSource, error) = idtoken.NewTokenSource

func (s *ADCSource) Token() (string, error) {
	s.mu.RLock()
	if s.token != "" && time.Now().Before(s.expiry.Add(-transportauth.RefreshMargin)) {
		tok := s.token
		s.mu.RUnlock()
		return tok, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	if s.token != "" && time.Now().Before(s.expiry.Add(-transportauth.RefreshMargin)) {
		return s.token, nil
	}

	if s.ts == nil {
		ts, err := newTokenSourceFunc(context.Background(), s.audience)
		if err != nil {
			return "", fmt.Errorf("oidc: ADC token source: %w", err)
		}
		s.ts = ts
	}

	tok, err := s.ts.Token()
	if err != nil {
		return "", fmt.Errorf("oidc: ADC token fetch: %w", err)
	}

	idToken, ok := tok.Extra("id_token").(string)
	if !ok || idToken == "" {
		return "", fmt.Errorf("oidc: ADC token source returned no id_token")
	}

	expiry, err := transportauth.ParseTokenExpiry(idToken)
	if err != nil {
		expiry = time.Now().Add(transportauth.DefaultTTL)
	}

	s.token = idToken
	s.expiry = expiry
	return idToken, nil
}

func (s *ADCSource) SetToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiry = expiry
}

func (s *ADCSource) Expiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiry
}

// Audience returns the OIDC audience this source requests tokens for.
func (s *ADCSource) Audience() string {
	return s.audience
}
