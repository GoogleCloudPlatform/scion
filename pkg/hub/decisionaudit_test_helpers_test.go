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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// capturingAuditEmitter records every DecisionAuditRecord it receives, for
// tests that need to inspect audit content rather than merely count calls.
//
// This type carries no sqlite dependency of its own, so it lives in a file
// without a no_sqlite build constraint: a caller under either build
// configuration must be able to resolve it. It moved here out of
// authz_broker_obo_test.go (which does carry the !no_sqlite constraint for
// its other, store-backed tests) so that identity_classification_test.go,
// which has no such constraint, keeps resolving it under -tags no_sqlite.
type capturingAuditEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (e *capturingAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, record)
}

func (e *capturingAuditEmitter) last() *store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.records) == 0 {
		return nil
	}
	return e.records[len(e.records)-1]
}
