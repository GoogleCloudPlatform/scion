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
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MaterialPurpose identifies why material selection is running. F.2a uses
// only the runtime-read purpose; later slices add launch-delivery purposes.
type MaterialPurpose string

// PurposeRuntimeRead is the only purpose F.2a exercises: an agent asking for
// a value by key at runtime (P7 fetch, P8 get, and the agent secret list).
const PurposeRuntimeRead MaterialPurpose = "runtime_read"

// MaterialKind identifies the kind of material a candidate represents.
// Declared here because later slices (F.2b-h) add further kinds; F.2a only
// ever produces MaterialKindSecret.
type MaterialKind string

// MaterialKindSecret is the only material kind F.2a selects.
const MaterialKindSecret MaterialKind = "secret"

// GrantKind identifies the named grant that authorized a Candidate.
type GrantKind string

const (
	// GrantProjectSecretRead is the runtime project read via the token's
	// project binding (check 7). It is not a delivery grant, and a runtime
	// read is never labelled with a delivery grant such as
	// GrantProjectAssociation (declared by F.2d, not by F.2a).
	GrantProjectSecretRead GrantKind = "project_secret_read"
	// GrantProgeny is the runtime user read grant (check 8).
	GrantProgeny GrantKind = "progeny"
)

// ProvenanceRoot identifies the human ancestor a runtime read is evaluated
// against. F.2a fills Kind and ID only; Edge and Revision are populated once
// B.3's ResolveProvenanceRoot lands.
type ProvenanceRoot struct {
	Kind     string                // F.2a: always "user"
	ID       string                // F.2a: Ancestry[0], confirmed an active user by check 5
	Edge     *store.DelegationEdge // nil in F.2a
	Revision int                   // 0 in F.2a
}

// TargetFacts holds store-sourced facts about the target agent. Nothing here
// comes from the presented token except what the caller passes in
// separately (subject and scopes) — every field below is read fresh from
// the store.
type TargetFacts struct {
	Agent     *store.Agent   // store.GetAgent(ident.ID()); DeletedAt zero
	ProjectID string         // Agent.ProjectID
	Ancestry  []string       // Agent.Ancestry as stored; never extended
	Root      ProvenanceRoot // F.2a rule: ProvenanceRoot{Kind: "user", ID: Ancestry[0]} (check 3), confirmed by check 5
	Project   *store.Project // loaded by ProjectID
}

// SourceRef identifies the user or agent that authored a user-scoped item,
// for audit purposes only.
type SourceRef struct{ Kind, ID string }

// Candidate is a single key under consideration for selection.
type Candidate struct {
	Kind          MaterialKind // MaterialKindSecret (F.2a)
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

// Reason codes (F.2a subset). These are audit-only: never sent to the
// caller, and never string-matched against B-owned decision text (v5, O-2).
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
)
