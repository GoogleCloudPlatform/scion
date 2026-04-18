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
//
// JSON blob types are mirrored here rather than imported from pkg/store or
// pkg/api to keep the schema package free of cycles and cheap to regenerate.
package schema

import (
	"encoding/json"
	"time"
)

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

// SharedDir mirrors api.SharedDir for the grove shared_dirs JSON column.
type SharedDir struct {
	Name        string `json:"name" yaml:"name"`
	ReadOnly    bool   `json:"read_only,omitempty" yaml:"read_only,omitempty"`
	InWorkspace bool   `json:"in_workspace,omitempty" yaml:"in_workspace,omitempty"`
}

// GitHubTokenPermissions mirrors store.GitHubTokenPermissions for the
// grove.github_permissions JSON column.
type GitHubTokenPermissions struct {
	Contents     string `json:"contents,omitempty"`
	PullRequests string `json:"pull_requests,omitempty"`
	Issues       string `json:"issues,omitempty"`
	Metadata     string `json:"metadata,omitempty"`
	Checks       string `json:"checks,omitempty"`
	Actions      string `json:"actions,omitempty"`
}

// GitHubAppGroveStatus mirrors store.GitHubAppGroveStatus for the
// grove.github_app_status JSON column.
type GitHubAppGroveStatus struct {
	State         string     `json:"state"`
	ErrorCode     string     `json:"error_code,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	LastTokenMint *time.Time `json:"last_token_mint,omitempty"`
	LastError     *time.Time `json:"last_error,omitempty"`
	LastChecked   time.Time  `json:"last_checked"`
}

// GitIdentityConfig mirrors store.GitIdentityConfig for the
// grove.git_identity JSON column.
type GitIdentityConfig struct {
	Mode  string `json:"mode"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// AgentAppliedConfig is stored as an opaque JSON blob. The Hub owns the
// canonical struct shape (store.AgentAppliedConfig); we avoid re-mirroring it
// here because the nested types pull in pkg/api and change often. Treating
// it as json.RawMessage at the Ent layer keeps the schema package stable
// while the Hub continues to marshal/unmarshal via its own type.
type AgentAppliedConfig = json.RawMessage
