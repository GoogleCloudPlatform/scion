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

package transportauth

import (
	"fmt"
	"sync"
	"time"
)

// LogFunc is a printf-style logging callback.
type LogFunc func(format string, args ...interface{})

// InjectedSource holds a transport token injected by the hub via the
// dispatch payload (cold start) and refreshed via the tokens[] array on
// subsequent refresh calls.
type InjectedSource struct {
	// WarnLog, if non-nil, is called when the token is near expiry.
	WarnLog LogFunc

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
}

// NewInjectedSource creates an empty InjectedSource. Call SetToken to
// populate it.
func NewInjectedSource() *InjectedSource {
	return &InjectedSource{}
}

func (s *InjectedSource) Token() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.token == "" {
		return "", fmt.Errorf("oidc: no transport token available")
	}

	if !s.expiresAt.IsZero() && time.Now().After(s.expiresAt.Add(-RefreshMargin)) {
		if s.WarnLog != nil {
			s.WarnLog("OIDC transport token is near expiry or expired, returning anyway")
		}
	}

	return s.token, nil
}

func (s *InjectedSource) SetToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiry
}

func (s *InjectedSource) Expiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}
