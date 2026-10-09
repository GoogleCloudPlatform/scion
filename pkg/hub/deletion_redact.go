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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Deletion detail visibility (ptone/scion#3122).
//
// The client-facing deletion view (store.DeletionInfo) carries three fields
// that describe the hub's internals rather than the agent: the failure code,
// the error text (often a broker or store error message) and the claim
// counter. Only an unscoped local platform admin sees them. Every other
// caller gets the generic view: the same deletion object, or the same null,
// with the same state, stage, soft flag and timestamps, and with code, error
// and claim omitted. Audit records and logs are not affected; this applies to
// response bodies and published events only.

// genericDeleteFailedMessage is the message of a failed DELETE's error body
// for a caller who does not see deletion detail.
const genericDeleteFailedMessage = "agent delete failed"

// callerSeesDeletionDetail reports whether the request's caller may see the
// deletion detail fields. It is evaluated once per request, from the user
// identity on the request context only, with the same predicate
// requireAdmin uses. A missing identity, an agent or broker identity, a
// scoped or federated user, or a user who is not an admin sees the generic
// view. No request header, query parameter or body field is consulted.
func callerSeesDeletionDetail(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	user := GetUserIdentityFromContext(ctx)
	if user == nil {
		return false
	}
	return IsUnscopedLocalPlatformAdmin(user)
}

// redactDeletionForCaller returns the deletion view to send to a caller. For
// an admin it is the view unchanged; for anyone else it is a copy with Code,
// Error and Claim cleared, which their omitempty tags drop from the JSON.
// A nil view stays nil, so the presence of the deletion object never depends
// on the caller. The input is never modified.
//
// This is the only place that decides which deletion fields a caller sees;
// every response and event that carries a DeletionInfo goes through it.
func redactDeletionForCaller(info *store.DeletionInfo, isAdmin bool) *store.DeletionInfo {
	if info == nil {
		return nil
	}
	out := *info
	if !isAdmin {
		out.Code = ""
		out.Error = ""
		out.Claim = 0
	}
	return &out
}

// deletionViewForCaller computes an agent's deletion view at now and
// applies redactDeletionForCaller to it.
func deletionViewForCaller(a *store.Agent, now time.Time, isAdmin bool) *store.DeletionInfo {
	return redactDeletionForCaller(store.ComputeAgentDeletion(a, now), isAdmin)
}
