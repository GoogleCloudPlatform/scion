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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MaterialPurpose identifies why material selection is running. Runtime
// material reads use only the runtime-read purpose; later changes add
// launch-delivery purposes.
type MaterialPurpose string

// PurposeRuntimeRead is the only purpose runtime material reads exercise: an
// agent asking for a value by key at runtime (the bulk fetch endpoint, the
// by-key get endpoint, and the agent secret list).
const PurposeRuntimeRead MaterialPurpose = "runtime_read"

// MaterialKind identifies the kind of material a candidate represents.
// Declared here because later changes add further kinds; runtime material
// selection only ever produces MaterialKindSecret.
type MaterialKind string

// MaterialKindSecret is the only material kind runtime material reads select.
const MaterialKindSecret MaterialKind = "secret"

// GrantKind identifies the named grant that authorized a Candidate.
type GrantKind string

const (
	// GrantProjectSecretRead is the runtime project read via the token's
	// project binding (check 7). It is not a delivery grant: a runtime read
	// is never labelled with a delivery grant such as GrantProjectAssociation,
	// which delivery paths declare and use separately.
	GrantProjectSecretRead GrantKind = "project_secret_read"
	// GrantProgeny is the runtime user read grant (check 8).
	GrantProgeny GrantKind = "progeny"
)

// ProvenanceRoot identifies the human ancestor a runtime read is evaluated
// against. Runtime material reads fill Kind and ID only; Edge and Revision
// are populated once a later change resolves them from recorded delegation
// provenance.
type ProvenanceRoot struct {
	Kind     string                // always "user" for a runtime read
	ID       string                // Ancestry[0], confirmed an active user by check 5
	Edge     *store.DelegationEdge // nil for a runtime read
	Revision int                   // 0 for a runtime read
}

// TargetFacts holds store-sourced facts about the target agent. Nothing here
// comes from the presented token except what the caller passes in
// separately (subject and scopes) — every field below is read fresh from
// the store.
type TargetFacts struct {
	Agent     *store.Agent   // store.GetAgent(ident.ID()); DeletedAt zero
	ProjectID string         // Agent.ProjectID
	Ancestry  []string       // Agent.Ancestry as stored; never extended
	Root      ProvenanceRoot // ProvenanceRoot{Kind: "user", ID: Ancestry[0]} (check 3), confirmed by check 5
	Project   *store.Project // loaded by ProjectID
}

// SourceRef identifies the user or agent that authored a user-scoped item,
// for audit purposes only.
type SourceRef struct{ Kind, ID string }

// Candidate is a single key under consideration for selection.
type Candidate struct {
	Kind          MaterialKind // MaterialKindSecret for a runtime read
	Key           string
	Scope         string            // "project" | "user"
	ScopeID       string            // ProjectID | Root.ID
	Grant         GrantKind         // GrantProjectSecretRead (project, check 7) | GrantProgeny (user, check 8)
	SharingSource *SourceRef        // user scope only: {Kind: "user"|"agent", ID: meta.CreatedBy}
	Meta          secret.SecretMeta // from GetMeta; the zero value when not found (Reason says not_found)
}

// ItemResult is the outcome of evaluating a single Candidate.
type ItemResult struct {
	Candidate
	Allowed  bool
	Selected bool   // allowed AND the value was read and passed check 9
	Reason   string // a reason code below; audit only, never sent to the caller
}

// Reason codes used by runtime material reads. These are audit-only: never
// sent to the caller, and never string-matched against the underlying
// decision text.
const (
	ReasonAllowed              = "allowed"
	ReasonNotFound             = "not_found"
	ReasonDeniedByPolicy       = "denied_by_policy"
	ReasonSharingDisabled      = "sharing_disabled"
	ReasonSourceInactive       = "source_inactive"
	ReasonBackendError         = "backend_error"
	ReasonRecordChanged        = "record_changed"
	ReasonTargetUnresolved     = "target_unresolved"
	ReasonTokenProjectMismatch = "token_project_mismatch"
	ReasonMembershipRequired   = "membership_required"
	ReasonCapabilityRequired   = "capability_required"
	ReasonIdentityNotLocal     = "identity_not_local"
	ReasonInvalidScope         = "invalid_scope"
	// ReasonGitCredentialNotAllowed: the key names a GitHub credential
	// (agent.IsGitCredentialEnvKey) and the agent's applied config does not
	// allow GitHub credentials (agentAllowsGitCredentials).
	ReasonGitCredentialNotAllowed = "git_credential_not_allowed"
)

// selectRuntimeMaterial composes checks 7-9 for one candidate key in one
// scope ("project" or "user"): the per-item authorize step (check 7 project,
// check 8 user) and, when it allows, the record-race fetch (check 9). It is
// the single per-item composition that both the fetch endpoint (looped over
// its key list) and the get endpoint (a single key) call, so the two do not
// each hand-compose their own copy of "authorize, then fetch". The agent
// secret list has no per-key fetch step and its own dual-scope aggregation,
// so it composes checks 1-7/8 on its own instead of through this function.
//
// Before either check, a GitHub credential key is refused for an agent
// without the allowance (gitCredentialKeyDenied): the item is not allowed
// and no metadata or value is read, so both endpoints report it as not
// found.
//
// decisionCache must be created once per request and passed to every
// project-scope call within that request, so the check-7 decision is
// evaluated once per request rather than once per key; it is ignored for
// scope == "user".
//
// Returns the final ItemResult (Selected set only when a value was read and
// passed check 9), the fetched value (nil unless Selected), the permission
// name for the audit item ("project.secret_read" for project scope, "" for
// user scope — no permission is named for a user-scope item), and the
// check-7 Detail string (Decision.Reason verbatim; "" for user scope, which
// makes no such decision).
func (s *Server) selectRuntimeMaterial(ctx context.Context, ident AgentIdentity, facts *TargetFacts, scope, key string, decisionCache *projectDecisionCache) (item ItemResult, value *secret.SecretWithValue, permission, detail string) {
	switch scope {
	case store.ScopeProject:
		permission = "project.secret_read"
		if gitCredentialKeyDenied(facts, key) {
			item = ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Key: key, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead},
				Reason:    ReasonGitCredentialNotAllowed,
			}
			return item, nil, permission, detail
		}
		decision, decErr := s.projectReadDecision(ctx, ident, facts, decisionCache)
		if decErr != nil {
			item = ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Key: key, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead},
				Reason:    ReasonBackendError,
			}
			return item, nil, permission, detail
		}
		item, detail = s.authorizeRuntimeProjectItem(ctx, key, facts, decision)
	case store.ScopeUser:
		if gitCredentialKeyDenied(facts, key) {
			item = ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Key: key, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny},
				Reason:    ReasonGitCredentialNotAllowed,
			}
			return item, nil, permission, detail
		}
		item = s.authorizeRuntimeUserItem(ctx, facts, key)
	default:
		return ItemResult{}, nil, "", ""
	}

	if !item.Allowed {
		return item, nil, permission, detail
	}

	sv, valReason := s.fetchAuthorizedValue(ctx, item)
	if valReason != ReasonAllowed {
		item.Reason = valReason
		return item, nil, permission, detail
	}

	item.Selected = true
	return item, sv, permission, detail
}

// gitCredentialKeyDenied reports whether a runtime read of key is refused
// because key names a GitHub credential (agent.IsGitCredentialEnvKey, which
// ignores case) and the agent's applied config does not allow GitHub
// credentials (agentAllowsGitCredentials). It is decided from the key name
// and the agent record alone, before any secret metadata or value is read,
// so a refused key is reported exactly like a missing one whether or not it
// exists. A nil facts, agent record or applied config is refused.
func gitCredentialKeyDenied(facts *TargetFacts, key string) bool {
	if !agent.IsGitCredentialEnvKey(key) {
		return false
	}
	var rec *store.Agent
	if facts != nil {
		rec = facts.Agent
	}
	return !agentAllowsGitCredentials(rec)
}
