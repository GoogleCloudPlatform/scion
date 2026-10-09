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

// perfAuditEmitter wraps the decision-audit emitter handed to the
// authorization service when server.hub.perf_trace is on. It forwards every
// record to the wrapped emitter unchanged and exactly once, and counts it by
// outcome against the request's trace. It never drops, copies, alters or
// reorders a record. It reads only the record's Result field, before the
// hand-off, because the emitter's asynchronous writer owns the record after
// it.
//
// The authorization service emits exactly one record per decision (Decide
// in authz.go), so with the default sampling rate of 1.0 the record count is
// the request's decision count. This seam lets decisions be counted without
// changing the authorization code.
type perfAuditEmitter struct {
	next DecisionAuditEmitter
}

// wrapAuditEmitterForPerfTrace returns e unchanged when enabled is false.
func wrapAuditEmitterForPerfTrace(e DecisionAuditEmitter, enabled bool) DecisionAuditEmitter {
	if !enabled || e == nil {
		return e
	}
	return perfAuditEmitter{next: e}
}

func (p perfAuditEmitter) EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord) {
	t := perfTraceFrom(ctx)
	if t == nil {
		p.next.EmitDecisionAudit(ctx, record)
		return
	}
	outcome := perfAuditOther
	if record != nil {
		switch record.Result {
		case "allow":
			outcome = perfAuditAllow
		case "deny":
			outcome = perfAuditDeny
		}
	}
	start := time.Now()
	p.next.EmitDecisionAudit(ctx, record)
	t.addAudit(outcome, time.Since(start))
}
