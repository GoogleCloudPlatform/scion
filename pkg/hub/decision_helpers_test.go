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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const testDecisionRequestID = "req-0123456789"

func decisionCtx() context.Context {
	return logging.ContextWithRequestMeta(context.Background(), &logging.RequestMeta{RequestID: testDecisionRequestID})
}

// inDomainDecisionRecord is a system-scoped project read by a user, with a
// UAT credential carrying a name and labels that must never be emitted.
func inDomainDecisionRecord() *store.DecisionAuditRecord {
	return &store.DecisionAuditRecord{
		// A non-UTC zone, as time.Now() is in production: the mapper must
		// convert to the same instant in UTC.
		Timestamp:                   time.Date(2026, 10, 9, 22, 0, 0, 0, time.FixedZone("UTC+1", 3600)),
		PrincipalKind:               "user",
		PrincipalID:                 "user-1",
		CredentialID:                "uat-1",
		CredentialType:              "uat",
		CredentialName:              "SECRET-NAME-CANARY",
		CredentialLabels:            `{"team":"LABEL-CANARY"}`,
		CredentialBoundaryKind:      "project",
		CredentialBoundaryProjectID: "proj-1",
		ResourceType:                "project",
		ResourceID:                  "proj-1",
		Permission:                  "read",
		PermissionID:                "project.read",
		Result:                      "allow",
		Reason:                      "granted by binding",
		MatchedPolicy:               "POLICY-CANARY",
		MatchedGrant:                "GRANT-CANARY",
		PolicyID:                    "POLICYID-CANARY",
		Route:                       "GET /api/v1/projects/{id}",
		Sampled:                     false,
	}
}

type panickingAuditSink struct{}

func (panickingAuditSink) Emit(context.Context, auditevent.EnvelopeV1) error { panic("sink panic") }
