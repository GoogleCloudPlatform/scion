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

// Package schema defines the Ent ORM schemas for Scion principal and
// authorization entities.
package schema

import "time"

// UserPreferences holds user-configurable preferences, stored as JSON.
type UserPreferences struct {
	DefaultTemplate string `json:"defaultTemplate,omitempty"`
	DefaultProfile  string `json:"defaultProfile,omitempty"`
	Theme           string `json:"theme,omitempty"`
}

// PolicyConditions provides optional conditional logic for policies,
// stored as JSON.
type PolicyConditions struct {
	Labels     map[string]string `json:"labels,omitempty"`
	ValidFrom  *time.Time        `json:"validFrom,omitempty"`
	ValidUntil *time.Time        `json:"validUntil,omitempty"`
	SourceIPs  []string          `json:"sourceIps,omitempty"`
}
