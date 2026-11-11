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
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

type perfRecordingEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func assertNoPerfHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for k := range h {
		assert.False(t, strings.HasPrefix(strings.ToLower(k), "x-scion-perf"), "unexpected perf header %s", k)
	}
}

func (e *perfRecordingEmitter) EmitDecisionAudit(_ context.Context, r *store.DecisionAuditRecord) {
	e.mu.Lock()
	e.records = append(e.records, r)
	e.mu.Unlock()
}

func (e *perfRecordingEmitter) take() []*store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.records
	e.records = nil
	return out
}
