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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// auditOperationMiddleware stores the audit operation context for a request
// at ingress (remaining-audit P4-1, slice 1). applyMiddleware installs it
// directly inside RequestLog, which mints the request ID, and outside
// UnifiedAuth, so the context reaches every audit emitter downstream,
// including the middleware-level broker auth_failure event.
//
// The rule, in order:
//
//  1. A context that already carries an operation keeps it. This is the
//     re-entry case: conduit_relay_admit.go runs applyMiddleware a second
//     time on a request derived from the outer one. The inner RequestLog
//     mints a new request ID, so the correlation stays the outer operation
//     while the current request ID differs. Envelope builders then omit
//     request.id, because auditevent.Validate requires request.id to equal
//     correlation_id when present (auditevent/validate.go:185 at fce92df9).
//  2. Otherwise a server-minted request ID that passes
//     auditevent.NewOperationContext (at most 128 bytes) becomes the
//     operation's correlation ID.
//  3. Otherwise nothing is stored. There is deliberately no StartOperation
//     fallback: production always mints a request ID, and a fallback would
//     only turn excluded_correlation decision records into recorded ones in
//     constructions without RequestLog, a disposition change.
//
// Decision dispositions are unchanged: the decision mapping uses the
// operation context, else the request ID, and the two are now the same
// value.
func auditOperationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if _, ok := auditevent.OperationFromContext(ctx); ok {
			next.ServeHTTP(w, r)
			return
		}
		requestID := logging.RequestIDFromContext(ctx)
		if requestID == "" {
			next.ServeHTTP(w, r)
			return
		}
		operation, err := auditevent.NewOperationContext(requestID)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(auditevent.ContextWithOperation(ctx, operation)))
	})
}
