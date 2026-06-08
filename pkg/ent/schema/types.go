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

// DelegatedFromCondition specifies a delegation source for policy matching.
type DelegatedFromCondition struct {
	PrincipalType string `json:"principalType"`
	PrincipalID   string `json:"principalId"`
}

// PolicyConditions provides optional conditional logic for policies,
// stored as JSON.
type PolicyConditions struct {
	Labels             map[string]string       `json:"labels,omitempty"`
	ValidFrom          *time.Time              `json:"validFrom,omitempty"`
	ValidUntil         *time.Time              `json:"validUntil,omitempty"`
	SourceIPs          []string                `json:"sourceIps,omitempty"`
	DelegatedFrom      *DelegatedFromCondition `json:"delegatedFrom,omitempty"`
	DelegatedFromGroup string                  `json:"delegatedFromGroup,omitempty"`
}

// LifecycleHookSelector describes which agents a lifecycle hook applies to.
// Matching is performed against attributes persisted on the agent. v1 supports
// project_id and template; label-based selection is a future enhancement and is
// intentionally omitted until agents carry persisted labels.
type LifecycleHookSelector struct {
	ProjectID string `json:"projectId,omitempty"`
	Template  string `json:"template,omitempty"`
}

// LifecycleHookAction describes the HTTP/webhook request a lifecycle hook
// performs when it fires. Stored as JSON.
type LifecycleHookAction struct {
	// Type is the action type: "http" (full authenticated request) or
	// "webhook" (unauthenticated POST; URL carries its own token).
	Type    string            `json:"type,omitempty"`
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// OnError is the failure policy: "log" (default) or "retry".
	OnError string `json:"onError,omitempty"`
	// TimeoutSeconds is the per-action timeout in seconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// AllowedUntrustedVars is the admin-curated allow-list of untrusted
	// variable names that may appear in the action body. Untrusted variables
	// used anywhere in the action are rejected unless listed here, and even
	// allow-listed variables are permitted only in the body (never URL
	// host/path, query, or headers). This field is stored in the action JSON
	// blob; no DB migration is needed.
	AllowedUntrustedVars []string `json:"allowedUntrustedVars,omitempty"`
}
