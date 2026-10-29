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

// Launch-delivery purposes (material_delivery.go). Each is authorized under
// the internal delivery credential (hubDeliveryIdentity), never under an
// agent token.
const (
	// PurposeDispatch is the material assembled for an agent start
	// (buildStartEnv at the start mint site).
	PurposeDispatch MaterialPurpose = "dispatch"
	// PurposeRestart is the material assembled for an agent restart
	// (buildStartEnv at the restart mint site).
	PurposeRestart MaterialPurpose = "restart"
	// PurposeAvailabilityProbe asks whether a key would be delivered,
	// without ever reading a value (the hub-side auth credential probe).
	PurposeAvailabilityProbe MaterialPurpose = "availability_probe"
	// PurposeProvisionCredential is the project-scope secrets the broker
	// uses as GitHub credentials to fetch the agent's gh:// skills
	// (resolveProvisionCredentials); never placed in the container.
	PurposeProvisionCredential MaterialPurpose = "provision_credential"
)

// MaterialKind identifies the kind of material a candidate represents.
// Declared here because later changes add further kinds; runtime material
// selection only ever produces MaterialKindSecret.
type MaterialKind string

// MaterialKindSecret is the only material kind runtime material reads select.
const MaterialKindSecret MaterialKind = "secret"

// MaterialKindEnvVar is a plain stored env var row, delivered at launch
// under env_var.deliver. A legacy env var row flagged secret is
// MaterialKindSecret, delivered under secret.deliver.
const MaterialKindEnvVar MaterialKind = "env_var"

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

	// Delivery association grants (material_delivery.go): a project-, hub-
	// or broker-scope item delivered at launch to an agent associated with
	// that scope. Each names the relationship rule of the same name
	// (authz_material_association.go). None is ever a runtime-read grant.
	GrantProjectAssociation GrantKind = "project_association"
	GrantHubAssociation     GrantKind = "hub_association"
	GrantBrokerAssociation  GrantKind = "broker_association"
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
		item = s.authorizeRuntimeUserItem(ctx, ident, facts, key)
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
