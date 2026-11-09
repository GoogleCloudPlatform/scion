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
	"fmt"
	"time"
)

// HubChatConfig holds web chat server limits (settings server.hub.chat).
type HubChatConfig struct {
	// Search bounds web chat search (server.hub.chat.search).
	Search HubChatSearchConfig `json:"search" yaml:"search" koanf:"search"`
}

// HubChatSearchConfig bounds web chat search (ptone/scion#7586). Read at
// startup.
type HubChatSearchConfig struct {
	// Timeout is the time limit of one search, a Go duration ("" = the hub
	// default, 3s).
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty" koanf:"timeout"`
	// MaxConcurrent caps the searches running hub-wide at once (0 = the
	// default, 2). On Postgres the hub clamps it to a quarter of the main pool,
	// at least 1.
	MaxConcurrent int `json:"maxConcurrent,omitempty" yaml:"maxConcurrent,omitempty" koanf:"maxConcurrent"`
}

// IsZero reports whether no chat search limit is configured.
func (c HubChatSearchConfig) IsZero() bool { return c.Timeout == "" && c.MaxConcurrent == 0 }

// TimeoutDuration parses Timeout. "" returns 0 (the hub default applies); a
// value that does not parse, or is not positive, is an error.
func (c HubChatSearchConfig) TimeoutDuration() (time.Duration, error) {
	if c.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.chat.search.timeout %q: %w", c.Timeout, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid server.hub.chat.search.timeout %q: must be positive", c.Timeout)
	}
	return d, nil
}
