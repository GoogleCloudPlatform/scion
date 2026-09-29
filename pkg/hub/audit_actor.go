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
	"encoding/json"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// E.2a: consolidated mutation-audit actor helper (plan §3.3).
//
// Before this file, four independent copies of "actor from context"
// extraction existed: Server.emitMutationAudit (audit_authz.go),
// UserAccessTokenService.createAuditRecord (useraccesstoken.go),
// ProjectMembershipService.createAuditRecord (project_membership_service.go),
// ProjectDeletionService.createAuditRecord (project_deletion_service.go), and
// Server.buildAuditActorFromContext (handlers_users_core.go), each
// hand-rolling the same principal/credential extraction and each missing the
// E.2a credential snapshot, correlation ID, and executor fields. All five now
// build an AuditActor through auditActorFromContext and apply it with
// ApplyActor.
// ---------------------------------------------------------------------------

// AuditActor is the consolidated actor/credential/correlation snapshot
// recorded on a mutation audit record. It mirrors (and, via ApplyActor, feeds
// directly into) the actor-shaped fields of store.MutationAuditRecord.
type AuditActor struct {
	PrincipalKind  string
	PrincipalID    string
	CredentialKind string
	CredentialID   string

	// CredentialName/CredentialBoundaryKind/CredentialBoundaryProjectID/
	// CredentialLabels are E.1's descriptive decoration, snapshotted at audit
	// time (not a reference — see plan §3.2 "audits outlive tokens"). Empty
	// for credentials E.1 does not decorate (non-UAT).
	CredentialName              string
	CredentialBoundaryKind      string
	CredentialBoundaryProjectID string
	CredentialLabels            string // bounded JSON object, "" when absent

	// CorrelationID is the request ID shared with the request log and
	// decision audit for the same request (plan §3.1(4)).
	CorrelationID string

	// ExecutorKind/ExecutorID identify what is currently executing, as
	// distinct from the initiating principal/credential above (plan §3.5).
	// Empty for an ordinary live request.
	ExecutorKind string
	ExecutorID   string
}

// auditActorFromContext builds the actor/credential/correlation snapshot for
// mutation audit from request context. This is the single extraction point
// plan §3.3 names; every mutation-audit writer reaches it through
// ApplyActor.
func auditActorFromContext(ctx context.Context) AuditActor {
	var actor AuditActor

	if identity := GetIdentityFromContext(ctx); identity != nil {
		actor.PrincipalKind = identity.Type()
		actor.PrincipalID = identity.ID()
	}

	cred := GetCredentialContextFromContext(ctx)
	if cred.Kind != "" {
		actor.CredentialKind = string(cred.Kind)
		actor.CredentialID = cred.ID
	}
	if cred.Decoration != nil {
		actor.CredentialName = sanitizeForLog(cred.Decoration.TokenName, uatMaxNameBytes)
		actor.CredentialBoundaryKind = cred.Decoration.Boundary.Kind
		actor.CredentialBoundaryProjectID = cred.Decoration.Boundary.ProjectID
		actor.CredentialLabels = boundedLabelsJSON(cred.Decoration.Labels)
	}

	actor.CorrelationID = logging.RequestIDFromContext(ctx)

	if ec, ok := ExecutorContextFromContext(ctx); ok {
		actor.ExecutorKind = ec.Kind
		actor.ExecutorID = ec.ID
	}

	return actor
}

// ApplyActor copies the AuditActor snapshot onto a MutationAuditRecord's
// actor/credential/correlation/executor fields, filling only fields the
// caller has not already set explicitly. This preserves existing callers
// that pre-populate specific actor fields (e.g. attributing a mutation to a
// resource's original creator rather than the live request's identity).
func (a AuditActor) ApplyActor(record *store.MutationAuditRecord) {
	if record.ActorPrincipalKind == "" {
		record.ActorPrincipalKind = a.PrincipalKind
	}
	if record.ActorPrincipalID == "" {
		record.ActorPrincipalID = a.PrincipalID
	}
	if record.ActorCredentialID == "" {
		record.ActorCredentialID = a.CredentialID
	}
	if record.ActorCredentialType == "" {
		record.ActorCredentialType = a.CredentialKind
	}
	if record.CredentialName == "" {
		record.CredentialName = a.CredentialName
	}
	if record.CredentialBoundaryKind == "" {
		record.CredentialBoundaryKind = a.CredentialBoundaryKind
	}
	if record.CredentialBoundaryProjectID == "" {
		record.CredentialBoundaryProjectID = a.CredentialBoundaryProjectID
	}
	if record.CredentialLabels == "" {
		record.CredentialLabels = a.CredentialLabels
	}
	if record.CorrelationID == "" {
		record.CorrelationID = a.CorrelationID
	}
	if record.ExecutorKind == "" {
		record.ExecutorKind = a.ExecutorKind
	}
	if record.ExecutorID == "" {
		record.ExecutorID = a.ExecutorID
	}
}

// boundedLabelsJSON renders a credential decoration's labels as a JSON
// object for audit snapshotting. E.1's label schema already bounds the
// serialized result well under 1KiB (at most 8 labels, each key ≤32 bytes
// and value ≤64 bytes — see credential_decoration.go), so no additional
// truncation is applied here; a legacy row written directly to the store
// could in principle exceed that, in which case this still returns valid
// (if larger) JSON rather than silently dropping the audit trail.
func boundedLabelsJSON(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
